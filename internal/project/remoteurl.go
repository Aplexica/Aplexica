package project

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/aplexica/aplexica/internal/securityerr"
)

// VCS is the closed set of repository types whose remote URLs may become a
// project identity.
type VCS string

const (
	VCSGit VCS = "git"
	VCSHg  VCS = "hg"
)

const maxRemoteURLBytes = 4096

var allowedRemoteSchemes = map[string]struct{}{
	"git":     {},
	"git+ssh": {},
	"http":    {},
	"https":   {},
	"ssh":     {},
}

// normalizeRemoteURL returns the credential-free canonical identity of a VCS
// remote. It deliberately never includes raw in an error because raw may hold
// a password, token, query parameter, or fragment credential.
func normalizeRemoteURL(raw string, vcs VCS) (string, error) {
	if vcs != VCSGit && vcs != VCSHg {
		return "", unsafeRemoteError("unsupported vcs")
	}
	if len(raw) == 0 || len(raw) > maxRemoteURLBytes || !utf8.ValidString(raw) || hasUnsafeRemoteRune(raw) {
		return "", unsafeRemoteError("invalid input")
	}

	s := strings.TrimSpace(raw)
	if s == "" {
		return "", unsafeRemoteError("empty input")
	}
	// Query and fragment data are never repository identity. Remove them before
	// suffix handling so a value such as repo.git?token=... cannot persist the
	// credential or prevent canonical suffix removal.
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimRight(s, "/")
	if s == "" {
		return "", unsafeRemoteError("empty authority")
	}

	var authority, remotePath string
	if strings.Contains(s, "://") {
		parsed, err := url.Parse(s)
		if err != nil || parsed.Opaque != "" {
			return "", unsafeRemoteError("malformed url")
		}
		scheme := strings.ToLower(parsed.Scheme)
		if _, ok := allowedRemoteSchemes[scheme]; !ok {
			return "", unsafeRemoteError("unsupported scheme")
		}
		if parsed.Hostname() == "" {
			return "", unsafeRemoteError("missing host")
		}
		authority, err = canonicalAuthority(parsed.Hostname(), parsed.Port())
		if err != nil {
			return "", err
		}
		remotePath = parsed.EscapedPath()
	} else {
		var err error
		authority, remotePath, err = splitSCPLikeOrPlain(s)
		if err != nil {
			return "", err
		}
	}

	path, err := canonicalRemotePath(remotePath, vcs)
	if err != nil {
		return "", err
	}
	// Existing project IDs are case-insensitive in this product. Preserve that
	// compatibility while ensuring at minimum that the host is lowercase.
	return strings.ToLower(authority + "/" + path), nil
}

// NormalizeRemoteIdentity exposes the single credential-stripping parser to
// wire and registry boundaries without exposing raw input in diagnostics.
func NormalizeRemoteIdentity(raw, vcs string) (string, error) {
	return normalizeRemoteURL(raw, VCS(vcs))
}

