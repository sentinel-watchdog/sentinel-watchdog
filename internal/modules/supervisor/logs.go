package supervisor

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/logsource"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/systemd"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/api"
)

const (
	// unitIDTimeout bounds the read of a unit's name for a request (D-099).
	unitIDTimeout = 3 * time.Second
	// logsAnswerMargin ends a read this long before its exchange: the transport
	// gives the connection and the command the same deadline, so an answer
	// ready at that instant could no longer be written.
	logsAnswerMargin = time.Second
)

// readTarget is what a request's name resolves to: nothing in a request
// reaches a read but a name that picks one of these from the configuration.
type readTarget struct {
	source api.LogSource
	unit   systemd.UnitName // a journal read: the configured unit
	id     string           // and its canonical name, when an observation read it
	output string           // or the program whose log output it holds (D-100)
	path   string           // a file read
}

// logs answers supervisor.logs. The state lock is held only to resolve the
// name: the read commands never wait for I/O under it.
func (in *instance) logs(ctx context.Context, args jsontext.Value) (any, *api.Error) {
	name, limit, apiErr := logsArgs(args)
	if apiErr != nil {
		return nil, apiErr
	}
	in.mu.Lock()
	target, apiErr := in.readTargetLocked(name)
	redact := in.redact
	in.mu.Unlock()
	if apiErr != nil {
		return nil, apiErr
	}
	if deadline, ok := ctx.Deadline(); ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline.Add(-logsAnswerMargin))
		defer cancel()
	}
	var (
		res logsource.Result
		err error
	)
	switch {
	case target.source == api.LogSourceFile:
		res, err = in.reader.File(ctx, target.path, limit, redact)
	case target.output != "":
		res, err = in.reader.Source(ctx, target.output, limit, redact)
	default:
		res, err = in.reader.Unit(ctx, in.unitID(target), limit, redact)
	}
	if err != nil {
		return nil, in.logsError(name, err, redact)
	}
	out := api.SupervisorLogsResult{Name: name, Source: target.source, Truncated: res.Truncated, Lines: make([]api.LogLine, len(res.Lines))}
	for i, l := range res.Lines {
		out.Lines[i] = api.LogLine{Time: l.Time, Text: l.Text, RunID: l.RunID}
	}
	return out, nil
}

// logsArgs checks the arguments of supervisor.logs.
func logsArgs(args jsontext.Value) (name string, limit int, apiErr *api.Error) {
	var a api.SupervisorLogsArgs
	if json.Unmarshal(args, &a, json.RejectUnknownMembers(true)) != nil || !api.ValidName(a.Name) {
		return "", 0, &api.Error{Code: api.CodeBadRequest, Message: api.SupervisorLogs.Name + ` takes {"name": <service>, "limit": <lines>}`}
	}
	if a.Limit == nil {
		return a.Name, api.DefaultLogLines, nil
	}
	if *a.Limit < 1 || *a.Limit > api.MaxLogLines {
		return "", 0, &api.Error{Code: api.CodeBadRequest, Message: fmt.Sprintf("limit must be 1 to %d", api.MaxLogLines)}
	}
	return a.Name, *a.Limit, nil
}

// readTargetLocked resolves name; the caller holds in.mu.
func (in *instance) readTargetLocked(name string) (readTarget, *api.Error) {
	if s, ok := in.byName[name]; ok {
		switch s.def.Type {
		case typeSystemd:
			return readTarget{source: api.LogSourceJournal, unit: s.def.Unit.Name, id: s.unit.last.ID}, nil
		case typeProcess:
			return in.outputTarget(name, s.def.Process.Output)
		}
		return readTarget{}, &api.Error{Code: api.CodeUnsupported, Message: "an http service has no output to read"}
	}
	if j, ok := in.jobByName[name]; ok {
		return in.outputTarget(name, j.def.Program.Output)
	}
	return readTarget{}, &api.Error{Code: api.CodeBadRequest, Message: fmt.Sprintf("no service or job named %q", name)}
}

// outputTarget resolves the output of the program name. Log output can be
// read only when it went to the journal as entries of its own, which happens
// when sentineld runs under systemd (D-100); otherwise it is lines of
// sentineld's own log, with nothing that tells whose they are.
func (in *instance) outputTarget(name string, o Output) (readTarget, *api.Error) {
	switch o.Kind {
	case outputFile:
		return readTarget{source: api.LogSourceFile, path: o.File}, nil
	case outputDiscard:
		return readTarget{}, &api.Error{Code: api.CodeUnsupported, Message: "its output is discarded"}
	}
	if !in.nativeJournal() {
		return readTarget{}, &api.Error{Code: api.CodeUnavailable,
			Message: "output sent to sentineld's log can be read only when sentineld runs under systemd; see sentineld's log"}
	}
	return readTarget{source: api.LogSourceJournal, output: name}, nil
}

// unitID says how to find the name the journal knows target's unit by: its Id.
// The journal records a unit's canonical name and journalctl --unit does not
// resolve an alias (measured on systemd 239 and 259), so a query by alias would
// find nothing and look like an empty log. A service never observed (disabled)
// is read for the request: reading changes nothing, and no answer is better
// than a wrong one.
func (in *instance) unitID(target readTarget) func(context.Context) (string, error) {
	if target.id != "" {
		return logsource.Named(target.id)
	}
	return func(ctx context.Context) (string, error) {
		ctx, cancel := context.WithTimeout(ctx, unitIDTimeout)
		defer cancel()
		st, err := in.units.Unit(ctx, target.unit)
		return st.ID, err
	}
}

// logsError maps a failed read to the answer: fixed text, the details only in
// the log, because an operator's tier is not root's.
func (in *instance) logsError(name string, err error, redact func(string) string) *api.Error {
	in.log.Warn("supervisor could not read logs", "name", name, "error", redact(err.Error()))
	switch {
	case errors.Is(err, logsource.ErrBusy):
		return &api.Error{Code: api.CodeUnavailable, Message: "too many log reads at once; try again"}
	case errors.Is(err, logsource.ErrUnsafeFile):
		return &api.Error{Code: api.CodeUnavailable, Message: "the output file is not safe to read; see sentineld's log"}
	case errors.Is(err, logsource.ErrUnavailable):
		return &api.Error{Code: api.CodeUnavailable, Message: "the logs cannot be read here; see sentineld's log"}
	}
	return &api.Error{Code: api.CodeInternal, Message: "the logs could not be read; see sentineld's log"}
}
