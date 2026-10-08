package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Limits that keep a load bounded whatever the directories contain.
const (
	// maxModuleFiles bounds the number of files read from one module directory.
	maxModuleFiles = 1000
	// maxTotalBytes bounds the bytes read across the whole load.
	maxTotalBytes = 16 << 20
)

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
	// are disabled or not named in the central file, for `sentinelctl
	// validate --all`. Their ModuleConfig has Enabled false.
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
// The configuration directory is opened once as an os.Root after its
// parents have been checked: every later file and directory is resolved
// relative to that descriptor, and symbolic links cannot leave it.
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
	errs, warns := redactProblems(l.errs, l.secrets), redactProblems(l.warns, l.secrets)
	if len(errs) > 0 {
		return nil, &ValidationError{Problems: errs, Warnings: warns}
	}
	cfg.Warnings = warns
	return cfg, nil
}

type loader struct {
	opts      LoadOptions
	euid      int
	baseDir   string
	bytesRead int
	errs      []Problem
	warns     []Problem
	// secrets are the environment values expanded in any file read.
	secrets []string
}

func (l *loader) errorf(file, path, format string, args ...any) {
	l.errs = append(l.errs, Problem{File: file, Path: path, Message: fmt.Sprintf(format, args...)})
}

func (l *loader) warnf(file, path, format string, args ...any) {
	l.warns = append(l.warns, Problem{File: file, Path: path, Message: fmt.Sprintf(format, args...)})
}

func (l *loader) load() *Config {
	cfg := &Config{}
	root, ok := l.openConfigDir()
	if !ok {
		return cfg
	}
	defer root.Close()

	main := l.opts.MainFile
	rootNode, secrets, ok := l.readRoot(root, filepath.Base(main), main)
	if !ok {
		return cfg
	}
	cfg.Files = append(cfg.Files, main)

	var doc centralDoc
	if err := (Section{File: main, node: without(rootNode, "version"), secrets: secrets}).Decode(&doc); err != nil {
		l.errs = append(l.errs, ProblemsOf(err, main, "")...)
		return cfg
	}
	cfg.Daemon, cfg.Notifications = doc.Daemon, doc.Notifications
	applyDefaults(cfg)
	warns, errs := validateCentral(main, cfg)
	l.warns = append(l.warns, warns...)
	l.errs = append(l.errs, errs...)

	configured := map[string]ModuleConfig{}
	for _, name := range sortedKeys(doc.Modules) {
		node := doc.Modules[name]
		if mc, ok := l.module(name, &node, secrets); ok {
			configured[name] = mc
		}
	}
	// validate --all: available modules not named in the central file are
	// implicitly disabled, and their directories are checked too.
	if l.opts.IncludeDisabled {
		for name, a := range l.opts.Modules {
			if _, named := configured[name]; !named && a == ModuleAvailable {
				configured[name] = ModuleConfig{Name: name, Availability: a, Dir: filepath.Join(l.baseDir, name)}
			}
		}
	}

	read := map[string]bool{}
	for _, name := range sortedKeys(configured) {
		mc := configured[name]
		if mc.Availability == ModuleAvailable && (mc.Enabled || l.opts.IncludeDisabled) {
			l.readModuleDir(root, &mc, cfg)
			read[name] = true
		}
		cfg.Modules = append(cfg.Modules, mc)
	}

	// Rule 3: directories of known modules that were not read are
	// reported, so an operator is not surprised that a file there has no
	// effect.
	for _, name := range sortedKeys(l.opts.Modules) {
		if info, err := root.Stat(name); err == nil && info.IsDir() && !read[name] {
			cfg.IgnoredDirs = append(cfg.IgnoredDirs, filepath.Join(l.baseDir, name))
		}
	}
	return cfg
}

// openConfigDir checks the parents of the configuration directory, opens
// it as an os.Root and checks the opened directory (rule 8, D-070).
func (l *loader) openConfigDir() (*os.Root, bool) {
	dir, err := filepath.Abs(l.baseDir)
	if err == nil {
		dir, err = filepath.EvalSymlinks(dir)
	}
	if err != nil {
		l.errorf(l.baseDir, "", "%s", fileError(err))
		return nil, false
	}
	if err := checkAncestors(dir, l.euid); err != nil {
		l.errorf(l.baseDir, "", "%s", fileError(err))
		return nil, false
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		l.errorf(l.baseDir, "", "%s", fileError(err))
		return nil, false
	}
	if !l.checkDir(root, l.baseDir) {
		_ = root.Close()
		return nil, false
	}
	return root, true
}

