package hermes

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
func skillEncode(content []byte) (json.RawMessage, error) {
	content = skilldialect.Normalize("hermes", content)
	return acf.EncodePayload(acf.SkillPayload{Format: "skill.md", Content: string(content)})
}

func skillDecode(e acf.Event) (string, error) {
	p, err := acf.DecodeSkillPayload(e)
	if err != nil {
		return "", err
	}
	return string(skilldialect.Denormalize("hermes", []byte(p.Content))), nil
}

// ImportSkill reads a SKILL.md and writes one skill artifact.
//
// Known fidelity gap (M0): a Hermes skill is a directory
// (~/.hermes/skills/<name>/) that may carry supporting files under
// references/, templates/, scripts/, assets/. Only the SKILL.md text is
// imported/exported; sibling files are NOT bundled, so a skill that references
// them will break on the receiving device. A skill-bundle ACF artifact that
// serializes the whole directory is deferred to a later milestone.
func (a *Adapter) ImportSkill(ctx context.Context, store *acf.Store, nativePath string) ([]string, error) {
	return adapter.ImportSkillReconciled(ctx, store, a.opaqueParams(), nativePath, skillEncode)
}

// ExportSkill replays the skill artifact's event log and writes the result.
func (a *Adapter) ExportSkill(ctx context.Context, store *acf.Store, artifactID, destPath string) error {
	return adapter.ExportSkillMarked(ctx, store, a.Name(), artifactID, destPath, skillDecode)
}
