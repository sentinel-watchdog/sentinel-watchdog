package supervisor

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/api"
)

// The read commands are read; every operator command is operate, audited
// with its service as target (D-089), and dispatched to its action.
func TestCommandsDeclared(t *testing.T) {
	cmds := New(Options{}).Commands()
	if len(cmds) != 8 || cmds[0] != api.SupervisorList || cmds[1] != api.SupervisorStatus ||
		cmds[0].Tier != api.TierRead || cmds[1].Tier != api.TierRead {
		t.Fatalf("commands %+v", cmds)
	}
	for _, c := range cmds[2:] {
		if c.Tier != api.TierOperate || !c.TargetsName || actions[c.Name] == "" || "supervisor."+string(actions[c.Name]) != c.Name {
			t.Errorf("operator command %+v is not operate, targeted and dispatched", c)
		}
	}
	if len(actions) != 6 {
		t.Errorf("actions %v", actions)
	}
}

// AC7: list and status, typed and sorted; arguments checked.
func TestListAndStatus(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := "version: 1\nservices:\n  - {name: web, url: \"https://web/\"}\n  - {name: api, url: \"http://api:8080/x\", check_interval: 5s}\n"
		in, _ := configure(t, map[string]string{"supervisor/a.yaml": cfg}, newFakes().build)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		res, apiErr := in.Handle(context.Background(), api.SupervisorList.Name, nil)
		list, ok := res.(api.SupervisorListResult)
		if apiErr != nil || !ok || len(list.Services) != 2 || list.Services[0].Name != "api" || list.Services[0].State != "healthy" {
			t.Fatalf("list %+v %v", res, apiErr)
		}
		res, apiErr = in.Handle(context.Background(), api.SupervisorStatus.Name, jsontext.Value(`{"name":"api"}`))
		st, ok := res.(api.SupervisorStatusResult)
		if apiErr != nil || !ok || len(st.Services) != 1 || st.Services[0].Endpoint != "http://api:8080" ||
			st.Services[0].CheckInterval != "5s" || st.MaxConcurrentChecks != 16 {
			t.Fatalf("status %+v %v", res, apiErr)
		}
		bad := []struct{ command, args, want string }{
			{api.SupervisorList.Name, `{"x":1}`, "no arguments"},
			{api.SupervisorStatus.Name, `{"name":"web","x":1}`, "takes"},
			{api.SupervisorStatus.Name, `{"name":5}`, "takes"},
			{api.SupervisorStatus.Name, `{"name":"Bad Name"}`, "invalid service name"},
			{api.SupervisorStatus.Name, `{"name":"nope"}`, `no service named "nope"`},
		}
		for _, b := range bad {
			_, apiErr := in.Handle(context.Background(), b.command, jsontext.Value(b.args))
			if apiErr == nil || apiErr.Code != api.CodeBadRequest || !strings.Contains(apiErr.Message, b.want) ||
				strings.Contains(apiErr.Message, "Bad Name") {
				t.Errorf("%s %s: %+v", b.command, b.args, apiErr)
			}
		}
	})
}

// AC6, D-080 (4): a URL's credentials, path and query never reach an
// event, status, a log or the state file; only scheme, host and port do.
func TestNothingLeaksSecrets(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fs := newFakes()
		in, _ := configure(t, map[string]string{"supervisor/a.yaml": webService}, fs.build)
		r := start(t, in, "")
		synctest.Wait()
		fs.get("web").set(failing)
		tick(10 * time.Second)
		tick(10 * time.Second)
		evs := r.drain()
		st, _ := in.status("")
		r.stop(t)
		state, err := os.ReadFile(filepath.Join(r.stateDir, moduleName, "state.json"))
		if err != nil {
			t.Fatal(err)
		}
		evJSON, _ := json.Marshal(evs)
		stJSON, _ := json.Marshal(st)
		all := string(evJSON) + string(stJSON) + r.logs.String() + string(state)
		for _, secret := range []string{"admin", "pw-SECRET", "TOKEN", "QSECRET", "/hook"} {
			if strings.Contains(all, secret) {
				t.Errorf("%q leaked:\n%s", secret, all)
			}
		}
		if len(evs) != 1 || !strings.Contains(string(stJSON), `"endpoint":"https://web.example.org:8443"`) {
			t.Errorf("events %s, status %s", evJSON, stJSON)
		}
	})
}

// AC6: a service's events go to its own notifications, filtered by
// event type.
func TestRouter(t *testing.T) {
	cfg := "version: 1\nservices:\n  - {name: web, url: \"https://web/\", notifications: [ops]}\n" +
		"  - {name: api, url: \"https://api/\", notifications: {channels: [pager], events: [service_recovered]}}\n" +
		"  - {name: quiet, url: \"https://q/\"}\n"
	in, err := configure(t, map[string]string{"supervisor/a.yaml": cfg}, newFakes().build)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		source string
		typ    string
		want   string
	}{
		{"web", "service_failed", "ops"},
		{"api", "service_failed", ""},
		{"api", "service_recovered", "pager"},
		{"quiet", "service_failed", ""},
		{"unknown", "service_failed", ""},
	}
	for _, tt := range tests {
		got := strings.Join(in.NotificationChannels(eventFor(tt.source, tt.typ)), ",")
		if got != tt.want {
			t.Errorf("%s %s: %q, want %q", tt.source, tt.typ, got, tt.want)
		}
	}
}
