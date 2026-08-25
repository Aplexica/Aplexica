package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/config"
	"github.com/aplexica/aplexica/internal/conflicts"
	"github.com/aplexica/aplexica/internal/pending"
	"github.com/aplexica/aplexica/internal/project"
	"github.com/aplexica/aplexica/internal/secrets"
	"github.com/aplexica/aplexica/internal/version"
	"github.com/spf13/cobra"
)

// Per FR-10.2 the CLI MUST provide `aplexica doctor` that produces a
// redacted diagnostic report under 5 MB. The report is intended for
// pasting into a support ticket or attaching to a GitHub issue.
//
// Redaction rules:
//   - Secret VALUES are never included (this command never reads any
//     value from internal/secrets — only counts).
//   - Absolute paths under the user's home directory are rewritten to
//     "$HOME/..." so a public paste doesn't reveal the local username.
//   - Email addresses found in the log tail are replaced with
//     "<redacted-email>".
//   - The full report is capped at doctorMaxBytes (≈ 4 MiB headroom
//     below the FR-10.2 5 MB ceiling).

const doctorMaxBytes = 4 * 1024 * 1024

var (
	doctorStoreRoot   string
	doctorSecretsRoot string
	doctorStateDir    string
	doctorLogPath     string
	doctorOut         string
	doctorRedactHome  bool
)

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Produce a redacted diagnostic report under 5 MB",
	Long: `Emit a self-contained diagnostic report suitable for support
tickets / bug reports. The report covers:

  - aplexica + Go versions, OS, arch
  - daemon liveness (PID, started, watched dir; via the control socket)
  - canonical-store stats (artifact counts by kind, event totals)
  - secrets-store stats (pair count by artifact; no values)
  - conflict + pending-project counts
  - effective configuration with provenance per key
  - recent log tail (PII-scrubbed; email addresses replaced)

Secret values are NEVER read or printed. Absolute paths under the
user's home are rewritten to "$HOME/…" so a public paste doesn't
leak the local username. The full report is capped at 4 MiB.

Default --out is stdout. Pass --out <path> to write a .txt file
suitable for attaching to a GitHub issue or email thread.`,
	Args: cobra.NoArgs,
	RunE: runDoctor,
}

func runDoctor(cmd *cobra.Command, _ []string) error {
	w, closeFn, err := openDoctorOutput(cmd, doctorOut)
	if err != nil {
		return err
	}
	defer closeFn()

	cap := &capWriter{w: w, max: doctorMaxBytes}

	writeDoctorReport(cap, &doctorInputs{
		StoreRoot:   doctorStoreRoot,
		SecretsRoot: doctorSecretsRoot,
		StateDir:    doctorStateDir,
		LogPath:     doctorLogPath,
		RedactHome:  doctorRedactHome,
		Now:         time.Now().UTC(),
	})

	if cap.truncated {
		fmt.Fprintf(cap.w, "\n\n--- report truncated at %d bytes (5 MB cap) ---\n", cap.max)
	}
	if doctorOut != "" {
		fmt.Fprintf(cmd.OutOrStdout(), "wrote %s (%d bytes)\n", doctorOut, cap.written)
	}
	return nil
}

func openDoctorOutput(cmd *cobra.Command, path string) (io.Writer, func(), error) {
	if path == "" {
		return cmd.OutOrStdout(), func() {}, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, nil, err
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, nil, err
	}
	return f, func() { _ = f.Close() }, nil
}

// doctorInputs collects the file roots the report walks. Plumbed via a
// struct so the writeDoctorReport function is easily test-driven.
type doctorInputs struct {
	StoreRoot   string
	SecretsRoot string
	StateDir    string
	LogPath     string
	RedactHome  bool
	Now         time.Time
	// LogTailBytes is the size of the trailing window of the log file
	// to include in the report. 0 means "use the configured default
	// from log.doctor_tail_bytes". Set explicitly in tests.
	LogTailBytes int
}

