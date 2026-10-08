package config

import "testing"

func TestChannelReturnsAnIndependentCopy(t *testing.T) {
	enabled := true
	cfg := &Config{Notifications: Notifications{Channels: []Channel{{
		Name: "w", Enabled: &enabled,
		Headers:            map[string]string{"Authorization": "original"},
		SuccessStatusCodes: []int{200},
	}}}}
	ch, ok := cfg.Channel("w")
	if !ok {
		t.Fatal("channel not found")
	}
	ch.Headers["Authorization"] = "changed"
	ch.SuccessStatusCodes[0] = 500
	*ch.Enabled = false

	orig := cfg.Notifications.Channels[0]
	if orig.Headers["Authorization"] != "original" || orig.SuccessStatusCodes[0] != 200 || !*orig.Enabled {
		t.Fatalf("the returned channel shares state with the configuration: %+v", orig)
	}
}
