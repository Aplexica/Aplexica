package audit

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
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aplexica/aplexica/internal/filelock"
	"github.com/aplexica/aplexica/internal/privatefs"
)

const (
	maxRecordBytes = 16 << 10
	maxLogBytes    = 10 << 20
	maxPending     = 4096
)

var allowedCodes = map[string]bool{
	"device.identity_created": true, "device.pairing_approved": true, "device.pairing_rejected": true, "device.revoked": true,
	"security.roster_accepted": true, "security.roster_rejected": true, "security.roster_equivocation": true,
	"namespace.key_rotated": true, "remote.event_quarantined": true, "bundle.unsigned_acknowledged": true,
	"bundle.restore_completed": true, "backup.created": true, "project.revoked": true, "security.audit_unavailable": true,
	"native.restore_completed": true, "security.permissions_repaired": true, "native.restore_legacy_acknowledged": true,
	"project.credentials_sanitized": true, "release.input_verified": true,
	"project.registry_v3_migrated": true,
}
var outcomes = map[string]bool{"pending": true, "success": true, "denied": true, "failed": true, "rolled-back": true}
var terminalOutcomes = map[string]bool{"success": true, "failed": true, "rolled-back": true}
var safeName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
var safeValue = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]{0,255}$`)
var uuid7 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

type fieldKind string

const (
	kindID     fieldKind = "id"
	kindHash   fieldKind = "hash_prefix"
	kindCount  fieldKind = "count"
	kindReason fieldKind = "reason"
)

type Field struct {
	Name   string    `json:"name"`
	Kind   fieldKind `json:"kind"`
	Text   string    `json:"text,omitempty"`
	Number uint64    `json:"number,omitempty"`
}

func SafeID(name, value string) (Field, error) {
	if !safeName.MatchString(name) || !safeValue.MatchString(value) {
		return Field{}, fmt.Errorf("audit: unsafe id field")
	}
	return Field{Name: name, Kind: kindID, Text: value}, nil
}
func HashPrefix(name string, value [32]byte) Field {
	return Field{Name: name, Kind: kindHash, Text: hex.EncodeToString(value[:16])}
}
func Count(name string, value uint64) Field { return Field{Name: name, Kind: kindCount, Number: value} }
func Reason(name, stableCode string) (Field, error) {
	if !safeName.MatchString(name) || !regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,127}$`).MatchString(stableCode) {
		return Field{}, fmt.Errorf("audit: unsafe reason")
	}
	return Field{Name: name, Kind: kindReason, Text: stableCode}, nil
}

type Event struct {
	Code          string    `json:"code"`
	OccurredAt    time.Time `json:"occurredAt"`
	Outcome       string    `json:"outcome"`
	TransactionID string    `json:"transactionId,omitempty"`
	Fields        []Field   `json:"fields,omitempty"`
}
type Recorder interface {
	Record(context.Context, Event) error
	BeginTransaction(context.Context, string, Event) error
	CompleteTransaction(context.Context, string, string) error
}

type pendingRecord struct {
	TransactionID string    `json:"transactionId"`
	Event         Event     `json:"event"`
	Digest        string    `json:"digest"`
	Terminal      string    `json:"terminal,omitempty"`
	TerminalAt    time.Time `json:"terminalAt,omitempty"`
}
type pendingIndex struct {
	Version int             `json:"version"`
	Records []pendingRecord `json:"records"`
}

type completedRecord struct {
	TransactionID string `json:"transactionId"`
	Outcome       string `json:"outcome"`
	Digest        string `json:"digest"`
}

type completedIndex struct {
	Version int               `json:"version"`
	Records []completedRecord `json:"records"`
}

