package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/privatefs"
)

func watermarkTestDigest(label string) string {
	sum := sha256.Sum256([]byte(label))
	return hex.EncodeToString(sum[:])
}

func watermarkFixture(position uint64) DurablePublishWatermark {
	return DurablePublishWatermark{
		Key:              DurablePublishWatermarkKey{StreamID: "stream-1", StreamEpoch: "epoch-1", ArtifactID: "artifact-1", BranchID: "main"},
		CanonicalEventID: "event-1", CanonicalEventHash: watermarkTestDigest("event-hash"), Position: position,
		RecipientFingerprint: watermarkTestDigest("recipients"), BodyDigest: watermarkTestDigest("body"),
		EventIdentityDigest: watermarkTestDigest("identity"), MetadataDigest: watermarkTestDigest("metadata"),
	}
}

func TestDurablePublishWatermarkAdvancePersistsAndLoads(t *testing.T) {
	root := filepath.Join(t.TempDir(), "watermarks")
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	store := &DurablePublishWatermarkStore{Root: root, now: func() time.Time { return now }}
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	want, err := store.Advance(watermarkFixture(7))
	if err != nil {
		t.Fatal(err)
	}
	if want.SchemaVersion != 1 || want.CommittedAt != now || !validateWatermarkDigest(want.Checksum) {
		t.Fatalf("unexpected persisted watermark: %+v", want)
	}
	got, err := store.Load(want.Key)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("loaded watermark differs\n got: %+v\nwant: %+v", got, want)
	}
	if err := privatefs.ValidateDir(root); err != nil {
		t.Fatalf("watermark root is not private: %v", err)
	}
}

func TestDurablePublishWatermarkExactRetryAndMonotonicAdvance(t *testing.T) {
	store := &DurablePublishWatermarkStore{Root: filepath.Join(t.TempDir(), "watermarks")}
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	first, err := store.Advance(watermarkFixture(7))
	if err != nil {
		t.Fatal(err)
	}
	retry, err := store.Advance(watermarkFixture(7))
	if err != nil || retry != first {
		t.Fatalf("exact retry = %+v, %v; want %+v", retry, err, first)
	}
	regressed := watermarkFixture(6)
	if _, err := store.Advance(regressed); !errors.Is(err, ErrDurablePublishWatermarkRegress) {
		t.Fatalf("regression error = %v", err)
	}
	next := watermarkFixture(11)
	next.CanonicalEventID = "event-2"
	next.CanonicalEventHash = watermarkTestDigest("event-hash-2")
	next.BodyDigest = watermarkTestDigest("body-2")
	next.EventIdentityDigest = watermarkTestDigest("identity-2")
	next.MetadataDigest = watermarkTestDigest("metadata-2")
	advanced, err := store.Advance(next)
	if err != nil || advanced.Position != 11 {
		t.Fatalf("advance = %+v, %v", advanced, err)
	}
}

func TestDurablePublishWatermarkRejectsSamePositionConflict(t *testing.T) {
	store := &DurablePublishWatermarkStore{Root: filepath.Join(t.TempDir(), "watermarks")}
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	first, err := store.Advance(watermarkFixture(7))
	if err != nil {
		t.Fatal(err)
	}
	conflict := watermarkFixture(7)
	conflict.CanonicalEventID = "different-event"
	if _, err := store.Advance(conflict); !errors.Is(err, ErrDurablePublishWatermarkConflict) {
		t.Fatalf("same-position conflict error = %v", err)
	}
	got, err := store.Load(first.Key)
	if err != nil || got != first {
		t.Fatalf("conflict changed stored watermark: %+v, %v", got, err)
	}
}

func TestDurablePublishWatermarkRejectsTamperAndEpochReuse(t *testing.T) {
	store := &DurablePublishWatermarkStore{Root: filepath.Join(t.TempDir(), "watermarks")}
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	stored, err := store.Advance(watermarkFixture(7))
	if err != nil {
		t.Fatal(err)
	}
	path, err := store.path(stored.Key)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.Replace(string(raw), `"position":7`, `"position":8`, 1))
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(stored.Key); !errors.Is(err, ErrDurablePublishWatermarkInvalid) {
		t.Fatalf("tamper error = %v", err)
	}

	otherEpoch := watermarkFixture(1)
	otherEpoch.Key.StreamEpoch = "epoch-2"
	otherEpoch.CanonicalEventID = "epoch-2-event"
	otherEpoch.CanonicalEventHash = watermarkTestDigest("epoch-2-event")
	otherEpoch.BodyDigest = watermarkTestDigest("epoch-2-body")
	otherEpoch.EventIdentityDigest = watermarkTestDigest("epoch-2-identity")
	otherEpoch.MetadataDigest = watermarkTestDigest("epoch-2-metadata")
	if _, err := store.Advance(otherEpoch); err != nil {
		t.Fatalf("new epoch must get an independent anchor: %v", err)
	}
}

func TestDurablePublishWatermarkListBindsFullRecoveryGeneration(t *testing.T) {
	store := &DurablePublishWatermarkStore{Root: filepath.Join(t.TempDir(), "watermarks")}
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	legacy := watermarkFixture(1)
	legacy.Key.ArtifactID = "legacy"
	if _, err := store.Advance(legacy); err != nil {
		t.Fatal(err)
	}
	bound := watermarkFixture(2)
	bound.Key.ArtifactID = "bound"
	bound.AccessGeneration = 7
	bound.SecurityGeneration = 9
	bound.SecurityBarrier = watermarkTestDigest("barrier")
	bound.KeyMode = "recipient-wrap-v2"
	if _, err := store.Advance(bound); err != nil {
		t.Fatal(err)
	}

	got, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !got[0].HasRecoveryGeneration() || got[1].HasRecoveryGeneration() {
		t.Fatalf("generation-bound/legacy anchors misclassified: %+v", got)
	}
}

func TestDurablePublishWatermarkRejectsUnknownOrMismatchedKeyMode(t *testing.T) {
	store := &DurablePublishWatermarkStore{Root: filepath.Join(t.TempDir(), "watermarks")}
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []struct {
		name    string
		mode    string
		version uint64
	}{
		{name: "unknown", mode: "future-mode", version: 0},
		{name: "recipient with namespace version", mode: "recipient-wrap-v2", version: 1},
		{name: "namespace without version", mode: "namespace-key-v1", version: 0},
	} {
		t.Run(mode.name, func(t *testing.T) {
			value := watermarkFixture(1)
			value.AccessGeneration = 1
			value.SecurityGeneration = 1
			value.SecurityBarrier = watermarkTestDigest("barrier")
			value.KeyMode = mode.mode
			value.KeyVersion = mode.version
			if _, err := store.Advance(value); !errors.Is(err, ErrDurablePublishWatermarkInvalid) {
				t.Fatalf("invalid mode/version accepted: %v", err)
			}
		})
	}
}
