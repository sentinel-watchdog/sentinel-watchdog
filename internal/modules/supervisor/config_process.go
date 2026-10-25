package supervisor

import (
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/config"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/fstrust"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/executor"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/privilege"
)

// Limits and defaults of a process service (D-005, D-009, D-058, D-086).
const (
	maxPathBytes   = 4096
	maxArgs        = 256
	maxArgBytes    = 4096
	maxArgsBytes   = 64 << 10
	maxScriptBytes = 64 << 10
	maxEnv         = 128
	maxEnvValue    = 32 << 10
	maxEnvBytes    = 128 << 10

	defaultShell        = "/bin/sh"
	defaultWorkingDir   = "/"
	defaultStopTimeout  = 10 * time.Second
	minStopTimeout      = time.Second
	maxStopTimeout      = 5 * time.Minute
	defaultStartupGrace = 5 * time.Second
	maxStartupGrace     = 10 * time.Minute
	defaultOutputMode   = 0o640

	// defaultPath is the PATH of a child whose environment does not set
	// one (D-086): scripts find the usual tools, and nothing of
	// sentineld's environment reaches the child. command itself is never
	// looked up in it (D-005).
	defaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
)

// Defaults and bounds of a process service's recovery (D-013, D-084 (5)).
const (
	defaultMaxAttempts = 5
	maxMaxAttempts     = 100
	defaultWindow      = 10 * time.Minute
	minWindow          = time.Minute
	maxWindow          = 24 * time.Hour
	defaultDelay       = time.Second
	minDelay           = 100 * time.Millisecond
	maxDelayValue      = 10 * time.Minute
	defaultMaxDelay    = time.Minute
	maxMaxDelay        = time.Hour
	defaultStableAfter = time.Minute
	minStableAfter     = time.Second
	maxStableAfter     = 24 * time.Hour
	defaultCooldown    = 10 * time.Minute
	minCooldown        = time.Minute
	maxCooldown        = 7 * 24 * time.Hour

	// recoveryRestart is the only recovery action (D-084 (6)).
	recoveryRestart = "restart"
)

// Output kinds (D-009).
const (
	outputLog     = "log"
	outputFile    = "file"
	outputDiscard = "discard"
)

// stopSignals are the signals a process service may be stopped with.
var stopSignals = []string{"SIGTERM", "SIGINT", "SIGQUIT", "SIGHUP", "SIGUSR1", "SIGUSR2", "SIGKILL"}

// envNameRe is the name of an environment variable.
var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

// Process is how a process service runs, defaults applied.
type Process struct {
	// Command is the program as configured, absolute: the shell for a
	// script. It is resolved and checked at every launch
	// (executor.TrustedExecutable).
	Command string
	// Args are argv[1:]; for a script, ["-c", script].
	Args   []string
	Script bool
	// User and Group are as configured ("": sentineld's); Cred is what
	// Configure resolved them to (nil: sentineld's identity).
	User, Group string
	Cred        *privilege.Cred
	WorkingDir  string
	// Env is the child's whole environment, "KEY=value" sorted by key.
	Env          []string
	Output       Output
	StopSignal   syscall.Signal
	StopTimeout  time.Duration
	StartupGrace time.Duration
	Recovery     Recovery
}

// Recovery is how a process service is started again after it fails
// (D-013, D-084 (5)): at most MaxAttempts launches in Window, each after a
// delay that grows with Backoff up to MaxDelay; once the budget is spent
// the service is exhausted for Cooldown, or until an operator's reset when
// Cooldown is 0 (D-013). A process that runs for StableAfter closes its
// incident.
type Recovery struct {
	MaxAttempts int
	Window      time.Duration
	Delay       time.Duration
	Backoff     config.Backoff
	MaxDelay    time.Duration
	StableAfter time.Duration
	Cooldown    time.Duration
}

// Output is where a process's stdout and stderr go (D-009).
type Output struct {
	Kind string // outputLog, outputFile or outputDiscard
	File string // absolute, for outputFile
	Mode fs.FileMode
}

