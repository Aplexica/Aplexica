package daemon

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/aplexica/aplexica/internal/privatefs"
)

const remoteTransferRootEnv = "APLEXICA_REMOTE_TRANSFER_ROOT"

const maxRemoteTransferActiveJobs = 1

const (
	maxRemoteTransferFiles = 8
	maxRemoteTransferBytes = int64(512 << 20)
)

var errRemoteTransferBusy = errors.New("daemon: staged checkpoint transfer busy")

type remoteTransferJob struct {
	params proto.RemotePublishStagedV1Params
}

type remoteTransferSession struct {
	root *privatefs.Root
	path string

	mu      sync.Mutex
	closing bool
	active  sync.WaitGroup

	jobsMu sync.Mutex
	jobs   map[string]remoteTransferJob
}

func prepareRemoteTransferSession(basePath string) (*remoteTransferSession, error) {
	if !filepath.IsAbs(basePath) {
		return nil, errors.New("daemon: remote transfer root must be absolute")
	}
	root, err := privatefs.OpenRoot(basePath, privatefs.DirPolicy{
		Access: privatefs.AccessPrivate, RepairOwned: true, AllowExisting: true,
	})
	if err != nil {
		return nil, fmt.Errorf("daemon: open remote transfer root: %w", err)
	}
	// The root is deliberately stable across plugin and daemon restarts. Exact
	// checkpoint bytes are owned by the durable outbox and are removed only
	// after the matching outbox intent reaches a terminal state. A per-process
	// session directory would make a crash discard the only idempotent retry
	// body before the outbox could be retired.
	return &remoteTransferSession{root: root, path: basePath, jobs: make(map[string]remoteTransferJob)}, nil
}

func configureRemoteTransferEnvironment(cmd *exec.Cmd, session *remoteTransferSession) {
	if cmd == nil {
		return
	}
	prefix := remoteTransferRootEnv + "="
	env := cmd.Environ()
	filtered := env[:0]
	for _, value := range env {
		if !strings.HasPrefix(strings.ToUpper(value), strings.ToUpper(prefix)) {
			filtered = append(filtered, value)
		}
	}
	if session != nil {
		filtered = append(filtered, prefix+session.path)
	}
	cmd.Env = filtered
}

func (s *remoteTransferSession) acquire() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing || s.root == nil {
		return false
	}
	s.active.Add(1)
	return true
}

func (s *remoteTransferSession) release() { s.active.Done() }

func (s *remoteTransferSession) stage(ctx context.Context, event proto.RemoteEvent, streamID, streamEpoch string) (proto.RemotePublishStagedV1Params, error) {
	params, _, err := s.stageOrReuse(ctx, event, streamID, streamEpoch)
	return params, err
}

// stageOrReuse retains one exact transfer descriptor across retryable plugin
// outcomes and lost responses. The daemon outbox still owns event.Bytes; this
// cache owns only the bounded private handoff copy and metadata needed to reuse
// it. A new session after either process crashes reconstructs the same bytes
// from that durable outbox intent with a fresh random basename.
func (s *remoteTransferSession) stageOrReuse(ctx context.Context, event proto.RemoteEvent, streamID, streamEpoch string) (proto.RemotePublishStagedV1Params, string, error) {
	var zero proto.RemotePublishStagedV1Params
	staged := event.DaemonStagedPayload
	sealedBytes := len(event.Bytes)
	if staged != nil {
		if sealedBytes != 0 || !validRemoteTransferToken(staged.FileID) || staged.SealedBytes <= proto.MaxSealedEventBytes || staged.SealedBytes > proto.MaxRemoteStagedCheckpointBytes || !validRemoteTransferToken(staged.BodyDigest) || event.BodyDigest != staged.BodyDigest {
			return zero, "", errors.New("daemon: invalid durable staged checkpoint descriptor")
		}
		sealedBytes = int(staged.SealedBytes)
	}
	if s == nil || s.root == nil || ctx == nil || streamID == "" || streamEpoch == "" || event.Lane != "retained" || event.Clear || sealedBytes <= proto.MaxSealedEventBytes || sealedBytes > proto.MaxRemoteStagedCheckpointBytes {
		return zero, "", errors.New("daemon: invalid staged checkpoint")
	}
	if staged == nil && event.BodyDigest == "" {
		digest := sha256.Sum256(event.Bytes)
		event.BodyDigest = hex.EncodeToString(digest[:])
	} else if !validRemoteTransferToken(event.BodyDigest) {
		return zero, "", errors.New("daemon: invalid staged checkpoint digest")
	}
	jobKey := remoteTransferJobKey(event, streamID, streamEpoch)
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	if job, ok := s.jobs[jobKey]; ok {
		return job.params, jobKey, nil
	}
	if len(s.jobs) >= maxRemoteTransferActiveJobs {
		return zero, "", errRemoteTransferBusy
	}
	var params proto.RemotePublishStagedV1Params
	var err error
	if staged != nil {
		if err = s.validateFile(ctx, staged.FileID, int64(staged.SealedBytes), staged.BodyDigest); err == nil {
			event.DaemonStagedPayload = nil
			transfer := proto.RemoteStagedFileV1{
				ProtocolVersion: proto.RemoteStagedTransferProtocolV1,
				TransferID:      staged.FileID,
				SealedBytes:     staged.SealedBytes,
				BodyDigest:      staged.BodyDigest,
				StreamID:        streamID,
				StreamEpoch:     streamEpoch,
			}
			transfer.BindingDigest = proto.RemoteStagedBindingDigest(event, transfer)
			params = proto.RemotePublishStagedV1Params{Event: event, Transfer: transfer}
		}
	} else {
		params, err = s.stageNew(ctx, event, streamID, streamEpoch)
	}
	if err != nil {
		return zero, "", err
	}
	s.jobs[jobKey] = remoteTransferJob{params: params}
	return params, jobKey, nil
}

