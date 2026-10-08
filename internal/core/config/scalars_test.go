package config

import (
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

func TestDurationUnmarshal(t *testing.T) {
	tests := []struct {
		in      string
		want    time.Duration
		wantErr string
	}{
		{`"15s"`, 15 * time.Second, ""},
		{`2h`, 2 * time.Hour, ""},
		{`1m30s`, 90 * time.Second, ""},
		{`15`, 0, "must be a string"},
		{`abc`, 0, "invalid duration"},
		{`[1]`, 0, "must be a string"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			var d Duration
			err := yaml.Unmarshal([]byte(tt.in), &d)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if d.Std() != tt.want {
				t.Errorf("got %s, want %s", d, tt.want)
			}
		})
	}
}

func TestParseByteSize(t *testing.T) {
	tests := []struct {
		in      string
		want    ByteSize
		wantErr bool
	}{
		{"1073741824", 1 << 30, false},
		{"1GiB", 1 << 30, false},
		{"512 MiB", 512 << 20, false},
		{"1GB", 1_000_000_000, false},
		{"10KB", 10_000, false},
		{"100B", 100, false},
		{"0", 0, false},
		{"-1", 0, true},
		{"1.5GiB", 0, true},
		{"1XB", 0, true},
		{"99999999999TiB", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseByteSize(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("got %d, want %d", got, tt.want)
			}
		})
	}
}

func TestFileModeUnmarshal(t *testing.T) {
	tests := []struct {
		in      string
		want    FileMode
		wantErr bool
	}{
		{`"0660"`, 0o660, false},
		{`"0o640"`, 0o640, false},
		{`"600"`, 0o600, false},
		{`0660`, 0, true}, // unquoted: rejected to avoid octal ambiguity
		{`"0999"`, 0, true},
		{`"01777"`, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			var m FileMode
			err := yaml.Unmarshal([]byte(tt.in), &m)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if m != tt.want {
				t.Errorf("got %s, want %s", m, tt.want)
			}
		})
	}
	if s := FileMode(0o640).String(); s != "0640" {
		t.Errorf("String() = %q", s)
	}
}