type (
	processServiceYAML struct {
		Name          string            `yaml:"name"`
		Type          string            `yaml:"type"`
		Command       string            `yaml:"command"`
		Args          []string          `yaml:"args"`
		Script        string            `yaml:"script"`
		Shell         string            `yaml:"shell"`
		User          string            `yaml:"user"`
		Group         string            `yaml:"group"`
		WorkingDir    string            `yaml:"working_dir"`
		Environment   map[string]string `yaml:"environment"`
		Output        any               `yaml:"output"` // a kind, or {file, mode}
		StopSignal    string            `yaml:"stop_signal"`
		StopTimeout   *config.Duration  `yaml:"stop_timeout"`
		StartupGrace  *config.Duration  `yaml:"startup_grace"`
		Notifications config.Route      `yaml:"notifications"`
		// Decoded only to be refused with a reason.
		FailurePolicy any   `yaml:"failure_policy"`
		CheckInterval any   `yaml:"check_interval"`
		Timeout       any   `yaml:"timeout"`
		Recovery      any   `yaml:"recovery"`
		Limits        any   `yaml:"limits"`
		Enabled       *bool `yaml:"enabled"`
	}
	recoveryYAML struct {
		Action      *string          `yaml:"action"`
		MaxAttempts *int             `yaml:"max_attempts"`
		Window      *config.Duration `yaml:"window"`
		Delay       *config.Duration `yaml:"delay"`
		Backoff     *string          `yaml:"backoff"`
		MaxDelay    *config.Duration `yaml:"max_delay"`
		StableAfter *config.Duration `yaml:"stable_after"`
		Cooldown    *config.Duration `yaml:"cooldown"`
	}
	outputFileYAML struct {
		File string           `yaml:"file"`
		Mode *config.FileMode `yaml:"mode"`
	}
)

// refusedProcessKeys are keys a process service does not take, and why.
var refusedProcessKeys = []struct{ key, why string }{
	{"failure_policy", "failure_policy is for checked services: every exit of a process is a failure (D-012)"},
	{"check_interval", "check_interval is for checked services: a process is watched, not polled"},
	{"timeout", "timeout is for checked services; a process has stop_timeout and startup_grace"},
	{"limits", "limits are planned but not implemented (Phase 3b-6)"},
}

// parseProcess decodes and checks a process service. Problems name keys
// and lines, never values: environment values, arguments and scripts may
// hold secrets.
func (p *parsed) parseProcess(f config.Section, path string, item config.Section, channels []string) (Service, bool) {
	before := len(p.problems)
	var y processServiceYAML
	bad := p.decode(item, &y, path)
	name := p.serviceName(item, path, y.Name)
	for _, r := range refusedProcessKeys {
		if item.Has(r.key) {
			p.addf(item, item.KeyLine(r.key), path+"."+r.key, "%s", r.why)
		}
	}
	proc := Process{User: y.User, Group: y.Group, Env: p.environment(item, path, y.Environment)}
	p.program(item, bad, path, y, &proc)
	proc.WorkingDir = defaultWorkingDir
	if item.Has("working_dir") && !bad[item.KeyLine("working_dir")] {
		if why := checkAbsPath(y.WorkingDir); why != "" {
			p.addf(item, item.KeyLine("working_dir"), path+".working_dir", "working_dir %s", why)
		} else {
			proc.WorkingDir = y.WorkingDir
		}
	}
	if (item.Has("user") && y.User == "") || (item.Has("group") && y.Group == "") {
		p.addf(item, item.Line(), path, "user and group must not be empty: remove them to keep sentineld's identity")
	}
	proc.Output = p.output(item, bad, path, y.Output)
	proc.StopSignal = syscall.SIGTERM
	if item.Has("stop_signal") && !bad[item.KeyLine("stop_signal")] {
		sig, ok := executor.ParseSignal(y.StopSignal)
		if !ok || !slices.Contains(stopSignals, "SIG"+strings.TrimPrefix(strings.ToUpper(y.StopSignal), "SIG")) {
			p.addf(item, item.KeyLine("stop_signal"), path+".stop_signal", "stop_signal must be one of %s", strings.Join(stopSignals, ", "))
		} else {
			proc.StopSignal = sig
		}
	}
	proc.StopTimeout = p.duration(item, bad, path, "stop_timeout", y.StopTimeout, defaultStopTimeout, minStopTimeout, maxStopTimeout)
	proc.StartupGrace = p.duration(item, bad, path, "startup_grace", y.StartupGrace, defaultStartupGrace, 0, maxStartupGrace)
	proc.Recovery = p.recovery(item, bad, path, proc.StartupGrace)
	p.route(f, item, path, y.Notifications, channels)
	return Service{Name: name, Type: typeProcess, Route: y.Notifications, Process: proc,
		Disabled: y.Enabled != nil && !*y.Enabled}, len(p.problems) == before
}

