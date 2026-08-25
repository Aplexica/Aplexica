package acf

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBundle_RoundTripsArtifactsAndEvents(t *testing.T) {
	// Source store with one memory artifact + one event.
	src := &Store{Root: filepath.Join(t.TempDir(), "src")}
	require.NoError(t, src.Init())

	a := newTestArtifact(NewID())
	a.Name = "CLAUDE.md"
	require.NoError(t, src.WriteArtifact(a))

	payload, _ := EncodePayload(MemoryPayload{Format: "markdown", Content: "# Hello\n"})
	e := Event{
		EventID:    NewID(),
		ArtifactID: a.ArtifactID,
		Type:       "create",
		Timestamp:  time.Now().UTC(),
		Provenance: Provenance{DeviceID: "dev", SourceAgent: "test", AdapterVersion: "0.0.0"},
		Payload:    payload,
		ParentHash: "",
	}
	require.NoError(t, src.AppendEvent(KindMemory, e))

	// Bundle into a buffer.
	var buf bytes.Buffer
	require.NoError(t, src.Bundle(&buf, BundleOpts{AplexicaVersion: "0.1.10"}))

	// Restore into a fresh store.
	dst := &Store{Root: filepath.Join(t.TempDir(), "dst")}
	require.NoError(t, dst.Init())
	require.NoError(t, dst.RestoreWithOptions(&buf, "", RestoreOptions{UnsignedOK: true}))

	// The dst store should have the same artifact + event.
	got, err := dst.ReadArtifact(KindMemory, a.ArtifactID)
	require.NoError(t, err)
	require.Equal(t, a.ArtifactID, got.ArtifactID)
	require.Equal(t, a.Name, got.Name)

	events, err := dst.ReadEvents(KindMemory, a.ArtifactID)
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.NoError(t, VerifyChain(events))

	decoded, err := DecodeMemoryPayload(events[0])
	require.NoError(t, err)
	require.Equal(t, "# Hello\n", decoded.Content)
}

func TestBundle_EmptyStore(t *testing.T) {
	src := &Store{Root: filepath.Join(t.TempDir(), "src")}
	require.NoError(t, src.Init())

	var buf bytes.Buffer
	require.NoError(t, src.Bundle(&buf, BundleOpts{AplexicaVersion: "0.1.10"}))

	dst := &Store{Root: filepath.Join(t.TempDir(), "dst")}
	require.NoError(t, dst.Init())
	require.NoError(t, dst.RestoreWithOptions(&buf, "", RestoreOptions{UnsignedOK: true}))

	// No artifacts should exist.
	for _, k := range []Kind{KindMemory, KindSkill, KindTool, KindConversation} {
		got, err := dst.ListArtifacts(k)
		require.NoError(t, err)
		require.Empty(t, got)
	}
}

func TestBundle_MultipleKinds(t *testing.T) {
	src := &Store{Root: filepath.Join(t.TempDir(), "src")}
	require.NoError(t, src.Init())

	// Write artifacts of all 4 kinds.
	for _, kind := range []Kind{KindMemory, KindSkill, KindTool, KindConversation} {
		a := newTestArtifact(NewID())
		a.Kind = kind
		a.Name = string(kind) + ".file"
		require.NoError(t, src.WriteArtifact(a))
	}

	var buf bytes.Buffer
	require.NoError(t, src.Bundle(&buf, BundleOpts{AplexicaVersion: "0.1.10"}))

	dst := &Store{Root: filepath.Join(t.TempDir(), "dst")}
	require.NoError(t, dst.Init())
	require.NoError(t, dst.RestoreWithOptions(&buf, "", RestoreOptions{UnsignedOK: true}))

	// Each kind should have one artifact in the restored store.
	for _, kind := range []Kind{KindMemory, KindSkill, KindTool, KindConversation} {
		got, err := dst.ListArtifacts(kind)
		require.NoError(t, err)
		require.Len(t, got, 1, "kind %s should have 1 artifact after restore", kind)
		require.Equal(t, string(kind)+".file", got[0].Name)
	}
}

