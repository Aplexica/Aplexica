package privatefs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
)

type JournalPhase string

const (
	JournalPrepared       JournalPhase = "prepared"
	JournalApplying       JournalPhase = "applying"
	JournalStateCommitted JournalPhase = "state-committed"
	JournalAuditCommitted JournalPhase = "audit-committed"
	journalVersion                     = uint16(1)
	journalDomain                      = "aplexica/privatefs-journal/v1\x00"
	maxJournalEntries                  = 100000
	maxJournalBytes                    = 32 << 20
)

type JournalEntry struct {
	RootID                 string   `json:"rootId"`
	ObjectKind             string   `json:"objectKind"`
	Operation              string   `json:"operation"`
	FinalRel               string   `json:"finalRel"`
	TempRel                string   `json:"tempRel,omitempty"`
	RollbackRel            string   `json:"rollbackRel,omitempty"`
	FinalExisted           bool     `json:"finalExisted"`
	CreatedByTransaction   bool     `json:"createdByTransaction"`
	ExpectedFinalSHA256    [32]byte `json:"expectedFinalSha256"`
	ExpectedRollbackSHA256 [32]byte `json:"expectedRollbackSha256"`
	Applied                bool     `json:"applied"`
}

type JournalPlan struct {
	Kind               string         `json:"kind"`
	TransactionID      string         `json:"transactionId"`
	AuditTransactionID string         `json:"auditTransactionId,omitempty"`
	NativePaths        bool           `json:"nativePaths,omitempty"`
	Entries            []JournalEntry `json:"entries"`
}

type JournalRecord struct {
	Version        uint16       `json:"version"`
	Plan           JournalPlan  `json:"plan"`
	Phase          JournalPhase `json:"phase"`
	RecordSequence uint64       `json:"recordSequence"`
	Checksum       [32]byte     `json:"checksum"`
}

type AuditCompleter interface {
	CompleteTransaction(context.Context, string, string) error
}

type Journal struct {
	control *Root
	rel     string
	record  JournalRecord
}

var journalUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
var journalToken = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)

func journalChecksum(record JournalRecord) ([32]byte, error) {
	record.Checksum = [32]byte{}
	b, err := json.Marshal(record)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(append([]byte(journalDomain), b...)), nil
}

func validateJournalPlan(plan JournalPlan) error {
	if !journalToken.MatchString(plan.Kind) || !journalUUID.MatchString(plan.TransactionID) {
		return fmt.Errorf("privatefs: invalid journal identity")
	}
	if plan.AuditTransactionID != "" && !journalUUID.MatchString(plan.AuditTransactionID) {
		return fmt.Errorf("privatefs: invalid audit transaction identity")
	}
	if len(plan.Entries) == 0 || len(plan.Entries) > maxJournalEntries {
		return fmt.Errorf("privatefs: invalid journal entry count")
	}
	seen := make(map[string]bool, len(plan.Entries)*3)
	zero := [32]byte{}
	cleanPath := cleanRel
	if plan.NativePaths {
		cleanPath = cleanRelNative
	}
	for _, entry := range plan.Entries {
		if !journalToken.MatchString(entry.RootID) || (entry.ObjectKind != "file" && entry.ObjectKind != "directory") {
			return fmt.Errorf("privatefs: invalid journal entry type")
		}
		if entry.Operation != "install" && entry.Operation != "replace" && entry.Operation != "create-dir" {
			return fmt.Errorf("privatefs: invalid journal operation")
		}
		final, err := cleanPath(entry.FinalRel, false)
		if err != nil || final != entry.FinalRel {
			return fmt.Errorf("privatefs: unsafe journal final")
		}
		for _, rel := range []string{entry.FinalRel, entry.TempRel, entry.RollbackRel} {
			if rel == "" {
				continue
			}
			clean, err := cleanPath(rel, false)
			if err != nil || clean != rel || seen[entry.RootID+"\x00"+rel] {
				return fmt.Errorf("privatefs: unsafe or duplicate journal path")
			}
			seen[entry.RootID+"\x00"+rel] = true
		}
		if entry.ObjectKind == "directory" {
			if entry.Operation != "create-dir" || entry.TempRel != "" || entry.RollbackRel != "" || entry.ExpectedFinalSHA256 != zero || entry.ExpectedRollbackSHA256 != zero {
				return fmt.Errorf("privatefs: invalid directory journal entry")
			}
		} else {
			if entry.Operation == "create-dir" || entry.TempRel == "" || entry.ExpectedFinalSHA256 == zero {
				return fmt.Errorf("privatefs: invalid file journal entry")
			}
			if entry.FinalExisted != (entry.RollbackRel != "") || entry.FinalExisted != (entry.ExpectedRollbackSHA256 != zero) {
				return fmt.Errorf("privatefs: inconsistent rollback journal entry")
			}
		}
	}
	return nil
}

