package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/secrets"
	"github.com/aplexica/aplexica/internal/version"
	"github.com/stretchr/testify/require"
)

func TestDoctor_BasicReport(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	secretsRoot := filepath.Join(tmp, "secrets")
	stateDir := filepath.Join(tmp, "state")
	require.NoError(t, (&acf.Store{Root: storeRoot}).Init())

	var buf bytes.Buffer
	writeDoctorReport(&buf, &doctorInputs{
		StoreRoot:   storeRoot,
		SecretsRoot: secretsRoot,
		StateDir:    stateDir,
		Now:         time.Date(2026, 5, 24, 0, 0, 0, 0, time.UTC),
	})

	out := buf.String()
	require.Contains(t, out, "aplexica diagnostic report")
	require.Contains(t, out, version.Version)
	require.Contains(t, out, "config layers")
	require.Contains(t, out, "canonical store")
	require.Contains(t, out, "secrets store")
	require.Contains(t, out, "values never read")
}

func TestDoctor_NeverPrintsSecretValues(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	secretsRoot := filepath.Join(tmp, "secrets")
	stateDir := filepath.Join(tmp, "state")
	require.NoError(t, (&acf.Store{Root: storeRoot}).Init())

	// Seed a known secret value — it MUST NOT appear in the report.
	ss := &secrets.Store{Root: secretsRoot}
	require.NoError(t, ss.Init())
	require.NoError(t, ss.Put("artifact-1", "API_KEY", "TOP-SECRET-VALUE-DO-NOT-LEAK"))

	var buf bytes.Buffer
	writeDoctorReport(&buf, &doctorInputs{
		StoreRoot:   storeRoot,
		SecretsRoot: secretsRoot,
		StateDir:    stateDir,
		Now:         time.Now().UTC(),
	})

	out := buf.String()
	require.NotContains(t, out, "TOP-SECRET-VALUE-DO-NOT-LEAK",
		"FR-10.2 redaction: secret value must NEVER appear in the report")
	// The count is allowed.
	require.Contains(t, out, "pairs:")
}

func TestDoctor_RedactsHomePath(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	require.NoError(t, (&acf.Store{Root: storeRoot}).Init())

	// Use the real home so the redact path matches.
	home, _ := os.UserHomeDir()
	logPath := filepath.Join(home, "nope-this-file-does-not-exist.log")

	var buf bytes.Buffer
	writeDoctorReport(&buf, &doctorInputs{
		StoreRoot:   storeRoot,
		SecretsRoot: filepath.Join(tmp, "secrets"),
		StateDir:    filepath.Join(tmp, "state"),
		LogPath:     logPath,
		RedactHome:  true,
		Now:         time.Now().UTC(),
	})
	out := buf.String()
	// $HOME placeholder is present; literal home directory is not.
	require.Contains(t, out, "$HOME")
	require.NotContains(t, out, home,
		"with --redact-home (default), the literal home directory MUST NOT appear")
}

func TestScrubPII_RedactsEmail(t *testing.T) {
	in := "2026-05-24 10:00:00 user logged in: user@example.com\n"
	got := scrubPII(in)
	require.Contains(t, got, "<redacted-email>")
	require.NotContains(t, got, "user@example.com")
}

func TestScrubPII_RedactsLongHexToken(t *testing.T) {
	in := "Authorization: Bearer abcdef0123456789abcdef0123456789abcdef0123"
	got := scrubPII(in)
	require.Contains(t, got, "<redacted-token>")
	require.NotContains(t, got, "abcdef0123456789abcdef0123456789abcdef0123")
}

func TestScrubPII_LeavesShortTokensAlone(t *testing.T) {
	in := "PID 12345 started at 10:00\n"
	got := scrubPII(in)
	require.Equal(t, in, got, "short numeric tokens (PIDs, etc.) must pass through")
}

func TestCapWriter_TruncatesAtMax(t *testing.T) {
	var buf bytes.Buffer
	cap := &capWriter{w: &buf, max: 10}
	n, err := cap.Write([]byte("0123456789ABCDEF"))
	require.NoError(t, err)
	require.Equal(t, 16, n, "Write must report all bytes accepted (FR-10.2: silent truncation)")
	require.Equal(t, 10, cap.written)
	require.True(t, cap.truncated)
	require.Equal(t, "0123456789", buf.String())
}

func TestDoctor_UnderFiveMB(t *testing.T) {
	// Synthetic stress test — seed a lot of artifacts and verify the
	// output stays under the FR-10.2 5 MB cap.
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())
	for i := 0; i < 200; i++ {
		a := acf.Artifact{
			AcfSchemaVersion: acf.SchemaVersion,
			ArtifactID:       acf.NewID(),
			Kind:             acf.KindMemory,
			Scope:            acf.ScopeGlobal,
			Name:             "CLAUDE.md",
			CreatedAt:        time.Now().UTC(),
			UpdatedAt:        time.Now().UTC(),
		}
		require.NoError(t, store.WriteArtifact(a))
	}

	var buf bytes.Buffer
	cap := &capWriter{w: &buf, max: doctorMaxBytes}
	writeDoctorReport(cap, &doctorInputs{
		StoreRoot:   storeRoot,
		SecretsRoot: filepath.Join(tmp, "secrets"),
		StateDir:    filepath.Join(tmp, "state"),
		Now:         time.Now().UTC(),
	})
	require.LessOrEqual(t, buf.Len(), doctorMaxBytes,
		"report MUST stay under 5 MB cap (FR-10.2)")
}

// runDoctorCmd invokes `aplexica doctor …` via rootCmd for end-to-end
// cobra wiring coverage.
func runDoctorCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs(append([]string{"doctor"}, args...))
	t.Cleanup(func() {
		doctorStoreRoot = ""
		doctorSecretsRoot = ""
		doctorStateDir = ""
		doctorLogPath = ""
		doctorOut = ""
		doctorRedactHome = true
	})
	err := rootCmd.Execute()
	return out.String(), err
}

func TestDoctorCmd_WritesToOutFile(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	require.NoError(t, (&acf.Store{Root: storeRoot}).Init())

	outPath := filepath.Join(tmp, "report.txt")
	out, err := runDoctorCmd(t,
		"--store", storeRoot,
		"--secrets-root", filepath.Join(tmp, "secrets"),
		"--state-dir", filepath.Join(tmp, "state"),
		"--log", filepath.Join(tmp, "no-log.log"),
		"--out", outPath,
	)
	require.NoError(t, err)
	require.Contains(t, out, "wrote "+outPath)

	body, err := os.ReadFile(outPath)
	require.NoError(t, err)
	require.True(t, strings.Contains(string(body), "diagnostic report"))
}