func loadCompleted(root *privatefs.Root) (completedIndex, error) {
	f, err := root.OpenReadRegular("completed.json")
	if errors.Is(err, os.ErrNotExist) {
		return completedIndex{Version: 1}, nil
	}
	if err != nil {
		return completedIndex{}, err
	}
	defer f.Close()
	var idx completedIndex
	dec := json.NewDecoder(io.LimitReader(f, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&idx); err != nil || idx.Version != 1 || len(idx.Records) > maxPending {
		return completedIndex{}, fmt.Errorf("audit: invalid completed index")
	}
	return idx, nil
}

func saveCompleted(root *privatefs.Root, idx completedIndex) error {
	if len(idx.Records) > maxPending {
		idx.Records = append([]completedRecord(nil), idx.Records[len(idx.Records)-maxPending:]...)
	}
	b, err := json.Marshal(idx)
	if err != nil {
		return err
	}
	return root.WriteFile("completed.json", b, privatefs.FilePolicy{RejectWritableByOthers: true})
}

func containsTransaction(root *privatefs.Root, id string) (bool, error) {
	needle := []byte(`"transactionId":"` + id + `"`)
	for _, name := range []string{"events.jsonl", "events.1.jsonl", "events.2.jsonl"} {
		f, err := root.OpenReadRegular(name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		b, readErr := io.ReadAll(io.LimitReader(f, maxLogBytes+1))
		closeErr := f.Close()
		if readErr != nil {
			return false, readErr
		}
		if closeErr != nil {
			return false, closeErr
		}
		if bytes.Contains(b, needle) {
			return true, nil
		}
	}
	return false, nil
}

func (r *FileRecorder) acquire() (*filelock.Lock, error) {
	if r == nil || r.Root == "" {
		return nil, fmt.Errorf("audit: recorder unavailable")
	}
	if err := privatefs.EnsureDir(r.Root, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true, AllowExisting: true}); err != nil {
		return nil, err
	}
	return filelock.Acquire(filepath.Join(r.Root, ".audit.lock"), 30*time.Second)
}

type FileRecorder struct {
	Root string
	mu   sync.Mutex
}

func normalizeEvent(e Event) (Event, []byte, error) {
	if !allowedCodes[e.Code] || !outcomes[e.Outcome] {
		return Event{}, nil, fmt.Errorf("audit: unsupported event code or outcome")
	}
	if e.OccurredAt.IsZero() {
		e.OccurredAt = time.Now().UTC()
	} else {
		e.OccurredAt = e.OccurredAt.UTC()
	}
	if e.TransactionID != "" && !uuid7.MatchString(e.TransactionID) {
		return Event{}, nil, fmt.Errorf("audit: invalid transaction id")
	}
	seen := map[string]bool{}
	for _, f := range e.Fields {
		if !safeName.MatchString(f.Name) || seen[f.Name] {
			return Event{}, nil, fmt.Errorf("audit: invalid or duplicate field")
		}
		seen[f.Name] = true
		switch f.Kind {
		case kindID:
			if !safeValue.MatchString(f.Text) {
				return Event{}, nil, fmt.Errorf("audit: invalid id")
			}
		case kindHash:
			if len(f.Text) != 32 {
				return Event{}, nil, fmt.Errorf("audit: invalid hash prefix")
			}
		case kindCount:
		case kindReason:
			if f.Text == "" {
				return Event{}, nil, fmt.Errorf("audit: empty reason")
			}
		default:
			return Event{}, nil, fmt.Errorf("audit: invalid field kind")
		}
	}
	sort.Slice(e.Fields, func(i, j int) bool { return e.Fields[i].Name < e.Fields[j].Name })
	b, err := json.Marshal(e)
	if err != nil || len(b) > maxRecordBytes {
		return Event{}, nil, fmt.Errorf("audit: record exceeds limit")
	}
	return e, b, nil
}

func (r *FileRecorder) open() (*privatefs.Root, error) {
	if r == nil || r.Root == "" {
		return nil, fmt.Errorf("audit: recorder unavailable")
	}
	if err := privatefs.EnsureDir(r.Root, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true, AllowExisting: true}); err != nil {
		return nil, err
	}
	return privatefs.OpenRoot(r.Root, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true})
}

func appendSync(root *privatefs.Root, rel string, b []byte) error {
	f, err := root.OpenAppendRegular(rel)
	if err != nil {
		return err
	}
	b = append(append([]byte(nil), b...), '\n')
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if ce := f.Close(); err == nil {
		err = ce
	}
	return err
}

