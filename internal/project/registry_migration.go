package project

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/aplexica/aplexica/internal/privatefs"
)

const (
	RegistryV3MigrationPlanSchema = "aplexica.registry-v3-migration-plan/v1"
	RegistryV3CollisionSchema     = "aplexica.registry-v3-collision-report/v1"
	registryMigrationMaximumBytes = 16 << 20
)

type registryV2State struct {
	Version  string            `json:"version"`
	Projects []registryV2Entry `json:"projects"`
}

type registryV2Entry struct {
	ID          string   `json:"id"`
	Path        string   `json:"path"`
	VCS         string   `json:"vcs"`
	Ephemeral   bool     `json:"ephemeral,omitempty"`
	DisplayName string   `json:"displayName,omitempty"`
	Scope       string   `json:"scope,omitempty"`
	Agents      []string `json:"agents,omitempty"`
}

type RegistryV3PathObservation struct {
	ProjectID     string        `json:"project_id"`
	Status        string        `json:"status"`
	CanonicalPath string        `json:"canonical_path"`
	FileIdentity  *FileIdentity `json:"file_identity,omitempty"`
}

type RegistryV3Collision struct {
	CanonicalPath string   `json:"canonical_path"`
	ProjectIDs    []string `json:"project_ids"`
	RetainedID    string   `json:"retained_id"`
	RemovedIDs    []string `json:"removed_ids"`
}

type RegistryV3MigrationPlan struct {
	Schema           string                      `json:"schema"`
	InputSHA256      string                      `json:"input_sha256"`
	PlannedAt        time.Time                   `json:"planned_at"`
	RetainedIDs      []string                    `json:"retained_ids"`
	RemovedIDs       []string                    `json:"removed_ids"`
	PathObservations []RegistryV3PathObservation `json:"path_observations"`
	Collisions       []RegistryV3Collision       `json:"collisions"`
	Output           registryState               `json:"output_registry"`
}

type RegistryV3CollisionReport struct {
	Schema      string                `json:"schema"`
	InputSHA256 string                `json:"input_sha256"`
	PlanSHA256  string                `json:"plan_sha256"`
	Collisions  []RegistryV3Collision `json:"collisions"`
}

type RegistryV3PlanOptions struct {
	StateDir            string
	ExpectedInputSHA256 string
	RetainIDs           []string
	RemoveIDs           []string
	PlannedAt           time.Time
}

type RegistryV3PlanResult struct {
	PlanPath       string
	PlanSHA256     string
	InputSHA256    string
	ProjectCount   int
	ActiveCount    int
	InactiveCount  int
	CollisionCount int
	RemovedCount   int
}

type RegistryV3ApplyOptions struct {
	StateDir           string
	ApprovedPlanSHA256 string
}

type RegistryV3ApplyResult struct {
	PlanPath            string
	BackupPath          string
	CollisionReportPath string
	RegistryPath        string
	RegistrySHA256      string
	ProjectCount        int
	ActiveCount         int
	InactiveCount       int
	CollisionCount      int
	TombstoneCount      int
}

func registryV3PlanFilename(digest string) string {
	return "projects-v3-migration-plan-" + digest + ".json"
}

func registryV2BackupFilename(digest string) string {
	return "projects-v2-" + digest + ".backup.json"
}

func registryV3CollisionFilename(digest string) string {
	return "projects-v3-collisions-" + digest + ".json"
}

