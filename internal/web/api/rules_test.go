package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aplexica/aplexica/internal/syncrules"
)

type fakeRulesAccessor struct {
	rules   []syncrules.Rule
	listErr error
	addErr  error
	patchFn func(name string, r syncrules.Rule) error
	delErr  error
}

func (f *fakeRulesAccessor) List() ([]syncrules.Rule, error) {
	return f.rules, f.listErr
}

func (f *fakeRulesAccessor) Get(name string) (syncrules.Rule, bool, error) {
	for _, r := range f.rules {
		if r.Name == name {
			return r, true, nil
		}
	}
	return syncrules.Rule{}, false, nil
}

func (f *fakeRulesAccessor) Add(r syncrules.Rule) error {
	if f.addErr != nil {
		return f.addErr
	}
	f.rules = append(f.rules, r)
	return nil
}

func (f *fakeRulesAccessor) Update(name string, r syncrules.Rule) error {
	if f.patchFn != nil {
		return f.patchFn(name, r)
	}
	for i, ex := range f.rules {
		if ex.Name == name {
			f.rules[i] = r
			return nil
		}
	}
	return ErrRuleNotFound
}

func (f *fakeRulesAccessor) Delete(name string) error {
	if f.delErr != nil {
		return f.delErr
	}
	out := f.rules[:0]
	found := false
	for _, r := range f.rules {
		if r.Name == name {
			found = true
			continue
		}
		out = append(out, r)
	}
	if !found {
		return ErrRuleNotFound
	}
	f.rules = out
	return nil
}

func TestRulesList_HappyPath(t *testing.T) {
	acc := &fakeRulesAccessor{
		rules: []syncrules.Rule{
			{Name: "rule-a"}, {Name: "rule-b"},
		},
	}
	h := NewRulesHandler(acc)

	req := httptest.NewRequest(http.MethodGet, "/api/rules", nil)
	rr := httptest.NewRecorder()
	h.List(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}
	var got []syncrules.Rule
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 2 || got[0].Name != "rule-a" {
		t.Errorf("rules = %+v", got)
	}
}

