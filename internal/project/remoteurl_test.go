package project

import (
	"errors"
	"strings"
	"testing"

	"github.com/aplexica/aplexica/internal/securityerr"
	"github.com/stretchr/testify/require"
)

func TestNormalizeRemoteURL_RemovesCredentialBearingFields(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		vcs  VCS
		want string
	}{
		{
			name: "standard URL user password query fragment",
			raw:  "https://alice:supersecret@GitHub.com/Owner/Repo.git?access_token=secret#private",
			vcs:  VCSGit,
			want: "github.com/owner/repo",
		},
		{
			name: "scp query fragment",
			raw:  "git@github.com:Owner/Repo.git?token=secret#private",
			vcs:  VCSGit,
			want: "github.com/owner/repo",
		},
		{
			name: "ipv6 URL with port",
			raw:  "ssh://alice:supersecret@[2001:DB8::1]:2222/Owner/Repo.git?token=secret",
			vcs:  VCSGit,
			want: "[2001:db8::1]:2222/owner/repo",
		},
		{
			name: "ipv6 scp",
			raw:  "git@[2001:db8::1]:Owner/Repo.git#private",
			vcs:  VCSGit,
			want: "[2001:db8::1]/owner/repo",
		},
		{
			name: "plain hg",
			raw:  "bitbucket.org/Owner/Repo.hg?token=secret",
			vcs:  VCSHg,
			want: "bitbucket.org/owner/repo",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeRemoteURL(tc.raw, tc.vcs)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
			for _, secret := range []string{"alice", "supersecret", "token", "private"} {
				require.NotContains(t, got, secret)
			}
		})
	}
}

func TestNormalizeRemoteURL_RejectsUnsafeFormsWithoutEcho(t *testing.T) {
	cases := []string{
		"file:///tmp/supersecret",
		"https:///owner/supersecret",
		"https://example.com/../supersecret",
		"https://example.com/owner\\supersecret",
		"https://example.com/owner/%2frepo-supersecret",
		"https://example.com/owner/supersecret\x00",
		"user@@example.com:owner/supersecret",
		"example.com",
		strings.Repeat("a", maxRemoteURLBytes+1),
	}
	for i, raw := range cases {
		_, err := normalizeRemoteURL(raw, VCSGit)
		require.Error(t, err, "case %d", i)
		require.True(t, errors.Is(err, securityerr.ErrUnsafeIdentifier), "case %d: %v", i, err)
		require.NotContains(t, err.Error(), "supersecret", "case %d", i)
	}
}

func FuzzNormalizeRemoteURL_NoSecretEcho(f *testing.F) {
	for _, seed := range []string{
		"https://user:password@example.com/owner/repo.git?token=secret#fragment",
		"git@example.com:owner/repo.git",
		"https://example.com/../repo",
		"[2001:db8::1]:owner/repo",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		if len(raw) > maxRemoteURLBytes*2 {
			t.Skip()
		}
		got, err := normalizeRemoteURL(raw, VCSGit)
		if err != nil {
			require.NotContains(t, err.Error(), raw)
			return
		}
		require.NotContains(t, got, "?")
		require.NotContains(t, got, "#")
		require.NotContains(t, got, "@")
	})
}
