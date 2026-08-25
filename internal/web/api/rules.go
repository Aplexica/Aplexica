package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/aplexica/aplexica/internal/syncrules"
)

// RulesAccessor is the seam between the live BRD-05 rule store and the
// API handler. Rules are identified by their Name field (unique across
// shipped defaults + user rules per syncrules.Validate). Per the CLI's
// existing semantics, only USER rules can be created/updated/deleted;
// shipped defaults are immutable. The daemon's implementation of this
// interface SHOULD enforce that boundary and return a useful error
// (which the handler propagates as 400 / validation).
type RulesAccessor interface {
	List() ([]syncrules.Rule, error)
	Get(name string) (syncrules.Rule, bool, error)
	Add(r syncrules.Rule) error
	Update(name string, r syncrules.Rule) error
	Delete(name string) error
}

// ErrRuleNotFound is the sentinel an accessor returns when Update or
// Delete is called against a missing rule name. The handler maps this
// to 404; any other error becomes 400 (treated as validation) — the
// accessor surfaces its own error message.
var ErrRuleNotFound = errors.New("api: rule not found")

// RulesHandler serves the five /api/rules{,...} endpoints.
type RulesHandler struct {
	acc RulesAccessor
}

// NewRulesHandler returns a RulesHandler bound to acc.
func NewRulesHandler(acc RulesAccessor) *RulesHandler {
	return &RulesHandler{acc: acc}
}

// Register attaches the rules routes (CRUD + the presets catalog).
//
// GET /api/rules/presets is registered BEFORE GET /api/rules/{id} so the
// literal "presets" path wins over the {id} wildcard (Go 1.22's mux
// prefers the more specific pattern, but the explicit ordering documents
// intent).
func (h *RulesHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/rules", h.List)
	mux.HandleFunc("POST /api/rules", h.Add)
	mux.HandleFunc("GET /api/rules/presets", h.Presets)
	mux.HandleFunc("GET /api/rules/{id}", h.Get)
	mux.HandleFunc("PATCH /api/rules/{id}", h.Update)
	mux.HandleFunc("DELETE /api/rules/{id}", h.Delete)
}

// List serves GET /api/rules.
func (h *RulesHandler) List(w http.ResponseWriter, _ *http.Request) {
	out, err := h.acc.List()
	if err != nil {
		WriteError(w, http.StatusInternalServerError, err.Error(), "internal")
		return
	}
	if out == nil {
		out = []syncrules.Rule{}
	}
	WriteJSON(w, http.StatusOK, out)
}

// Add serves POST /api/rules. The body MUST be a complete Rule with at
// least a non-empty Name. Per-rule shape validation runs in the accessor
// implementation (which re-runs syncrules.Validate).
func (h *RulesHandler) Add(w http.ResponseWriter, r *http.Request) {
	var rule syncrules.Rule
	if err := json.NewDecoder(r.Body).Decode(&rule); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid JSON: "+err.Error(), "validation")
		return
	}
	if rule.Name == "" {
		WriteError(w, http.StatusBadRequest, "rule name is required", "validation")
		return
	}
	if err := h.acc.Add(rule); err != nil {
		WriteError(w, http.StatusBadRequest, err.Error(), "validation")
		return
	}
	WriteJSON(w, http.StatusCreated, rule)
}

// Get serves GET /api/rules/{id}.
func (h *RulesHandler) Get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		WriteError(w, http.StatusBadRequest, "rule id required", "validation")
		return
	}
	rule, ok, err := h.acc.Get(id)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, err.Error(), "internal")
		return
	}
	if !ok {
		WriteError(w, http.StatusNotFound, "rule not found: "+id, "not_found")
		return
	}
	WriteJSON(w, http.StatusOK, rule)
}

// Update serves PATCH /api/rules/{id}. Decodes the request body into
// the existing rule so missing fields preserve their current values;
// Name is forced to match the URL path (renames via PATCH are
// rejected silently — clients must DELETE+POST to rename).
func (h *RulesHandler) Update(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		WriteError(w, http.StatusBadRequest, "rule id required", "validation")
		return
	}
	existing, ok, err := h.acc.Get(id)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, err.Error(), "internal")
		return
	}
	if !ok {
		WriteError(w, http.StatusNotFound, "rule not found: "+id, "not_found")
		return
	}
	if err := json.NewDecoder(r.Body).Decode(&existing); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid JSON: "+err.Error(), "validation")
		return
	}
	existing.Name = id
	if err := h.acc.Update(id, existing); err != nil {
		if errors.Is(err, ErrRuleNotFound) {
			WriteError(w, http.StatusNotFound, "rule not found: "+id, "not_found")
			return
		}
		WriteError(w, http.StatusBadRequest, err.Error(), "validation")
		return
	}
	WriteJSON(w, http.StatusOK, existing)
}

