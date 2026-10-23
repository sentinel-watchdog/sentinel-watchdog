package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
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

// reloadTimeout is how long sentinelctl reload waits by default.
const reloadTimeout = 2 * time.Minute

// remote runs a command of the control socket and prints its result.
func remote(cmd api.Command, args []string, stdout, stderr io.Writer) int {
	name := strings.NewReplacer(".", " ", "_", " ").Replace(strings.TrimPrefix(cmd.Name, "core."))
	flags := flag.NewFlagSet("sentinelctl "+name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	socket := flags.String("socket", config.DefaultSocket, "control socket of sentineld")
	asJSON := flags.Bool("json", false, "print the result as JSON")
	var limit *int
	switch cmd {
	case api.CoreAudit:
		limit = flags.Int("n", api.DefaultAuditLimit, fmt.Sprintf("number of records, 1 to %d", api.MaxAuditLimit))
	case api.CoreEvents:
		limit = flags.Int("n", api.DefaultEventsLimit, fmt.Sprintf("number of events, 1 to %d", api.MaxEventsLimit))
	}
	timeout := transport.DefaultTimeout
	if cmd == api.CoreReload {
		// The daemon allows twice daemon.shutdown_timeout (30s by default).
		flags.DurationVar(&timeout, "timeout", reloadTimeout, "how long to wait for the reload")
	}
	// Flags may come before or after the operands (supervisor status web -json).
	var operands []string
	for {
		if err := flags.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return exitOK
			}
			return exitUsage
		}
		if flags.NArg() == 0 {
			break
		}
		operands = append(operands, flags.Arg(0))
		args = flags.Args()[1:]
	}
	maxOperands := 0
	if cmd == api.SupervisorStatus {
		maxOperands = 1 // a service name
	}
	if len(operands) > maxOperands {
		fmt.Fprintf(stderr, "sentinelctl %s: unexpected argument %q\n", name, operands[maxOperands])
		return exitUsage
	}
	req := api.Request{Version: api.Version, Command: cmd.Name}
	switch {
	case limit != nil:
		// The daemon checks the bounds: the CLI is not trusted. Both
		// commands take {"limit": n}; a struct of one int always encodes.
		req.Args, _ = json.Marshal(api.AuditArgs{Limit: limit})
	case len(operands) == 1:
		// The daemon checks the name.
		req.Args, _ = json.Marshal(api.SupervisorStatusArgs{Name: operands[0]}) // a struct of one string always encodes
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	resp, err := transport.Call(ctx, *socket, req)
	if err != nil {
		fmt.Fprintln(stderr, "sentinelctl:", explain(err, *socket))
		if cmd == api.CoreReload && errors.Is(err, os.ErrDeadlineExceeded) {
			fmt.Fprintf(stderr, "sentinelctl: no answer within %s; the reload may still be running (see sentinelctl status)\n", timeout)
		}
		return exitFailure
	}
	if resp.Error != nil {
		if resp.Error.Code == api.CodeInvalidConfig {
			// One problem per line, each made safe for the terminal.
			fmt.Fprintln(stderr, "sentinelctl: the configuration was not reloaded; sentineld keeps the running one:")
			for line := range strings.Lines(resp.Error.Message) {
				fmt.Fprintln(stderr, "  "+printable(strings.TrimRight(line, "\n")))
			}
			return exitFailure
		}
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
		auditLine(tw, st.Audit)
		fmt.Fprintf(tw, "configuration: %s\n", printable(st.ConfigDigest))
		if r := st.LastReload; r != nil {
			fmt.Fprintf(tw, "last reload: %s at %s\n", printable(r.Result), r.At.UTC().Format(time.RFC3339))
		}
		if st.NotifyAbandoned > 0 {
			fmt.Fprintf(tw, "notifications abandoned by reloads: %d\n", st.NotifyAbandoned)
		}
	case api.CoreModules:
		var mods []api.ModuleInfo
		if err := json.Unmarshal(result, &mods); err != nil {
			return err
		}
		modulesTable(tw, mods)
	case api.CoreReload:
		var r api.ReloadResult
		if err := json.Unmarshal(result, &r); err != nil {
			return err
		}
		fmt.Fprintf(tw, "configuration reloaded: %s\n", printable(r.Digest))
		for _, l := range []struct {
			what  string
			names []string
		}{{"restarted", r.Restarted}, {"started", r.Started}, {"stopped", r.Stopped}, {"failed", r.Failed}} {
			if len(l.names) > 0 {
				fmt.Fprintf(tw, "%s: %s\n", l.what, printable(strings.Join(l.names, ", ")))
			}
		}
		for _, w := range r.Warnings {
			fmt.Fprintf(tw, "warning: %s\n", printable(w))
		}
	case api.CoreAudit:
		var a api.AuditResult
		if err := json.Unmarshal(result, &a); err != nil {
			return err
		}
		auditLine(tw, a.Audit)
		fmt.Fprintln(tw, "\nSEQ\tTIME\tACTOR\tACTION\tRESULT\tDETAIL")
		for _, r := range a.Records {
			fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n", r.Seq, r.Time.UTC().Format(time.RFC3339), actor(r.Actor),
				printable(r.Action), printable(r.Result), detail(r.Detail))
		}
		if a.Skipped > 0 {
			fmt.Fprintf(tw, "\n%d lines are not valid records\n", a.Skipped)
		}
	case api.CoreEvents:
		var r api.EventsResult
		if err := json.Unmarshal(result, &r); err != nil {
			return err
		}
		if len(r.Events) == 0 {
			fmt.Fprintln(tw, "no events since sentineld started")
		} else {
			fmt.Fprintln(tw, "TIME\tSEVERITY\tMODULE\tTYPE\tSOURCE\tMESSAGE")
			for _, e := range r.Events {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", when(e.Timestamp, "-"), printable(string(e.Severity)),
					printable(e.Module), printable(string(e.Type)), printable(e.Source), printable(e.Message))
			}
		}
		if r.Dropped > 0 {
			fmt.Fprintf(tw, "\n%d events were lost to a full queue since sentineld started; the journal has every one it logged\n", r.Dropped)
		}
	case api.SupervisorList:
		var l api.SupervisorListResult
		if err := json.Unmarshal(result, &l); err != nil {
			return err
		}
		if len(l.Services) == 0 {
			fmt.Fprintln(tw, "no services are supervised")
			break
		}
		fmt.Fprintln(tw, "SERVICE\tTYPE\tSTATE\tSINCE")
		for _, s := range l.Services {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", printable(s.Name), printable(s.Type), printable(s.State), when(s.Since, "-"))
		}
	case api.SupervisorStatus:
		var st api.SupervisorStatusResult
		if err := json.Unmarshal(result, &st); err != nil {
			return err
		}
		serviceStatus(tw, st)
	default:
		fmt.Fprintln(tw, string(result))
	}
	return tw.Flush()
}

