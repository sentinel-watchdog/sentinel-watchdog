// Command sentineld is the Sentinel Watchdog daemon. It loads the central
// configuration, starts the core services and the enabled modules, and
// runs until SIGTERM or SIGINT.
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

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/config"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/logging"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/daemon"
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
	// From the first signal on, a second SIGTERM or SIGINT ends the process
	// at once, even while modules are still starting.
	context.AfterFunc(ctx, stop)
	cfg, mods, err := daemon.Load(reg, *configFile)
	if err != nil {
		_ = config.WriteDiagnostics(stderr, cfg, err) // stderr is all we have
		return exitFailure
	}
	logger, err := newLogger(cfg.Daemon.Log, stderr)
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
	hostname, err := os.Hostname()
	if err != nil {
		logger.Warn("hostname unknown; events carry none", "error", err)
	}
	d := daemon.New(cfg, mods, daemon.Options{Logger: logger, Hostname: hostname, Version: version.Version})
	// A signal during start cancels it: the module starting returns and no
	// further module starts.
	if err := d.Start(ctx); err != nil {
		logger.Error("start failed", "error", err)
		return exitFailure
	}
	<-ctx.Done()
	logger.Info("stopping")
	if err := d.Stop(context.Background()); err != nil {
		logger.Error("stop incomplete", "error", err)
		return exitFailure
	}
	return exitOK
}

// newLogger builds the daemon's logger from daemon.log, which the
// configuration has already validated.
func newLogger(c config.Log, out io.Writer) (*slog.Logger, error) {
	level, err := logging.ParseLevel(c.Level)
	if err != nil {
		return nil, err
	}
	var lv slog.LevelVar // a reload (2c-3) will change it in place
	lv.Set(level)
	return logging.New(logging.Options{Level: &lv, Format: c.Format, Output: out})
}
