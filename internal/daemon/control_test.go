package daemon

import (
	"context"
	"encoding/json/v2"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/authz"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/clock"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/config"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/notify"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/transport"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/api"
)

const webhookToken = "hook-token-abc123"

// controlHarness runs a daemon with a fake clock, two configured modules
// (one fails to start) and a channel whose URL and header hold a token.
func controlHarness(t *testing.T) (*harness, *clock.Fake) {
	t.Helper()
	h := newHarness(t, &testModule{name: "alpha"}, &testModule{name: "broken", startErr: errors.New("no")})
	fake := clock.NewFake(time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC))
	h.d.clock, h.d.opts.Clock = fake, fake
	h.d.opts.Modules = map[string]config.ModuleAvailability{
		"alpha": config.ModuleAvailable, "broken": config.ModuleAvailable,
		"supervisor": config.ModulePlanned, "firewall": config.ModulePlanned,
	}
	h.cfg.Modules = []config.ModuleConfig{{Name: "alpha", Enabled: true}, {Name: "broken", Enabled: true}}
	h.cfg.Notifications.Channels[0].URL = "https://hooks.example.com/T0/" + webhookToken
	h.cfg.Notifications.Channels[0].Headers = map[string]string{"Authorization": "Bearer " + webhookToken}
	h.cfg.IgnoredDirs = []string{"/etc/sentinel/firewall"}
	if err := h.d.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.d.Stop(context.Background()) })
	return h, fake
}

func (h *harness) call(t *testing.T, command, args string) api.Response {
	t.Helper()
	req := api.Request{Version: api.Version, Command: command}
	if args != "" {
		req.Args = []byte(args)
	}
	resp, err := transport.Call(context.Background(), h.cfg.Daemon.Socket, req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestControlStatus(t *testing.T) {
	h, fake := controlHarness(t)
	fake.Advance(90 * time.Second)
	resp := h.call(t, api.CoreStatus.Name, "")
	if resp.Error != nil {
		t.Fatal(resp.Error)
	}
	var st api.Status
	if err := json.Unmarshal(resp.Result, &st); err != nil {
		t.Fatal(err)
	}
	if st.Version != "test" || st.UptimeSeconds != 90 || len(st.Channels) != 1 || st.Channels[0].Name != "ops" ||
		len(st.IgnoredDirs) != 1 || len(st.Modules) != 4 {
		t.Errorf("status %+v", st)
	}
}

// statusOf copies every counter of the snapshot.
func TestStatusOfCopiesCounters(t *testing.T) {
	h := newHarness(t)
	st := h.d.statusOf(Snapshot{
		NotifyDropped: 3, LogDropped: 4,
		Channels: map[string]notify.Stats{"ops": {Delivered: 5, Failed: 6, Dropped: 7, Suppressed: 8, SuppressedLost: 9}},
	})
	c := st.Channels[0]
	if st.NotifyDropped != 3 || st.LogDropped != 4 || c.Delivered != 5 || c.Failed != 6 || c.Dropped != 7 ||
		c.Suppressed != 8 || c.SuppressedLost != 9 {
		t.Errorf("status %+v", st)
	}
}

func TestControlModules(t *testing.T) {
	h, _ := controlHarness(t)
	resp := h.call(t, api.CoreModules.Name, "")
	var got []api.ModuleInfo
	if err := json.Unmarshal(resp.Result, &got); err != nil {
		t.Fatal(err)
	}
	want := []api.ModuleInfo{
		{Name: "alpha", Availability: "available", Enabled: true, State: "running"},
		{Name: "broken", Availability: "available", Enabled: true, State: "failed"},
		{Name: "firewall", Availability: "planned"},
		{Name: "supervisor", Availability: "planned"},
	}
	if len(got) != len(want) {
		t.Fatalf("modules %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("module %d: %+v, want %+v", i, got[i], want[i])
		}
	}
}

// config_show never carries a webhook secret.
func TestControlConfigShowIsRedacted(t *testing.T) {
	h, _ := controlHarness(t)
	resp := h.call(t, api.CoreConfigShow.Name, "")
	if resp.Error != nil || !strings.Contains(string(resp.Result), "hooks.example.com") {
		t.Fatalf("response %+v", resp)
	}
	if strings.Contains(string(resp.Result), webhookToken) {
		t.Errorf("secret in config_show: %s", resp.Result)
	}
}

func TestControlErrors(t *testing.T) {
	h, _ := controlHarness(t)
	for _, tt := range []struct {
		command, args string
		code          api.Code
	}{
		{api.CoreEvents.Name, "", api.CodeUnsupported},
		{api.CoreStatus.Name, `{"verbose":true}`, api.CodeBadRequest},
		{"core.reload", "", api.CodeUnknownCommand},
	} {
		resp := h.call(t, tt.command, tt.args)
		if resp.Error == nil || resp.Error.Code != tt.code {
			t.Errorf("%s %s: %+v, want %s", tt.command, tt.args, resp.Error, tt.code)
		}
	}
	if resp := h.call(t, api.CoreStatus.Name, "{}"); resp.Error != nil {
		t.Errorf("empty args refused: %+v", resp.Error)
	}
}

// A user outside the socket's group sees read commands; tiers come from
// the kernel's credentials (here faked), whatever the client sends.
func TestControlServesReadTier(t *testing.T) {
	h, _ := controlHarness(t)
	h.peer.Store(&authz.Peer{UID: 1000, GID: 1000})
	if resp := h.call(t, api.CoreStatus.Name, ""); resp.Error != nil {
		t.Errorf("read command denied to a reader: %+v", resp.Error)
	}
}

// A second daemon on the same socket stops before starting any module.
func TestSecondDaemonFailsBeforeModules(t *testing.T) {
	h, _ := controlHarness(t)
	second := newHarness(t, &testModule{name: "late"})
	second.cfg.Daemon.Socket = h.cfg.Daemon.Socket
	if err := second.d.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("second daemon: %v", err)
	}
	if calls := second.rec.list(); len(calls) != 0 {
		t.Errorf("second daemon ran modules: %v", calls)
	}
}

func TestUnknownSocketGroupFailsStart(t *testing.T) {
	h := newHarness(t)
	h.cfg.Daemon.SocketGroup = "nobody-here"
	if err := h.d.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "daemon.socket_group") {
		t.Errorf("err = %v", err)
	}
}

// Review 2c-2 R2: an empty argument object is empty however it is spelled.
func TestNoArgsAcceptsAnyEmptyObject(t *testing.T) {
	req, err := api.ReadRequest(strings.NewReader("{\"version\":1,\"command\":\"core.status\",\"args\":{ \t }}\n"))
	if err != nil {
		t.Fatal(err)
	}
	ran := false
	run := noArgs(func() (any, *api.Error) { ran = true; return nil, nil })
	if _, err := run(context.Background(), req.Args); err != nil || !ran {
		t.Errorf("empty object refused: %v", err)
	}
}
