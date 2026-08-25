package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/aplexica/aplexica/internal/daemon"
)

// ConfigAccessor is the seam between the live daemon.Config persistence
// and the API handler. RawPath returns the absolute path to the
// on-disk config.json file (so the SPA can show "Open in editor"
// affordances when a future native-tray bridge lands).
type ConfigAccessor interface {
	Load() (*daemon.Config, error)
	Patch(updates map[string]any) error
	RawPath() string
}

// configPatchWhitelist enumerates the top-level keys the SPA is
// permitted to mutate via PATCH /api/config. The UI renders only the
// subset that is safe to change from inside the browser, but the API
// keeps the lower-level tray/web keys for installer, CLI, and native
// integration flows.
//
// Anything else in the PATCH body returns 400 — never silently drop a
// field the user thought they set.
var configPatchWhitelist = map[string]struct{}{
	"logLevel": {},

	// Retention-related fields (BRD-03 §4.8).
	"snapshotCadenceConversation": {},
	"snapshotCadenceMemory":       {},
	"snapshotCadenceSkill":        {},
	"snapshotCadenceTool":         {},
	"snapshotMaxAgeConversation":  {},
	"snapshotMaxAgeMemory":        {},
	"snapshotMaxAgeSkill":         {},
	"snapshotMaxAgeTool":          {},
	"storeHighWatermarkGB":        {},

	// Hermes polling-watch period (FR-03.5) for detecting new or changed
	// Hermes conversation sessions in state.db. This is not a cloud sync
	// interval.
	"hermesWatchInterval": {},

	// Nested objects — the accessor enforces per-field whitelisting
	// inside tray/web (only tray.enabled, web.enabled, web.port are
	// safe to twiddle).
	"tray": {},
	"web":  {},
}

// ConfigHandler serves the three /api/config endpoints.
type ConfigHandler struct {
	acc ConfigAccessor
}

// NewConfigHandler returns a ConfigHandler bound to acc.
func NewConfigHandler(acc ConfigAccessor) *ConfigHandler {
	return &ConfigHandler{acc: acc}
}

// Register attaches the three config routes.
func (h *ConfigHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/config", h.Get)
	mux.HandleFunc("PATCH /api/config", h.Patch)
	mux.HandleFunc("GET /api/config/raw-path", h.RawPath)
}

// Get serves GET /api/config. Returns the entire loaded Config as JSON; the
// SPA filters by what it cares to render. This is the documented contract
// (local-web-ui-design.md §6.8): the daemon UI is loopback-only and same-user,
// and daemon.Config carries no secrets (RemoteConfig holds only an executable
// path / enabled / syncMode / interval — pairing tokens live with the plugin,
// not in config.json), so returning the full config is safe within that trust
// zone. INVARIANT: if a secret-bearing field is ever added to daemon.Config,
// project a safe subset here (or redact it) before returning — never let a
// credential leak by default as Config grows.
func (h *ConfigHandler) Get(w http.ResponseWriter, _ *http.Request) {
	cfg, err := h.acc.Load()
	if err != nil {
		WriteError(w, http.StatusInternalServerError, err.Error(), "internal")
		return
	}
	if cfg == nil {
		cfg = &daemon.Config{}
	}
	WriteJSON(w, http.StatusOK, cfg)
}

