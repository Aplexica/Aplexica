package proto

import (
	"encoding/json"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
)

func TestManifestRoundTrip(t *testing.T) {
	m := Manifest{
		ManifestVersion: 1,
		Name:            "claude-code",
		Version:         "0.2.0",
		ABIVersion:      "1",
		Executable:      "./aplexica-plugin-claudecode",
		Kinds:           []acf.Kind{acf.KindMemory, acf.KindSkill},
		Formats: map[acf.Kind][]string{
			acf.KindMemory: {"markdown"},
		},
		Homepage: "https://example.com",
		Author:   "Aplexica",
		License:  "AGPL-3.0",
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Manifest
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Name != m.Name || got.ABIVersion != m.ABIVersion {
		t.Errorf("round-trip mismatch: %+v vs %+v", got, m)
	}
	if len(got.Kinds) != 2 || got.Kinds[0] != acf.KindMemory {
		t.Errorf("kinds round-trip mismatch: %v", got.Kinds)
	}
}

func TestManifestValidate(t *testing.T) {
	cases := []struct {
		name string
		m    Manifest
		ok   bool
	}{
		{"good", Manifest{ManifestVersion: 1, Name: "x", Version: "1.0", ABIVersion: "1", Executable: "x", Kinds: []acf.Kind{acf.KindMemory}}, true},
		{"bad manifest_version", Manifest{ManifestVersion: 0, Name: "x", Version: "1.0", ABIVersion: "1", Executable: "x", Kinds: []acf.Kind{acf.KindMemory}}, false},
		{"empty name", Manifest{ManifestVersion: 1, Version: "1.0", ABIVersion: "1", Executable: "x", Kinds: []acf.Kind{acf.KindMemory}}, false},
		{"abi mismatch", Manifest{ManifestVersion: 1, Name: "x", Version: "1.0", ABIVersion: "2", Executable: "x", Kinds: []acf.Kind{acf.KindMemory}}, false},
		{"no kinds", Manifest{ManifestVersion: 1, Name: "x", Version: "1.0", ABIVersion: "1", Executable: "x"}, false},
	}
	for _, c := range cases {
		err := c.m.Validate()
		if (err == nil) != c.ok {
			t.Errorf("%s: Validate err=%v, want ok=%v", c.name, err, c.ok)
		}
	}
}