func TestBundle_RestoreRejectsExistingArtifact(t *testing.T) {
	src := &Store{Root: filepath.Join(t.TempDir(), "src")}
	require.NoError(t, src.Init())
	a := newTestArtifact(NewID())
	require.NoError(t, src.WriteArtifact(a))

	var buf bytes.Buffer
	require.NoError(t, src.Bundle(&buf, BundleOpts{AplexicaVersion: "0.1.10"}))

	// Pre-populate dst with the same artifact (simulates a collision).
	dst := &Store{Root: filepath.Join(t.TempDir(), "dst")}
	require.NoError(t, dst.Init())
	require.NoError(t, dst.WriteArtifact(a))

	err := dst.RestoreWithOptions(&buf, "", RestoreOptions{UnsignedOK: true})
	require.Error(t, err)
	require.Contains(t, err.Error(), "already exists",
		"restore must refuse to overwrite an existing artifact")
}

func TestBundle_MetaIsFirstFile(t *testing.T) {
	src := &Store{Root: filepath.Join(t.TempDir(), "src")}
	require.NoError(t, src.Init())

	var buf bytes.Buffer
	require.NoError(t, src.Bundle(&buf, BundleOpts{AplexicaVersion: "0.1.10"}))

	// Decode the bundle's first file and verify it's meta.json.
	meta, err := PeekBundleMeta(bytes.NewReader(buf.Bytes()))
	require.NoError(t, err)
	require.Equal(t, "1.1", meta.BundleVersion)
	require.Equal(t, "0.1.10", meta.AplexicaVersion)
}

func TestBundle_RestoreRejectsNewerSchema(t *testing.T) {
	// Hand-craft a bundle with a future BundleVersion to test version gating.
	var buf bytes.Buffer
	// Use the internal helper to write a meta with a fake-future version.
	require.NoError(t, writeFutureVersionBundle(&buf))

	dst := &Store{Root: filepath.Join(t.TempDir(), "dst")}
	require.NoError(t, dst.Init())
	err := dst.RestoreWithOptions(&buf, "", RestoreOptions{UnsignedOK: true})
	require.Error(t, err)
	require.Contains(t, err.Error(), "bundle version")
}

func TestBundle_FullRoundTrip_AllKinds(t *testing.T) {
	// Build a source store with one artifact per kind, each with a real event.
	src := &Store{Root: filepath.Join(t.TempDir(), "src")}
	require.NoError(t, src.Init())

	artifactIDs := map[Kind]string{}
	for _, k := range []Kind{KindMemory, KindSkill, KindTool, KindConversation} {
		a := newTestArtifact(NewID())
		a.Kind = k
		a.Name = string(k) + ".file"
		require.NoError(t, src.WriteArtifact(a))

		var payload []byte
		var encErr error
		switch k {
		case KindMemory:
			payload, encErr = EncodePayload(MemoryPayload{Format: "markdown", Content: "# " + string(k) + "\n"})
		case KindSkill:
			payload, encErr = EncodePayload(SkillPayload{Format: "skill.md", Content: "skill body\n"})
		case KindTool:
			payload, encErr = EncodePayload(ToolPayload{Format: "test", Content: `{"x":1}`})
		case KindConversation:
			payload, encErr = EncodePayload(ConversationPayload{Format: "test", Content: `{"t":"x"}` + "\n"})
		}
		require.NoError(t, encErr)

		e := Event{
			EventID:    NewID(),
			ArtifactID: a.ArtifactID,
			Type:       "create",
			Timestamp:  time.Now().UTC(),
			Provenance: Provenance{DeviceID: "dev", SourceAgent: "test", AdapterVersion: "0.0.0"},
			Payload:    payload,
			ParentHash: "",
		}
		require.NoError(t, src.AppendEvent(k, e))
		artifactIDs[k] = a.ArtifactID
	}

	// Bundle.
	var buf bytes.Buffer
	require.NoError(t, src.Bundle(&buf, BundleOpts{AplexicaVersion: "0.1.10"}))

	// Restore.
	dst := &Store{Root: filepath.Join(t.TempDir(), "dst")}
	require.NoError(t, dst.Init())
	require.NoError(t, dst.RestoreWithOptions(&buf, "", RestoreOptions{UnsignedOK: true}))

	// Verify every artifact survived with chain intact.
	for k, id := range artifactIDs {
		a, err := dst.ReadArtifact(k, id)
		require.NoError(t, err, "kind=%s id=%s should be present", k, id)
		require.Equal(t, id, a.ArtifactID)

		events, err := dst.ReadEvents(k, id)
		require.NoError(t, err)
		require.Len(t, events, 1)
		require.NoError(t, VerifyChain(events))
	}
}

