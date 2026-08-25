package openclaw

import (
	"context"
	"encoding/json"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/adapter/skilldialect"
)

// skillEncode normalizes the allowed-tools dialect (ADR-0043) so the stored
// artifact is agent-neutral; skillDecode denormalizes back to this agent's
// vocabulary on export. Body + all other frontmatter stay byte-verbatim.
// openclaw's dialect map is empty in v1 (no observed vocabulary), so both
// directions are pure passthrough until evidence exists.
func skillEncode(content []byte) (json.RawMessage, error) {
	content = skilldialect.Normalize("openclaw", content)
	return acf.EncodePayload(acf.SkillPayload{Format: "skill.md", Content: string(content)})
}

func skillDecode(e acf.Event) (string, error) {
	p, err := acf.DecodeSkillPayload(e)
	if err != nil {
		return "", err
	}
	return string(skilldialect.Denormalize("openclaw", []byte(p.Content))), nil
}

// ImportSkill reads a SKILL.md and writes one skill artifact.
func (a *Adapter) ImportSkill(ctx context.Context, store *acf.Store, nativePath string) ([]string, error) {
	return adapter.ImportSkillReconciled(ctx, store, a.opaqueParams(), nativePath, skillEncode)
}

// ExportSkill replays the skill artifact's event log and writes the result.
func (a *Adapter) ExportSkill(ctx context.Context, store *acf.Store, artifactID, destPath string) error {
	return adapter.ExportSkillMarked(ctx, store, a.Name(), artifactID, destPath, skillDecode)
}