func (r *FileRecorder) rotate(root *privatefs.Root) error {
	f, err := root.OpenReadRegular("events.jsonl")
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	info, err := f.Stat()
	_ = f.Close()
	if err != nil || info.Size() < maxLogBytes {
		return err
	}
	if ok, _ := regular(root, "events.2.jsonl"); ok {
		_ = root.RemoveRegular("events.2.jsonl")
	}
	if ok, _ := regular(root, "events.1.jsonl"); ok {
		if err := root.Rename("events.1.jsonl", "events.2.jsonl"); err != nil {
			return err
		}
	}
	return root.Rename("events.jsonl", "events.1.jsonl")
}
func regular(root *privatefs.Root, rel string) (bool, error) {
	f, e := root.OpenReadRegular(rel)
	if errors.Is(e, os.ErrNotExist) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	return true, f.Close()
}

func (r *FileRecorder) Record(_ context.Context, e Event) error {
	_, b, err := normalizeEvent(e)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	lock, err := r.acquire()
	if err != nil {
		return err
	}
	defer lock.Close()
	root, err := r.open()
	if err != nil {
		return err
	}
	defer root.Close()
	if err = r.rotate(root); err != nil {
		return err
	}
	return appendSync(root, "events.jsonl", b)
}

func loadPending(root *privatefs.Root) (pendingIndex, error) {
	f, err := root.OpenReadRegular("pending.json")
	if errors.Is(err, os.ErrNotExist) {
		return pendingIndex{Version: 1}, nil
	}
	if err != nil {
		return pendingIndex{}, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil {
		return pendingIndex{}, err
	}
	var idx pendingIndex
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&idx); err != nil || idx.Version != 1 || len(idx.Records) > maxPending {
		return pendingIndex{}, fmt.Errorf("audit: invalid pending index")
	}
	return idx, nil
}
func savePending(root *privatefs.Root, idx pendingIndex) error {
	b, e := json.Marshal(idx)
	if e != nil {
		return e
	}
	return root.WriteFile("pending.json", b, privatefs.FilePolicy{RejectWritableByOthers: true})
}

func (r *FileRecorder) BeginTransaction(_ context.Context, id string, e Event) error {
	if !uuid7.MatchString(id) {
		return fmt.Errorf("audit: transaction id must be canonical UUIDv7")
	}
	e.Outcome = "pending"
	if e.OccurredAt.IsZero() {
		e.OccurredAt = transactionTime(id)
	}
	e, b, err := normalizeEvent(e)
	if err != nil {
		return err
	}
	d := sha256.Sum256(b)
	digest := hex.EncodeToString(d[:])
	r.mu.Lock()
	defer r.mu.Unlock()
	lock, err := r.acquire()
	if err != nil {
		return err
	}
	defer lock.Close()
	root, err := r.open()
	if err != nil {
		return err
	}
	defer root.Close()
	idx, err := loadPending(root)
	if err != nil {
		return err
	}
	for _, p := range idx.Records {
		if p.TransactionID == id {
			if p.Digest == digest {
				return nil
			}
			return fmt.Errorf("audit: transaction content conflict")
		}
	}
	completed, err := loadCompleted(root)
	if err != nil {
		return err
	}
	for _, p := range completed.Records {
		if p.TransactionID == id {
			if p.Digest == digest {
				return nil
			}
			return fmt.Errorf("audit: transaction content conflict")
		}
	}
	if len(idx.Records) >= maxPending {
		return fmt.Errorf("audit: pending transaction limit")
	}
	idx.Records = append(idx.Records, pendingRecord{TransactionID: id, Event: e, Digest: digest})
	sort.Slice(idx.Records, func(i, j int) bool { return idx.Records[i].TransactionID < idx.Records[j].TransactionID })
	return savePending(root, idx)
}

func transactionTime(id string) time.Time {
	hexMillis := strings.ReplaceAll(id, "-", "")[:12]
	ms, err := strconv.ParseInt(hexMillis, 16, 64)
	if err != nil {
		return time.Unix(0, 0).UTC()
	}
	return time.UnixMilli(ms).UTC()
}