// RulePreset is one entry in the GET /api/rules/presets catalog. A
// preset describes a rule (or, for a group, several rules) the user can
// add with a single POST per rule. Adding a preset reuses the existing
// POST /api/rules write path — there is no separate preset write API.
//
// ID is a stable client-side key (the rule's Name for singletons; a
// synthetic key like "recommended-starter-set" for groups). Title and
// Description are display strings. Rules is the concrete rule object(s)
// the client POSTs (one POST per element). Group is true for bundles.
type RulePreset struct {
	ID          string           `json:"id"`
	Title       string           `json:"title"`
	Description string           `json:"description"`
	Group       bool             `json:"group"`
	Rules       []syncrules.Rule `json:"rules"`
}

// presetMeta maps each shipped-default rule Name to a friendly title +
// description for the catalog. Names not present fall back to the Name
// itself as the title.
var presetMeta = map[string]struct{ Title, Description string }{
	"default-all-to-all": {
		Title:       "Sync everything everywhere",
		Description: "Fan every artifact out to all installed agents (the classic zero-config behavior).",
	},
	"fork-respects-origin": {
		Title:       "Forks stay on their origin agent",
		Description: "Artifacts tagged fork-of:* route back only to the agent they were forked from.",
	},
	"private-stays-local": {
		Title:       "Private artifacts never leave this device",
		Description: "Anything tagged private or secret is excluded from any remote transport.",
	},
	"tool-secrets-default-local": {
		Title:       "Keep tool secrets local",
		Description: "Tool artifacts sync without their secret values by default.",
	},
	"ephemeral-projects-stay-local": {
		Title:       "Ephemeral projects stay local",
		Description: "Artifacts in ephemeral projects are excluded from remote transports.",
	},
}

// buildPresetCatalog derives the presets catalog from the shipped
// BRD-05 §6 defaults: each default rule individually, plus a
// "recommended-starter-set" group bundling all of them. Adding a preset
// = the client POSTs each rule in Rules to POST /api/rules.
func buildPresetCatalog() ([]RulePreset, error) {
	cfg, err := syncrules.ParseDefault()
	if err != nil {
		return nil, err
	}
	out := make([]RulePreset, 0, len(cfg.Sync.Rules)+1)
	for _, r := range cfg.Sync.Rules {
		meta := presetMeta[r.Name]
		title := meta.Title
		if title == "" {
			title = r.Name
		}
		out = append(out, RulePreset{
			ID:          r.Name,
			Title:       title,
			Description: meta.Description,
			Group:       false,
			Rules:       []syncrules.Rule{r},
		})
	}
	out = append(out, RulePreset{
		ID:          "recommended-starter-set",
		Title:       "Recommended starter set",
		Description: "Sync everything everywhere plus the four safety guards (forks, private, tool secrets, ephemeral projects).",
		Group:       true,
		Rules:       append([]syncrules.Rule{}, cfg.Sync.Rules...),
	})
	return out, nil
}

// Presets serves GET /api/rules/presets — the read-only catalog of
// opt-in rule presets (the classic BRD-05 defaults). Stateless: the
// catalog is derived entirely from syncrules.ParseDefault().
func (h *RulesHandler) Presets(w http.ResponseWriter, _ *http.Request) {
	out, err := buildPresetCatalog()
	if err != nil {
		WriteError(w, http.StatusInternalServerError, err.Error(), "internal")
		return
	}
	WriteJSON(w, http.StatusOK, out)
}

// Delete serves DELETE /api/rules/{id}.
func (h *RulesHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		WriteError(w, http.StatusBadRequest, "rule id required", "validation")
		return
	}
	if err := h.acc.Delete(id); err != nil {
		if errors.Is(err, ErrRuleNotFound) {
			WriteError(w, http.StatusNotFound, "rule not found: "+id, "not_found")
			return
		}
		WriteError(w, http.StatusBadRequest, err.Error(), "validation")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