// serviceStatus prints supervisor.status: the module's counters, then one
// block per service.
func serviceStatus(w io.Writer, st api.SupervisorStatusResult) {
	fmt.Fprintf(w, "checks in flight: %d of %d\n", st.ChecksInFlight, st.MaxConcurrentChecks)
	if !st.ProcessesArmed {
		fmt.Fprintln(w, "process services: not started (modules.supervisor.processes is false)")
	}
	if st.StateNotSaved {
		fmt.Fprintln(w, "state file: NOT SAVED: the last save failed; a restart would forget what changed since")
	}
	if st.PendingCleanups > 0 {
		fmt.Fprintf(w, "process groups left by a previous run: %d not cleaned up yet\n", st.PendingCleanups)
	}
	if st.StateRecovered {
		fmt.Fprintln(w, "state file: was corrupt at start: moved aside; process recovery budgets treated as spent for one window")
	}
	for _, s := range st.Services {
		fmt.Fprintf(w, "\n%s (%s): %s\n", printable(s.Name), printable(s.Type), printable(s.State))
		row := func(key, format string, args ...any) { fmt.Fprintf(w, "  %s\t%s\n", key, fmt.Sprintf(format, args...)) }
		if s.Type == "process" {
			processRows(row, s)
			continue
		}
		row("endpoint", "%s", printable(s.Endpoint))
		if !s.Since.IsZero() {
			row("since", "%s", when(s.Since, ""))
		}
		row("interval, timeout", "%s, %s", printable(s.CheckInterval), printable(s.Timeout))
		if s.LastCheck.IsZero() {
			row("last check", "never")
		} else {
			row("last check", "%s, took %dms, waited %dms for a slot", when(s.LastCheck, ""), s.LastDurationMS, s.LastSlotWaitMS)
		}
		if s.LastFailure != "" {
			row("last failure", "%s", printable(s.LastFailure))
		}
		row("in a row", "%d failures, %d successes", s.ConsecutiveFailures, s.ConsecutiveSuccesses)
		row("checks", "%d, %d failed", s.Checks, s.Failures)
		if s.IncidentID != "" {
			row("incident", "%s", printable(s.IncidentID))
		}
		if !s.WaitingSince.IsZero() {
			row("waiting", "for a check slot since %s (max_concurrent_checks)", when(s.WaitingSince, ""))
		}
		if s.Stalled {
			row("stalled", "a timed-out check has not returned; no new check starts until it does")
		}
	}
}

