package anonymize

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestScrub_RewritesHomePaths(t *testing.T) {
	opts := Options{HomeDir: "/Users/alice"}
	in := []byte("workspace at /Users/alice/code/proj plus /Users/alice/notes.md")
	out, matches := Scrub(in, opts)
	require.NotContains(t, string(out), "/Users/alice")
	require.Contains(t, string(out), "~/code/proj")
	require.Contains(t, string(out), "~/notes.md")
	require.GreaterOrEqual(t, len(matches), 2)
}

func TestScrub_RedactsEmails(t *testing.T) {
	opts := Options{RedactEmails: true}
	in := []byte("contact alice@example.com or bob.smith+filter@example.org for details")
	out, matches := Scrub(in, opts)
	require.NotContains(t, string(out), "alice@example.com")
	require.NotContains(t, string(out), "bob.smith+filter@example.org")
	require.Contains(t, string(out), "[REDACTED-EMAIL]")
	require.GreaterOrEqual(t, len(matches), 2)
}

func TestScrub_ScrubsCommonSecretPatterns(t *testing.T) {
	opts := Options{ScrubSecrets: true}
	in := []byte(`{
		"github_token": "ghp_1234567890abcdefghijklmnopqrstuvwxyzAB",
		"slack_token": "xoxb-1234567890-abcdefghij",
		"openai_key": "sk-1234567890abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMN",
		"aws_key": "AKIAIOSFODNN7EXAMPLE"
	}`)
	out, matches := Scrub(in, opts)
	require.NotContains(t, string(out), "ghp_1234567890abcdefghijklmnopqrstuvwxyzAB")
	require.NotContains(t, string(out), "xoxb-1234567890")
	require.NotContains(t, string(out), "sk-1234567890abc")
	require.NotContains(t, string(out), "AKIAIOSFODNN7EXAMPLE")
	require.Contains(t, string(out), "[REDACTED-SECRET]")
	require.GreaterOrEqual(t, len(matches), 4)
}

func TestScrub_AllOptionsCombined(t *testing.T) {
	opts := Options{
		HomeDir:      "/Users/alice",
		RedactEmails: true,
		ScrubSecrets: true,
	}
	in := []byte("user alice@example.com cd /Users/alice/proj && export TOKEN=ghp_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	out, _ := Scrub(in, opts)
	require.NotContains(t, string(out), "alice@example.com")
	require.NotContains(t, string(out), "/Users/alice")
	require.NotContains(t, string(out), "ghp_aaaa")
}

func TestScrub_Idempotent(t *testing.T) {
	opts := Options{HomeDir: "/Users/alice", RedactEmails: true}
	in := []byte("alice@example.com at /Users/alice")
	once, _ := Scrub(in, opts)
	twice, _ := Scrub(once, opts)
	require.Equal(t, once, twice)
}

func TestScrub_EmptyOptionsNoOp(t *testing.T) {
	opts := Options{}
	in := []byte("alice@example.com /Users/alice ghp_AAAAAAAAAAAAAAAAAAAA")
	out, matches := Scrub(in, opts)
	require.Equal(t, in, out)
	require.Empty(t, matches)
}