func (r *FileRecorder) CompleteTransaction(_ context.Context, id, outcome string) error {
	if !uuid7.MatchString(id) || !terminalOutcomes[outcome] {
		return fmt.Errorf("audit: invalid transaction completion")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	lock, err := r.acquire()
	if err != nil {
		return err
	}
	defer lock.Close()
	root, err := r.open()
	if err != nil {
		return err
	}
	defer root.Close()
	idx, err := loadPending(root)
	if err != nil {
		return err
	}
	at := -1
	for i, p := range idx.Records {
		if p.TransactionID == id {
			at = i
			if p.Terminal != "" {
				if p.Terminal != outcome {
					return fmt.Errorf("audit: terminal outcome conflict")
				}
			}
			break
		}
	}
	completed, err := loadCompleted(root)
	if err != nil {
		return err
	}
	if at < 0 {
		for _, record := range completed.Records {
			if record.TransactionID == id {
				if record.Outcome == outcome {
					return nil
				}
				return fmt.Errorf("audit: terminal outcome conflict")
			}
		}
		return fmt.Errorf("audit: unknown pending transaction")
	}
	pending := &idx.Records[at]
	if pending.Terminal != "" && pending.Terminal != outcome {
		return fmt.Errorf("audit: terminal outcome conflict")
	}
	if pending.Terminal == "" {
		pending.Terminal = outcome
		pending.TerminalAt = time.Now().UTC()
		if err := savePending(root, idx); err != nil {
			return err
		}
	}
	e := pending.Event
	e.Outcome = pending.Terminal
	e.OccurredAt = pending.TerminalAt
	e.TransactionID = id
	_, b, err := normalizeEvent(e)
	if err != nil {
		return err
	}
	if err = r.rotate(root); err != nil {
		return err
	}
	alreadyLogged, err := containsTransaction(root, id)
	if err != nil {
		return err
	}
	if !alreadyLogged {
		if err = appendSync(root, "events.jsonl", b); err != nil {
			return err
		}
	}
	completed.Records = append(completed.Records, completedRecord{TransactionID: id, Outcome: outcome, Digest: pending.Digest})
	if err := saveCompleted(root, completed); err != nil {
		return err
	}
	idx.Records = append(idx.Records[:at], idx.Records[at+1:]...)
	return savePending(root, idx)
}

type MemoryRecorder struct {
	Mu        sync.Mutex
	Events    []Event
	Pending   map[string]Event
	Completed map[string]string
}

func (m *MemoryRecorder) Record(_ context.Context, e Event) error {
	e, _, err := normalizeEvent(e)
	if err != nil {
		return err
	}
	m.Mu.Lock()
	defer m.Mu.Unlock()
	m.Events = append(m.Events, e)
	return nil
}
func (m *MemoryRecorder) BeginTransaction(_ context.Context, id string, e Event) error {
	if !uuid7.MatchString(id) {
		return fmt.Errorf("audit: invalid transaction id")
	}
	e.Outcome = "pending"
	if e.OccurredAt.IsZero() {
		e.OccurredAt = transactionTime(id)
	}
	e, _, err := normalizeEvent(e)
	if err != nil {
		return err
	}
	m.Mu.Lock()
	defer m.Mu.Unlock()
	if m.Pending == nil {
		m.Pending = map[string]Event{}
	}
	if old, ok := m.Pending[id]; ok {
		a, _ := json.Marshal(old)
		b, _ := json.Marshal(e)
		if string(a) == string(b) {
			return nil
		}
		return fmt.Errorf("audit: transaction conflict")
	}
	m.Pending[id] = e
	return nil
}
func (m *MemoryRecorder) CompleteTransaction(_ context.Context, id, outcome string) error {
	if !terminalOutcomes[outcome] {
		return fmt.Errorf("audit: invalid outcome")
	}
	m.Mu.Lock()
	defer m.Mu.Unlock()
	if m.Completed == nil {
		m.Completed = map[string]string{}
	}
	if old, ok := m.Completed[id]; ok {
		if old == outcome {
			return nil
		}
		return fmt.Errorf("audit: completion conflict")
	}
	if _, ok := m.Pending[id]; !ok {
		return fmt.Errorf("audit: unknown transaction")
	}
	delete(m.Pending, id)
	m.Completed[id] = outcome
	return nil
}

var _ Recorder = (*FileRecorder)(nil)
var _ Recorder = (*MemoryRecorder)(nil)
