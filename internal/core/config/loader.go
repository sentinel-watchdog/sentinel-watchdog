package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"
)

// maxModuleFiles bounds the number of files read from one module directory.
const maxModuleFiles = 1000

// LoadOptions controls where and how configuration is read.
type LoadOptions struct {
	// MainFile is the central file; required. Module directories are its
	// siblings: <dir of MainFile>/<module name>/.
	MainFile string
	// Modules lists every module name this binary knows, with its
	// availability. The daemon derives it from its module registry. A
	// name under `modules` that is not listed here is an error.
	Modules map[string]ModuleAvailability
	// IncludeDisabled also reads the directories of available modules that
	// are disabled, for `sentinelctl validate --all`. Their ModuleConfig
	// has Enabled false.
	IncludeDisabled bool
	// Lookup resolves ${VARIABLE} references; defaults to os.LookupEnv.
	Lookup LookupEnv
}

// centralDoc is the YAML shape of the central file, without `version`.
type centralDoc struct {
	Daemon        Daemon               `yaml:"daemon"`
	Notifications Notifications        `yaml:"notifications"`
	Modules       map[string]yaml.Node `yaml:"modules"`
}

// Load reads, decodes, defaults and validates the central file, then reads
// the directory of every enabled module. Module content is not validated
// here: each module validates its own ModuleConfig.
//
// Every failure is reported as a *ValidationError listing all problems.
func Load(opts LoadOptions) (*Config, error) {
	if opts.MainFile == "" {
		return nil, errors.New("config: main file path is empty")
	}
	if opts.Lookup == nil {
		opts.Lookup = os.LookupEnv
	}
	l := &loader{opts: opts, euid: os.Geteuid(), baseDir: filepath.Dir(opts.MainFile)}
	cfg := l.load()
	if len(l.errs) > 0 {
		return nil, &ValidationError{Problems: l.errs, Warnings: l.warns}
	}
	cfg.Warnings = l.warns
	return cfg, nil
}

type loader struct {
	opts    LoadOptions
	euid    int
	baseDir string
	errs    []Problem
	warns   []Problem
}

func (l *loader) errorf(file, path, format string, args ...any) {
	l.errs = append(l.errs, Problem{File: file, Path: path, Message: fmt.Sprintf(format, args...)})
}

func (l *loader) warnf(file, path, format string, args ...any) {
	l.warns = append(l.warns, Problem{File: file, Path: path, Message: fmt.Sprintf(format, args...)})
}

func (l *loader) load() *Config {
	cfg := &Config{}
	// The configuration directory decides which module directories may
	// appear: it must be as protected as the files themselves.
	l.checkDir(l.baseDir)

	main := l.opts.MainFile
	root, ok := l.readRoot(main)
	if !ok {
		return cfg
	}
	cfg.Files = append(cfg.Files, main)

	var doc centralDoc
	if err := (Section{File: main, node: without(root, "version")}).Decode(&doc); err != nil {
		l.errs = append(l.errs, ProblemsOf(err, main, "")...)
		return cfg
	}
	cfg.Daemon, cfg.Notifications = doc.Daemon, doc.Notifications
	applyDefaults(cfg)
	warns, errs := validateCentral(main, cfg)
	l.warns = append(l.warns, warns...)
	l.errs = append(l.errs, errs...)

	read := map[string]bool{}
	for _, name := range sortedKeys(doc.Modules) {
		node := doc.Modules[name]
		mc, ok := l.module(name, &node)
		if !ok {
			continue
		}
		if mc.Availability == ModuleAvailable && (mc.Enabled || l.opts.IncludeDisabled) {
			l.readModuleDir(&mc, cfg)
			read[name] = true
		}
		cfg.Modules = append(cfg.Modules, mc)
	}

	// Rule 3: directories of modules that are not read are reported, so
	// an operator is not surprised that a file there has no effect.
	for _, name := range sortedKeys(l.opts.Modules) {
		dir := filepath.Join(l.baseDir, name)
		if info, err := os.Stat(dir); err == nil && info.IsDir() && !read[name] {
			cfg.IgnoredDirs = append(cfg.IgnoredDirs, dir)
		}
	}
	return cfg
}

// readRoot reads and parses one file after checking its ownership and
// version. It reports problems itself and returns ok false on failure.
func (l *loader) readRoot(path string) (*yaml.Node, bool) {
	data, err := readFile(path, func(info os.FileInfo) error { return checkOwnership(info, l.euid) })
	if err != nil {
		l.errorf(path, "", "%s", fileError(err))
		return nil, false
	}
	root, err := parseDocument(data, l.opts.Lookup)
	if err != nil {
		l.errs = append(l.errs, problemsFromYAML(path, err)...)
		return nil, false
	}
	if err := checkVersion(root); err != nil {
		l.errorf(path, "version", "%s", err)
		return nil, false
	}
	return root, true
}

