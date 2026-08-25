package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testP256PublicKey = `-----BEGIN PUBLIC KEY-----
MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEhyQCx0E9wQWSFI9ULGwy3BuRklnt
IqozONbbdbqz11hlRJy9c7SG+hdcFl9jE9uE/dwtuwU2MqU9T/cN0YkWww==
-----END PUBLIC KEY-----
`

func TestCheckAcceptsExactP256PKIXPEM(t *testing.T) {
	filename := writeKey(t, testP256PublicKey)
	got, err := check(filename)
	if err != nil {
		t.Fatal(err)
	}
	if got.PEMSHA256 != "f4cea466e5e887a45da5031757fa1d32655d83420639dc1758749b744179f126" {
		t.Fatalf("PEM SHA-256 = %q", got.PEMSHA256)
	}
	if got.SPKISHA256Base64 != "rbgIqwcUgkJ7ehNbmfZAWAVTdXxGRUtJ1tryJ8Ga748=" {
		t.Fatalf("SPKI key hint = %q", got.SPKISHA256Base64)
	}
}

func TestCheckRejectsMalformedOrExpandedTrustAnchors(t *testing.T) {
	for name, contents := range map[string]string{
		"empty":           "",
		"certificate":     strings.Replace(testP256PublicKey, "PUBLIC KEY", "CERTIFICATE", 2),
		"trailing text":   testP256PublicKey + "not part of the key\n",
		"multiple blocks": testP256PublicKey + testP256PublicKey,
		"invalid DER":     "-----BEGIN PUBLIC KEY-----\nYWJj\n-----END PUBLIC KEY-----\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := check(writeKey(t, contents)); err == nil {
				t.Fatal("check accepted malformed trust anchor")
			}
		})
	}
}

func writeKey(t *testing.T, contents string) string {
	t.Helper()
	filename := filepath.Join(t.TempDir(), "release.pub")
	if err := os.WriteFile(filename, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return filename
}