func CreateRegistryV3MigrationPlan(options RegistryV3PlanOptions) (RegistryV3PlanResult, error) {
	var result RegistryV3PlanResult
	if !validSHA256(options.ExpectedInputSHA256) {
		return result, fmt.Errorf("project: expected input SHA-256 must be 64 lowercase hexadecimal characters")
	}
	root, canonicalStateDir, err := openRegistryMigrationRoot(options.StateDir)
	if err != nil {
		return result, err
	}
	defer root.Close()
	lock, err := acquireRegistryLock(filepath.Join(canonicalStateDir, "projects.json.lock"), 5*time.Second)
	if err != nil {
		return result, err
	}
	defer lock.release()

	source, err := readMigrationRootFile(root, "projects.json")
	if err != nil {
		return result, err
	}
	inputDigest := sha256Hex(source)
	if inputDigest != options.ExpectedInputSHA256 {
		return result, fmt.Errorf("project: pre-v3 registry SHA-256 does not match the independently supplied pin")
	}
	plannedAt := options.PlannedAt.UTC().Round(0)
	if plannedAt.IsZero() {
		plannedAt = time.Now().UTC().Round(0)
	}
	plan, err := buildRegistryV3Plan(source, inputDigest, options.RetainIDs, options.RemoveIDs, plannedAt)
	if err != nil {
		return result, err
	}
	planRaw, err := marshalCanonicalMigrationPlan(plan)
	if err != nil {
		return result, err
	}
	planDigest := sha256Hex(planRaw)
	planName := registryV3PlanFilename(planDigest)
	if err := writeNoClobberOrVerify(root, planName, planRaw); err != nil {
		return result, fmt.Errorf("project: persist migration plan: %w", err)
	}
	active, inactive := countMigrationProjects(plan.Output.Projects)
	return RegistryV3PlanResult{
		PlanPath: filepath.Join(canonicalStateDir, planName), PlanSHA256: planDigest,
		InputSHA256: inputDigest, ProjectCount: len(plan.Output.Projects), ActiveCount: active,
		InactiveCount: inactive, CollisionCount: len(plan.Collisions), RemovedCount: len(plan.RemovedIDs),
	}, nil
}