func TestRulesAdd_HappyPath(t *testing.T) {
	acc := &fakeRulesAccessor{}
	h := NewRulesHandler(acc)

	body, _ := json.Marshal(syncrules.Rule{Name: "new-rule"})
	req := httptest.NewRequest(http.MethodPost, "/api/rules", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	h.Add(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	if len(acc.rules) != 1 || acc.rules[0].Name != "new-rule" {
		t.Errorf("rules after add = %+v", acc.rules)
	}
}

func TestRulesAdd_RouteDevicesRoundTrips(t *testing.T) {
	// route.devices is a cloud-stage predicate the local engine ignores,
	// but it MUST survive the rules API unchanged so the cloud portal can
	// author and read it back. POST a rule carrying route.devices, then
	// GET it and assert the field round-trips verbatim.
	acc := &fakeRulesAccessor{}
	h := NewRulesHandler(acc)

	body, _ := json.Marshal(syncrules.Rule{
		Name:  "phone-only",
		Route: syncrules.RouteSpec{Agents: []string{"claude-code"}, Devices: []string{"dev-a", "dev-b"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/rules", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	h.Add(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("add status = %d; body=%s", rr.Code, rr.Body.String())
	}

	getReq := httptest.NewRequest(http.MethodGet, "/api/rules/phone-only", nil)
	getReq.SetPathValue("id", "phone-only")
	getRR := httptest.NewRecorder()
	h.Get(getRR, getReq)
	if getRR.Code != http.StatusOK {
		t.Fatalf("get status = %d; body=%s", getRR.Code, getRR.Body.String())
	}

	var got syncrules.Rule
	if err := json.Unmarshal(getRR.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Route.Devices) != 2 || got.Route.Devices[0] != "dev-a" || got.Route.Devices[1] != "dev-b" {
		t.Errorf("route.devices = %v, want [dev-a dev-b]", got.Route.Devices)
	}

	// Sanity: the JSON wire form uses the camelCase "devices" key.
	if !bytes.Contains(getRR.Body.Bytes(), []byte(`"devices":["dev-a","dev-b"]`)) {
		t.Errorf("expected camelCase devices key in body: %s", getRR.Body.String())
	}
}

func TestRulesAdd_MissingName(t *testing.T) {
	acc := &fakeRulesAccessor{}
	h := NewRulesHandler(acc)

	body, _ := json.Marshal(map[string]any{})
	req := httptest.NewRequest(http.MethodPost, "/api/rules", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	h.Add(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	var be ErrorBody
	if err := json.Unmarshal(rr.Body.Bytes(), &be); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if be.Code != "validation" {
		t.Errorf("code = %q, want validation", be.Code)
	}
}

func TestRulesAdd_BadJSON(t *testing.T) {
	acc := &fakeRulesAccessor{}
	h := NewRulesHandler(acc)

	req := httptest.NewRequest(http.MethodPost, "/api/rules", bytes.NewReader([]byte("{notjson")))
	rr := httptest.NewRecorder()
	h.Add(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestRulesGet_HappyPath(t *testing.T) {
	acc := &fakeRulesAccessor{rules: []syncrules.Rule{{Name: "existing"}}}
	h := NewRulesHandler(acc)

	req := httptest.NewRequest(http.MethodGet, "/api/rules/existing", nil)
	req.SetPathValue("id", "existing")
	rr := httptest.NewRecorder()
	h.Get(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
}

func TestRulesGet_NotFound(t *testing.T) {
	acc := &fakeRulesAccessor{}
	h := NewRulesHandler(acc)

	req := httptest.NewRequest(http.MethodGet, "/api/rules/missing", nil)
	req.SetPathValue("id", "missing")
	rr := httptest.NewRecorder()
	h.Get(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestRulesPatch_HappyPath(t *testing.T) {
	acc := &fakeRulesAccessor{rules: []syncrules.Rule{{Name: "existing", Mode: "live"}}}
	h := NewRulesHandler(acc)

	body, _ := json.Marshal(map[string]any{"mode": "manual"})
	req := httptest.NewRequest(http.MethodPatch, "/api/rules/existing", bytes.NewReader(body))
	req.SetPathValue("id", "existing")
	rr := httptest.NewRecorder()
	h.Update(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}
	if acc.rules[0].Mode != "manual" {
		t.Errorf("rule.Mode = %q, want manual", acc.rules[0].Mode)
	}
}

func TestRulesPatch_EnabledFalseRoundTrips(t *testing.T) {
	// BRD-05 §6.5: a portal PATCH {"enabled":false} must disable a rule
	// (not be silently dropped). The handler json-merges the body into the
	// existing rule, so once Rule.Enabled exists this round-trips through
	// Get → Update → Get. Assert the stored rule is now disabled and that a
	// subsequent GET reports enabled:false on the wire.
	acc := &fakeRulesAccessor{rules: []syncrules.Rule{{Name: "togglable", Mode: "live"}}}
	h := NewRulesHandler(acc)

	body, _ := json.Marshal(map[string]any{"enabled": false})
	req := httptest.NewRequest(http.MethodPatch, "/api/rules/togglable", bytes.NewReader(body))
	req.SetPathValue("id", "togglable")
	rr := httptest.NewRecorder()
	h.Update(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("patch status = %d; body=%s", rr.Code, rr.Body.String())
	}
	if acc.rules[0].Enabled == nil || *acc.rules[0].Enabled {
		t.Fatalf("rule.Enabled = %v, want non-nil false", acc.rules[0].Enabled)
	}
	// Other fields must be preserved by the merge (PATCH semantics).
	if acc.rules[0].Mode != "live" {
		t.Errorf("rule.Mode = %q, want live (preserved across PATCH)", acc.rules[0].Mode)
	}

	getReq := httptest.NewRequest(http.MethodGet, "/api/rules/togglable", nil)
	getReq.SetPathValue("id", "togglable")
	getRR := httptest.NewRecorder()
	h.Get(getRR, getReq)
	if getRR.Code != http.StatusOK {
		t.Fatalf("get status = %d; body=%s", getRR.Code, getRR.Body.String())
	}
	if !bytes.Contains(getRR.Body.Bytes(), []byte(`"enabled":false`)) {
		t.Errorf("expected enabled:false in GET body: %s", getRR.Body.String())
	}

	// Re-enabling via PATCH {"enabled":true} must flip it back on.
	body2, _ := json.Marshal(map[string]any{"enabled": true})
	req2 := httptest.NewRequest(http.MethodPatch, "/api/rules/togglable", bytes.NewReader(body2))
	req2.SetPathValue("id", "togglable")
	rr2 := httptest.NewRecorder()
	h.Update(rr2, req2)
	if rr2.Code != http.StatusOK {
		t.Fatalf("re-enable patch status = %d; body=%s", rr2.Code, rr2.Body.String())
	}
	if acc.rules[0].Enabled == nil || !*acc.rules[0].Enabled {
		t.Errorf("rule.Enabled after re-enable = %v, want non-nil true", acc.rules[0].Enabled)
	}
}

func TestRulesDelete_HappyPath(t *testing.T) {
	acc := &fakeRulesAccessor{rules: []syncrules.Rule{{Name: "kill-me"}}}
	h := NewRulesHandler(acc)

	req := httptest.NewRequest(http.MethodDelete, "/api/rules/kill-me", nil)
	req.SetPathValue("id", "kill-me")
	rr := httptest.NewRecorder()
	h.Delete(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
	if len(acc.rules) != 0 {
		t.Errorf("rules after delete = %d, want 0", len(acc.rules))
	}
}

func TestRulesPresets_Catalog(t *testing.T) {
	h := NewRulesHandler(&fakeRulesAccessor{})

	req := httptest.NewRequest(http.MethodGet, "/api/rules/presets", nil)
	rr := httptest.NewRecorder()
	h.Presets(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}
	var got []RulePreset
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// 5 classic defaults individually + 1 recommended-starter-set group.
	if len(got) != 6 {
		t.Fatalf("presets = %d, want 6: %+v", len(got), got)
	}
	var (
		sawAllToAll bool
		group       *RulePreset
	)
	for i := range got {
		switch got[i].ID {
		case "default-all-to-all":
			sawAllToAll = true
			if len(got[i].Rules) != 1 || got[i].Rules[0].Name != "default-all-to-all" {
				t.Errorf("default-all-to-all preset carries wrong rule: %+v", got[i])
			}
			if got[i].Group {
				t.Errorf("default-all-to-all should not be a group")
			}
			if got[i].Title == "" {
				t.Errorf("preset %q missing title", got[i].ID)
			}
		case "recommended-starter-set":
			group = &got[i]
		}
	}
	if !sawAllToAll {
		t.Errorf("missing default-all-to-all preset")
	}
	if group == nil {
		t.Fatalf("missing recommended-starter-set group")
	}
	if !group.Group {
		t.Errorf("recommended-starter-set Group = false, want true")
	}
	if len(group.Rules) != 5 {
		t.Errorf("recommended-starter-set bundles %d rules, want 5", len(group.Rules))
	}
}

func TestRulesDelete_NotFound(t *testing.T) {
	acc := &fakeRulesAccessor{delErr: ErrRuleNotFound}
	h := NewRulesHandler(acc)

	req := httptest.NewRequest(http.MethodDelete, "/api/rules/nope", nil)
	req.SetPathValue("id", "nope")
	rr := httptest.NewRecorder()
	h.Delete(rr, req)

	// Delete uses generic error path; ensure non-2xx and an envelope.
	if rr.Code < 400 {
		t.Fatalf("status = %d, want 4xx/5xx", rr.Code)
	}
}
