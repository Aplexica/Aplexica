package proto

import "testing"

func TestABIVersion(t *testing.T) {
	if ABIVersion != "1" {
		t.Errorf("ABIVersion = %q, want %q", ABIVersion, "1")
	}
}