// writeFutureVersionBundle is a test helper that emits a minimal valid tar.gz
// with a meta.json declaring a future BundleVersion. It uses the same
// archive/tar + compress/gzip stdlib paths as the production code so the
// restore can read its structure.
func writeFutureVersionBundle(w *bytes.Buffer) error {
	// Implementation goes inline in the test file; uses BundleMeta marshalling.
	meta := BundleMeta{BundleVersion: "999.0", AplexicaVersion: "0.0.0"}
	metaBytes, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return writeMetaOnlyBundle(w, metaBytes)
}

func TestRestore_SecretsRoutedToSecretsRoot(t *testing.T) {
	// Build a bundle that includes a secrets/ entry, then restore it with a
	// separate secretsRoot path. Confirm the secret lands at <secretsRoot>/...
	// and NOT under <store>/secrets/.
	srcStore := &Store{Root: filepath.Join(t.TempDir(), "src")}
	require.NoError(t, srcStore.Init())
	srcSecretsRoot := filepath.Join(t.TempDir(), "src-secrets")
	require.NoError(t, os.MkdirAll(srcSecretsRoot, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(srcSecretsRoot, "token-abc"), []byte("secret-value"), 0o600))

	// Write one artifact so the bundle has store content too.
	a := newTestArtifact(NewID())
	require.NoError(t, srcStore.WriteArtifact(a))

	// Bundle with SecretsRoot set — the actual API is Bundle (not Backup) and
	// includes-secrets is implicit when SecretsRoot is non-empty.
	var buf bytes.Buffer
	require.NoError(t, srcStore.Bundle(&buf, BundleOpts{
		AplexicaVersion: "0.17.2",
		SecretsRoot:     srcSecretsRoot,
	}))

	// Restore into a fresh store + separate secrets-root.
	dstStore := &Store{Root: filepath.Join(t.TempDir(), "dst")}
	require.NoError(t, dstStore.Init())
	dstSecretsRoot := filepath.Join(t.TempDir(), "dst-secrets")
	require.NoError(t, dstStore.RestoreWithOptions(&buf, dstSecretsRoot, RestoreOptions{UnsignedOK: true}))

	// Secret should be at dst-secrets/, NOT inside dst/secrets/.
	secret, err := os.ReadFile(filepath.Join(dstSecretsRoot, "token-abc"))
	require.NoError(t, err)
	require.Equal(t, "secret-value", string(secret))
	_, err = os.Stat(filepath.Join(dstStore.Root, "secrets", "token-abc"))
	require.True(t, os.IsNotExist(err), "secret must NOT land under store/secrets when secretsRoot was given")
}

// readBundleMetaAndBodies decodes a bundle buffer and returns the parsed
// meta.json plus a map of every non-meta archive path -> its raw body bytes
// (after any transform, exactly as restored). Used by the manifest tests to
// assert the per-artifact hashes/sizes the bundler recorded match the bytes
// actually shipped.
func readBundleMetaAndBodies(t *testing.T, buf *bytes.Buffer) (BundleMeta, map[string][]byte) {
	t.Helper()
	var meta BundleMeta
	bodies := map[string][]byte{}
	gz, err := gzip.NewReader(buf)
	require.NoError(t, err)
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if hdr.Typeflag == tar.TypeDir {
			continue
		}
		body, err := io.ReadAll(tr)
		require.NoError(t, err)
		if hdr.Name == "meta.json" {
			require.NoError(t, json.Unmarshal(body, &meta))
			continue
		}
		bodies[hdr.Name] = body
	}
	return meta, bodies
}