func (s *remoteTransferSession) stageNew(ctx context.Context, event proto.RemoteEvent, streamID, streamEpoch string) (proto.RemotePublishStagedV1Params, error) {
	var zero proto.RemotePublishStagedV1Params
	select {
	case <-ctx.Done():
		return zero, ctx.Err()
	default:
	}
	sealedBytes := len(event.Bytes)
	digest := event.BodyDigest
	if digest == "" {
		sum := sha256.Sum256(event.Bytes)
		digest = hex.EncodeToString(sum[:])
	}
	if !validRemoteTransferToken(digest) {
		return zero, errors.New("daemon: invalid staged checkpoint digest")
	}
	transferID := remoteTransferFileID(event, digest)
	existingErr := s.validateFile(ctx, transferID, int64(len(event.Bytes)), digest)
	if existingErr == nil {
		event.BodyDigest = digest
		event.Bytes = nil
		transfer := proto.RemoteStagedFileV1{ProtocolVersion: proto.RemoteStagedTransferProtocolV1, TransferID: transferID, SealedBytes: uint64(sealedBytes), BodyDigest: digest, StreamID: streamID, StreamEpoch: streamEpoch}
		transfer.BindingDigest = proto.RemoteStagedBindingDigest(event, transfer)
		return proto.RemotePublishStagedV1Params{Event: event, Transfer: transfer}, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return zero, ctxErr
	}
	if !errors.Is(existingErr, fs.ErrNotExist) {
		return zero, fmt.Errorf("daemon: existing staged checkpoint is invalid: %w", existingErr)
	}
	if err := s.ensureCapacity(int64(len(event.Bytes))); err != nil {
		return zero, err
	}
	f, err := s.root.CreateExclusive(transferID, privatefs.FilePolicy{RequirePrivateParent: true, RejectWritableByOthers: true})
	if err != nil {
		return zero, fmt.Errorf("daemon: create staged checkpoint: %w", err)
	}
	remove := true
	defer func() {
		_ = f.Close()
		if remove {
			_ = s.root.RemoveRegular(transferID)
		}
	}()
	h := sha256.New()
	reader := &remoteTransferContextReader{ctx: ctx, reader: bytes.NewReader(event.Bytes)}
	written, err := io.CopyBuffer(io.MultiWriter(f, h), reader, make([]byte, 64<<10))
	if err == nil && written != int64(len(event.Bytes)) {
		err = io.ErrShortWrite
	}
	writtenDigest := hex.EncodeToString(h.Sum(nil))
	if err == nil && digest != writtenDigest {
		err = errors.New("daemon: staged checkpoint digest mismatch")
	}
	if err == nil {
		event.BodyDigest = writtenDigest
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = s.root.SyncDir(".")
	}
	if err != nil {
		return zero, fmt.Errorf("daemon: persist staged checkpoint: %w", err)
	}
	event.Bytes = nil
	transfer := proto.RemoteStagedFileV1{
		ProtocolVersion: proto.RemoteStagedTransferProtocolV1,
		TransferID:      transferID,
		SealedBytes:     uint64(written),
		BodyDigest:      writtenDigest,
		StreamID:        streamID,
		StreamEpoch:     streamEpoch,
	}
	transfer.BindingDigest = proto.RemoteStagedBindingDigest(event, transfer)
	remove = false
	return proto.RemotePublishStagedV1Params{Event: event, Transfer: transfer}, nil
}