// readRoot reads and parses one file of root after checking its ownership,
// size and version, and returns its root mapping with the environment
// values expanded into it. display is the path used in problems. It
// reports problems itself and returns ok false on failure.
func (l *loader) readRoot(root *os.Root, name, display string) (*yaml.Node, []string, bool) {
	data, err := readFile(root, name, func(info os.FileInfo) error {
		if err := checkOwnership(info, l.euid); err != nil {
			return err
		}
		if l.bytesRead+int(info.Size()) > maxTotalBytes {
			return fmt.Errorf("the configuration exceeds %d bytes in total", maxTotalBytes)
		}
		return nil
	})
	if err != nil {
		l.errorf(display, "", "%s", fileError(err))
		return nil, nil, false
	}
	if l.bytesRead += len(data); l.bytesRead > maxTotalBytes {
		l.errorf(display, "", "the configuration exceeds %d bytes in total", maxTotalBytes)
		return nil, nil, false
	}
	node, secrets, err := parseDocument(data, l.opts.Lookup)
	if err != nil {
		l.errs = append(l.errs, problemsFromYAML(display, err)...)
		return nil, nil, false
	}
	l.secrets = append(l.secrets, secrets...)
	if err := checkVersion(node); err != nil {
		l.errorf(display, "version", "%s", err)
		return nil, nil, false
	}
	return node, secrets, true
}

// module interprets one entry under `modules` (rules 1, 2 and 5); secrets
// are the environment values expanded in the central file.
func (l *loader) module(name string, node *yaml.Node, secrets []string) (ModuleConfig, bool) {
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
		Central:      Section{File: main, node: without(node, "enabled"), secrets: secrets},
		Dir:          filepath.Join(l.baseDir, name),
	}, true
}

// readModuleDir reads the files of a module directory (rules 4, 6 and 8).
// The directory is opened as its own os.Root, so files are resolved
// relative to the checked directory and symbolic links cannot leave it.
func (l *loader) readModuleDir(root *os.Root, mc *ModuleConfig, cfg *Config) {
	info, err := root.Stat(mc.Name)
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
	dir, err := root.OpenRoot(mc.Name)
	if err != nil {
		l.errorf(mc.Dir, "", "%s", fileError(err))
		return
	}
	defer dir.Close()
	if !l.checkDir(dir, mc.Dir) {
		return
	}

	entries, err := readDirNames(dir)
	if err != nil {
		l.errorf(mc.Dir, "", "%s", fileError(err))
		return
	}
	var settingsIn []string
	files := 0
	for _, name := range entries {
		path := filepath.Join(mc.Dir, name)
		if strings.HasPrefix(name, ".") {
			continue
		}
		info, err := dir.Stat(name) // follows symbolic links inside dir only
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
		if !info.Mode().IsRegular() {
			l.warnf(path, "", "not a regular file; ignored")
			continue
		}
		if files++; files > maxModuleFiles {
			l.errorf(mc.Dir, "", "more than %d configuration files", maxModuleFiles)
			return
		}
		node, secrets, ok := l.readRoot(dir, name, path)
		if !ok {
			continue
		}
		section := Section{File: path, node: without(node, "version"), secrets: secrets}
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

// readDirNames returns the names in the root directory of r, sorted.
func readDirNames(r *os.Root) ([]string, error) {
	d, err := r.Open(".")
	if err != nil {
		return nil, err
	}
	defer d.Close()
	names, err := d.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	slices.Sort(names)
	return names, nil
}

// checkDir applies the ownership rule to the opened directory of r.
func (l *loader) checkDir(r *os.Root, display string) bool {
	info, err := r.Stat(".")
	if err != nil {
		l.errorf(display, "", "%s", fileError(err))
		return false
	}
	if err := checkOwnership(info, l.euid); err != nil {
		l.errorf(display, "", "directory %s", err)
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
