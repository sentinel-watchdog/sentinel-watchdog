// Command sentinelctl is the command-line client of Sentinel Watchdog.
//
//	sentinelctl status|modules [-socket path] [-json]
//	sentinelctl events|audit [-n N] [-socket path] [-json]
//	sentinelctl reload [-timeout duration] [-socket path] [-json]
//	sentinelctl config show [-socket path] [-json]
//	sentinelctl supervisor list [-socket path] [-json]
//	sentinelctl supervisor status [name] [-socket path] [-json]
//	sentinelctl supervisor start|stop|restart|enable|disable|reset <name> [-socket path] [-json]
//	sentinelctl validate [-all] [-config /etc/sentinel/sentinel.yaml]
//	sentinelctl version
//
// validate reads the configuration files itself, so it works when the
// daemon is stopped; the other commands ask sentineld over its control
// socket, which decides what the calling user may see (ADR-0012).
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/config"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/daemon"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/version"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/api"
)

// Exit codes.
const (
	exitOK      = 0
	exitFailure = 1 // invalid configuration, daemon unreachable, request refused
	exitUsage   = 2
)

const usage = `usage: sentinelctl <command> [flags]

commands:
  status [-socket path] [-json]        daemon status, modules and counters
  modules [-socket path] [-json]       modules known to sentineld
  events [-n N] [-socket path] [-json] the most recent events, newest first
  audit [-n N] [-socket path] [-json]  last audit records and the chain's integrity
  reload [-timeout d] [-socket path]   reread the configuration (operate tier)
  config show [-socket path] [-json]   the core configuration, secrets redacted
  supervisor list [-socket path] [-json]
                                       supervised services and their state
  supervisor status [name] [-socket path] [-json]
                                       details of one or every supervised service
  supervisor start|stop|restart <name> [-socket path] [-json]
                                       start, stop (held until start) or restart a
                                       process service (operate tier)
  supervisor enable|disable <name> [-socket path] [-json]
                                       take a service out of supervision and back
                                       (operate tier)
  supervisor reset <name> [-socket path] [-json]
                                       renew the recovery budget of a process service
                                       (operate tier)
  validate [-all] [-config file]       check the configuration files
  version                              print the version
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
	case cmd == "status":
		return remote(api.CoreStatus, rest, stdout, stderr)
	case cmd == "modules":
		return remote(api.CoreModules, rest, stdout, stderr)
	case cmd == "events":
		return remote(api.CoreEvents, rest, stdout, stderr)
	case cmd == "audit":
		return remote(api.CoreAudit, rest, stdout, stderr)
	case cmd == "reload":
		return remote(api.CoreReload, rest, stdout, stderr)
	case cmd == "config" && len(rest) > 0 && rest[0] == "show":
		return remote(api.CoreConfigShow, rest[1:], stdout, stderr)
	case cmd == "supervisor" && len(rest) > 0 && rest[0] == "list":
		return remote(api.SupervisorList, rest[1:], stdout, stderr)
	case cmd == "supervisor" && len(rest) > 0 && rest[0] == "status":
		return remote(api.SupervisorStatus, rest[1:], stdout, stderr)
	case cmd == "supervisor" && len(rest) > 0 && supervisorActions[rest[0]].Name != "":
		return remote(supervisorActions[rest[0]], rest[1:], stdout, stderr)
	case cmd == "help" || cmd == "-h" || cmd == "-help" || cmd == "--help":
		fmt.Fprint(stdout, usage)
		return exitOK
	default:
		fmt.Fprintf(stderr, "sentinelctl: unknown command %q\n%s", cmd, usage)
		return exitUsage
	}
}

// supervisorActions are the supervisor's operator commands by subcommand.
var supervisorActions = map[string]api.Command{
	"start": api.SupervisorStart, "stop": api.SupervisorStop, "restart": api.SupervisorRestart,
	"enable": api.SupervisorEnable, "disable": api.SupervisorDisable, "reset": api.SupervisorReset,
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