// processRows prints the rows of a process service.
func processRows(row func(key, format string, args ...any), s api.ServiceStatus) {
	row("phase", "%s", printable(s.Phase))
	if s.Pid != 0 {
		row("pid", "%d, up %s", s.Pid, time.Since(s.StartedAt).Truncate(time.Second))
	}
	if !s.Since.IsZero() {
		row("since", "%s", when(s.Since, ""))
	}
	row("starts", "%d, %d failed, %d restarts", s.Starts, s.Failures, s.Restarts)
	if s.MaxAttempts > 0 {
		row("recovery", "%d of %d attempts in the window", s.Attempts, s.MaxAttempts)
	}
	switch s.Phase { // the phase decides which wait is current (review 3b-3 R3)
	case "exhausted":
		row("exhausted", "since %s, next attempt after the cooldown, at %s", when(s.ExhaustedAt, "?"), when(s.ResumeAt, "?"))
	case "blocked":
		row("blocked", "%s; next try at %s", printable(s.BlockedReason), when(s.NextAttemptAt, "?"))
	case "restarting":
		row("next attempt", "%s", when(s.NextAttemptAt, "?"))
	}
	if e := s.LastExit; e != nil {
		how := fmt.Sprintf("status %d", e.Code)
		if e.Signal != "" {
			how = "signal " + printable(strings.TrimPrefix(e.Signal, "signal "))
		}
		row("last exit", "%s at %s", how, when(e.At, ""))
	}
	if s.LastFailure != "" {
		row("last failure", "%s", printable(s.LastFailure))
	}
	if s.IncidentID != "" {
		row("incident", "%s", printable(s.IncidentID))
	}
	if s.OutputLost {
		row("output", "LOST: the output file failed; the rest of the output was discarded")
	}
}

// when formats t in UTC, or none when t is zero.
func when(t time.Time, none string) string {
	if t.IsZero() {
		return none
	}
	return t.UTC().Format(time.RFC3339)
}

// auditLine says whether the audit chain is intact, and what a broken one
// means for the operator.
func auditLine(w io.Writer, a api.AuditStatus) {
	if a.Integrity == "ok" {
		fmt.Fprintf(w, "audit chain: ok, last record %d\n", a.Seq)
		return
	}
	fmt.Fprintf(w, "audit chain: %s (%s at %s): operate and admin requests are refused until the evidence is restored\n",
		strings.ToUpper(printable(a.Integrity)), printable(a.Reason), printable(a.Where))
}

func actor(a api.AuditActor) string {
	out := printable(a.Kind)
	if a.UID != nil {
		out += fmt.Sprintf(" uid %d", *a.UID)
	}
	if a.PID != 0 {
		out += fmt.Sprintf(" pid %d", a.PID)
	}
	return out
}

func detail(d map[string]string) string {
	if len(d) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(d))
	for _, k := range slices.Sorted(maps.Keys(d)) {
		parts = append(parts, printable(k)+"="+printable(d[k]))
	}
	return strings.Join(parts, " ")
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