// recovery checks a process service's recovery block and applies the
// defaults. stable_after is never shorter than startup_grace: a process
// is not recovered before it is running.
func (p *parsed) recovery(item config.Section, bad map[int]bool, path string, grace time.Duration) Recovery {
	r := Recovery{MaxAttempts: defaultMaxAttempts, Window: defaultWindow, Delay: defaultDelay,
		Backoff: config.BackoffExponential, MaxDelay: defaultMaxDelay, StableAfter: max(defaultStableAfter, grace),
		Cooldown: defaultCooldown}
	if !item.Has("recovery") || bad[item.KeyLine("recovery")] {
		return r
	}
	block, err := item.Child("recovery")
	if err != nil {
		p.addf(item, item.KeyLine("recovery"), path+".recovery", "recovery must be a mapping")
		return r
	}
	path += ".recovery"
	var y recoveryYAML
	bad = p.decode(block, &y, path)
	// set reports whether key holds a value that decoded: a null leaves
	// its pointer nil and the default stands (review 3b-3 R1).
	set := func(key string, decoded bool) bool { return decoded && block.Has(key) && !bad[block.KeyLine(key)] }
	if set("action", y.Action != nil) && *y.Action != recoveryRestart {
		p.addf(block, block.KeyLine("action"), path+".action", "action must be %s, the only recovery action", recoveryRestart)
	}
	if set("max_attempts", y.MaxAttempts != nil) {
		if n := *y.MaxAttempts; n < 1 || n > maxMaxAttempts {
			p.addf(block, block.KeyLine("max_attempts"), path+".max_attempts", "must be between 1 and %d", maxMaxAttempts)
		} else {
			r.MaxAttempts = n
		}
	}
	r.Window = p.duration(block, bad, path, "window", y.Window, r.Window, minWindow, maxWindow)
	r.Delay = p.duration(block, bad, path, "delay", y.Delay, r.Delay, minDelay, maxDelayValue)
	if set("backoff", y.Backoff != nil) {
		switch b := config.Backoff(*y.Backoff); b {
		case config.BackoffFixed, config.BackoffExponential:
			r.Backoff = b
		default:
			p.addf(block, block.KeyLine("backoff"), path+".backoff", "backoff must be fixed or exponential")
		}
	}
	switch {
	case r.Backoff == config.BackoffFixed && block.Has("max_delay"):
		p.addf(block, block.KeyLine("max_delay"), path+".max_delay", "max_delay goes with backoff: exponential; a fixed backoff waits delay every time")
	case set("max_delay", y.MaxDelay != nil):
		if d := y.MaxDelay.Std(); d < r.Delay || d > maxMaxDelay {
			p.addf(block, block.KeyLine("max_delay"), path+".max_delay", "must be between delay (%s) and %s", r.Delay, maxMaxDelay)
		} else {
			r.MaxDelay = d
		}
	}
	if r.Backoff == config.BackoffFixed {
		r.MaxDelay = r.Delay // every attempt waits delay; blocked launches retry at that pace too
	}
	r.MaxDelay = max(r.MaxDelay, r.Delay)
	if set("stable_after", y.StableAfter != nil) {
		r.StableAfter = p.duration(block, bad, path, "stable_after", y.StableAfter, r.StableAfter, minStableAfter, maxStableAfter)
		if y.StableAfter.Std() < grace {
			p.addf(block, block.KeyLine("stable_after"), path+".stable_after", "must not be shorter than startup_grace (%s)", grace)
		}
	}
	if set("cooldown", y.Cooldown != nil) && *y.Cooldown == 0 {
		r.Cooldown = 0 // exhausted until `sentinelctl supervisor reset` (D-013)
	} else {
		r.Cooldown = p.duration(block, bad, path, "cooldown", y.Cooldown, r.Cooldown, minCooldown, maxCooldown)
	}
	return r
}