func ApplyRegistryV3Migration(options RegistryV3ApplyOptions) (RegistryV3ApplyResult, error) {
	var result RegistryV3ApplyResult
	if !validSHA256(options.ApprovedPlanSHA256) {
		return result, fmt.Errorf("project: approved plan SHA-256 must be 64 lowercase hexadecimal characters")
	}
	root, canonicalStateDir, err := openRegistryMigrationRoot(options.StateDir)
	if err != nil {
		return result, err
	}
	defer root.Close()
	lock, err := acquireRegistryLock(filepath.Join(canonicalStateDir, "projects.json.lock"), 5*time.Second)
	if err != nil {
		return result, err
	}
	defer lock.release()

	planName := registryV3PlanFilename(options.ApprovedPlanSHA256)
	planRaw, err := readMigrationRootFile(root, planName)
	if err != nil {
		return result, fmt.Errorf("project: read independently approved migration plan: %w", err)
	}
	if sha256Hex(planRaw) != options.ApprovedPlanSHA256 {
		return result, fmt.Errorf("project: approved migration plan digest mismatch")
	}
	var plan RegistryV3MigrationPlan
	if err := decodeStrictJSON(planRaw, &plan); err != nil {
		return result, fmt.Errorf("project: invalid migration plan: %w", err)
	}
	canonicalPlan, err := marshalCanonicalMigrationPlan(plan)
	if err != nil || !bytes.Equal(planRaw, canonicalPlan) {
		return result, fmt.Errorf("project: migration plan is not in its exact canonical encoding")
	}
	if err := validateMigrationPlanEnvelope(plan); err != nil {
		return result, err
	}

	source, err := readMigrationRootFile(root, "projects.json")
	if err != nil {
		return result, err
	}
	inputDigest := sha256Hex(source)
	if inputDigest != plan.InputSHA256 {
		return result, fmt.Errorf("project: pre-v3 registry changed after plan approval")
	}
	recomputed, err := buildRegistryV3Plan(source, inputDigest, plan.RetainedIDs, plan.RemovedIDs, plan.PlannedAt)
	if err != nil {
		return result, fmt.Errorf("project: revalidate approved migration plan: %w", err)
	}
	recomputedRaw, err := marshalCanonicalMigrationPlan(recomputed)
	if err != nil || !bytes.Equal(planRaw, recomputedRaw) {
		return result, fmt.Errorf("project: filesystem or migration semantics changed after plan approval")
	}

	registryRaw, err := marshalRegistry(plan.Output)
	if err != nil {
		return result, err
	}
	backupName := registryV2BackupFilename(inputDigest)
	if err := writeNoClobberOrVerify(root, backupName, source); err != nil {
		return result, fmt.Errorf("project: persist exact pre-v3 backup: %w", err)
	}
	report := RegistryV3CollisionReport{Schema: RegistryV3CollisionSchema, InputSHA256: inputDigest,
		PlanSHA256: options.ApprovedPlanSHA256, Collisions: append([]RegistryV3Collision{}, plan.Collisions...)}
	reportRaw, err := json.Marshal(report)
	if err != nil {
		return result, err
	}
	reportName := registryV3CollisionFilename(options.ApprovedPlanSHA256)
	if err := writeNoClobberOrVerify(root, reportName, reportRaw); err != nil {
		return result, fmt.Errorf("project: persist collision report: %w", err)
	}

	// Re-read and re-resolve after every durable evidence output is installed.
	// A source edit or project-directory replacement cannot be hidden behind an
	// already-approved plan.
	latestSource, err := readMigrationRootFile(root, "projects.json")
	if err != nil || !bytes.Equal(source, latestSource) {
		return result, fmt.Errorf("project: pre-v3 registry changed before atomic commit")
	}
	latestPlan, err := buildRegistryV3Plan(latestSource, inputDigest, plan.RetainedIDs, plan.RemovedIDs, plan.PlannedAt)
	latestPlanRaw, latestMarshalErr := marshalCanonicalMigrationPlan(latestPlan)
	if err != nil || latestMarshalErr != nil || !bytes.Equal(planRaw, latestPlanRaw) {
		return result, fmt.Errorf("project: physical project identity changed before atomic commit")
	}

	temporary, tempName, err := root.CreateTemp(".", ".projects-v3-stage-")
	if err != nil {
		return result, fmt.Errorf("project: create migration stage: %w", err)
	}
	cleanupStage := true
	defer func() {
		if cleanupStage {
			_ = temporary.Close()
			_ = root.RemoveRegular(tempName)
		}
	}()
	if err := writeAndSync(temporary, registryRaw); err != nil {
		return result, fmt.Errorf("project: stage Registry v3: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return result, fmt.Errorf("project: close Registry v3 stage: %w", err)
	}
	finalSource, err := readMigrationRootFile(root, "projects.json")
	if err != nil || !bytes.Equal(source, finalSource) {
		return result, fmt.Errorf("project: pre-v3 registry changed after staging")
	}
	finalPlan, err := buildRegistryV3Plan(finalSource, inputDigest, plan.RetainedIDs, plan.RemovedIDs, plan.PlannedAt)
	finalPlanRaw, finalMarshalErr := marshalCanonicalMigrationPlan(finalPlan)
	if err != nil || finalMarshalErr != nil || !bytes.Equal(planRaw, finalPlanRaw) {
		return result, fmt.Errorf("project: physical project identity changed after staging")
	}
	if err := root.Rename(tempName, "projects.json"); err != nil {
		return result, fmt.Errorf("project: atomically install Registry v3: %w", err)
	}
	cleanupStage = false
	installed, err := readMigrationRootFile(root, "projects.json")
	if err != nil || !bytes.Equal(installed, registryRaw) {
		return result, fmt.Errorf("project: installed Registry v3 failed exact post-commit verification")
	}
	verified, _, verifyErr := decodeRegistry(installed)
	if verifyErr != nil || verified.Version != registryVersion || verified.Revision != 1 {
		return result, fmt.Errorf("project: installed Registry v3 failed semantic post-commit verification")
	}
	active, inactive := countMigrationProjects(plan.Output.Projects)
	return RegistryV3ApplyResult{
		PlanPath: filepath.Join(canonicalStateDir, planName), BackupPath: filepath.Join(canonicalStateDir, backupName),
		CollisionReportPath: filepath.Join(canonicalStateDir, reportName), RegistryPath: filepath.Join(canonicalStateDir, "projects.json"),
		RegistrySHA256: sha256Hex(registryRaw), ProjectCount: len(plan.Output.Projects), ActiveCount: active,
		InactiveCount: inactive, CollisionCount: len(plan.Collisions), TombstoneCount: len(plan.Output.Tombstones),
	}, nil
}

func buildRegistryV3Plan(source []byte, inputDigest string, retainIDs, removeIDs []string, plannedAt time.Time) (RegistryV3MigrationPlan, error) {
	var legacy registryV2State
	if err := decodeStrictJSON(source, &legacy); err != nil {
		return RegistryV3MigrationPlan{}, fmt.Errorf("project: invalid pre-v3 registry: %w", err)
	}
	if legacy.Version != "2" || legacy.Projects == nil {
		return RegistryV3MigrationPlan{}, fmt.Errorf("project: migration requires an explicit version 2 registry with a projects array")
	}
	canonicalLegacy, err := json.MarshalIndent(legacy, "", "  ")
	if err != nil || !bytes.Equal(source, canonicalLegacy) {
		return RegistryV3MigrationPlan{}, fmt.Errorf("project: pre-v3 registry is not in its exact canonical persisted encoding")
	}
	retained, err := strictDecisionSet(retainIDs, "retained")
	if err != nil {
		return RegistryV3MigrationPlan{}, err
	}
	removed, err := strictDecisionSet(removeIDs, "removed")
	if err != nil {
		return RegistryV3MigrationPlan{}, err
	}
	for id := range retained {
		if _, overlap := removed[id]; overlap {
			return RegistryV3MigrationPlan{}, fmt.Errorf("project: one collision ID cannot be both retained and removed")
		}
	}

	entries := make([]Entry, 0, len(legacy.Projects))
	observations := make([]RegistryV3PathObservation, 0, len(legacy.Projects))
	seenIDs := make(map[string]struct{}, len(legacy.Projects))
	canonicalPathByID := make(map[string]string, len(legacy.Projects))
	for _, old := range legacy.Projects {
		if err := validateLegacyProject(old); err != nil {
			return RegistryV3MigrationPlan{}, err
		}
		if _, duplicate := seenIDs[old.ID]; duplicate {
			return RegistryV3MigrationPlan{}, fmt.Errorf("project: duplicate project ID in pre-v3 registry")
		}
		seenIDs[old.ID] = struct{}{}
		canonical, inactive, identity, err := resolveMigrationProjectPath(old.Path)
		if err != nil {
			return RegistryV3MigrationPlan{}, fmt.Errorf("project: resolve pre-v3 project path: %w", err)
		}
		scope := old.Scope
		if scope == "" {
			scope = "local"
		}
		agents := append([]string(nil), old.Agents...)
		sort.Strings(agents)
		entry := Entry{ID: old.ID, Path: canonical, VCS: old.VCS, Ephemeral: old.Ephemeral, Inactive: inactive,
			DisplayName: old.DisplayName, Scope: scope, Agents: agents, AuthorizationGeneration: 1}
		observation := RegistryV3PathObservation{ProjectID: old.ID, CanonicalPath: canonical}
		if inactive {
			observation.Status = "inactive_missing"
		} else {
			identityCopy := identity
			entry.FileIdentity = &identityCopy
			observation.Status = "active"
			observation.FileIdentity = &identityCopy
		}
		entries = append(entries, entry)
		observations = append(observations, observation)
		canonicalPathByID[old.ID] = canonical
	}

	collisions := make([]RegistryV3Collision, 0)
	usedRetained, usedRemoved := make(map[string]struct{}), make(map[string]struct{})
	for _, ids := range registryV3CollisionGroups(entries) {
		retainedID := ""
		removedForCollision := make([]string, 0, len(ids)-1)
		for _, id := range ids {
			if _, keep := retained[id]; keep {
				if retainedID != "" {
					return RegistryV3MigrationPlan{}, fmt.Errorf("project: each collision requires exactly one retained ID")
				}
				retainedID = id
				usedRetained[id] = struct{}{}
			} else if _, drop := removed[id]; drop {
				removedForCollision = append(removedForCollision, id)
				usedRemoved[id] = struct{}{}
			} else {
				return RegistryV3MigrationPlan{}, fmt.Errorf("project: collision resolution is partial; every colliding ID requires an explicit retain/remove decision")
			}
		}
		if retainedID == "" || len(removedForCollision) != len(ids)-1 {
			return RegistryV3MigrationPlan{}, fmt.Errorf("project: each collision requires one retained ID and all other IDs removed")
		}
		collisions = append(collisions, RegistryV3Collision{CanonicalPath: canonicalPathByID[retainedID], ProjectIDs: append([]string(nil), ids...),
			RetainedID: retainedID, RemovedIDs: append([]string(nil), removedForCollision...)})
	}
	if len(usedRetained) != len(retained) || len(usedRemoved) != len(removed) {
		return RegistryV3MigrationPlan{}, fmt.Errorf("project: retain/remove decisions may name only IDs in a physical path collision")
	}
	sort.Slice(collisions, func(i, j int) bool { return collisions[i].CanonicalPath < collisions[j].CanonicalPath })

	outputProjects := make([]Entry, 0, len(entries)-len(removed))
	for _, entry := range entries {
		if _, drop := removed[entry.ID]; drop {
			continue
		}
		outputProjects = append(outputProjects, entry)
	}
	sort.Slice(outputProjects, func(i, j int) bool { return outputProjects[i].ID < outputProjects[j].ID })
	tombstones := make([]Tombstone, 0, len(removed))
	for id := range removed {
		tombstones = append(tombstones, Tombstone{ID: id, AuthorizationGeneration: 2, RemovedAt: plannedAt})
	}
	sort.Slice(tombstones, func(i, j int) bool { return tombstones[i].ID < tombstones[j].ID })
	sort.Slice(observations, func(i, j int) bool { return observations[i].ProjectID < observations[j].ProjectID })
	retainedList, removedList := sortedSetKeys(retained), sortedSetKeys(removed)
	return RegistryV3MigrationPlan{
		Schema: RegistryV3MigrationPlanSchema, InputSHA256: inputDigest, PlannedAt: plannedAt,
		RetainedIDs: retainedList, RemovedIDs: removedList, PathObservations: observations, Collisions: collisions,
		Output: registryState{Version: registryVersion, Revision: 1, Projects: outputProjects, Tombstones: tombstones},
	}, nil
}

// registryV3CollisionGroups computes the transitive closure of every
// location collision enforced by Registry v3: the platform path-comparison
// key and, for active entries, the opened physical directory identity. An
// active entry has both keys, so a single-key grouping can miss overlaps (for
// example two distinct directories whose paths differ only by case on a
// case-sensitive macOS volume). Unioning both keys makes the reviewed
// migration decisions exactly match the runtime registry invariant.
func registryV3CollisionGroups(entries []Entry) [][]string {
	parents := make([]int, len(entries))
	for index := range parents {
		parents[index] = index
	}
	var find func(int) int
	find = func(index int) int {
		if parents[index] != index {
			parents[index] = find(parents[index])
		}
		return parents[index]
	}
	union := func(left, right int) {
		leftRoot, rightRoot := find(left), find(right)
		if leftRoot != rightRoot {
			parents[rightRoot] = leftRoot
		}
	}

	pathOwners := make(map[string]int, len(entries))
	identityOwners := make(map[FileIdentity]int, len(entries))
	for index, entry := range entries {
		pathKey := registryPathKey(entry.Path)
		if prior, exists := pathOwners[pathKey]; exists {
			union(index, prior)
		} else {
			pathOwners[pathKey] = index
		}
		if entry.Inactive || entry.FileIdentity == nil {
			continue
		}
		identity := *entry.FileIdentity
		if prior, exists := identityOwners[identity]; exists {
			union(index, prior)
		} else {
			identityOwners[identity] = index
		}
	}

	byRoot := make(map[int][]string)
	for index, entry := range entries {
		root := find(index)
		byRoot[root] = append(byRoot[root], entry.ID)
	}
	groups := make([][]string, 0, len(byRoot))
	for _, ids := range byRoot {
		if len(ids) < 2 {
			continue
		}
		sort.Strings(ids)
		groups = append(groups, ids)
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i][0] < groups[j][0] })
	return groups
}

