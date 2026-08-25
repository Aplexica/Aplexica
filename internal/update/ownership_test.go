//go:build !windows

package update

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClassifierUsesManagerReceiptsNotPathSubstringAlone(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "Cellar", "aplexica", "1.2.3", "bin", "aplexica")
	runner := fakeRunner(func(name string, _ ...string) ([]byte, error) {
		switch name {
		case "brew":
			return []byte(filepath.Dir(filepath.Dir(executable)) + "\n"), nil
		default:
			return nil, fmt.Errorf("not installed")
		}
	})
	installation, err := (Classifier{Runner: runner}).Classify(t.Context(), executable, releaseProvenance("v1.2.3"))
	if err != nil {
		t.Fatal(err)
	}
	if installation.Method != MethodHomebrew {
		t.Fatalf("manager classification = %+v", installation)
	}
	installation, err = (Classifier{Runner: fakeRunner(func(string, ...string) ([]byte, error) {
		return nil, fmt.Errorf("no receipt")
	})}).Classify(t.Context(), executable, releaseProvenance("v1.2.3"))
	if err != nil {
		t.Fatal(err)
	}
	if installation.Method != MethodUnknown {
		t.Fatalf("path substring alone classified as %+v", installation)
	}
}

func TestClassifierRecognizesHomebrewInstallationThroughOptSymlink(t *testing.T) {
	root, keg, optPrefix := homebrewOwnershipFixture(t)
	linked := filepath.Join(root, "bin", "aplexica")
	runner := fakeRunner(func(name string, _ ...string) ([]byte, error) {
		if name == "brew" {
			return []byte(optPrefix + "\n"), nil
		}
		return nil, fmt.Errorf("not installed")
	})
	installation, err := (Classifier{Runner: runner}).Classify(t.Context(), linked, Provenance{
		Version: "v1.2.3", GitCommit: strings.Repeat("a", 40), ReleaseTrain: officialReleaseTrain,
	})
	if err != nil {
		t.Fatal(err)
	}
	if installation.Method != MethodHomebrew {
		t.Fatalf("brew-linked executable classified as %+v", installation)
	}
	if installation.Executable != keg {
		t.Fatalf("classified executable = %q, want the resolved keg path %q", installation.Executable, keg)
	}
}

// `brew --prefix aplexica` exits non-zero whenever the bare formula name is not
// installed, which is a normal state for a tapped formula. The keg under the
// Homebrew prefix is still a valid ownership receipt.
func TestClassifierRecognizesHomebrewKegWhenTheFormulaProbeFails(t *testing.T) {
	prefix, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(prefix, "Cellar", "aplexica", "1.2.3", "bin", "aplexica")
	if err := os.MkdirAll(filepath.Dir(executable), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	runner := fakeRunner(func(name string, args ...string) ([]byte, error) {
		if name != "brew" || len(args) != 1 || args[0] != "--prefix" {
			return nil, fmt.Errorf("not installed")
		}
		return []byte(prefix + "\n"), nil
	})
	installation, err := (Classifier{Runner: runner}).Classify(t.Context(), executable, Provenance{
		Version: "v1.2.3", GitCommit: strings.Repeat("a", 40), ReleaseTrain: officialReleaseTrain,
	})
	if err != nil {
		t.Fatal(err)
	}
	if installation.Method != MethodHomebrew || installation.ChannelEnabled ||
		installation.ManagerCommand != "brew upgrade aplexica" {
		t.Fatalf("keg under the Homebrew prefix classified as %+v", installation)
	}
}

// The Cellar fallback must stay a receipt, not a path test: a directory named
// Cellar somewhere outside the prefix brew reports proves nothing.
func TestClassifierRejectsCellarPathOutsideTheHomebrewPrefix(t *testing.T) {
	prefix, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(t.TempDir(), "Cellar", "aplexica", "1.2.3", "bin", "aplexica")
	runner := fakeRunner(func(name string, args ...string) ([]byte, error) {
		if name != "brew" || len(args) != 1 || args[0] != "--prefix" {
			return nil, fmt.Errorf("not installed")
		}
		return []byte(prefix + "\n"), nil
	})
	installation, err := (Classifier{Runner: runner}).Classify(t.Context(), executable, Provenance{
		Version: "v1.2.3", GitCommit: strings.Repeat("a", 40), ReleaseTrain: officialReleaseTrain,
	})
	if err != nil {
		t.Fatal(err)
	}
	if installation.Method != MethodUnknown {
		t.Fatalf("foreign Cellar path classified as %+v", installation)
	}
}

func TestClassifierRejectsAmbiguousManagerOwnership(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "Cellar", "aplexica", "1.2.3", "bin", "aplexica")
	prefix := filepath.Dir(filepath.Dir(executable))
	runner := fakeRunner(func(name string, _ ...string) ([]byte, error) {
		switch name {
		case "brew":
			return []byte(prefix + "\n"), nil
		case "dpkg-query":
			return []byte("aplexica: " + executable + "\n"), nil
		default:
			return nil, fmt.Errorf("not installed")
		}
	})
	installation, err := (Classifier{Runner: runner}).Classify(t.Context(), executable, Provenance{
		Version: "v1.2.3", GitCommit: strings.Repeat("a", 40), ReleaseTrain: officialReleaseTrain,
	})
	if err != nil {
		t.Fatal(err)
	}
	if installation.Method != MethodAmbiguous {
		t.Fatalf("ambiguous receipts classified as %+v", installation)
	}
}

