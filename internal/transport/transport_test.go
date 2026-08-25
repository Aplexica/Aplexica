package transport

import (
	"encoding/json"
	"testing"
)

func TestLocalOnlyShape(t *testing.T) {
	if LocalOnly.Mode != ModeLocal {
		t.Errorf("LocalOnly.Mode = %q, want %q", LocalOnly.Mode, ModeLocal)
	}
	if len(LocalOnly.Available) != 1 || LocalOnly.Available[0] != ModeLocal {
		t.Errorf("LocalOnly.Available = %v, want [%q]", LocalOnly.Available, ModeLocal)
	}
	if LocalOnly.BYO != nil {
		t.Errorf("LocalOnly.BYO = %+v, want nil", LocalOnly.BYO)
	}
}

func TestLocalOnlyJSONShape(t *testing.T) {
	// The wire shape matters — the SPA reads these field names.
	data, err := json.Marshal(LocalOnly)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"mode":"local","available":["local"]}`
	if string(data) != want {
		t.Errorf("Marshal(LocalOnly) = %s\n               want %s", data, want)
	}
}

func TestBYORelayOptsOmitemptyOnZero(t *testing.T) {
	// Empty BYO opts should marshal to just "{}" — no spurious
	// empty fields that look like configuration.
	data, err := json.Marshal(BYORelayOpts{})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"url":""}`
	if string(data) != want {
		t.Errorf("Marshal(BYORelayOpts{}) = %s\n               want %s", data, want)
	}
}

func TestModeStringsAreStable(t *testing.T) {
	// SPA keys off these wire strings.
	cases := map[Mode]string{
		ModeLocal:    "local",
		ModeBYORelay: "byo-relay",
		ModeHosted:   "hosted",
	}
	for m, want := range cases {
		if string(m) != want {
			t.Errorf("Mode %q != %q", m, want)
		}
	}
}
