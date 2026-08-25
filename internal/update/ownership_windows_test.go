//go:build windows

package update

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// ownedByWinGet had no coverage at all: ownership_test.go is //go:build
// !windows and nothing replaced it, so the one classification branch that only
// exists on Windows was never executed even though the Test workflow runs a
// Windows leg.
//
// The gate is two-part on purpose. The path prefix alone proves nothing — a
// user can create a directory called WinGet\Packages — so a receipt from
// `winget list` is required as well; and the receipt alone proves nothing
// either, because WinGet can perfectly well have some other Aplexica install
// registered while this executable came from somewhere else.
func TestClassifierRequiresBothTheWinGetPathAndTheWinGetReceipt(t *testing.T) {
	packages := filepath.Join(t.TempDir(), "Microsoft", "WinGet", "Packages",
		"Aplexica.Aplexica_Microsoft.Winget.Source_8wekyb3d8bbwe")
	inWinGet := filepath.Join(packages, "aplexica.exe")
	elsewhere := filepath.Join(t.TempDir(), "tools", "aplexica.exe")

	receipt := fakeRunner(func(name string, _ ...string) ([]byte, error) {
		if name == "winget" {
			return []byte("Name    Id                   Version\r\n" +
				"Aplexica Aplexica.Aplexica   1.2.3\r\n"), nil
		}
		return nil, fmt.Errorf("not installed")
	})
	noReceipt := fakeRunner(func(string, ...string) ([]byte, error) {
		return nil, fmt.Errorf("no package found matching input criteria")
	})
	unrelatedReceipt := fakeRunner(func(name string, _ ...string) ([]byte, error) {
		if name == "winget" {
			return []byte("No installed package found matching input criteria.\r\n"), nil
		}
		return nil, fmt.Errorf("not installed")
	})

	for name, testCase := range map[string]struct {
		executable string
		runner     fakeRunner
		want       InstallMethod
	}{
		"winget path and receipt":  {inWinGet, receipt, MethodWinGet},
		"winget path, no receipt":  {inWinGet, noReceipt, MethodUnknown},
		"winget path, empty list":  {inWinGet, unrelatedReceipt, MethodUnknown},
		"receipt but foreign path": {elsewhere, receipt, MethodUnknown},
	} {
		t.Run(name, func(t *testing.T) {
			installation, err := (Classifier{Runner: testCase.runner}).Classify(
				t.Context(), testCase.executable, releaseProvenance("v1.2.3"),
			)
			if err != nil {
				t.Fatal(err)
			}
			if installation.Method != testCase.want {
				t.Fatalf("classified as %q, want %q (%+v)", installation.Method, testCase.want, installation)
			}
			if testCase.want != MethodWinGet {
				return
			}
			if installation.ChannelEnabled ||
				installation.ManagerCommand != "winget upgrade --id Aplexica.Aplexica --exact" {
				t.Fatalf("paused WinGet install carries unexpected channel state: %+v", installation)
			}
		})
	}
}

// The path gate is matched case-insensitively on a slash-normalised path,
// because Windows hands back either separator and any casing.
func TestClassifierRecognizesWinGetPathRegardlessOfCaseAndSeparator(t *testing.T) {
	receipt := fakeRunner(func(name string, _ ...string) ([]byte, error) {
		if name == "winget" {
			return []byte("Aplexica.Aplexica  1.2.3\r\n"), nil
		}
		return nil, fmt.Errorf("not installed")
	})
	root := t.TempDir()
	for _, segments := range [][]string{
		{"Microsoft", "WinGet", "Packages", "aplexica.exe"},
		{"AppData", "Local", "MICROSOFT", "WINGET", "Packages", "aplexica.exe"},
		{"WinGet", "Packages", "Aplexica.Aplexica", "aplexica.exe"},
	} {
		executable := filepath.Join(append([]string{root}, segments...)...)
		installation, err := (Classifier{Runner: receipt}).Classify(
			t.Context(), executable, releaseProvenance("v1.2.3"),
		)
		if err != nil {
			t.Fatal(err)
		}
		if installation.Method != MethodWinGet {
			t.Fatalf("%s classified as %q, want %q",
				strings.Join(segments, "/"), installation.Method, MethodWinGet)
		}
	}
}
