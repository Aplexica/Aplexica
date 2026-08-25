package version

import (
	"strings"
	"testing"
)

func TestStringContainsVersion(t *testing.T) {
	s := String()
	if !strings.Contains(s, Version) {
		t.Errorf("String()=%q must contain Version=%q", s, Version)
	}
	if !strings.Contains(s, "aplexica") {
		t.Errorf("String()=%q must contain the binary name", s)
	}
}

func TestStringDoesNotAppendTextAfterVersion(t *testing.T) {
	if got, want := String(), "aplexica "+Version; got != want {
		t.Fatalf("String()=%q, want exact numeric release identity %q", got, want)
	}
}
