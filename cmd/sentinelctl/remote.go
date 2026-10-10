package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/config"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/transport"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/api"
)

// remote runs a command of the control socket and prints its result.
func remote(cmd api.Command, args []string, stdout, stderr io.Writer) int {
	name := strings.ReplaceAll(strings.TrimPrefix(cmd.Name, "core."), "_", " ")
	flags := flag.NewFlagSet("sentinelctl "+name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	socket := flags.String("socket", config.DefaultSocket, "control socket of sentineld")
	asJSON := flags.Bool("json", false, "print the result as JSON")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "sentinelctl %s: unexpected argument %q\n", name, flags.Arg(0))
		return exitUsage
	}
	resp, err := transport.Call(context.Background(), *socket, api.Request{Version: api.Version, Command: cmd.Name})
	if err != nil {
		fmt.Fprintln(stderr, "sentinelctl:", explain(err, *socket))
		return exitFailure
	}
	if resp.Error != nil {
		fmt.Fprintf(stderr, "sentinelctl: %s (%s)\n", printable(resp.Error.Message), resp.Error.Code)
		return exitFailure
	}
	if *asJSON || cmd == api.CoreConfigShow {
		out := resp.Result.Clone()
		if err := out.Indent(); err != nil {
			fmt.Fprintln(stderr, "sentinelctl: invalid result:", err)
			return exitFailure
		}
		fmt.Fprintln(stdout, string(out))
		return exitOK
	}
	if err := render(stdout, cmd, resp.Result); err != nil {
		fmt.Fprintln(stderr, "sentinelctl: invalid result:", err)
		return exitFailure
	}
	return exitOK
}

// explain turns a connection error into advice for the operator.
func explain(err error, socket string) string {
	switch {
	case errors.Is(err, syscall.ENOENT), errors.Is(err, syscall.ECONNREFUSED):
		return fmt.Sprintf("sentineld is not running (nothing listens on %s; use -socket for another path)", socket)
	case errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM):
		return fmt.Sprintf("no access to %s: run as root or as a member of the socket's group (daemon.socket_group)", socket)
	case errors.Is(err, io.EOF), errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE):
		return "sentineld closed the connection without answering (too many clients, or it is stopping)"
	default:
		return err.Error()
	}
}

func render(w io.Writer, cmd api.Command, result jsontext.Value) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	switch cmd {
	case api.CoreStatus:
		var st api.Status
		if err := json.Unmarshal(result, &st); err != nil {
			return err
		}
		fmt.Fprintf(tw, "sentineld %s, up %s (since %s)\n\n", printable(st.Version),
			time.Duration(st.UptimeSeconds)*time.Second, st.StartedAt.Format(time.RFC3339))
		modulesTable(tw, st.Modules)
		fmt.Fprintln(tw, "\nCHANNEL\tDELIVERED\tFAILED\tDROPPED\tSUPPRESSED\tSUPPRESSED LOST")
		for _, c := range st.Channels {
			fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\n", printable(c.Name), c.Delivered, c.Failed, c.Dropped, c.Suppressed, c.SuppressedLost)
		}
		fmt.Fprintf(tw, "\nevents dropped: notifications %d, log %d\n", st.NotifyDropped, st.LogDropped)
		for _, dir := range st.IgnoredDirs {
			fmt.Fprintf(tw, "ignored module directory: %s\n", printable(dir))
		}
	case api.CoreModules:
		var mods []api.ModuleInfo
		if err := json.Unmarshal(result, &mods); err != nil {
			return err
		}
		modulesTable(tw, mods)
	default:
		fmt.Fprintln(tw, string(result))
	}
	return tw.Flush()
}

func modulesTable(w io.Writer, mods []api.ModuleInfo) {
	fmt.Fprintln(w, "MODULE\tAVAILABILITY\tENABLED\tSTATE")
	for _, m := range mods {
		state := m.State
		if state == "" {
			state = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%t\t%s\n", printable(m.Name), printable(m.Availability), m.Enabled, printable(state))
	}
}

// printable quotes a string that holds a character a terminal could
// interpret: the daemon is trusted, but text in it may come from outside.
func printable(s string) string {
	if strings.IndexFunc(s, func(r rune) bool { return !unicode.IsGraphic(r) }) >= 0 {
		return strconv.Quote(s)
	}
	return s
}
