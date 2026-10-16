// Command sentineld is the Sentinel Watchdog daemon. It loads the central
// configuration, starts the core services and the enabled modules, and
// runs until SIGTERM or SIGINT. SIGHUP reloads the configuration, as
// `sentinelctl reload` does. Under systemd (Type=notify) it reports when
// it is ready and when it stops (D-079).
//
//	sentineld [-config /etc/sentinel/sentinel.yaml]
//	sentineld -validate [-config file]
//	sentineld -version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/audit"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/config"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/logging"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/module"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/daemon"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/sdnotify"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/version"
)

// Exit codes.
const (
	exitOK      = 0
	exitFailure = 1 // invalid configuration, failed start or unclean stop
	exitUsage   = 2
)

// umask keeps every file sentineld creates out of reach of other users,
// whatever umask it inherits; explicit modes (state 0600/0700, logs 0640)
// can only be narrowed by it, never widened (D-075).
const umask = 0o027

// modules returns the registry of known modules. Tests replace it to run
// test modules through the real command (signals during start).
var modules = daemon.Modules

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is main without the process exit, so tests can call it.
func run(args []string, stdout, stderr io.Writer) int {
	syscall.Umask(umask)
	flags := flag.NewFlagSet("sentineld", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configFile := flags.String("config", config.DefaultMainFile, "central configuration `file`")
	validate := flags.Bool("validate", false, "validate the configuration and exit")
	showVersion := flags.Bool("version", false, "print the version and exit")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "sentineld: unexpected argument %q\n", flags.Arg(0))
		return exitUsage
	}
	if *showVersion {
		fmt.Fprintln(stdout, "sentineld", version.String())
		return exitOK
	}
	reg, err := modules()
	if err != nil {
		fmt.Fprintln(stderr, "sentineld:", err)
		return exitFailure
	}
	if *validate {
		cfg, err := daemon.Validate(reg, *configFile, false)
		if werr := config.WriteDiagnostics(stdout, cfg, err); werr != nil || err != nil {
			return exitFailure
		}
		fmt.Fprintln(stdout, "configuration is valid")
		return exitOK
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	// SIGHUP before Start would otherwise end the process. One pending
	// signal is enough: bursts coalesce into one reload.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	// From the first signal on, a second SIGTERM or SIGINT ends the process
	// at once, even while modules are still starting.
	context.AfterFunc(ctx, stop)
	cfg, mods, err := daemon.Load(reg, *configFile)
	if err != nil {
		_ = config.WriteDiagnostics(stderr, cfg, err) // stderr is all we have
		return exitFailure
	}
	logger, level, err := newLogger(cfg.Daemon.Log, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "sentineld:", err)
		return exitFailure
	}
	for _, w := range cfg.Warnings {
		logger.Warn("configuration warning", "problem", w.String())
	}
	for _, dir := range cfg.IgnoredDirs {
		logger.Info("module directory ignored (module disabled or not available)", "dir", dir)
	}
	systemd := systemdNotifier(logger)
	hostname, err := os.Hostname()
	if err != nil {
		logger.Warn("hostname unknown; events carry none", "error", err)
	}
	d := daemon.New(cfg, mods, daemon.Options{Logger: logger, LogLevel: level, Hostname: hostname,
		Version: version.Version, Modules: reg.Availability(), Commands: reg.Commands(),
		Load: func() (*config.Config, []module.Instance, error) { return daemon.Load(reg, *configFile) }})
	// A signal during start cancels it: the module starting returns and no
	// further module starts.
	if err := d.Start(ctx); err != nil {
		logger.Error("start failed", "error", err)
		return exitFailure
	}
	// A start cut short by a signal is not ready: modules were skipped.
	if ctx.Err() == nil {
		notify(logger, systemd, sdnotify.Ready, sdnotify.Status(readyStatus(d.Snapshot())))
	}
	// A SIGHUP received during start-up is pending: it reloads now, once
	// Start has returned, so it never runs beside the start (D-078).
	for done := false; !done; {
		select {
		case <-ctx.Done():
			done = true
		case <-hup:
			// A stop signal cancels a reload in progress.
			if _, apiErr := d.Reload(ctx, audit.Actor{Kind: audit.ActorSignal}); apiErr != nil {
				logger.Error("reload on SIGHUP failed", "code", apiErr.Code, "error", apiErr.Message)
			}
		}
	}
	logger.Info("stopping")
	notify(logger, systemd, sdnotify.Stopping, sdnotify.Status("stopping"))
	if err := d.Stop(context.Background()); err != nil {
		logger.Error("stop incomplete", "error", err)
		return exitFailure
	}
	return exitOK
}

// systemdNotifier reads NOTIFY_SOCKET and removes it from the environment,
// as sd_notify(3) does with unset_environment: the processes sentineld
// starts must not report state for it. An invalid value disables the
// notifications and is logged without the value (untrusted environment).
func systemdNotifier(logger *slog.Logger) *sdnotify.Notifier {
	n, err := sdnotify.Parse(os.Getenv(sdnotify.EnvSocket))
	if uerr := os.Unsetenv(sdnotify.EnvSocket); uerr != nil {
		logger.Warn("cannot remove "+sdnotify.EnvSocket+" from the environment", "error", uerr)
	}
	if err != nil {
		logger.Warn("systemd notifications disabled", "error", err)
	}
	return n
}

// notify sends states to systemd. A failure is logged and never stops the
// daemon; the context is fresh, because the signal context may be done.
func notify(logger *slog.Logger, n *sdnotify.Notifier, states ...string) {
	if err := n.Notify(context.Background(), states...); err != nil {
		logger.Warn("systemd notification failed", "error", err)
	}
}

// readyStatus summarises the start for systemctl status. Modules that
// failed do not delay readiness (D-075: the daemon keeps running).
func readyStatus(s daemon.Snapshot) string {
	failed := 0
	for _, state := range s.Modules {
		if state != daemon.ModuleRunning {
			failed++
		}
	}
	if failed > 0 {
		return fmt.Sprintf("ready, degraded: %d of %d modules not running (sentinelctl modules)", failed, len(s.Modules))
	}
	return fmt.Sprintf("ready: %d modules running", len(s.Modules))
}

// newLogger builds the daemon's logger from daemon.log, which the
// configuration has already validated, and returns its level, which a
// reload changes in place.
func newLogger(c config.Log, out io.Writer) (*slog.Logger, *slog.LevelVar, error) {
	level, err := logging.ParseLevel(c.Level)
	if err != nil {
		return nil, nil, err
	}
	lv := new(slog.LevelVar)
	lv.Set(level)
	logger, err := logging.New(logging.Options{Level: lv, Format: c.Format, Output: out})
	return logger, lv, err
}