// TestBundle_MetaCarriesHostnameTotalBytesAndHashes (FR-01.8 / FR-01.24)
// asserts the bundle manifest now records the source hostname, the summed
// pre-compression artifact body size, and a per-artifact-path sha256 map.
func TestBundle_MetaCarriesHostnameTotalBytesAndHashes(t *testing.T) {
	src := &Store{Root: filepath.Join(t.TempDir(), "src")}
	require.NoError(t, src.Init())

	// Two memory artifacts, each with a real event, so the manifest has
	// several archive entries to hash.
	for i, name := range []string{"CLAUDE.md", "AGENTS.md"} {
		a := newTestArtifact(NewID())
		a.Name = name
		require.NoError(t, src.WriteArtifact(a))
		payload, encErr := EncodePayload(MemoryPayload{
			Format: "markdown", Content: "# body " + name + "\n",
		})
		require.NoError(t, encErr)
		e := Event{
			EventID:    NewID(),
			ArtifactID: a.ArtifactID,
			Type:       EventTypeCreate,
			Timestamp:  time.Now().UTC().Add(time.Duration(i) * time.Second),
			Provenance: Provenance{DeviceID: "dev", SourceAgent: "test", AdapterVersion: "0.0.0"},
			Payload:    payload,
			ParentHash: "",
		}
		require.NoError(t, src.AppendEvent(KindMemory, e))
	}

	var buf bytes.Buffer
	require.NoError(t, src.Bundle(&buf, BundleOpts{AplexicaVersion: "0.1.10"}))

	meta, bodies := readBundleMetaAndBodies(t, &buf)

	// Hostname matches the machine that produced the bundle.
	wantHost, _ := os.Hostname()
	require.Equal(t, wantHost, meta.Hostname, "manifest must record the source hostname")

	// Hashes: one entry per non-meta archive body, 64-hex sha256 matching bytes.
	require.NotEmpty(t, bodies)
	require.Len(t, meta.Hashes, len(bodies), "every archive entry must have a hash")
	hexRe := regexp.MustCompile(`^[0-9a-f]{64}$`)
	var summed int64
	for path, body := range bodies {
		got, ok := meta.Hashes[path]
		require.True(t, ok, "missing hash for %s", path)
		require.Regexp(t, hexRe, got, "hash for %s must be 64 lowercase hex chars", path)
		sum := sha256.Sum256(body)
		require.Equal(t, hex.EncodeToString(sum[:]), got, "hash for %s must match its bytes", path)
		summed += int64(len(body))
	}

	// TotalBytes equals the sum of the shipped artifact body lengths.
	require.Greater(t, meta.TotalBytes, int64(0))
	require.Equal(t, summed, meta.TotalBytes, "TotalBytes must equal summed artifact bytes")
}

// TestBundleMeta_BackwardCompatOldMetaWithoutNewFields proves a pre-FR-01.8
// bundle whose meta.json lacks hostname/totalBytes/hashes still restores: the
// new fields decode to their zero values and Restore proceeds normally.
func TestBundleMeta_BackwardCompatOldMetaWithoutNewFields(t *testing.T) {
	// Hand-build a v1.0 bundle whose meta.json predates the new fields and
	// which carries one real artifact entry, then restore it.
	src := &Store{Root: filepath.Join(t.TempDir(), "src")}
	require.NoError(t, src.Init())
	a := newTestArtifact(NewID())
	require.NoError(t, src.WriteArtifact(a))
	artBytes, err := os.ReadFile(src.artifactPath(a.Kind, a.ArtifactID))
	require.NoError(t, err)

	// Marshal an OLD-shaped meta (only the original six fields) to prove the
	// new fields are not required on the wire.
	oldMeta := struct {
		BundleVersion   string         `json:"bundleVersion"`
		CreatedAt       time.Time      `json:"createdAt"`
		AplexicaVersion string         `json:"aplexicaVersion"`
		ArtifactCounts  map[string]int `json:"artifactCounts"`
		IncludesSecrets bool           `json:"includesSecrets"`
	}{
		BundleVersion:   "1.0",
		CreatedAt:       time.Now().UTC(),
		AplexicaVersion: "0.1.10",
		ArtifactCounts:  map[string]int{"memory": 1},
		IncludesSecrets: false,
	}
	metaBytes, err := json.Marshal(oldMeta)
	require.NoError(t, err)
	require.NotContains(t, string(metaBytes), "hostname")
	require.NotContains(t, string(metaBytes), "totalBytes")
	require.NotContains(t, string(metaBytes), "hashes")

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	require.NoError(t, writeTarFile(tw, "meta.json", metaBytes, 0o644))
	require.NoError(t, writeTarFile(tw,
		"acf/memories/"+a.ArtifactID+".json", artBytes, 0o644))
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())

	// PeekBundleMeta tolerates the absent fields (zero values).
	peeked, err := PeekBundleMeta(bytes.NewReader(buf.Bytes()))
	require.NoError(t, err)
	require.Empty(t, peeked.Hostname)
	require.Zero(t, peeked.TotalBytes)
	require.Nil(t, peeked.Hashes)

	// Restore succeeds despite the old-shaped meta.
	dst := &Store{Root: filepath.Join(t.TempDir(), "dst")}
	require.NoError(t, dst.Init())
	require.NoError(t, dst.RestoreWithOptions(&buf, "", RestoreOptions{UnsignedOK: true}))
	got, err := dst.ReadArtifact(KindMemory, a.ArtifactID)
	require.NoError(t, err)
	require.Equal(t, a.ArtifactID, got.ArtifactID)
}

