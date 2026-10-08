package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/fstrust"
)

// errTotal reports that the configuration does not fit in maxTotalBytes.
var errTotal = fmt.Errorf("the configuration exceeds %d bytes in total", maxTotalBytes)

// Limits that keep a load bounded whatever the directories contain.
const (
	// maxFileSize bounds each configuration file.
	maxFileSize = 4 << 20
	// maxDirEntries bounds the entries of any kind (files, directories,
	// hidden files) listed in one module directory.
	maxDirEntries = 1000
	// maxTotalBytes bounds the bytes read across the whole load: every
	// read is limited by what remains, and failed reads count too.
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
// relative to that descriptor, and a symbolic link may only name an entry
// of its own directory.
//
// Every failure is reported as a *ValidationError listing all problems.
func Load(opts LoadOptions) (*Config, error) {
	if opts.MainFile == "" {
		return nil, errors.New("config: main file path is empty")
	}
	if opts.Lookup == nil {
		opts.Lookup = os.LookupEnv
	}
	l := &loader{opts: opts, euid: os.Geteuid(), baseDir: filepath.Dir(opts.MainFile), readFile: readFile}
	cfg := l.load()
	errs, warns := redactProblems(l.errs, l.secrets), redactProblems(l.warns, l.secrets)
	if len(errs) > 0 {
		return nil, &ValidationError{Problems: errs, Warnings: warns}
	}
	cfg.Warnings = warns
	return cfg, nil
}

type loader struct {
	problems
	opts      LoadOptions
	euid      int
	baseDir   string
	bytesRead int
	// secrets are the environment values expanded in any file read.
	secrets []string
	// readFile is the package function; tests replace it to simulate a
	// file that changes between the check and the read.
	readFile func(root *os.Root, name string, limit int, check func(os.FileInfo) error) ([]byte, error)
}

// document is one parsed configuration file.
type document struct {
	file    string     // path used in problems
	root    *yaml.Node // root mapping, with ${VARIABLE} references expanded
	secrets []string   // values substituted from the environment
}

// section returns the document as a Section without the given top-level
// keys (such as `version`).
func (d document) section(drop ...string) Section {
	return Section{File: d.file, node: without(d.root, drop...), secrets: d.secrets}
}

func (l *loader) load() *Config {
	cfg := &Config{}
	root, ok := l.openConfigDir()
	if !ok {
		return cfg
	}
	defer root.Close()

	modules, ok := l.loadCentral(root, cfg)
	if !ok {
		return cfg
	}
	read := l.loadModules(root, modules, cfg)
	cfg.IgnoredDirs = l.ignoredDirs(root, read)
	return cfg
}

// loadCentral reads the central file into cfg and returns the modules it
// names, plus, for `validate --all`, every other available module.
func (l *loader) loadCentral(root *os.Root, cfg *Config) (map[string]ModuleConfig, bool) {
	main := l.opts.MainFile
	if err := checkLink(root, filepath.Base(main)); err != nil {
		l.errorf(main, "", "%s", fileError(err))
		return nil, false
	}
	doc, ok := l.readDocument(root, filepath.Base(main), main)
	if !ok {
		return nil, false
	}
	cfg.Files = append(cfg.Files, main)

	var central centralDoc
	if err := doc.section("version").Decode(&central); err != nil {
		l.errs = append(l.errs, ProblemsOf(err, main, "")...)
		return nil, false
	}
	cfg.Daemon, cfg.Notifications = central.Daemon, central.Notifications
	applyDefaults(cfg)
	warns, errs := validateCentral(main, cfg)
	l.warns = append(l.warns, warns...)
	l.errs = append(l.errs, errs...)

	modules := map[string]ModuleConfig{}
	for _, name := range sortedKeys(central.Modules) {
		node := central.Modules[name]
		if mc, ok := l.module(name, &node, doc.secrets); ok {
			modules[name] = mc
		}
	}
	// validate --all: available modules not named in the central file are
	// implicitly disabled, and their directories are checked too.
	if l.opts.IncludeDisabled {
		for name, a := range l.opts.Modules {
			if _, named := modules[name]; !named && a == ModuleAvailable {
				modules[name] = ModuleConfig{Name: name, Availability: a, Dir: filepath.Join(l.baseDir, name)}
			}
		}
	}
	return modules, true
}

// loadModules reads the directory of every module that needs it, appends
// all modules to cfg in name order and returns the names it read.
func (l *loader) loadModules(root *os.Root, modules map[string]ModuleConfig, cfg *Config) map[string]bool {
	read := map[string]bool{}
	for _, name := range sortedKeys(modules) {
		mc := modules[name]
		if mc.Availability == ModuleAvailable && (mc.Enabled || l.opts.IncludeDisabled) {
			mc.Files, mc.DirExists = l.readModuleDir(root, mc.Name, mc.Dir)
			for _, s := range mc.Files {
				cfg.Files = append(cfg.Files, s.File)
			}
			read[name] = true
		}
		cfg.Modules = append(cfg.Modules, mc)
	}
	return read
}

// ignoredDirs lists the directories of known modules that were not read
// (rule 3), so an operator is not surprised that a file there has no
// effect.
func (l *loader) ignoredDirs(root *os.Root, read map[string]bool) []string {
	var dirs []string
	for _, name := range sortedKeys(l.opts.Modules) {
		if info, err := root.Stat(name); err == nil && info.IsDir() && !read[name] {
			dirs = append(dirs, filepath.Join(l.baseDir, name))
		}
	}
	return dirs
}

// openConfigDir resolves the configuration directory, checking every
// directory on the way (see fstrust.Resolve), opens it as an os.Root and
// checks the opened directory (rule 8, D-070).
func (l *loader) openConfigDir() (*os.Root, bool) {
	resolved, err := filepath.Abs(l.baseDir)
	if err == nil {
		resolved, err = fstrust.Resolve(resolved, l.euid)
	}
	if err != nil {
		l.errorf(l.baseDir, "", "%s", fileError(err))
		return nil, false
	}
	root, err := os.OpenRoot(resolved)
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

// readDocument reads and parses one file of root after checking its
// ownership, size and version. display is the path used in problems. It
// reports problems itself and returns ok false on failure.
func (l *loader) readDocument(root *os.Root, name, display string) (document, bool) {
	// Every read is limited by what remains of the total, and the bytes
	// actually read are charged, failed reads included.
	remaining := maxTotalBytes - l.bytesRead
	if remaining <= 0 {
		l.errorf(display, "", "%s", errTotal)
		return document{}, false
	}
	limit := min(maxFileSize, remaining)
	data, err := l.readFile(root, name, limit, func(info os.FileInfo) error {
		if err := fstrust.CheckOwnership(info, l.euid); err != nil {
			return err
		}
		if info.Size() > int64(limit) {
			return errLimit // rejected before reading
		}
		return nil
	})
	l.bytesRead += len(data)
	if errors.Is(err, errLimit) {
		err = errTotal
		if limit == maxFileSize {
			err = fmt.Errorf("file exceeds %d bytes", maxFileSize)
		}
	}
	if err != nil {
		l.errorf(display, "", "%s", fileError(err))
		return document{}, false
	}
	node, secrets, err := parseDocument(data, l.opts.Lookup)
	if err != nil {
		l.errs = append(l.errs, problemsFromYAML(display, err)...)
		return document{}, false
	}
	l.secrets = append(l.secrets, secrets...)
	if err := checkVersion(node); err != nil {
		l.errorf(display, "version", "%s", err)
		return document{}, false
	}
	return document{file: display, root: node, secrets: secrets}, true
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

	node = resolveAlias(node)
	switch {
	case node.Kind == yaml.ScalarNode && node.ShortTag() == "!!null":
		node = nil // `supervisor:` with nothing after it: an empty block
	case node.Kind != yaml.MappingNode:
		l.errorf(main, path, "line %d: must be a mapping such as {enabled: true}", node.Line)
		return ModuleConfig{}, false
	}

	enabled := false
	if v, ok := lookupKey(node, "enabled"); ok {
		v = resolveAlias(v)
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

// readModuleDir reads the files of the module directory name of root
// (rules 4, 6 and 8) and reports whether the directory exists. dir is the
// path used in problems. The directory is opened as its own os.Root, so
// files are resolved relative to the checked directory.
func (l *loader) readModuleDir(root *os.Root, name, dir string) (files []Section, exists bool) {
	if err := checkLink(root, name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		l.errorf(dir, "", "%s", fileError(err))
		return nil, false
	}
	info, err := root.Stat(name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, false // the module decides whether it needs a directory (rule 4)
	case err != nil:
		l.errorf(dir, "", "%s", fileError(err))
		return nil, false
	case !info.IsDir():
		l.errorf(dir, "", "is not a directory")
		return nil, false
	}
	moduleRoot, err := root.OpenRoot(name)
	if err != nil {
		l.errorf(dir, "", "%s", fileError(err))
		return nil, true
	}
	defer moduleRoot.Close()
	if !l.checkDir(moduleRoot, dir) {
		return nil, true
	}

	entries, err := readDirNames(moduleRoot, maxDirEntries)
	if err != nil {
		l.errorf(dir, "", "%s", fileError(err))
		return nil, true
	}
	var settingsIn []string
	for _, entry := range entries {
		path := filepath.Join(dir, entry)
		if strings.HasPrefix(entry, ".") {
			continue
		}
		if err := checkLink(moduleRoot, entry); err != nil {
			l.errorf(path, "", "%s", fileError(err))
			continue
		}
		info, err := moduleRoot.Stat(entry)
		if err != nil {
			l.errorf(path, "", "%s", fileError(err))
			continue
		}
		if info.IsDir() {
			l.warnf(path, "", "subdirectories are not read; move the files into %s", dir)
			continue
		}
		if ext := filepath.Ext(entry); ext != ".yaml" && ext != ".yml" {
			continue
		}
		if !info.Mode().IsRegular() {
			l.warnf(path, "", "not a regular file; ignored")
			continue
		}
		doc, ok := l.readDocument(moduleRoot, entry, path)
		if !ok {
			continue
		}
		section := doc.section("version")
		if section.Has("settings") {
			settingsIn = append(settingsIn, path)
		}
		files = append(files, section)
	}
	if len(settingsIn) > 1 {
		l.errorf(dir, "modules."+name, "`settings` may appear in only one file of the module directory, found in: %s",
			strings.Join(settingsIn, ", "))
	}
	return files, true
}

// checkDir applies the ownership rule to the opened directory of r.
func (l *loader) checkDir(r *os.Root, display string) bool {
	info, err := r.Stat(".")
	if err != nil {
		l.errorf(display, "", "%s", fileError(err))
		return false
	}
	if err := fstrust.CheckOwnership(info, l.euid); err != nil {
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