func remoteTransferFileID(event proto.RemoteEvent, digest string) string {
	h := sha256.New()
	_, _ = h.Write([]byte("aplexica/daemon-staged-payload/v1\x00"))
	for _, value := range []string{event.NamespaceID, event.BranchID, event.ArtifactID, event.EventID, event.EventHash, event.CheckpointAlignmentHash, event.Origin, digest} {
		_, _ = h.Write([]byte(value))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (s *remoteTransferSession) validateFile(ctx context.Context, fileID string, size int64, digest string) error {
	if s == nil || s.root == nil || ctx == nil || !validRemoteTransferToken(fileID) || size <= 0 || !validRemoteTransferToken(digest) {
		return errors.New("daemon: invalid staged checkpoint file")
	}
	f, err := s.root.OpenReadRegularIntegrity(fileID)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != size {
		return errors.New("daemon: staged checkpoint file size mismatch")
	}
	h := sha256.New()
	written, err := io.CopyBuffer(h, &remoteTransferContextReader{ctx: ctx, reader: io.LimitReader(f, size+1)}, make([]byte, 64<<10))
	if err != nil || written != size || hex.EncodeToString(h.Sum(nil)) != digest {
		return errors.New("daemon: staged checkpoint file digest mismatch")
	}
	return nil
}

func (s *remoteTransferSession) readInbound(ctx context.Context, event proto.RemoteEvent, staged proto.RemoteStagedFileV1) ([]byte, error) {
	if s == nil || s.root == nil || ctx == nil || len(event.Bytes) != 0 || event.Lane != "retained" || event.Clear ||
		staged.ProtocolVersion != proto.RemoteStagedTransferProtocolV1 || !validRemoteTransferToken(staged.TransferID) ||
		staged.SealedBytes <= proto.MaxSealedEventBytes || staged.SealedBytes > proto.MaxRemoteStagedCheckpointBytes ||
		!validRemoteTransferToken(staged.BodyDigest) || staged.BodyDigest != event.BodyDigest ||
		!validRemoteTransferToken(staged.BindingDigest) || staged.BindingDigest != proto.RemoteStagedBindingDigest(event, staged) {
		return nil, errors.New("daemon: invalid staged inbound checkpoint")
	}
	f, err := s.root.OpenReadRegularIntegrity(staged.TransferID)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != int64(staged.SealedBytes) {
		return nil, errors.New("daemon: staged inbound size mismatch")
	}
	body, err := io.ReadAll(io.LimitReader(&remoteTransferContextReader{ctx: ctx, reader: f}, int64(staged.SealedBytes)+1))
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(body)
	if len(body) != int(staged.SealedBytes) || hex.EncodeToString(digest[:]) != staged.BodyDigest || !json.Valid(body) {
		return nil, errors.New("daemon: staged inbound digest mismatch")
	}
	return body, nil
}

func (s *remoteTransferSession) ensureCapacity(incoming int64) error {
	entries, err := s.root.ReadDir(".")
	if err != nil {
		return fmt.Errorf("daemon: inspect staged checkpoint capacity: %w", err)
	}
	var total int64
	files := 0
	for _, entry := range entries {
		if !validRemoteTransferToken(entry.Name()) || entry.Type()&fs.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return errors.New("daemon: unsafe node in remote transfer root")
		}
		info, err := entry.Info()
		if err != nil || info.Size() < 0 || total > maxRemoteTransferBytes-info.Size() {
			return errors.New("daemon: invalid staged checkpoint capacity")
		}
		total += info.Size()
		files++
	}
	if files >= maxRemoteTransferFiles || incoming <= 0 || incoming > maxRemoteTransferBytes || total > maxRemoteTransferBytes-incoming {
		return errors.New("daemon: staged checkpoint capacity reached")
	}
	return nil
}

func remoteTransferJobKey(event proto.RemoteEvent, streamID, streamEpoch string) string {
	sealedBytes := len(event.Bytes)
	if event.DaemonStagedPayload != nil {
		sealedBytes = int(event.DaemonStagedPayload.SealedBytes)
	}
	event.Bytes = nil
	event.DaemonStagedPayload = nil
	transfer := proto.RemoteStagedFileV1{
		ProtocolVersion: proto.RemoteStagedTransferProtocolV1,
		SealedBytes:     uint64(sealedBytes),
		BodyDigest:      event.BodyDigest,
		StreamID:        streamID,
		StreamEpoch:     streamEpoch,
	}
	return proto.RemoteStagedBindingDigest(event, transfer)
}

func (s *remoteTransferSession) remove(transferID string) error {
	if s == nil || s.root == nil || !validRemoteTransferToken(transferID) {
		return errors.New("daemon: invalid staged checkpoint cleanup")
	}
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	err := s.root.RemoveRegular(transferID)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for key, job := range s.jobs {
		if job.params.Transfer.TransferID == transferID {
			delete(s.jobs, key)
		}
	}
	return nil
}

func (s *remoteTransferSession) complete(jobKey, transferID string) error {
	if s == nil || s.root == nil || jobKey == "" || !validRemoteTransferToken(transferID) {
		return errors.New("daemon: invalid staged checkpoint completion")
	}
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	job, ok := s.jobs[jobKey]
	if !ok {
		return nil
	}
	if job.params.Transfer.TransferID != transferID {
		return errors.New("daemon: staged checkpoint completion mismatch")
	}
	// The durable outbox owns the referenced file and removes it only after its
	// terminal JSON intent is durably retired. Deleting here would create a
	// crash window where an accepted response is lost after the file disappears
	// but before the outbox entry is removed.
	delete(s.jobs, jobKey)
	return nil
}

func (s *remoteTransferSession) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return nil
	}
	s.closing = true
	s.mu.Unlock()
	s.active.Wait()
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	var first error
	if s.root != nil {
		first = s.root.Close()
	}
	s.jobs = nil
	return first
}

func randomRemoteTransferToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("daemon: generate remote transfer token: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

func validRemoteTransferToken(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			if char < 'a' || char > 'f' {
				return false
			}
		}
	}
	return true
}

type remoteTransferContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func stagedRemoteCheckpointCandidate(event proto.RemoteEvent) bool {
	if event.Lane != "retained" || event.Clear {
		return false
	}
	if event.DaemonStagedPayload != nil {
		staged := event.DaemonStagedPayload
		return len(event.Bytes) == 0 && validRemoteTransferToken(staged.FileID) &&
			staged.SealedBytes > proto.MaxSealedEventBytes && staged.SealedBytes <= proto.MaxRemoteStagedCheckpointBytes &&
			validRemoteTransferToken(staged.BodyDigest) && event.BodyDigest == staged.BodyDigest
	}
	return len(event.Bytes) > proto.MaxSealedEventBytes && len(event.Bytes) <= proto.MaxRemoteStagedCheckpointBytes
}

// stagedRemoteCheckpointAuthority requires both the exact signed plugin image
// capability and a current, additive server negotiation naming the stream. It
// deliberately permits shadow/durable-read so checkpoint anchors can be seeded
// before cutover, but never invents a singular or version-derived descriptor.
func (r *RemoteRunner) stagedRemoteCheckpointAuthority(event proto.RemoteEvent) (string, string, bool) {
	if r == nil || !stagedRemoteCheckpointCandidate(event) {
		return "", "", false
	}
	r.syncMu.RLock()
	decision := r.syncMode
	signed := r.syncStagedCheckpointSigned
	r.syncMu.RUnlock()
	if !signed || remoteSyncModeRank(decision.Mode) < remoteSyncModeRankShadow || decision.SelectedProtocol != proto.RemoteStagedTransferProtocolV1 ||
		!decision.FeatureGateEnabled || !decision.AllActiveDevicesCapable ||
		!remoteStringSetContains(decision.ServerCapabilities, proto.CapabilityDurableDeltaSyncV1) ||
		!remoteStringSetContains(decision.ServerCapabilities, proto.CapabilityStagedCheckpointV1) || len(decision.Streams) == 0 {
		return "", "", false
	}
	for _, descriptor := range decision.Streams {
		if descriptor.NamespaceID == event.NamespaceID && descriptor.StreamID != "" && descriptor.StreamEpoch != "" {
			return descriptor.StreamID, descriptor.StreamEpoch, true
		}
	}
	return "", "", false
}

