package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aplexica/aplexica/internal/atomicfile"
	"github.com/google/uuid"
)

const claudeDesktopRegistrationTimeout = 5 * time.Second

type desktopSessionUpsert struct {
	SessionID string
	Title     string
	CWD       string
	Activity  time.Time
}

// bestEffortUpsertDesktopSession makes a durable synthetic CLI transcript
// visible in Claude Code Desktop without launching or activating the app.
// CLI-only installations and catalog write failures remain successful because
// the shared ~/.claude transcript is already the source of truth.
func (a *Adapter) bestEffortUpsertDesktopSession(sessionID, title, cwd string, activity time.Time) {
	if sessionID == "" || a.HomeDir == "" || a.upsertDesktopSession == nil || !a.claudeDesktopSurfaceInstalled() {
		return
	}
	request := desktopSessionUpsert{
		SessionID: sessionID,
		Title:     strings.Join(strings.Fields(title), " "),
		CWD:       cwd,
		Activity:  activity,
	}
	ctx, cancel := context.WithTimeout(context.Background(), claudeDesktopRegistrationTimeout)
	defer cancel()

	// Catalog updates are serialized with each other so two simultaneous fanout
	// operations cannot replace the same deterministic record from stale maps.
	a.desktopRegistrationMu.Lock()
	defer a.desktopRegistrationMu.Unlock()
	_ = a.upsertDesktopSession(ctx, request)
}

// upsertClaudeDesktopSession creates or updates one deterministic catalog
// record beside Desktop's newest existing account/workspace record. Aplexica
// never invents Claude's private account hierarchy and never invokes a URL or
// process launcher. Existing unknown fields and explicit user titles survive.
func (a *Adapter) upsertClaudeDesktopSession(ctx context.Context, request desktopSessionUpsert) error {
	parsed, err := uuid.Parse(request.SessionID)
	if err != nil || parsed.String() != request.SessionID {
		return fmt.Errorf("claudecode: invalid Desktop session ID")
	}
	if request.CWD == "" {
		request.CWD = a.HomeDir
	}
	request.CWD, err = filepath.Abs(request.CWD)
	if err != nil {
		return fmt.Errorf("claudecode: resolve Desktop session cwd: %w", err)
	}
	if request.Activity.IsZero() {
		request.Activity = time.Now().UTC()
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	target, template, exists, err := a.desktopSessionUpsertTarget(request.SessionID)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !safeClaudeDesktopCatalogTarget(a.desktopSessionCatalogRoots(), target, exists) {
		return fmt.Errorf("claudecode: unsafe Desktop session catalog target")
	}

	fields := make(map[string]json.RawMessage)
	mode := os.FileMode(0o600)
	if exists {
		fields, mode, err = readClaudeDesktopSessionFields(target, request.SessionID)
		if err != nil {
			return err
		}
	} else {
		// Copy only portable account preferences that Desktop places on every
		// local session. Do not copy focus, worktree, model, or session identity.
		templateFields, _, readErr := readClaudeDesktopSessionFields(template, "")
		if readErr != nil {
			return readErr
		}
		for _, key := range []string{
			"permissionMode",
			"chromePermissionMode",
			"alwaysAllowedReasons",
			"sessionPermissionUpdates",
			"enabledMcpTools",
			"remoteMcpServersConfig",
		} {
			if value, ok := templateFields[key]; ok {
				fields[key] = append(json.RawMessage(nil), value...)
			}
		}
	}

	activityMillis := request.Activity.UTC().UnixMilli()
	createdMillis := rawJSONInt64(fields["createdAt"])
	if createdMillis <= 0 {
		createdMillis = activityMillis
	}
	lastActivityMillis := rawJSONInt64(fields["lastActivityAt"])
	if activityMillis > lastActivityMillis {
		lastActivityMillis = activityMillis
	}
	setClaudeDesktopJSONField(fields, "sessionId", "local_"+request.SessionID)
	setClaudeDesktopJSONField(fields, "cliSessionId", request.SessionID)
	setClaudeDesktopJSONField(fields, "cwd", request.CWD)
	setClaudeDesktopJSONField(fields, "originCwd", request.CWD)
	setClaudeDesktopJSONField(fields, "createdAt", createdMillis)
	setClaudeDesktopJSONField(fields, "lastActivityAt", lastActivityMillis)
	if !exists {
		setClaudeDesktopJSONField(fields, "lastFocusedAt", int64(0))
		setClaudeDesktopJSONField(fields, "isArchived", false)
	}

	var existingTitle, titleSource string
	_ = json.Unmarshal(fields["title"], &existingTitle)
	_ = json.Unmarshal(fields["titleSource"], &titleSource)
	if titleSource != "user" || existingTitle == "" {
		setClaudeDesktopJSONField(fields, "title", request.Title)
		setClaudeDesktopJSONField(fields, "titleSource", "auto")
	}

	updated, err := json.Marshal(fields)
	if err != nil {
		return fmt.Errorf("claudecode: encode Desktop session catalog record: %w", err)
	}
	if err := atomicfile.WriteFile(target, updated, mode.Perm()); err != nil {
		return fmt.Errorf("claudecode: write Desktop session catalog record: %w", err)
	}
	return nil
}

// desktopSessionUpsertTarget returns the newest existing matching record, or a
// deterministic sibling path in the newest valid account/workspace leaf.
func (a *Adapter) desktopSessionUpsertTarget(sessionID string) (target, template string, exists bool, err error) {
	candidates, _ := scanDesktopSessionCandidates(a.desktopSessionCatalogRoots(), desktopSessionFileMaxCount)
	var bestMatch, bestTemplate desktopSessionCandidate
	var bestMatchRecord, bestTemplateRecord desktopSessionRecord
	for _, candidate := range candidates {
		if !candidate.regularFile {
			continue
		}
		record, ok := readDesktopSessionRecord(candidate.path)
		if !ok {
			continue
		}
		if bestTemplate.path == "" || desktopCandidateNewer(candidate, record, bestTemplate, bestTemplateRecord) {
			bestTemplate = candidate
			bestTemplateRecord = record
		}
		if record.CLISessionID == sessionID || record.SessionID == "local_"+sessionID {
			if bestMatch.path == "" || desktopCandidateNewer(candidate, record, bestMatch, bestMatchRecord) {
				bestMatch = candidate
				bestMatchRecord = record
			}
		}
	}
	if bestMatch.path != "" {
		return bestMatch.path, bestMatch.path, true, nil
	}
	if bestTemplate.path == "" {
		return "", "", false, fmt.Errorf("claudecode: Desktop catalog has no account/workspace record")
	}
	target = filepath.Join(filepath.Dir(bestTemplate.path), "local_"+sessionID+".json")
	if info, statErr := os.Lstat(target); statErr == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return "", "", false, fmt.Errorf("claudecode: unsafe existing Desktop session target")
		}
		return target, bestTemplate.path, true, nil
	} else if !os.IsNotExist(statErr) {
		return "", "", false, fmt.Errorf("claudecode: inspect Desktop session target: %w", statErr)
	}
	return target, bestTemplate.path, false, nil
}

