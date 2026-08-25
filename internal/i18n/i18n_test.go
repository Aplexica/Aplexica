package i18n

import (
	"strings"
	"testing"
)

func TestT_KnownKey_EnUS(t *testing.T) {
	SetLocale("en-US")
	got := T("tray_menu_pause")
	if got != "Pause sync" {
		t.Errorf("T(tray_menu_pause) = %q, want %q", got, "Pause sync")
	}
}

func TestT_MissingKey_ReturnsBracketed(t *testing.T) {
	SetLocale("en-US")
	got := T("no_such_key_definitely")
	if !strings.HasPrefix(got, "⟨") || !strings.HasSuffix(got, "⟩") {
		t.Errorf("missing key should render as ⟨key⟩, got %q", got)
	}
	if !strings.Contains(got, "no_such_key_definitely") {
		t.Errorf("missing-key marker should include the key, got %q", got)
	}
}

func TestT_FallsBackToEnUS_WhenLocaleMissing(t *testing.T) {
	SetLocale("xx-ZZ") // no such catalog
	got := T("tray_menu_pause")
	if got != "Pause sync" {
		t.Errorf("unknown locale should fall back to en-US, got %q", got)
	}
}

func TestTf_FormatsKnownKey(t *testing.T) {
	SetLocale("en-US")
	got := Tf("tray_menu_conflicts_count", 3)
	if got != "Conflicts (3)" {
		t.Errorf("Tf(...,3) = %q, want %q", got, "Conflicts (3)")
	}
}

func TestT_StateNames(t *testing.T) {
	SetLocale("en-US")
	for _, k := range []string{
		"tray_state_idle", "tray_state_active", "tray_state_paused",
		"tray_state_conflict", "tray_state_error", "tray_state_unknown",
	} {
		got := T(k)
		if strings.HasPrefix(got, "⟨") {
			t.Errorf("state key %s should be in en-US catalog, got %q", k, got)
		}
	}
}

func TestLocale_RoundTrip(t *testing.T) {
	SetLocale("de-DE")
	if got := Locale(); got != "de-DE" {
		t.Errorf("Locale() = %q, want de-DE", got)
	}
}

// injectCatalog registers a synthetic catalog under loc for the duration
// of a test, restoring (or removing) the prior entry on cleanup. It calls
// ensureLoaded first so the one-time embed load can't clobber the
// injection afterwards.
func injectCatalog(t *testing.T, loc string, cat map[string]string) {
	t.Helper()
	ensureLoaded()
	mu.Lock()
	prev, had := catalogs[loc]
	catalogs[loc] = cat
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		if had {
			catalogs[loc] = prev
		} else {
			delete(catalogs, loc)
		}
		mu.Unlock()
	})
}

// TestT_LanguageOnlyFallback asserts that a regional locale (fr-CA)
// resolves against a language-only catalog (fr) when no exact-match
// catalog exists, rather than silently falling through to en-US. This
// is the NFR §6.2 guarantee: dropping a <language>.toml should Just
// Work for every regional variant without an engineering change.
func TestT_LanguageOnlyFallback(t *testing.T) {
	injectCatalog(t, "fr", map[string]string{
		"tray_menu_pause": "Suspendre la synchronisation",
	})
	SetLocale("fr-CA") // no fr-CA catalog; must fall back to "fr"
	defer SetLocale("en-US")

	got := T("tray_menu_pause")
	if got != "Suspendre la synchronisation" {
		t.Errorf("T(tray_menu_pause) for fr-CA = %q, want the fr catalog value %q (got en-US fallback?)",
			got, "Suspendre la synchronisation")
	}
}

// TestT_ExactMatchWinsOverLanguageOnly asserts that when both an exact
// regional catalog (fr-CA) and a language-only catalog (fr) define a
// key, the exact match takes precedence — the language-only tier must
// sit strictly between exact-match and the en-US fallback.
func TestT_ExactMatchWinsOverLanguageOnly(t *testing.T) {
	injectCatalog(t, "fr", map[string]string{
		"tray_menu_pause": "fr-generic",
	})
	injectCatalog(t, "fr-CA", map[string]string{
		"tray_menu_pause": "fr-CA-specific",
	})
	SetLocale("fr-CA")
	defer SetLocale("en-US")

	got := T("tray_menu_pause")
	if got != "fr-CA-specific" {
		t.Errorf("T(tray_menu_pause) for fr-CA = %q, want exact-match %q", got, "fr-CA-specific")
	}
}

// TestT_StateNamesAfterLocaleSwitch asserts that flipping to a locale
// with no catalog still returns sensible state names (via fallback
// to en-US). This is the regression-guard for the v0.41.0
// localized-TrayState.String() integration: if someone refactors
// state.go's String() method away from i18n.T(), this test stays
// passing only if the en-US fallback still resolves.
func TestT_StateNamesAfterLocaleSwitch(t *testing.T) {
	// Force a locale that won't have a catalog (so we exercise the
	// "fall back to en-US" branch in T).
	SetLocale("zz-ZZ")
	defer SetLocale("en-US")
	// Each state name MUST resolve to a non-bracketed string when
	// en-US is the fallback. Bracketed (⟨…⟩) means the key is missing
	// from BOTH the active locale AND en-US — a real bug.
	for _, k := range []string{
		"tray_state_idle", "tray_state_active", "tray_state_paused",
		"tray_state_conflict", "tray_state_error", "tray_state_unknown",
	} {
		got := T(k)
		if strings.HasPrefix(got, "⟨") {
			t.Errorf("locale fallback broke for %q: got %q", k, got)
		}
		// Belt-and-suspenders: the en-US catalog defines each of the
		// five state names; they must round-trip to short lowercase
		// English strings (not arbitrary content).
		if len(got) == 0 || len(got) > 20 {
			t.Errorf("state key %q resolved to suspicious value %q", k, got)
		}
	}
}
