// Command sentinelctl is the command-line client of Sentinel Watchdog.
//
//	sentinelctl validate [-all] [-config /etc/sentinel/sentinel.yaml]
//	sentinelctl version
//
// validate reads the configuration files itself, so it works when the
// daemon is stopped. The commands that talk to sentineld over its socket
// arrive with the control socket (Phase 2c).
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/config"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/daemon"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/version"
)

// Exit codes.
const (
	exitOK      = 0
	exitFailure = 1 // invalid configuration
	exitUsage   = 2
)

// planned lists the commands of the control socket, not implemented yet:
// they fail loudly instead of looking like typos.
var planned = []string{"audit", "config", "events", "modules", "reload", "status"}

const usage = `usage: sentinelctl <command> [flags]

commands:
  validate [-all] [-config file]   check the configuration files
  version                          print the version
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is main without the process exit, so tests can call it.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	switch cmd, rest := args[0], args[1:]; {
	case cmd == "version":
		if len(rest) > 0 {
			fmt.Fprintln(stderr, "sentinelctl: version takes no arguments")
			return exitUsage
		}
		fmt.Fprintln(stdout, "sentinelctl", version.String())
		return exitOK
	case cmd == "validate":
		return validate(rest, stdout, stderr)
	case cmd == "help" || cmd == "-h" || cmd == "-help" || cmd == "--help":
		fmt.Fprint(stdout, usage)
		return exitOK
	case slices.Contains(planned, cmd):
		fmt.Fprintf(stderr, "sentinelctl: %s needs the control socket, which is not implemented yet\n", cmd)
		return exitUsage
	default:
		fmt.Fprintf(stderr, "sentinelctl: unknown command %q\n%s", cmd, usage)
		return exitUsage
	}
}

func validate(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("sentinelctl validate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configFile := flags.String("config", config.DefaultMainFile, "central configuration `file`")
	all := flags.Bool("all", false, "also validate the directories of disabled modules")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "sentinelctl validate: unexpected argument %q\n", flags.Arg(0))
		return exitUsage
	}
	reg, err := daemon.Modules()
	if err != nil {
		fmt.Fprintln(stderr, "sentinelctl:", err)
		return exitFailure
	}
	cfg, err := daemon.Validate(reg, *configFile, *all)
	if werr := config.WriteDiagnostics(stdout, cfg, err); werr != nil || err != nil {
		return exitFailure
	}
	fmt.Fprintln(stdout, "configuration is valid")
	return exitOK
}
