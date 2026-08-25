package daemon

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

// TestWebEnabledTriState verifies the *bool pattern: nil falls back to
// WebEnabledDefault (true), explicit true/false honor the user's choice.
// Mirrors the TrayEnabled tri-state semantics.
func TestWebEnabledTriState(t *testing.T) {
	tFalse := false
	tTrue := true
	cases := []struct {
		name string
		cfg  *Config
		want bool
	}{
		{"nil cfg uses default (true)", nil, true},
		{"zero cfg uses default (true)", &Config{}, true},
		{"explicit Web.Enabled=nil uses default (true)", &Config{Web: WebConfig{Enabled: nil}}, true},
		{"explicit Web.Enabled=true", &Config{Web: WebConfig{Enabled: &tTrue}}, true},
		{"explicit Web.Enabled=false", &Config{Web: WebConfig{Enabled: &tFalse}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := WebEnabled(tc.cfg); got != tc.want {
				t.Errorf("WebEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestWebEnabledDefault returns the platform-independent V1 default: true.
// Local web UI is opt-out, not opt-in.
func TestWebEnabledDefault(t *testing.T) {
	if got := WebEnabledDefault(); got != true {
		t.Errorf("WebEnabledDefault() = %v, want true", got)
	}
}

// TestWebBindDefault returns the V1 fixed default ("127.0.0.1") regardless
// of platform. ::1 is supported via the listener constructor but is not
// the documented default.
func TestWebBindDefault(t *testing.T) {
	if WebBindDefault != "127.0.0.1" {
		t.Errorf("WebBindDefault = %q, want \"127.0.0.1\"", WebBindDefault)
	}
}

// TestWebBindFallback verifies the empty-string fallback to WebBindDefault.
func TestWebBindFallback(t *testing.T) {
	cases := []struct {
		name string
		cfg  *Config
		want string
	}{
		{"nil cfg uses default", nil, "127.0.0.1"},
		{"zero cfg uses default", &Config{}, "127.0.0.1"},
		{"empty Bind uses default", &Config{Web: WebConfig{Bind: ""}}, "127.0.0.1"},
		{"explicit ::1 honored", &Config{Web: WebConfig{Bind: "::1"}}, "::1"},
		{"explicit 127.0.0.1 honored", &Config{Web: WebConfig{Bind: "127.0.0.1"}}, "127.0.0.1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := WebBind(tc.cfg); got != tc.want {
				t.Errorf("WebBind() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestWebConfigJSONRoundtrip confirms `omitempty` on each field works as
// expected: a zero WebConfig marshals to an empty object {} and survives
// round-trip without acquiring spurious fields. Critical for the
// upgrade-detection path in W2.8 — first-run notice fires only when the
// on-disk config has no `web` section at all, and an empty {} would be
// indistinguishable from a "user explicitly left web at defaults" state.
func TestWebConfigJSONRoundtrip(t *testing.T) {
	tFalse := false
	cases := []struct {
		name      string
		in        WebConfig
		wantJSON  string
		wantEmpty bool
	}{
		{
			name:      "zero value marshals empty",
			in:        WebConfig{},
			wantJSON:  "{}",
			wantEmpty: true,
		},
		{
			name:      "explicit enabled=false serializes",
			in:        WebConfig{Enabled: &tFalse},
			wantJSON:  `{"enabled":false}`,
			wantEmpty: false,
		},
		{
			name:      "explicit port serializes",
			in:        WebConfig{Port: 7600},
			wantJSON:  `{"port":7600}`,
			wantEmpty: false,
		},
		{
			name:      "explicit bind serializes",
			in:        WebConfig{Bind: "::1"},
			wantJSON:  `{"bind":"::1"}`,
			wantEmpty: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(tc.in)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(data) != tc.wantJSON {
				t.Errorf("Marshal() = %q, want %q", data, tc.wantJSON)
			}
			var out WebConfig
			if err := json.Unmarshal(data, &out); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			// Spot-check: the round-tripped value should compare equal on
			// the relevant fields. Enabled is a pointer so check via deref.
			if (tc.in.Enabled == nil) != (out.Enabled == nil) {
				t.Errorf("Enabled pointer state changed after round-trip")
			}
			if tc.in.Enabled != nil && *tc.in.Enabled != *out.Enabled {
				t.Errorf("Enabled value changed: in=%v out=%v", *tc.in.Enabled, *out.Enabled)
			}
			if tc.in.Bind != out.Bind {
				t.Errorf("Bind changed: in=%q out=%q", tc.in.Bind, out.Bind)
			}
			if tc.in.Port != out.Port {
				t.Errorf("Port changed: in=%d out=%d", tc.in.Port, out.Port)
			}
		})
	}
}

// TestConfigEmbedsWebAsEmptyObjectOnZero documents the actual serialization
// behavior: Go's encoding/json omitempty does NOT omit zero-value nested
// structs (a known stdlib limitation that would require a pointer field
// to work), so a fresh Config with default WebConfig produces "web":{}.
// This matches the existing TrayConfig behavior; the upgrade-detection
// path in W2.8 uses raw-JSON inspection (ConfigHasWebSection) instead of
// relying on omitempty semantics.
func TestConfigEmbedsWebAsEmptyObjectOnZero(t *testing.T) {
	cfg := Config{Dir: "/tmp"}
	data, err := json.Marshal(&cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got := string(data)
	if !contains(got, `"web":{}`) {
		t.Errorf("zero Web should marshal as \"web\":{} (matching TrayConfig behavior); got %s", got)
	}
}

// TestLoadConfigPreservesWeb confirms the loader correctly round-trips the
// Web section through a real file system path (atomicfile, JSON I/O).
func TestLoadConfigPreservesWeb(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	tFalse := false
	in := &Config{
		Dir: "/example",
		Web: WebConfig{
			Enabled: &tFalse,
			Bind:    "::1",
			Port:    7600,
		},
	}
	if err := WriteConfig(path, in); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	out, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if out.Web.Enabled == nil || *out.Web.Enabled != false {
		t.Errorf("Web.Enabled lost across write/read: %+v", out.Web)
	}
	if out.Web.Bind != "::1" {
		t.Errorf("Web.Bind = %q, want \"::1\"", out.Web.Bind)
	}
	if out.Web.Port != 7600 {
		t.Errorf("Web.Port = %d, want 7600", out.Web.Port)
	}
}

// contains is a small helper to avoid pulling strings into the test file's
// imports (and to mirror the existing config_test.go's local-helper style).
func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