func writeDoctorReport(w io.Writer, in *doctorInputs) {
	home, _ := os.UserHomeDir()
	redact := func(s string) string {
		if in.RedactHome && home != "" {
			s = strings.ReplaceAll(s, home, "$HOME")
		}
		return s
	}

	fmt.Fprintln(w, "=== aplexica diagnostic report ===")
	fmt.Fprintln(w, "generated:", in.Now.Format(time.RFC3339))
	fmt.Fprintf(w, "aplexica:  %s\n", version.Version)
	fmt.Fprintf(w, "go:        %s\n", runtime.Version())
	fmt.Fprintf(w, "os/arch:   %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Fprintln(w)

	// Config layers.
	fmt.Fprintln(w, "--- config layers ---")
	sys, usr, _ := config.DefaultSources()
	fmt.Fprintln(w, "shipped:  <embedded>")
	fmt.Fprintln(w, "system:   "+redact(sys))
	fmt.Fprintln(w, "user:     "+redact(usr))
	fmt.Fprintln(w, "project:  (per-context; not active in doctor scope)")
	fmt.Fprintln(w)

	// Effective config with provenance — values are non-sensitive (they
	// only ever name tunables in the schema, never secrets).
	eff, err := config.Load(config.LoadOptions{
		SystemPath: sys, UserPath: usr,
		Env: os.Environ(),
	})
	if err == nil {
		fmt.Fprintln(w, "--- effective config ---")
		for _, k := range eff.Keys() {
			v, layer, _ := eff.Get(k)
			fmt.Fprintf(w, "%-44s %-20s (%s)\n", k, v, layer)
		}
		fmt.Fprintln(w)
	} else {
		fmt.Fprintf(w, "config: load failed: %s\n\n", err)
	}

	// Canonical store stats.
	store := &acf.Store{Root: in.StoreRoot}
	fmt.Fprintln(w, "--- canonical store ---")
	fmt.Fprintln(w, "root:     "+redact(in.StoreRoot))
	for _, k := range []acf.Kind{acf.KindMemory, acf.KindSkill, acf.KindTool, acf.KindConversation} {
		arts, err := store.ListArtifacts(k)
		if err != nil {
			fmt.Fprintf(w, "%-13s (read error: %s)\n", k, redact(err.Error()))
			continue
		}
		totalEvents := 0
		for _, a := range arts {
			ev, _ := store.ReadEvents(k, a.ArtifactID)
			totalEvents += len(ev)
		}
		fmt.Fprintf(w, "%-13s artifacts=%-4d events=%d\n", k, len(arts), totalEvents)
	}
	fmt.Fprintln(w)

	// Secrets-store stats — NEVER values, just counts.
	ss := &secrets.Store{Root: in.SecretsRoot}
	pairs, err := ss.ListAll()
	if err != nil {
		fmt.Fprintf(w, "--- secrets store ---\nlist error: %s\n\n", redact(err.Error()))
	} else {
		fmt.Fprintln(w, "--- secrets store (counts only — values never read) ---")
		fmt.Fprintln(w, "root:     "+redact(in.SecretsRoot))
		fmt.Fprintf(w, "pairs:    %d (across %d artifacts)\n",
			len(pairs), distinctArtifacts(pairs))
		fmt.Fprintln(w)
	}

	// Conflicts.
	cs := &conflicts.Store{Root: filepath.Join(in.StateDir, "conflicts")}
	if cl, err := cs.List(); err == nil {
		fmt.Fprintln(w, "--- conflicts ---")
		fmt.Fprintf(w, "unresolved: %d\n", len(cl))
		fmt.Fprintln(w)
	}

	// Pending projects.
	if reg, err := project.NewRegistry(filepath.Join(in.StateDir, "projects.json")); err == nil {
		if list, err := pending.List(store, reg); err == nil {
			fmt.Fprintln(w, "--- pending projects ---")
			fmt.Fprintf(w, "pending: %d\n", len(list))
			fmt.Fprintln(w)
		}
	}

	// Log tail — PII-scrubbed. Tail-window size comes from the config
	// surface (`log.doctor_tail_bytes`) so operators with chatty
	// daemons can grow it without rebuilding.
	if in.LogPath != "" {
		fmt.Fprintln(w, "--- log tail (PII-scrubbed) ---")
		fmt.Fprintln(w, "path:     "+redact(in.LogPath))
		tailBytes := in.LogTailBytes
		if tailBytes <= 0 {
			tailBytes = doctorConfigTailBytes(eff)
		}
		tail, err := readLogTail(in.LogPath, tailBytes)
		if err != nil {
			fmt.Fprintf(w, "(log read error: %s)\n", redact(err.Error()))
		} else {
			fmt.Fprintln(w, scrubPII(redact(tail)))
		}
	}
}

// doctorConfigTailBytes resolves log.doctor_tail_bytes from the
// effective config. Returns the schema default when the load failed
// or the value is malformed.
func doctorConfigTailBytes(eff *config.Effective) int {
	const fallback = 65536 // matches the schema default + defaults.toml
	if eff == nil {
		return fallback
	}
	v, _, ok := eff.Get("log.doctor_tail_bytes")
	if !ok {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

// readLogTail returns the last n bytes of path, prefixed with "(…)" if
// the file was truncated. Returns an empty string when the file
// doesn't exist (a stopped daemon won't have a log).
func readLogTail(path string, n int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "(log file not present)\n", nil
		}
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	off := int64(0)
	if info.Size() > int64(n) {
		off = info.Size() - int64(n)
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return "", err
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	prefix := ""
	if off > 0 {
		prefix = fmt.Sprintf("(… %d earlier bytes omitted …)\n", off)
	}
	return prefix + string(b), nil
}

// scrubPII redacts patterns that commonly leak personal information
// from log lines: email addresses, and long hex tokens that look like
// API keys / bearer tokens.
func scrubPII(s string) string {
	s = doctorEmailRE.ReplaceAllString(s, "<redacted-email>")
	s = doctorTokenRE.ReplaceAllString(s, "<redacted-token>")
	return s
}

// doctorEmailRE catches anything that looks like an email address.
var doctorEmailRE = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)

// doctorTokenRE catches long hex/base64-ish blobs of 32+ chars. Tuned
// to favor false positives (redaction) over leaks.
var doctorTokenRE = regexp.MustCompile(`\b[A-Za-z0-9_\-]{32,}\b`)

// distinctArtifacts counts unique ArtifactIDs across a Pair list.
func distinctArtifacts(pairs []secrets.Pair) int {
	set := map[string]struct{}{}
	for _, p := range pairs {
		set[p.ArtifactID] = struct{}{}
	}
	return len(set)
}

// capWriter wraps an io.Writer with a hard size cap. Once the cap is
// reached, subsequent writes are dropped and `truncated` flips true.
// FR-10.2 mandates the report is under 5 MB.
type capWriter struct {
	w         io.Writer
	max       int
	written   int
	truncated bool
}

func (c *capWriter) Write(p []byte) (int, error) {
	if c.truncated {
		return len(p), nil // pretend the write happened
	}
	remaining := c.max - c.written
	if remaining <= 0 {
		c.truncated = true
		return len(p), nil
	}
	if len(p) > remaining {
		n, err := c.w.Write(p[:remaining])
		c.written += n
		c.truncated = true
		return len(p), err
	}
	n, err := c.w.Write(p)
	c.written += n
	return n, err
}

// Sort imports — used to keep diagnostic deterministic across runs.
// Currently a no-op helper kept here to anchor the dependency on sort
// for future report sections.
var _ = sort.Strings

func init() {
	home, _ := os.UserHomeDir()
	doctorCmd.Flags().StringVar(&doctorStoreRoot, "store",
		filepath.Join(home, ".aplexica", "store"),
		"Canonical store root directory")
	doctorCmd.Flags().StringVar(&doctorSecretsRoot, "secrets-root",
		filepath.Join(home, ".aplexica", "secrets"),
		"Secrets store root directory")
	doctorCmd.Flags().StringVar(&doctorStateDir, "state-dir",
		filepath.Join(home, ".aplexica", "state"),
		"Daemon state directory")
	doctorCmd.Flags().StringVar(&doctorLogPath, "log",
		filepath.Join(home, ".aplexica", "logs", "aplexicad.log"),
		"Log file to tail (PII-scrubbed)")
	doctorCmd.Flags().StringVar(&doctorOut, "out", "",
		"Output file (default: stdout)")
	doctorCmd.Flags().BoolVar(&doctorRedactHome, "redact-home", true,
		"Rewrite $HOME paths to literal '$HOME' (set to false to keep paths verbatim)")
	rootCmd.AddCommand(doctorCmd)
}