func (r *RemoteRunner) inboundStagedCheckpointAuthority(event proto.RemoteEvent, staged *proto.RemoteStagedFileV1) bool {
	if r == nil || staged == nil || len(event.Bytes) != 0 || event.Lane != "retained" || event.Clear ||
		event.CheckpointCoverage == 0 || !validRemoteTransferToken(event.CheckpointGeneration) || !validRemoteTransferToken(event.CheckpointAlignmentHash) ||
		staged.ProtocolVersion != proto.RemoteStagedTransferProtocolV1 || staged.SealedBytes <= proto.MaxSealedEventBytes || staged.SealedBytes > proto.MaxRemoteStagedCheckpointBytes ||
		!validRemoteTransferToken(staged.TransferID) || !validRemoteTransferToken(staged.BodyDigest) || staged.BodyDigest != event.BodyDigest ||
		!validRemoteTransferToken(staged.BindingDigest) || staged.BindingDigest != proto.RemoteStagedBindingDigest(event, *staged) {
		return false
	}
	r.syncMu.RLock()
	decision, signed := r.syncMode, r.syncStagedCheckpointSigned
	r.syncMu.RUnlock()
	if !signed || remoteSyncModeRank(decision.Mode) < remoteSyncModeRankDurableRead || decision.SelectedProtocol != proto.RemoteStagedTransferProtocolV1 ||
		!decision.FeatureGateEnabled || !decision.AllActiveDevicesCapable || !remoteStringSetContains(decision.ServerCapabilities, proto.CapabilityStagedCheckpointV1) {
		return false
	}
	if len(decision.Streams) == 0 {
		return event.NamespaceID == "" && decision.StreamID == staged.StreamID && decision.StreamEpoch == staged.StreamEpoch
	}
	for _, descriptor := range decision.Streams {
		if descriptor.StreamID == staged.StreamID && descriptor.StreamEpoch == staged.StreamEpoch && descriptor.NamespaceID == event.NamespaceID {
			return true
		}
	}
	return false
}

// HydrateInboundStagedCheckpoint reads an exact plugin-staged ciphertext only
// after signed capability and negotiated-stream checks. The returned delivery
// is an in-memory working copy; the caller persists the original descriptor so
// a 256 MiB body never enters the durable inbox JSON record.
func (r *RemoteRunner) HydrateInboundStagedCheckpoint(ctx context.Context, delivery proto.RemoteInboundDeliveryV2) (proto.RemoteInboundDeliveryV2, error) {
	if len(delivery.Events) != 1 || !r.inboundStagedCheckpointAuthority(delivery.Events[0], delivery.StagedCheckpoint) ||
		delivery.StagedCheckpoint.StreamID != delivery.StreamID || delivery.StagedCheckpoint.StreamEpoch != delivery.StreamEpoch {
		return proto.RemoteInboundDeliveryV2{}, errors.New("daemon: staged inbound authority unavailable")
	}
	r.proxyMu.Lock()
	transfer := r.transfer
	acquired := transfer != nil && transfer.acquire()
	r.proxyMu.Unlock()
	if !acquired {
		return proto.RemoteInboundDeliveryV2{}, ErrRemoteReconnecting
	}
	defer transfer.release()
	body, err := transfer.readInbound(ctx, delivery.Events[0], *delivery.StagedCheckpoint)
	if err != nil {
		return proto.RemoteInboundDeliveryV2{}, err
	}
	hydrated := delivery
	hydrated.Events = append([]proto.RemoteEvent(nil), delivery.Events...)
	hydrated.Events[0].Bytes = body
	return hydrated, nil
}

// CompleteInboundStagedCheckpoint removes the exact private file only after
// the durable inbox has stored a terminal ACK. A lost response is safe: the
// cached ACK no longer needs body bytes and retry cleanup is idempotent.
func (r *RemoteRunner) CompleteInboundStagedCheckpoint(delivery proto.RemoteInboundDeliveryV2) error {
	if len(delivery.Events) != 1 || !r.inboundStagedCheckpointAuthority(delivery.Events[0], delivery.StagedCheckpoint) {
		return errors.New("daemon: staged inbound cleanup unavailable")
	}
	r.proxyMu.Lock()
	transfer := r.transfer
	acquired := transfer != nil && transfer.acquire()
	r.proxyMu.Unlock()
	if !acquired {
		return ErrRemoteReconnecting
	}
	defer transfer.release()
	return transfer.remove(delivery.StagedCheckpoint.TransferID)
}

