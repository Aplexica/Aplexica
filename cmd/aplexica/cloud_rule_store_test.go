package main

import (
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/syncrules"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func ruleNames(rs []syncrules.Rule) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.Name)
	}
	return out
}

func TestMergeRules_CloudWinsOnConflict(t *testing.T) {
	user := []syncrules.Rule{{Name: "a"}, {Name: "shared"}}
	cloud := []syncrules.Rule{{Name: "shared"}, {Name: "b"}}
	// Surviving user rule (a) first, then all cloud rules; the user "shared"
	// is dropped because the cloud rule of the same name wins.
	assert.Equal(t, []string{"a", "shared", "b"}, ruleNames(mergeRules(user, cloud)))
}

func TestMergeRules_Empty(t *testing.T) {
	assert.Empty(t, mergeRules(nil, nil))
	assert.Equal(t, []string{"x"}, ruleNames(mergeRules([]syncrules.Rule{{Name: "x"}}, nil)))
	assert.Equal(t, []string{"y"}, ruleNames(mergeRules(nil, []syncrules.Rule{{Name: "y"}})))
}

func TestCloudRuleStore_GetNilSafe(t *testing.T) {
	var s *cloudRuleStore
	assert.Nil(t, s.get()) // nil receiver must not panic (web-disabled / tests)
}

func newTestRulesAccessor(t *testing.T, local, cloud []syncrules.Rule) *rulesWebAccessor {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rules.toml")
	require.NoError(t, writeUserRules(path, syncrules.Config{Sync: syncrules.SyncSection{Rules: local}}))
	store := newCloudRuleStore()
	store.set(cloud)
	return &rulesWebAccessor{deps: &webAPIDeps{rulesPath: path, cloudRules: store}}
}

func TestRulesWebAccessor_ListTagsAndMergesCloud(t *testing.T) {
	acc := newTestRulesAccessor(t,
		[]syncrules.Rule{{Name: "local-only"}, {Name: "shared"}},
		[]syncrules.Rule{{Name: "shared"}, {Name: "cloud-only"}},
	)
	out, err := acc.List()
	require.NoError(t, err)

	src := map[string]string{}
	for _, r := range out {
		src[r.Name] = r.Source
	}
	assert.Equal(t, "local", src["local-only"])
	assert.Equal(t, "cloud", src["cloud-only"])
	assert.Equal(t, "cloud", src["shared"], "cloud wins on a name conflict")

	shared := 0
	for _, r := range out {
		if r.Name == "shared" {
			shared++
		}
	}
	assert.Equal(t, 1, shared, "the shadowed local rule is dropped")
}

func TestRulesWebAccessor_CloudRulesAreReadOnly(t *testing.T) {
	acc := newTestRulesAccessor(t, nil, []syncrules.Rule{{Name: "cloudy"}})
	assert.Error(t, acc.Add(syncrules.Rule{Name: "cloudy"}), "cannot add over a cloud rule")
	assert.Error(t, acc.Update("cloudy", syncrules.Rule{Name: "cloudy"}), "cannot edit a cloud rule")
	assert.Error(t, acc.Delete("cloudy"), "cannot delete a cloud rule")
}