// module interprets one entry under `modules` (rules 1, 2 and 5).
func (l *loader) module(name string, node *yaml.Node) (ModuleConfig, bool) {
	main := l.opts.MainFile
	path := "modules." + name
	availability, known := l.opts.Modules[name]
	if !known {
		l.errorf(main, path, "unknown module %q (known modules: %s)", name, knownModules(l.opts.Modules))
		return ModuleConfig{}, false
	}

	switch {
	case node.Kind == yaml.ScalarNode && node.ShortTag() == "!!null":
		node = nil // `supervisor:` with nothing after it: an empty block
	case node.Kind != yaml.MappingNode:
		l.errorf(main, path, "line %d: must be a mapping such as {enabled: true}", node.Line)
		return ModuleConfig{}, false
	}

	enabled := false
	if v, ok := lookupKey(node, "enabled"); ok {
		if v.Kind != yaml.ScalarNode || v.ShortTag() != "!!bool" || v.Decode(&enabled) != nil {
			l.errorf(main, path+".enabled", "line %d: must be true or false", v.Line)
			return ModuleConfig{}, false
		}
	}

	if enabled {
		switch availability {
		case ModulePlanned:
			l.errorf(main, path, "module %q is planned but not implemented in this release", name)
		case ModuleNotBuilt:
			l.errorf(main, path, "module %q is not built into this binary", name)
		}
	}

	return ModuleConfig{
		Name:         name,
		Enabled:      enabled,
		Availability: availability,
		Central:      Section{File: main, node: without(node, "enabled")},
		Dir:          filepath.Join(l.baseDir, name),
	}, true
}

// readModuleDir reads the files of a module directory (rules 4, 6 and 8).
func (l *loader) readModuleDir(mc *ModuleConfig, cfg *Config) {
	info, err := os.Stat(mc.Dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return // the module decides whether it needs a directory (rule 4)
	case err != nil:
		l.errorf(mc.Dir, "", "%s", fileError(err))
		return
	case !info.IsDir():
		l.errorf(mc.Dir, "", "is not a directory")
		return
	}
	mc.DirExists = true
	if !l.checkDir(mc.Dir) {
		return
	}

	entries, err := os.ReadDir(mc.Dir) // sorted by file name
	if err != nil {
		l.errorf(mc.Dir, "", "%s", fileError(err))
		return
	}
	var settingsIn []string
	files := 0
	for _, e := range entries {
		name := e.Name()
		path := filepath.Join(mc.Dir, name)
		if strings.HasPrefix(name, ".") {
			continue
		}
		info, err := os.Stat(path) // follows symlinks; readFile checks the opened file again
		if err != nil {
			l.errorf(path, "", "%s", fileError(err))
			continue
		}
		if info.IsDir() {
			l.warnf(path, "", "subdirectories are not read; move the files into %s", mc.Dir)
			continue
		}
		if ext := filepath.Ext(name); ext != ".yaml" && ext != ".yml" {
			continue
		}
		if files++; files > maxModuleFiles {
			l.errorf(mc.Dir, "", "more than %d configuration files", maxModuleFiles)
			return
		}
		root, ok := l.readRoot(path)
		if !ok {
			continue
		}
		section := Section{File: path, node: without(root, "version")}
		if section.Has("settings") {
			settingsIn = append(settingsIn, path)
		}
		mc.Files = append(mc.Files, section)
		cfg.Files = append(cfg.Files, path)
	}
	if len(settingsIn) > 1 {
		l.errorf(mc.Dir, "modules."+mc.Name, "`settings` may appear in only one file of the module directory, found in: %s",
			strings.Join(settingsIn, ", "))
	}
}

// checkDir applies the ownership rule to a directory and reports problems.
func (l *loader) checkDir(dir string) bool {
	info, err := os.Stat(dir)
	if err != nil {
		l.errorf(dir, "", "%s", fileError(err))
		return false
	}
	if err := checkOwnership(info, l.euid); err != nil {
		l.errorf(dir, "", "directory %s", err)
		return false
	}
	return true
}

// fileError strips the path that os errors repeat; Problem.File has it.
func fileError(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Op + ": " + pe.Err.Error()
	}
	return err.Error()
}

func knownModules(m map[string]ModuleAvailability) string {
	if len(m) == 0 {
		return "none in this build"
	}
	names := make([]string, 0, len(m))
	for _, n := range sortedKeys(m) {
		names = append(names, fmt.Sprintf("%s (%s)", n, m[n]))
	}
	return strings.Join(names, ", ")
}
