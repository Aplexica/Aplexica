// Package anonymize scrubs personally-identifiable + secret patterns
// from bundle payload bytes. Implements ADR-0011 V2 scope.
package anonymize

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"
)

// Options controls which scrub passes run. Zero value = no-op.
type Options struct {
	// HomeDir, when non-empty, rewrites occurrences of this absolute prefix
	// to "~/". Typically os.UserHomeDir() at call time.
	HomeDir string

	// RedactEmails replaces email-shaped substrings with "[REDACTED-EMAIL]".
	RedactEmails bool

	// ScrubSecrets replaces common secret patterns (GitHub PAT, OpenAI key,
	// Slack token, AWS access key, generic 32+ hex chars) with
	// "[REDACTED-SECRET]". Best-effort — pattern coverage is finite.
	ScrubSecrets bool
}

// Match describes a single redaction for the dry-run path. Offset is in
// the INPUT bytes; Length is the length of the matched substring.
type Match struct {
	Kind   string // "path", "email", "secret"
	Offset int
	Length int
	Sample string // first ~80 chars of the matched text (truncated)
}

var (
	emailRegexp = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)

	// Conservative secret regexps — high precision, may miss novel formats.
	secretRegexps = []*regexp.Regexp{
		regexp.MustCompile(`ghp_[A-Za-z0-9]{36,}`),          // GitHub PAT
		regexp.MustCompile(`gho_[A-Za-z0-9]{36,}`),          // GitHub OAuth
		regexp.MustCompile(`xox[baprs]-[A-Za-z0-9\-]{10,}`), // Slack tokens
		regexp.MustCompile(`sk-[A-Za-z0-9]{32,}`),           // OpenAI keys
		regexp.MustCompile(`AKIA[0-9A-Z]{16}`),              // AWS access key ID
		regexp.MustCompile(`(?i)\b[a-f0-9]{40,}\b`),         // Generic hex secrets ≥40 chars
	}
)

const (
	emailPlaceholder  = "[REDACTED-EMAIL]"
	secretPlaceholder = "[REDACTED-SECRET]"
)

// Scrub applies the requested scrub passes to data. Returns the scrubbed
// bytes + a slice of matches (offsets are into the ORIGINAL data, useful
// for dry-run UIs). Idempotent: a second pass over already-scrubbed
// output produces no new matches and identical bytes.
func Scrub(data []byte, opts Options) ([]byte, []Match) {
	var matches []Match
	out := data

	// 1. Path rewrites. Done first so subsequent regex passes don't get
	//    confused by raw home paths embedded in tool output etc.
	if opts.HomeDir != "" {
		homeBytes := []byte(opts.HomeDir)
		idx := 0
		for {
			i := bytes.Index(out[idx:], homeBytes)
			if i < 0 {
				break
			}
			absoluteOffset := idx + i
			matches = append(matches, Match{
				Kind:   "path",
				Offset: absoluteOffset,
				Length: len(homeBytes),
				Sample: opts.HomeDir,
			})
			idx = absoluteOffset + len(homeBytes)
		}
		out = bytes.ReplaceAll(out, homeBytes, []byte("~"))
	}

	// 2. Emails.
	if opts.RedactEmails {
		out = scrubRegexp(out, emailRegexp, emailPlaceholder, "email", &matches)
	}

	// 3. Secrets — iterate the patterns in priority order. Each pass
	//    rewrites in place; subsequent patterns may not find anything if
	//    a prior pattern already redacted.
	if opts.ScrubSecrets {
		for _, re := range secretRegexps {
			out = scrubRegexp(out, re, secretPlaceholder, "secret", &matches)
		}
	}

	return out, matches
}

func scrubRegexp(data []byte, re *regexp.Regexp, placeholder, kind string, matches *[]Match) []byte {
	indexes := re.FindAllIndex(data, -1)
	for _, idx := range indexes {
		matched := data[idx[0]:idx[1]]
		sample := string(matched)
		if len(sample) > 80 {
			sample = sample[:80] + "…"
		}
		*matches = append(*matches, Match{
			Kind:   kind,
			Offset: idx[0],
			Length: idx[1] - idx[0],
			Sample: sample,
		})
	}
	return re.ReplaceAll(data, []byte(placeholder))
}

// MatchesByKind groups matches into a kind→count map for summary UI.
func MatchesByKind(matches []Match) map[string]int {
	out := map[string]int{}
	for _, m := range matches {
		out[m.Kind]++
	}
	return out
}

// FormatSummary renders a human-readable summary line for the dry-run mode.
func FormatSummary(matches []Match) string {
	if len(matches) == 0 {
		return "no matches"
	}
	counts := MatchesByKind(matches)
	parts := []string{}
	for _, kind := range []string{"path", "email", "secret"} {
		if n, ok := counts[kind]; ok {
			parts = append(parts, kind+":"+strconv.Itoa(n))
		}
	}
	return strings.Join(parts, " ")
}
