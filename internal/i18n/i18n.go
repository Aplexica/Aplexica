// Package i18n is the Aplexica string-localization layer (FR-03.31).
//
// Catalogs are flat TOML files at internal/i18n/catalogs/<locale>.toml,
// embedded into the binary via //go:embed and decoded once at first use
// into map[string]string. The English catalog (en-US.toml) is the
// authoritative key list; non-English catalogs are partial — missing
// keys fall back to en-US automatically.
//
// Locale detection at process start reads, in order:
//
//	$LC_MESSAGES, $LC_ALL, $LANG
//
// stripping any ".UTF-8"-style charset suffix and normalizing
// underscores to dashes ("en_US.UTF-8" → "en-US"). C / POSIX / unset
// values fall through to the en-US fallback. Tests force a specific
// locale via SetLocale.
//
// Usage:
//
//	i18n.T("tray_menu_pause")             // "Pause sync"
//	i18n.Tf("tray_menu_conflicts_count",  // "Conflicts (3)"
//	        3)
//
// Missing keys render as "⟨key⟩" so the visual debugging signal is
// obvious without panic'ing in user-facing flows.
package i18n

import (
	"embed"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"
)

//go:embed catalogs/*.toml
var catalogFS embed.FS

const fallbackLocale = "en-US"

var (
	initOnce sync.Once
	mu       sync.RWMutex
	locale   string
	catalogs map[string]map[string]string
)

func ensureLoaded() {
	initOnce.Do(func() {
		mu.Lock()
		defer mu.Unlock()
		catalogs = make(map[string]map[string]string)
		entries, _ := catalogFS.ReadDir("catalogs")
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".toml") {
				continue
			}
			data, err := catalogFS.ReadFile("catalogs/" + e.Name())
			if err != nil {
				continue
			}
			cat := make(map[string]string)
			if _, err := toml.Decode(string(data), &cat); err != nil {
				continue
			}
			loc := strings.TrimSuffix(e.Name(), ".toml")
			catalogs[loc] = cat
		}
		locale = detectLocale()
	})
}

// detectLocale reads the standard POSIX locale env vars and returns
// the first usable BCP-47-ish identifier. Returns fallbackLocale when
// nothing is set or the value is the no-locale sentinel (C / POSIX).
func detectLocale() string {
	for _, env := range []string{"LC_MESSAGES", "LC_ALL", "LANG"} {
		v := os.Getenv(env)
		if v == "" || v == "C" || v == "POSIX" {
			continue
		}
		// "en_US.UTF-8" → "en_US"
		if i := strings.IndexByte(v, '.'); i >= 0 {
			v = v[:i]
		}
		// "en_US" → "en-US"
		v = strings.ReplaceAll(v, "_", "-")
		return v
	}
	return fallbackLocale
}

// T returns the localized string for key, or "⟨key⟩" if the key is
// missing from both the current locale and en-US. Safe to call from
// any goroutine.
func T(key string) string {
	ensureLoaded()
	mu.RLock()
	defer mu.RUnlock()
	if cat, ok := catalogs[locale]; ok {
		if v, ok := cat[key]; ok {
			return v
		}
	}
	// Language-only fallback: a regional locale ("fr-CA") resolves
	// against a language-only catalog ("fr") before dropping to en-US,
	// so shipping a <language>.toml covers every regional variant
	// without an exact-match catalog per region (NFR §6.2).
	if i := strings.IndexByte(locale, '-'); i > 0 {
		if cat, ok := catalogs[locale[:i]]; ok {
			if v, ok := cat[key]; ok {
				return v
			}
		}
	}
	if cat, ok := catalogs[fallbackLocale]; ok {
		if v, ok := cat[key]; ok {
			return v
		}
	}
	return "⟨" + key + "⟩"
}

// Tf is T composed with fmt.Sprintf. Format-specifier mismatches
// between the catalog string and args (a real risk when translators
// reorder %s/%d positionals) surface as Go's standard %!(EXTRA …) /
// MISSING errors — same as direct fmt.Sprintf usage.
func Tf(key string, args ...any) string {
	return fmt.Sprintf(T(key), args...)
}

// SetLocale forces the active locale. Intended for tests and the
// optional future `aplexica config locale = ...` user-config knob;
// regular code should rely on the env-driven default.
func SetLocale(loc string) {
	ensureLoaded()
	mu.Lock()
	locale = loc
	mu.Unlock()
}

// Locale returns the currently active locale identifier (for diagnostic
// surfaces — `aplexica status` or future `aplexica config show`).
func Locale() string {
	ensureLoaded()
	mu.RLock()
	defer mu.RUnlock()
	return locale
}