func (r *RemoteRunner) HydrateRecoveryStagedCheckpoint(ctx context.Context, record *proto.RemoteRecoveryEventV1) (*proto.RemoteRecoveryEventV1, error) {
	if record == nil || !r.inboundStagedCheckpointAuthority(record.Event, record.StagedCheckpoint) {
		return nil, errors.New("daemon: staged recovery authority unavailable")
	}
	r.proxyMu.Lock()
	transfer := r.transfer
	acquired := transfer != nil && transfer.acquire()
	r.proxyMu.Unlock()
	if !acquired {
		return nil, ErrRemoteReconnecting
	}
	defer transfer.release()
	body, err := transfer.readInbound(ctx, record.Event, *record.StagedCheckpoint)
	if err != nil {
		return nil, err
	}
	hydrated := *record
	hydrated.Event.Bytes = body
	return &hydrated, nil
}

func (r *RemoteRunner) CompleteRecoveryStagedCheckpoint(record *proto.RemoteRecoveryEventV1) error {
	if record == nil {
		return errors.New("daemon: staged recovery cleanup unavailable")
	}
	event := record.Event
	event.Bytes = nil
	if !r.inboundStagedCheckpointAuthority(event, record.StagedCheckpoint) {
		return errors.New("daemon: staged recovery cleanup unavailable")
	}
	r.proxyMu.Lock()
	transfer := r.transfer
	acquired := transfer != nil && transfer.acquire()
	r.proxyMu.Unlock()
	if !acquired {
		return ErrRemoteReconnecting
	}
	defer transfer.release()
	return transfer.remove(record.StagedCheckpoint.TransferID)
}

// SupportsLargeRetainedCheckpoint reports whether the exact authenticated
// plugin image and fresh server decision currently authorize the additive
// private-file handoff. It is intentionally an optional publisher capability:
// legacy/self-hosted plugins retain the old 4 MiB refusal unchanged.
func (r *RemoteRunner) SupportsLargeRetainedCheckpoint(event proto.RemoteEvent) bool {
	_, _, ok := r.stagedRemoteCheckpointAuthority(event)
	return ok
}

// PrepareLargeRetainedCheckpoint persists the exact sealed body into the
// stable private staging root and returns a lightweight event descriptor for
// the durable JSON outbox. The caller must append that descriptor before it
// queues the event; on append failure it calls DiscardLargeRetainedCheckpoint.
func (r *RemoteRunner) PrepareLargeRetainedCheckpoint(ctx context.Context, event proto.RemoteEvent) (proto.RemoteEvent, error) {
	if r == nil || ctx == nil {
		return proto.RemoteEvent{}, errors.New("daemon: staged checkpoint unavailable")
	}
	r.proxyMu.Lock()
	transfer := r.transfer
	acquired := transfer != nil && transfer.acquire()
	r.proxyMu.Unlock()
	if !acquired {
		return proto.RemoteEvent{}, ErrRemoteReconnecting
	}
	defer transfer.release()
	streamID, streamEpoch, ok := r.stagedRemoteCheckpointAuthority(event)
	if !ok {
		return proto.RemoteEvent{}, ErrRemoteNotConfigured
	}
	params, _, err := transfer.stageOrReuse(ctx, event, streamID, streamEpoch)
	if err != nil {
		return proto.RemoteEvent{}, err
	}
	event.Bytes = nil
	event.BodyDigest = params.Transfer.BodyDigest
	event.DaemonStagedPayload = &proto.RemoteDaemonStagedPayloadV1{
		FileID: params.Transfer.TransferID, SealedBytes: params.Transfer.SealedBytes, BodyDigest: params.Transfer.BodyDigest,
	}
	return event, nil
}

func (r *RemoteRunner) DiscardLargeRetainedCheckpoint(event proto.RemoteEvent) error {
	if r == nil || event.DaemonStagedPayload == nil {
		return nil
	}
	r.proxyMu.Lock()
	transfer := r.transfer
	acquired := transfer != nil && transfer.acquire()
	r.proxyMu.Unlock()
	if !acquired {
		return ErrRemoteReconnecting
	}
	defer transfer.release()
	return transfer.remove(event.DaemonStagedPayload.FileID)
}

func (r *remoteTransferContextReader) Read(p []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	default:
		return r.reader.Read(p)
	}
}