// program sets the program and its arguments: command with args, xor
// script with its shell (D-005).
func (p *parsed) program(item config.Section, bad map[int]bool, path string, y processServiceYAML, proc *Process) {
	hasCmd, hasScript := item.Has("command"), item.Has("script")
	switch {
	case hasCmd && hasScript:
		p.addf(item, item.KeyLine("script"), path, "command and script exclude each other: a command is run directly, a script by its shell (D-005)")
		return
	case hasCmd:
		if item.Has("shell") {
			p.addf(item, item.KeyLine("shell"), path+".shell", "shell goes with script, not with command: a command is never run through a shell (D-005)")
		}
		if why := checkAbsPath(y.Command); why != "" && !bad[item.KeyLine("command")] {
			p.addf(item, item.KeyLine("command"), path+".command", "command %s (no $PATH lookup, D-005)", why)
		}
		proc.Command = y.Command
		proc.Args = p.args(item, path, y.Args)
	case hasScript:
		if item.Has("args") {
			p.addf(item, item.KeyLine("args"), path+".args", "args go with command; a script holds its own arguments")
		}
		switch {
		case bad[item.KeyLine("script")]:
		case y.Script == "":
			p.addf(item, item.KeyLine("script"), path+".script", "script is empty")
		case len(y.Script) > maxScriptBytes:
			p.addf(item, item.KeyLine("script"), path+".script", "script is longer than %d bytes", maxScriptBytes)
		case strings.ContainsRune(y.Script, 0):
			p.addf(item, item.KeyLine("script"), path+".script", "script contains a NUL byte")
		}
		proc.Command, proc.Script = defaultShell, true
		if item.Has("shell") && !bad[item.KeyLine("shell")] {
			if why := checkAbsPath(y.Shell); why != "" {
				p.addf(item, item.KeyLine("shell"), path+".shell", "shell %s", why)
			} else {
				proc.Command = y.Shell
			}
		}
		proc.Args = []string{"-c", y.Script}
	default: // an explicit type: process without either
		p.addf(item, item.Line(), path, "a process service needs command or script")
	}
}

// args checks a command's arguments.
func (p *parsed) args(item config.Section, path string, args []string) []string {
	line := item.KeyLine("args")
	total := 0
	for i, a := range args {
		total += len(a)
		switch {
		case len(a) > maxArgBytes:
			p.addf(item, line, fmt.Sprintf("%s.args[%d]", path, i), "argument is longer than %d bytes", maxArgBytes)
		case strings.ContainsRune(a, 0):
			p.addf(item, line, fmt.Sprintf("%s.args[%d]", path, i), "argument contains a NUL byte")
		}
	}
	switch {
	case len(args) > maxArgs:
		p.addf(item, line, path+".args", "more than %d arguments", maxArgs)
	case total > maxArgsBytes:
		p.addf(item, line, path+".args", "arguments are longer than %d bytes together", maxArgsBytes)
	}
	return slices.Clone(args)
}