// Patch serves PATCH /api/config. Body is a partial map; only keys in
// configPatchWhitelist are accepted. Returns 400 for empty bodies and
// for any non-whitelisted key.
func (h *ConfigHandler) Patch(w http.ResponseWriter, r *http.Request) {
	var updates map[string]any
	if err := json.NewDecoder(r.Body).Decode(&updates); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid JSON: "+err.Error(), "validation")
		return
	}
	if len(updates) == 0 {
		WriteError(w, http.StatusBadRequest, "patch body must contain at least one key", "validation")
		return
	}
	for k, v := range updates {
		if _, ok := configPatchWhitelist[k]; !ok {
			WriteError(w, http.StatusBadRequest,
				"key not allowed in PATCH /api/config: "+k, "validation")
			return
		}
		// Validate the value's type BEFORE handing off to the accessor.
		// The accessor applies each key behind a guarded type assertion
		// with no else branch, so a mismatched value (e.g. a number for
		// logLevel, or an unparseable duration string) is silently
		// dropped while the success envelope below would still claim it
		// was "updated". Reject here so the response never asserts an
		// outcome the accessor did not perform.
		if err := validateConfigValue(k, v); err != nil {
			WriteError(w, http.StatusBadRequest, err.Error(), "validation")
			return
		}
	}
	if err := h.acc.Patch(updates); err != nil {
		WriteError(w, http.StatusBadRequest, err.Error(), "validation")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"updated": updates})
}

// validateConfigValue checks that v has a type the accessor will
// actually apply for the whitelisted key k. It mirrors the per-key type
// assertions in the daemon's configWebAccessor.Patch so a value the
// accessor would silently drop is rejected with 400 instead of reported
// as "updated". k is assumed to be in configPatchWhitelist already.
//
// JSON numbers decode to float64; the SPA sends durations either as a
// Go duration string ("5s", "24h") or as a number of seconds, matching
// the accessor's durationFromAny.
func validateConfigValue(k string, v any) error {
	switch k {
	case "logLevel":
		if _, ok := v.(string); !ok {
			return fmt.Errorf("config: %s: expected string, got %s", k, jsonTypeName(v))
		}
	case "storeHighWatermarkGB",
		"snapshotCadenceConversation",
		"snapshotCadenceMemory",
		"snapshotCadenceSkill",
		"snapshotCadenceTool":
		if _, ok := v.(float64); !ok {
			return fmt.Errorf("config: %s: expected number, got %s", k, jsonTypeName(v))
		}
	case "snapshotMaxAgeConversation",
		"snapshotMaxAgeMemory",
		"snapshotMaxAgeSkill",
		"snapshotMaxAgeTool",
		"hermesWatchInterval":
		if !isDurationValue(v) {
			return fmt.Errorf("config: %s: expected duration string or number of seconds, got %s", k, jsonTypeName(v))
		}
	case "tray":
		m, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("config: %s: expected object, got %s", k, jsonTypeName(v))
		}
		if en, present := m["enabled"]; present {
			if _, ok := en.(bool); !ok {
				return fmt.Errorf("config: %s.enabled: expected bool, got %s", k, jsonTypeName(en))
			}
		}
	case "web":
		m, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("config: %s: expected object, got %s", k, jsonTypeName(v))
		}
		if en, present := m["enabled"]; present {
			if _, ok := en.(bool); !ok {
				return fmt.Errorf("config: %s.enabled: expected bool, got %s", k, jsonTypeName(en))
			}
		}
		if p, present := m["port"]; present {
			if _, ok := p.(float64); !ok {
				return fmt.Errorf("config: %s.port: expected number, got %s", k, jsonTypeName(p))
			}
		}
	}
	return nil
}

// isDurationValue reports whether v is a form the accessor's
// durationFromAny accepts: a number (seconds) or a parseable Go duration
// string. An unparseable string is rejected so it is not silently
// dropped.
func isDurationValue(v any) bool {
	switch x := v.(type) {
	case float64:
		return true
	case string:
		_, err := time.ParseDuration(x)
		return err == nil
	}
	return false
}

// jsonTypeName returns a short human label for the JSON-decoded type of
// v, for use in validation error messages.
func jsonTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "bool"
	case float64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return fmt.Sprintf("%T", v)
	}
}

// RawPath serves GET /api/config/raw-path.
func (h *ConfigHandler) RawPath(w http.ResponseWriter, _ *http.Request) {
	WriteJSON(w, http.StatusOK, map[string]any{"path": h.acc.RawPath()})
}
