package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Default locations.
const (
	DefaultMainFile   = "/etc/sentinel/sentinel.yaml"
	DefaultIncludeDir = "/etc/sentinel/conf.d"
)

// MaxFileSize caps each configuration file to keep parsing bounded.
const MaxFileSize = 4 << 20

// LoadOptions controls where configuration is read from.
type LoadOptions struct {
	// MainFile is required.
	MainFile string
	// IncludeDir holds optional fragments. Empty means "conf.d" next to
	// MainFile. A missing directory is not an error.
	IncludeDir string
	// NoIncludeDir disables fragment loading entirely.
	NoIncludeDir bool
	// Lookup resolves ${VARIABLE} references; defaults to os.LookupEnv.
	Lookup LookupEnv
}

// Result is a successfully loaded configuration.
type Result struct {
	Config *Config
	// Files lists the files read, in load order.
	Files []string
	// Warnings are non-fatal findings (insecure but legal settings).
	Warnings []Problem
}

// fileDoc is the YAML shape of one configuration file.
type fileDoc struct {
	Version       *int                  `yaml:"version"`
	Settings      *Settings             `yaml:"settings"`
	Notifications []NotificationChannel `yaml:"notifications"`
	Supervisors   []Supervisor          `yaml:"supervisors"`
}

// Load reads, merges, defaults and validates the configuration. Every
// failure is reported as a *ValidationError listing all problems found.
func Load(opts LoadOptions) (*Result, error) {
	if opts.MainFile == "" {
		return nil, errors.New("config: main file path is empty")
	}
	if opts.Lookup == nil {
		opts.Lookup = os.LookupEnv
	}

	files := []string{opts.MainFile}
	if !opts.NoIncludeDir {
		dir := opts.IncludeDir
		if dir == "" {
			dir = filepath.Join(filepath.Dir(opts.MainFile), "conf.d")
		}
		fragments, err := listFragments(dir)
		if err != nil {
			return nil, &ValidationError{Problems: []Problem{{File: dir, Message: err.Error()}}}
		}
		files = append(files, fragments...)
	}

	var (
		cfg         = &Config{Version: SchemaVersion}
		problems    []Problem
		supervisors = map[string]string{} // name -> defining file
		channels    = map[string]string{}
	)
	for i, file := range files {
		doc, err := parseFile(file, opts.Lookup)
		if err != nil {
			problems = append(problems, problemsFromError(file, err)...)
			continue
		}
		problems = append(problems, mergeDoc(cfg, doc, file, i == 0, supervisors, channels)...)
	}
	if len(problems) > 0 {
		return nil, &ValidationError{Problems: problems}
	}

	applyDefaults(cfg)
	warnings, errs := validate(cfg)
	if len(errs) > 0 {
		return nil, &ValidationError{Problems: errs, Warnings: warnings}
	}
	return &Result{Config: cfg, Files: files, Warnings: warnings}, nil
}

// listFragments returns *.yaml and *.yml regular files in dir, sorted by
// byte order. Hidden files are ignored.
func listFragments(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read include directory: %w", err)
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		ext := filepath.Ext(name)
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		path := filepath.Join(dir, name)
		info, err := os.Stat(path) // follows symlinks
		if err != nil {
			return nil, fmt.Errorf("stat %s: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		out = append(out, path)
	}
	slices.Sort(out) // ReadDir already sorts; keep the guarantee explicit
	return out, nil
}

func readFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxFileSize {
		return nil, fmt.Errorf("file exceeds %d bytes", MaxFileSize)
	}
	return data, nil
}

func parseFile(path string, lookup LookupEnv) (*fileDoc, error) {
	data, err := readFile(path)
	if err != nil {
		return nil, err
	}
	return parseBytes(data, lookup)
}

// parseBytes turns one YAML document into a fileDoc, expanding variables
// and rejecting unknown keys.
func parseBytes(data []byte, lookup LookupEnv) (*fileDoc, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var root yaml.Node
	if err := dec.Decode(&root); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("file is empty")
		}
		return nil, err
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("multiple YAML documents are not allowed")
	}
	if err := expandNode(&root, lookup); err != nil {
		return nil, err
	}
	if err := checkKnownFields(&root, reflect.TypeFor[fileDoc]()); err != nil {
		return nil, err
	}
	var doc fileDoc
	if err := root.Decode(&doc); err != nil {
		return nil, err
	}
	return &doc, nil
}

func mergeDoc(cfg *Config, doc *fileDoc, file string, isMain bool, supervisors, channels map[string]string) []Problem {
	var problems []Problem
	add := func(path, format string, args ...any) {
		problems = append(problems, Problem{File: file, Path: path, Message: fmt.Sprintf(format, args...)})
	}

	switch {
	case doc.Version == nil:
		add("version", "is required (version: %d)", SchemaVersion)
	case *doc.Version != SchemaVersion:
		add("version", "unsupported configuration version %d (this release supports %d)", *doc.Version, SchemaVersion)
	}

	if doc.Settings != nil {
		if isMain {
			cfg.Settings = *doc.Settings
		} else {
			add("settings", "is only allowed in the main configuration file")
		}
	}

	for _, ch := range doc.Notifications {
		if prev, dup := channels[ch.Name]; dup && ch.Name != "" {
			add("notifications["+ch.Name+"]", "duplicate notification channel name (first defined in %s)", prev)
			continue
		}
		channels[ch.Name] = file
		cfg.Notifications = append(cfg.Notifications, ch)
	}
	for _, m := range doc.Supervisors {
		if prev, dup := supervisors[m.Name]; dup && m.Name != "" {
			add("supervisors["+m.Name+"]", "duplicate supervisor name (first defined in %s)", prev)
			continue
		}
		supervisors[m.Name] = file
		cfg.Supervisors = append(cfg.Supervisors, m)
	}
	return problems
}

// problemsFromError splits yaml.TypeError and joined errors into one
// Problem per message.
func problemsFromError(file string, err error) []Problem {
	var msgs []string
	var te *yaml.TypeError
	switch {
	case errors.As(err, &te):
		msgs = te.Errors
	default:
		for _, e := range unwrapJoined(err) {
			msgs = append(msgs, e.Error())
		}
	}
	out := make([]Problem, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, Problem{File: file, Message: strings.TrimPrefix(m, "yaml: ")})
	}
	return out
}