func TestClassifierRecognizesDevelopmentBuild(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "aplexica")
	installation, err := (Classifier{Runner: fakeRunner(func(string, ...string) ([]byte, error) {
		return nil, fmt.Errorf("not installed")
	})}).Classify(t.Context(), executable, Provenance{Version: "dev", GitCommit: "unknown"})
	if err != nil {
		t.Fatal(err)
	}
	if installation.Method != MethodSource {
		t.Fatalf("development build classified as %+v", installation)
	}
}

// The top-level Makefile intentionally stamps a tag-shaped version and a full
// Git commit into ordinary local builds. Those values alone do not make an
// artifact an official release: only GoReleaser stamps ReleaseTrain.
func TestClassifierTreatsAnOrdinaryMakeBuildAsSource(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "aplexica")
	installation, err := (Classifier{Runner: fakeRunner(func(string, ...string) ([]byte, error) {
		return nil, fmt.Errorf("not installed")
	})}).Classify(t.Context(), executable, Provenance{
		Version: "v1.0.68", GitCommit: strings.Repeat("b", 40), ReleaseTrain: "",
	})
	if err != nil {
		t.Fatal(err)
	}
	if installation.Method != MethodSource {
		t.Fatalf("ordinary Makefile build classified as %+v", installation)
	}
}

// homebrewOwnershipFixture builds the directory layout `brew link` produces:
// the real binary lives in the versioned keg, `opt/<formula>` points at that
// keg, and `bin/<program>` points into the keg's bin directory. It returns the
// Homebrew prefix, the keg executable, and the opt prefix that
// `brew --prefix <formula>` prints.
func homebrewOwnershipFixture(t *testing.T) (string, string, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	kegRelative := filepath.Join("..", "Cellar", "aplexica", "1.2.3")
	keg := filepath.Join(root, "Cellar", "aplexica", "1.2.3")
	for _, directory := range []string{filepath.Join(keg, "bin"), filepath.Join(root, "bin"), filepath.Join(root, "opt")} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	executable := filepath.Join(keg, "bin", "aplexica")
	if err := os.WriteFile(executable, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	optPrefix := filepath.Join(root, "opt", "aplexica")
	if err := os.Symlink(kegRelative, optPrefix); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(kegRelative, "bin", "aplexica"), filepath.Join(root, "bin", "aplexica")); err != nil {
		t.Fatal(err)
	}
	return root, executable, optPrefix
}