// TestRestoreDryRun_ClassifiesAddsAndCollisions (FR-01.13) builds a bundle
// from store A, then dry-runs it into an empty store B (all would-add) and
// into a store C already holding one of the artifacts (one collision
// reported, correct ID, nothing written to C).
func TestRestoreDryRun_ClassifiesAddsAndCollisions(t *testing.T) {
	srcA := &Store{Root: filepath.Join(t.TempDir(), "A")}
	require.NoError(t, srcA.Init())

	ids := map[Kind]string{}
	for _, k := range []Kind{KindMemory, KindSkill} {
		a := newTestArtifact(NewID())
		a.Kind = k
		a.Name = string(k) + ".file"
		require.NoError(t, srcA.WriteArtifact(a))
		ids[k] = a.ArtifactID
	}

	var bundleA bytes.Buffer
	require.NoError(t, srcA.Bundle(&bundleA, BundleOpts{AplexicaVersion: "0.1.10"}))
	bundleBytes := bundleA.Bytes()

	// B: empty target → every artifact is a would-add, zero collisions.
	dstB := &Store{Root: filepath.Join(t.TempDir(), "B")}
	require.NoError(t, dstB.Init())
	resB, err := dstB.RestoreDryRunWithOptions(bytes.NewReader(bundleBytes), "", RestoreOptions{UnsignedOK: true})
	require.NoError(t, err)
	require.Equal(t, 2, resB.TotalAdds())
	require.Equal(t, 0, resB.TotalCollisions())
	require.Empty(t, resB.CollisionIDs())

	// C: pre-populate one of A's artifacts → exactly one collision, by ID.
	dstC := &Store{Root: filepath.Join(t.TempDir(), "C")}
	require.NoError(t, dstC.Init())
	collA := newTestArtifact(ids[KindMemory])
	collA.Kind = KindMemory
	require.NoError(t, dstC.WriteArtifact(collA))

	// Capture C's on-disk state so we can prove the dry-run wrote nothing.
	collPath := dstC.artifactPath(KindMemory, ids[KindMemory])
	before, err := os.ReadFile(collPath)
	require.NoError(t, err)

	resC, err := dstC.RestoreDryRunWithOptions(bytes.NewReader(bundleBytes), "", RestoreOptions{UnsignedOK: true})
	require.NoError(t, err)
	require.Equal(t, 1, resC.TotalAdds(), "the skill is still a would-add")
	require.Equal(t, 1, resC.TotalCollisions())
	require.Equal(t, []string{ids[KindMemory]}, resC.CollisionIDs())

	// C must be byte-for-byte unchanged, and the skill must NOT have been written.
	after, err := os.ReadFile(collPath)
	require.NoError(t, err)
	require.Equal(t, before, after, "dry-run must not modify the colliding artifact")
	_, err = os.Stat(dstC.artifactPath(KindSkill, ids[KindSkill]))
	require.True(t, os.IsNotExist(err), "dry-run must not write the would-add artifact")
}

func TestRestore_EmptySecretsRootFallsBackToStoreLocal(t *testing.T) {
	srcStore := &Store{Root: filepath.Join(t.TempDir(), "src")}
	require.NoError(t, srcStore.Init())
	srcSecretsRoot := filepath.Join(t.TempDir(), "src-secrets")
	require.NoError(t, os.MkdirAll(srcSecretsRoot, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(srcSecretsRoot, "fallback"), []byte("v"), 0o600))

	a := newTestArtifact(NewID())
	require.NoError(t, srcStore.WriteArtifact(a))

	var buf bytes.Buffer
	require.NoError(t, srcStore.Bundle(&buf, BundleOpts{
		AplexicaVersion: "0.17.2",
		SecretsRoot:     srcSecretsRoot,
	}))

	dstStore := &Store{Root: filepath.Join(t.TempDir(), "dst")}
	require.NoError(t, dstStore.Init())
	require.NoError(t, dstStore.RestoreWithOptions(&buf, "", RestoreOptions{UnsignedOK: true})) // empty → store-local fallback

	secret, err := os.ReadFile(filepath.Join(dstStore.Root, "secrets", "fallback"))
	require.NoError(t, err, "with empty secretsRoot, secrets land under store/secrets (pre-v0.17.2 behavior)")
	require.Equal(t, "v", string(secret))
}