func validateJournalRecord(record JournalRecord) error {
	if record.Version != journalVersion || record.RecordSequence == 0 {
		return fmt.Errorf("privatefs: unsupported journal record")
	}
	switch record.Phase {
	case JournalPrepared, JournalApplying, JournalStateCommitted, JournalAuditCommitted:
	default:
		return fmt.Errorf("privatefs: invalid journal phase")
	}
	if err := validateJournalPlan(record.Plan); err != nil {
		return err
	}
	want, err := journalChecksum(record)
	if err != nil || want != record.Checksum {
		return fmt.Errorf("privatefs: journal checksum mismatch")
	}
	return nil
}

func BeginJournal(control *Root, plan JournalPlan) (*Journal, error) {
	if control == nil {
		return nil, fmt.Errorf("privatefs: journal control root required")
	}
	if err := validateJournalPlan(plan); err != nil {
		return nil, err
	}
	record := JournalRecord{Version: journalVersion, Plan: plan, Phase: JournalPrepared, RecordSequence: 1}
	record.Checksum, _ = journalChecksum(record)
	rel := ".journal-" + plan.Kind + "-" + plan.TransactionID + ".json"
	b, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	f, err := control.CreateExclusive(rel, FilePolicy{RejectWritableByOthers: true})
	if err != nil {
		return nil, err
	}
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = control.SyncDir(".")
	}
	if err != nil {
		_ = control.RemoveRegular(rel)
		return nil, err
	}
	return &Journal{control: control, rel: rel, record: record}, nil
}

func (j *Journal) persist(next JournalRecord) error {
	if j == nil || j.control == nil {
		return fmt.Errorf("privatefs: closed journal")
	}
	next.RecordSequence = j.record.RecordSequence + 1
	next.Checksum, _ = journalChecksum(next)
	b, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if err := j.control.WriteFile(j.rel, b, FilePolicy{RejectWritableByOthers: true}); err != nil {
		return err
	}
	if err := j.control.SyncDir("."); err != nil {
		return err
	}
	j.record = next
	return nil
}

func (j *Journal) MarkApplied(index int) error {
	if j.record.Phase != JournalPrepared && j.record.Phase != JournalApplying {
		return fmt.Errorf("privatefs: invalid journal apply transition")
	}
	if index < 0 || index >= len(j.record.Plan.Entries) || j.record.Plan.Entries[index].Applied {
		return fmt.Errorf("privatefs: invalid journal entry transition")
	}
	next := j.record
	next.Plan.Entries = append([]JournalEntry(nil), next.Plan.Entries...)
	next.Plan.Entries[index].Applied = true
	next.Phase = JournalApplying
	return j.persist(next)
}

func (j *Journal) MarkStateCommitted() error {
	if j.record.Phase != JournalApplying {
		return fmt.Errorf("privatefs: invalid state commit transition")
	}
	for _, entry := range j.record.Plan.Entries {
		if !entry.Applied {
			return fmt.Errorf("privatefs: unapplied journal entry")
		}
	}
	next := j.record
	next.Phase = JournalStateCommitted
	return j.persist(next)
}