func splitSCPLikeOrPlain(input string) (string, string, error) {
	s := input
	if at := strings.IndexByte(s, '@'); at >= 0 {
		boundary := firstPositive(strings.IndexByte(s, ':'), strings.IndexByte(s, '/'))
		if at == 0 || (boundary >= 0 && at > boundary) || strings.Contains(s[at+1:], "@") {
			return "", "", unsafeRemoteError("malformed user info")
		}
		s = s[at+1:]
	}

	if strings.HasPrefix(s, "[") {
		close := strings.IndexByte(s, ']')
		if close < 0 {
			return "", "", unsafeRemoteError("malformed ipv6 host")
		}
		host := s[1:close]
		rest := s[close+1:]
		switch {
		case strings.HasPrefix(rest, ":"):
			tail := rest[1:]
			if slash := strings.IndexByte(tail, '/'); slash >= 0 && allDecimal(tail[:slash]) {
				authority, err := canonicalAuthority(host, tail[:slash])
				return authority, tail[slash+1:], err
			}
			authority, err := canonicalAuthority(host, "")
			return authority, tail, err
		case strings.HasPrefix(rest, "/"):
			authority, err := canonicalAuthority(host, "")
			return authority, rest[1:], err
		default:
			return "", "", unsafeRemoteError("missing remote path")
		}
	}

	slash := strings.IndexByte(s, '/')
	colon := strings.IndexByte(s, ':')
	if colon >= 0 && (slash < 0 || colon < slash) {
		host := s[:colon]
		tail := s[colon+1:]
		if slash >= 0 {
			port := s[colon+1 : slash]
			if allDecimal(port) {
				authority, err := canonicalAuthority(host, port)
				return authority, s[slash+1:], err
			}
		}
		authority, err := canonicalAuthority(host, "")
		return authority, tail, err
	}
	if slash <= 0 {
		return "", "", unsafeRemoteError("missing remote path")
	}
	authority, err := canonicalAuthority(s[:slash], "")
	return authority, s[slash+1:], err
}

func canonicalAuthority(host, port string) (string, error) {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" || strings.ContainsAny(host, "\\/@%[]") || hasUnsafeRemoteRune(host) {
		return "", unsafeRemoteError("invalid host")
	}
	isIPv6 := strings.Contains(host, ":")
	if isIPv6 {
		if net.ParseIP(host) == nil {
			return "", unsafeRemoteError("invalid ipv6 host")
		}
	} else {
		for _, label := range strings.Split(host, ".") {
			if label == "" || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
				return "", unsafeRemoteError("invalid host")
			}
			for _, r := range label {
				if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-') {
					return "", unsafeRemoteError("invalid host")
				}
			}
		}
	}
	if port != "" {
		value, err := strconv.ParseUint(port, 10, 16)
		if err != nil || value == 0 {
			return "", unsafeRemoteError("invalid port")
		}
		if isIPv6 {
			return net.JoinHostPort(host, port), nil
		}
		return host + ":" + port, nil
	}
	if isIPv6 {
		return "[" + host + "]", nil
	}
	return host, nil
}

func canonicalRemotePath(escaped string, vcs VCS) (string, error) {
	escaped = strings.Trim(escaped, "/")
	if escaped == "" || strings.Contains(escaped, "\\") {
		return "", unsafeRemoteError("invalid path")
	}
	lowerEscaped := strings.ToLower(escaped)
	if strings.Contains(lowerEscaped, "%2f") || strings.Contains(lowerEscaped, "%5c") || strings.Contains(lowerEscaped, "%00") {
		return "", unsafeRemoteError("ambiguous escaped path")
	}
	decoded, err := url.PathUnescape(escaped)
	if err != nil || !utf8.ValidString(decoded) || hasUnsafeRemoteRune(decoded) {
		return "", unsafeRemoteError("invalid escaped path")
	}
	parts := strings.Split(decoded, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", unsafeRemoteError("invalid path component")
		}
	}
	suffix := ".git"
	if vcs == VCSHg {
		suffix = ".hg"
	}
	if strings.HasSuffix(strings.ToLower(decoded), suffix) {
		decoded = decoded[:len(decoded)-len(suffix)]
	}
	decoded = strings.TrimRight(decoded, "/")
	if decoded == "" {
		return "", unsafeRemoteError("empty repository path")
	}
	return decoded, nil
}

func firstPositive(values ...int) int {
	result := -1
	for _, value := range values {
		if value >= 0 && (result < 0 || value < result) {
			result = value
		}
	}
	return result
}

func allDecimal(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func hasUnsafeRemoteRune(s string) bool {
	for _, r := range s {
		if r == 0 || r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func unsafeRemoteError(category string) error {
	return fmt.Errorf("project: remote %s: %w", category, securityerr.ErrUnsafeIdentifier)
}