func validateMigrationPlanEnvelope(plan RegistryV3MigrationPlan) error {
	_, offset := plan.PlannedAt.Zone()
	if plan.Schema != RegistryV3MigrationPlanSchema || !validSHA256(plan.InputSHA256) || plan.PlannedAt.IsZero() || offset != 0 || plan.Output.Version != registryVersion || plan.Output.Revision != 1 {
		return fmt.Errorf("project: migration plan envelope is invalid")
	}
	return nil
}

func marshalCanonicalMigrationPlan(plan RegistryV3MigrationPlan) ([]byte, error) {
	if err := validateMigrationPlanEnvelope(plan); err != nil {
		return nil, err
	}
	return json.MarshalIndent(plan, "", "  ")
}

func validateLegacyProject(entry registryV2Entry) error {
	if !validRegistryProjectID(entry.ID) {
		return fmt.Errorf("project: pre-v3 registry contains an unsafe project ID")
	}
	if entry.VCS != "git" && entry.VCS != "hg" && entry.VCS != "none" {
		return fmt.Errorf("project: pre-v3 registry contains an unsupported VCS")
	}
	if entry.Scope != "" && entry.Scope != "local" && entry.Scope != "global" {
		return fmt.Errorf("project: pre-v3 registry contains an unsupported scope")
	}
	if hasControl(entry.DisplayName) {
		return fmt.Errorf("project: pre-v3 registry contains an unsafe display name")
	}
	if !sort.StringsAreSorted(entry.Agents) {
		return fmt.Errorf("project: pre-v3 registry contains noncanonical agent ordering")
	}
	seenAgent := make(map[string]struct{}, len(entry.Agents))
	for _, agent := range entry.Agents {
		if agent == "" || hasControl(agent) {
			return fmt.Errorf("project: pre-v3 registry contains an unsafe agent name")
		}
		if _, duplicate := seenAgent[agent]; duplicate {
			return fmt.Errorf("project: pre-v3 registry contains duplicate agent names")
		}
		seenAgent[agent] = struct{}{}
	}
	return nil
}