func (j *Journal) MarkAuditCommitted() error {
	if j.record.Phase != JournalStateCommitted || j.record.Plan.AuditTransactionID == "" {
		return fmt.Errorf("privatefs: invalid audit commit transition")
	}
	next := j.record
	next.Phase = JournalAuditCommitted
	return j.persist(next)
}

func (j *Journal) CloseAndRemove() error {
	if j == nil || j.control == nil {
		return nil
	}
	if j.record.Plan.AuditTransactionID != "" && j.record.Phase != JournalAuditCommitted {
		return fmt.Errorf("privatefs: journal audit not committed")
	}
	if j.record.Plan.AuditTransactionID == "" && j.record.Phase != JournalStateCommitted && j.record.Phase != JournalAuditCommitted {
		return fmt.Errorf("privatefs: journal state not committed")
	}
	if err := j.control.RemoveRegular(j.rel); err != nil {
		return err
	}
	err := j.control.SyncDir(".")
	j.control = nil
	return err
}

func readJournal(control *Root, rel string) (JournalRecord, error) {
	f, err := control.OpenReadRegular(rel)
	if err != nil {
		return JournalRecord{}, err
	}
	b, err := io.ReadAll(io.LimitReader(f, maxJournalBytes+1))
	closeErr := f.Close()
	if err != nil {
		return JournalRecord{}, err
	}
	if closeErr != nil {
		return JournalRecord{}, closeErr
	}
	if len(b) > maxJournalBytes {
		return JournalRecord{}, fmt.Errorf("privatefs: journal exceeds limit")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var record JournalRecord
	if err := dec.Decode(&record); err != nil {
		return JournalRecord{}, err
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return JournalRecord{}, fmt.Errorf("privatefs: trailing journal data")
	}
	return record, validateJournalRecord(record)
}

func hashRegular(root *Root, rel string) ([32]byte, bool, error) {
	var f *os.File
	var err error
	if root.access == AccessIntegrityOnly {
		f, err = root.OpenReadRegularIntegrity(rel)
	} else {
		f, err = root.OpenReadRegular(rel)
	}
	if errors.Is(err, os.ErrNotExist) {
		return [32]byte{}, false, nil
	}
	if err != nil {
		return [32]byte{}, false, err
	}
	h := sha256.New()
	_, copyErr := io.Copy(h, f)
	closeErr := f.Close()
	if copyErr != nil {
		return [32]byte{}, false, copyErr
	}
	if closeErr != nil {
		return [32]byte{}, false, closeErr
	}
	var digest [32]byte
	copy(digest[:], h.Sum(nil))
	return digest, true, nil
}

func rollbackJournal(record JournalRecord, roots map[string]*Root) error {
	for i := len(record.Plan.Entries) - 1; i >= 0; i-- {
		e := record.Plan.Entries[i]
		root := roots[e.RootID]
		if root == nil {
			return fmt.Errorf("privatefs: journal root unavailable")
		}
		if e.ObjectKind == "directory" {
			if e.Applied || e.CreatedByTransaction {
				if err := root.RemoveEmptyDir(e.FinalRel); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
			}
			continue
		}
		finalDigest, finalExists, err := hashRegular(root, e.FinalRel)
		if err != nil {
			return err
		}
		rollbackDigest, rollbackExists, err := hashRegular(root, e.RollbackRel)
		if e.RollbackRel == "" {
			rollbackExists = false
			err = nil
		}
		if err != nil {
			return err
		}
		if e.FinalExisted {
			if rollbackExists {
				if rollbackDigest != e.ExpectedRollbackSHA256 {
					return fmt.Errorf("privatefs: rollback digest mismatch")
				}
				if finalExists {
					if finalDigest != e.ExpectedFinalSHA256 {
						return fmt.Errorf("privatefs: unexpected final during rollback")
					}
					if err := root.RemoveRegular(e.FinalRel); err != nil {
						return err
					}
				}
				if err := root.Rename(e.RollbackRel, e.FinalRel); err != nil {
					return err
				}
			} else if !finalExists || finalDigest != e.ExpectedRollbackSHA256 {
				return fmt.Errorf("privatefs: original final unavailable during rollback")
			}
		} else if finalExists {
			if finalDigest != e.ExpectedFinalSHA256 {
				return fmt.Errorf("privatefs: unexpected installed final")
			}
			if err := root.RemoveRegular(e.FinalRel); err != nil {
				return err
			}
		}
		if _, exists, err := hashRegular(root, e.TempRel); err != nil {
			return err
		} else if exists {
			if err := root.RemoveRegular(e.TempRel); err != nil {
				return err
			}
		}
	}
	return nil
}

func verifyCommitted(record JournalRecord, roots map[string]*Root) error {
	for _, e := range record.Plan.Entries {
		root := roots[e.RootID]
		if root == nil {
			return fmt.Errorf("privatefs: journal root unavailable")
		}
		if e.ObjectKind == "directory" {
			entries, err := root.ReadDir(e.FinalRel)
			if err != nil {
				return err
			}
			_ = entries
			continue
		}
		digest, exists, err := hashRegular(root, e.FinalRel)
		if err != nil || !exists || digest != e.ExpectedFinalSHA256 {
			return fmt.Errorf("privatefs: committed final digest mismatch")
		}
	}
	return nil
}

func cleanupJournal(record JournalRecord, roots map[string]*Root) error {
	for _, e := range record.Plan.Entries {
		root := roots[e.RootID]
		if root == nil {
			return fmt.Errorf("privatefs: journal root unavailable")
		}
		for _, rel := range []string{e.TempRel, e.RollbackRel} {
			if rel == "" {
				continue
			}
			if _, exists, err := hashRegular(root, rel); err != nil {
				return err
			} else if exists {
				if err := root.RemoveRegular(rel); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func RecoverJournals(ctx context.Context, control *Root, roots map[string]*Root, kind string, audit AuditCompleter) error {
	if control == nil || !journalToken.MatchString(kind) {
		return fmt.Errorf("privatefs: invalid journal recovery request")
	}
	entries, err := control.ReadDir(".")
	if err != nil {
		return err
	}
	prefix := ".journal-" + kind + "-"
	var names []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), prefix) && strings.HasSuffix(entry.Name(), ".json") {
			if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
				return fmt.Errorf("privatefs: unsafe journal object")
			}
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		record, err := readJournal(control, name)
		if err != nil {
			digest := sha256.Sum256([]byte(name))
			return fmt.Errorf("privatefs: recover %s: %w", hex.EncodeToString(digest[:8]), err)
		}
		if record.Plan.Kind != kind {
			return fmt.Errorf("privatefs: journal kind mismatch")
		}
		for _, entry := range record.Plan.Entries {
			if roots[entry.RootID] == nil {
				return fmt.Errorf("privatefs: journal references unknown root")
			}
		}
		switch record.Phase {
		case JournalPrepared, JournalApplying:
			if err := rollbackJournal(record, roots); err != nil {
				return err
			}
			if record.Plan.AuditTransactionID != "" {
				if audit == nil {
					return fmt.Errorf("privatefs: audit completer required")
				}
				if err := audit.CompleteTransaction(ctx, record.Plan.AuditTransactionID, "rolled-back"); err != nil {
					return err
				}
			}
		case JournalStateCommitted:
			if err := verifyCommitted(record, roots); err != nil {
				return err
			}
			if record.Plan.AuditTransactionID != "" {
				if audit == nil {
					return fmt.Errorf("privatefs: audit completer required")
				}
				if err := audit.CompleteTransaction(ctx, record.Plan.AuditTransactionID, "success"); err != nil {
					return err
				}
			}
			if err := cleanupJournal(record, roots); err != nil {
				return err
			}
		case JournalAuditCommitted:
			if err := verifyCommitted(record, roots); err != nil {
				return err
			}
			if err := cleanupJournal(record, roots); err != nil {
				return err
			}
		}
		if err := control.RemoveRegular(name); err != nil {
			return err
		}
		if err := control.SyncDir("."); err != nil {
			return err
		}
	}
	return nil
}