func desktopCandidateNewer(candidate desktopSessionCandidate, record desktopSessionRecord, best desktopSessionCandidate, bestRecord desktopSessionRecord) bool {
	candidateActive := record.lastActive()
	bestActive := bestRecord.lastActive()
	if !candidateActive.Equal(bestActive) {
		return candidateActive.After(bestActive)
	}
	return candidate.modTime > best.modTime
}

func readClaudeDesktopSessionFields(path, expectedCLISessionID string) (map[string]json.RawMessage, os.FileMode, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > desktopSessionFileMaxBytes {
		return nil, 0, fmt.Errorf("claudecode: unsafe Desktop session catalog record")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, fmt.Errorf("claudecode: read Desktop session catalog record: %w", err)
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return nil, 0, fmt.Errorf("claudecode: invalid Desktop session catalog record")
	}
	var identity struct {
		SessionID    string `json:"sessionId"`
		CLISessionID string `json:"cliSessionId"`
	}
	if json.Unmarshal(raw, &identity) != nil || identity.SessionID == "" {
		return nil, 0, fmt.Errorf("claudecode: invalid Desktop session catalog identity")
	}
	if expectedCLISessionID != "" && identity.CLISessionID != expectedCLISessionID && identity.SessionID != "local_"+expectedCLISessionID {
		return nil, 0, fmt.Errorf("claudecode: Desktop session catalog identity changed")
	}
	return fields, info.Mode().Perm(), nil
}

func setClaudeDesktopJSONField(fields map[string]json.RawMessage, key string, value any) {
	encoded, _ := json.Marshal(value)
	fields[key] = encoded
}

func rawJSONInt64(raw json.RawMessage) int64 {
	var value int64
	_ = json.Unmarshal(raw, &value)
	return value
}

func safeClaudeDesktopCatalogTarget(roots []string, target string, exists bool) bool {
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return false
	}
	for _, root := range roots {
		absRoot, rootErr := filepath.Abs(root)
		if rootErr != nil {
			continue
		}
		rel, relErr := filepath.Rel(absRoot, absTarget)
		if relErr != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		rootInfo, statErr := os.Lstat(absRoot)
		if statErr != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
			continue
		}
		current := absRoot
		safe := true
		for _, part := range strings.Split(filepath.Dir(rel), string(filepath.Separator)) {
			if part == "" || part == "." {
				continue
			}
			current = filepath.Join(current, part)
			info, infoErr := os.Lstat(current)
			if infoErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				safe = false
				break
			}
		}
		if !safe {
			continue
		}
		if exists {
			info, infoErr := os.Lstat(absTarget)
			if infoErr != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
				continue
			}
		}
		return true
	}
	return false
}
