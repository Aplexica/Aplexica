package adapter

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/google/uuid"
)

const (
	conversationTitleMaxRunes = 160
	sessionIndexScannerBuffer = 1 << 20
	conversationCWDScanLines  = 256
)

// ResolveConversationTitle returns the best native user-facing title available
// for a conversation. Artifact.Name is already the portable title for newer
// imports. Older Codex artifacts used the rollout filename instead, so local
// source artifacts are repaired from Codex Desktop's session index.
func ResolveConversationTitle(homeDir, sourceAgent, sourcePath, artifactName string) string {
	if sourceAgent == "codex" {
		if title := codexConversationTitle(homeDir, sourcePath); title != "" {
			return title
		}
	}
	if !nativeConversationFilename(artifactName) {
		return normalizeConversationTitle(artifactName)
	}
	return ""
}

// ConversationBranchDisplayTitle makes a non-main conversation branch
// distinguishable in native agent session lists without changing the portable
// artifact title or any conversation turn. Main keeps its existing title.
func ConversationBranchDisplayTitle(title, branch string) string {
	title = normalizeConversationTitle(title)
	normalized, err := acf.NormalizeBranchName(branch)
	if err != nil || normalized == acf.MainBranch {
		return title
	}
	prefix := "[" + normalized + "]"
	if title == "" {
		return prefix
	}
	if strings.EqualFold(title, prefix) ||
		strings.HasPrefix(strings.ToLower(title), strings.ToLower(prefix)+" ") {
		return title
	}
	return normalizeConversationTitle(prefix + " " + title)
}

// PersistConversationTitle updates the portable artifact label and appends a
// full-state conversation update carrying the current payload. The payload is
// intentionally content-equivalent: the new head is the durable signal that a
// metadata-only native title change must fan out through the ordinary sync
// pipeline without duplicating turns.
func PersistConversationTitle(store *acf.Store, artifactID, title, deviceID, sourceAgent, adapterVersion string) (bool, error) {
	art, err := store.ReadArtifact(acf.KindConversation, artifactID)
	if err != nil {
		return false, err
	}
	if title == "" || art.Name == title {
		return false, nil
	}
	payload, ok, err := store.MaterializedConversationPayloadFromStore(artifactID)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	raw, err := acf.EncodePayload(payload)
	if err != nil {
		return false, err
	}
	now := time.Now().UTC()
	event := acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: artifactID,
		Type:       acf.EventTypeUpdate,
		Timestamp:  now,
		Branch:     acf.MainBranch,
		Provenance: acf.Provenance{
			DeviceID:       deviceID,
			SourceAgent:    sourceAgent,
			AgentVersion:   acf.UnknownAgentVersion,
			AdapterVersion: adapterVersion,
		},
		Payload:    raw,
		ParentHash: art.HeadEventHash,
	}
	if err := store.AppendEvent(acf.KindConversation, event); err != nil {
		return false, err
	}
	art, err = store.ReadArtifact(acf.KindConversation, artifactID)
	if err != nil {
		return false, err
	}
	art.Name = title
	art.UpdatedAt = now
	if err := store.WriteArtifact(art); err != nil {
		return false, err
	}
	return true, nil
}

// ResolveConversationCWD returns a local working directory recorded by a
// source-native conversation. It deliberately reads only conversation files
// beneath the source agent's own per-user session root: SourcePath is portable
// provenance and may point at another device, so it must never become an
// arbitrary local file-read primitive.
//
// Claude Code Desktop runs sessions in <project>/.claude/worktrees/<name>.
// Other agents should group the imported conversation with the original
// project, not with that disposable worktree, so the path is normalized back
// to the project root.
func ResolveConversationCWD(homeDir, sourceAgent, sourcePath string) string {
	if homeDir == "" || sourcePath == "" || sourceAgent != "claude-code" {
		return ""
	}
	sessionsRoot := filepath.Join(homeDir, ".claude", "projects")
	absSource, err := filepath.Abs(sourcePath)
	if err != nil || !pathWithin(sessionsRoot, absSource) {
		return ""
	}

	f, err := os.Open(absSource)
	if err != nil {
		return ""
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, conversationTitleMaxRunes), sessionIndexScannerBuffer)
	for line := 0; line < conversationCWDScanLines && scanner.Scan(); line++ {
		var row struct {
			CWD       string `json:"cwd"`
			OriginCWD string `json:"originCwd"`
		}
		if json.Unmarshal(scanner.Bytes(), &row) != nil {
			continue
		}
		cwd := row.OriginCWD
		if cwd == "" {
			cwd = row.CWD
		}
		if resolved := normalizeConversationCWD(cwd); resolved != "" {
			return resolved
		}
	}
	return ""
}

func pathWithin(root, candidate string) bool {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(absRoot, candidate)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func normalizeConversationCWD(cwd string) string {
	if !filepath.IsAbs(cwd) {
		return ""
	}
	cwd = filepath.Clean(cwd)
	worktreeMarker := string(filepath.Separator) + filepath.Join(".claude", "worktrees") + string(filepath.Separator)
	if marker := strings.Index(cwd, worktreeMarker); marker > 0 {
		cwd = cwd[:marker]
	}
	info, err := os.Stat(cwd)
	if err != nil || !info.IsDir() {
		return ""
	}
	return cwd
}

func codexConversationTitle(homeDir, sourcePath string) string {
	if homeDir == "" || sourcePath == "" {
		return ""
	}
	sessionsRoot := filepath.Join(homeDir, ".codex", "sessions")
	absSource, err := filepath.Abs(sourcePath)
	if err != nil {
		return ""
	}
	rel, err := filepath.Rel(sessionsRoot, absSource)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	sessionID := conversationSessionIDFromFilename(absSource)
	if sessionID == "" {
		return ""
	}

	f, err := os.Open(filepath.Join(homeDir, ".codex", "session_index.jsonl"))
	if err != nil {
		return ""
	}
	defer f.Close()

	var title string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, conversationTitleMaxRunes), sessionIndexScannerBuffer)
	for scanner.Scan() {
		var entry struct {
			ID         string `json:"id"`
			ThreadName string `json:"thread_name"`
		}
		if json.Unmarshal(scanner.Bytes(), &entry) == nil && entry.ID == sessionID {
			title = normalizeConversationTitle(entry.ThreadName)
		}
	}
	return title
}

func conversationSessionIDFromFilename(path string) string {
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	idLength := len(uuid.Nil.String())
	if len(base) < idLength {
		return ""
	}
	candidate := base[len(base)-idLength:]
	parsed, err := uuid.Parse(candidate)
	if err != nil || parsed.String() != strings.ToLower(candidate) {
		return ""
	}
	return parsed.String()
}

func nativeConversationFilename(name string) bool {
	name = strings.TrimSpace(name)
	return name == "" || strings.EqualFold(filepath.Ext(name), ".jsonl")
}

func normalizeConversationTitle(title string) string {
	title = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return ' '
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, title)
	title = strings.Join(strings.Fields(title), " ")
	if title == "" || !utf8.ValidString(title) {
		return ""
	}
	runes := []rune(title)
	if len(runes) > conversationTitleMaxRunes {
		title = string(runes[:conversationTitleMaxRunes])
	}
	return title
}