// environment checks the configured variables and returns the child's
// whole environment, sorted: only these, plus PATH when they do not set
// it (D-086). Names are quoted: keys are never expanded (D-006).
func (p *parsed) environment(item config.Section, path string, env map[string]string) []string {
	line := item.KeyLine("environment")
	if len(env) > maxEnv {
		p.addf(item, line, path+".environment", "more than %d variables", maxEnv)
		return nil
	}
	names := slices.Sorted(maps.Keys(env))
	total := 0
	out := make([]string, 0, len(env)+1)
	for _, k := range names {
		v := env[k]
		total += len(k) + len(v) + 1
		switch {
		case !envNameRe.MatchString(k):
			p.addf(item, line, path+".environment", "invalid variable name %q: letters, digits and '_', not starting with a digit", k)
		case len(v) > maxEnvValue:
			p.addf(item, line, path+".environment."+k, "value is longer than %d bytes", maxEnvValue)
		case strings.ContainsRune(v, 0):
			p.addf(item, line, path+".environment."+k, "value contains a NUL byte")
		default:
			out = append(out, k+"="+v)
		}
	}
	if total > maxEnvBytes {
		p.addf(item, line, path+".environment", "variables are longer than %d bytes together", maxEnvBytes)
	}
	if _, ok := env["PATH"]; !ok {
		out = append(out, "PATH="+defaultPath)
		slices.Sort(out)
	}
	return out
}

// output checks the output key: "log" (default), "discard", or a file
// {file: <absolute path>, mode: "0640"}.
func (p *parsed) output(item config.Section, bad map[int]bool, path string, v any) Output {
	out := Output{Kind: outputLog}
	line := item.KeyLine("output")
	if !item.Has("output") || bad[line] {
		return out
	}
	if kind, ok := v.(string); ok {
		switch kind {
		case outputLog, outputDiscard:
			out.Kind = kind
		default:
			p.addf(item, line, path+".output", "output must be log, discard or {file: <path>, mode: \"0640\"}")
		}
		return out
	}
	block, err := item.Child("output")
	if err != nil {
		p.addf(item, line, path+".output", "output must be log, discard or {file: <path>, mode: \"0640\"}")
		return out
	}
	var y outputFileYAML
	fileBad := p.decode(block, &y, path+".output")
	if len(fileBad) > 0 {
		return out
	}
	if why := checkAbsPath(y.File); why != "" {
		p.addf(item, lineOr(block, "file"), path+".output.file", "output file %s", why)
		return out
	}
	mode := fs.FileMode(defaultOutputMode)
	if y.Mode != nil {
		mode = y.Mode.Perm()
	}
	if mode&^fs.ModePerm != 0 || mode&0o7000 != 0 || mode&0o022 != 0 {
		p.addf(item, lineOr(block, "mode"), path+".output.mode", "mode must not be writable by group or others, nor set special bits")
		return out
	}
	return Output{Kind: outputFile, File: y.File, Mode: mode}
}

// checkAbsPath says what is wrong with a configured path, or "".
func checkAbsPath(path string) string {
	switch {
	case path == "":
		return "is empty"
	case len(path) > maxPathBytes:
		return fmt.Sprintf("is longer than %d bytes", maxPathBytes)
	case strings.ContainsRune(path, 0):
		return "contains a NUL byte"
	case !filepath.IsAbs(path):
		return "must be an absolute path"
	case filepath.Clean(path) != path:
		return "must be a clean path (no '.', '..', '//' or trailing '/')"
	}
	return ""
}

// resolveProcess completes a process service at Configure, reading the
// system without changing it (D-080 (4)): its identity from the account
// files, and the trust of its program and of its output file's directory
// (D-086), so that `sentinelctl validate` reports them. Both are checked
// again at every launch.
func (m *Module) resolveProcess(proc *Process) string {
	if proc.User != "" || proc.Group != "" {
		lookup := m.opts.Lookup
		if lookup.User == nil || lookup.Group == nil {
			lookup = privilege.System
		}
		cred, err := lookup.Resolve(proc.User, proc.Group, uint32(os.Geteuid())) //nolint:gosec // geteuid is never negative
		if err != nil {
			return "user and group cannot be resolved: " + err.Error() // privilege errors name no value
		}
		proc.Cred = &cred
	}
	if _, err := executor.TrustedExecutable(proc.Command); err != nil {
		return "the program cannot be run: " + err.Error()
	}
	if proc.Output.Kind == outputFile {
		if _, err := fstrust.Resolve(filepath.Dir(proc.Output.File), os.Geteuid()); err != nil {
			return "the output file's directory is not trusted: " + err.Error()
		}
	}
	return ""
}