func resolveMigrationProjectPath(path string) (string, bool, FileIdentity, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Clean(path) == filepath.VolumeName(path)+string(filepath.Separator) {
		return "", false, FileIdentity{}, fmt.Errorf("project: path must be a clean absolute non-root path")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		resolved, err = filepath.Abs(resolved)
		if err != nil || !filepath.IsAbs(resolved) || filepath.Clean(resolved) != resolved || filepath.Clean(resolved) == filepath.VolumeName(resolved)+string(filepath.Separator) {
			return "", false, FileIdentity{}, fmt.Errorf("project: resolved path is unsafe")
		}
		info, statErr := os.Stat(resolved)
		if statErr != nil || !info.IsDir() {
			return "", false, FileIdentity{}, fmt.Errorf("project: resolved path is not a directory")
		}
		identity, identityErr := measureProjectIdentity(resolved, info)
		if identityErr != nil {
			return "", false, FileIdentity{}, identityErr
		}
		return resolved, false, identity, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", false, FileIdentity{}, fmt.Errorf("project: resolve physical path: %w", err)
	}

	current := path
	missing := make([]string, 0, 4)
	for {
		if info, lstatErr := os.Lstat(current); lstatErr == nil && info.Mode()&os.ModeSymlink != 0 {
			if _, evalErr := filepath.EvalSymlinks(current); evalErr != nil {
				return "", false, FileIdentity{}, fmt.Errorf("project: broken path alias rejected")
			}
		}
		if physical, evalErr := filepath.EvalSymlinks(current); evalErr == nil {
			info, statErr := os.Stat(physical)
			if statErr != nil || !info.IsDir() {
				return "", false, FileIdentity{}, fmt.Errorf("project: existing path ancestor is not a directory")
			}
			for index := len(missing) - 1; index >= 0; index-- {
				physical = filepath.Join(physical, missing[index])
			}
			physical = filepath.Clean(physical)
			if !filepath.IsAbs(physical) || physical == filepath.VolumeName(physical)+string(filepath.Separator) {
				return "", false, FileIdentity{}, fmt.Errorf("project: inactive path is unsafe")
			}
			return physical, true, FileIdentity{}, nil
		} else if !errors.Is(evalErr, os.ErrNotExist) {
			return "", false, FileIdentity{}, fmt.Errorf("project: resolve path ancestor: %w", evalErr)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", false, FileIdentity{}, fmt.Errorf("project: no resolvable path ancestor")
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func currentEntryIdentity(entry Entry) bool {
	if entry.Inactive || entry.FileIdentity == nil || entry.FileIdentity.validate() != nil {
		return false
	}
	physical, inactive, identity, err := resolveMigrationProjectPath(entry.Path)
	return err == nil && !inactive && physical == entry.Path && reflect.DeepEqual(identity, *entry.FileIdentity)
}

func strictDecisionSet(ids []string, label string) (map[string]struct{}, error) {
	result := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if !validRegistryProjectID(id) {
			return nil, fmt.Errorf("project: %s collision ID is unsafe", label)
		}
		if _, duplicate := result[id]; duplicate {
			return nil, fmt.Errorf("project: duplicate %s collision ID", label)
		}
		result[id] = struct{}{}
	}
	return result, nil
}

func sortedSetKeys(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func hasControl(value string) bool {
	return strings.IndexFunc(value, unicode.IsControl) >= 0
}

func validRegistryProjectID(value string) bool {
	lower := strings.ToLower(value)
	return value != "" && len(value) <= 4096 && !hasControl(value) && !strings.ContainsAny(value, "@?#") &&
		!strings.Contains(lower, "://") && !strings.Contains(lower, "%40")
}

func openRegistryMigrationRoot(stateDir string) (*privatefs.Root, string, error) {
	if stateDir == "" || !filepath.IsAbs(stateDir) || filepath.Clean(stateDir) != stateDir {
		return nil, "", fmt.Errorf("project: migration state directory must be clean and absolute")
	}
	physical, err := filepath.EvalSymlinks(stateDir)
	if err != nil || physical != stateDir {
		return nil, "", fmt.Errorf("project: migration state directory aliases are rejected")
	}
	root, err := privatefs.OpenRoot(stateDir, privatefs.DirPolicy{Access: privatefs.AccessPrivate})
	if err != nil {
		return nil, "", fmt.Errorf("project: open private migration state: %w", err)
	}
	return root, stateDir, nil
}

func readMigrationRootFile(root *privatefs.Root, name string) ([]byte, error) {
	file, err := root.OpenReadRegular(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var buffer bytes.Buffer
	read, err := io.CopyN(&buffer, file, registryMigrationMaximumBytes+1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if read > registryMigrationMaximumBytes {
		return nil, fmt.Errorf("project: migration input exceeds size limit")
	}
	return buffer.Bytes(), nil
}

func writeNoClobberOrVerify(root *privatefs.Root, name string, data []byte) error {
	if existing, err := readMigrationRootFile(root, name); err == nil {
		if !bytes.Equal(existing, data) {
			return fmt.Errorf("immutable migration output already exists with different bytes")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := root.CreateExclusive(name, privatefs.FilePolicy{RequirePrivateParent: true, RejectWritableByOthers: true})
	if err != nil {
		return err
	}
	cleanup := true
	defer func() {
		_ = file.Close()
		if cleanup {
			_ = root.RemoveRegular(name)
		}
	}()
	if err := writeAndSync(file, data); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := root.SyncDir("."); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func writeAndSync(file *os.File, data []byte) error {
	for len(data) > 0 {
		written, err := file.Write(data)
		if err != nil {
			return err
		}
		if written <= 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return file.Sync()
}

func validSHA256(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && value == hex.EncodeToString(decoded)
}

func sha256Hex(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func countMigrationProjects(projects []Entry) (active, inactive int) {
	for _, project := range projects {
		if project.Inactive {
			inactive++
		} else {
			active++
		}
	}
	return active, inactive
}
