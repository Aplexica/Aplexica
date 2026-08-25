package codex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"sync"

	"github.com/aplexica/aplexica/internal/acf"
)

// convEncodeCache keeps the canonical projection of a small LRU set of active
// Codex rollout files. After a verified append it reads and parses only bytes
// after prefixLen. Active rollouts can exceed 100 MiB, so avoiding os.ReadFile
// on every 500 ms watcher settle is essential for steady-state CPU and memory.
const (
	defaultConvCacheMaxEntries = 32
	defaultConvCacheMaxBytes   = 256 << 20
	convHeadSampleBytes        = 4096
	convTailSampleBytes        = 256
	convPendingMaxBytes        = 1 << 20

	// The byte budget is intentionally an upper-bound estimate of memory kept
	// alive by a cache entry, not the size of its source rollout. Native Codex
	// logs routinely contain hundreds of megabytes of filtered execution
	// policy/tool traffic while their portable user/assistant projection is
	// only a few kilobytes. Charging readLen made those cheap projections evict
	// each other and turned every small append back into a full-file parse.
	convEntryRetainedOverhead  = 512
	convEventRetainedOverhead  = 256
	convBlockRetainedOverhead  = 64
	convStringRetainedOverhead = 16
)

type convEncodeEntry struct {
	mu               sync.Mutex
	prefixLen        int64
	readLen          int64
	headHash         uint64
	tailHash         uint64
	events           []acf.ConversationEvent
	legacyEvents     []acf.ConversationEvent
	legacyKnown      bool
	pending          []byte
	pendingOversized bool
	generated        bool
	generatedKnown   bool
	fileInfo         os.FileInfo
	retainedBytes    int64
	tick             uint64
	inUse            int
}

// convProjectionSnapshot is one immutable byte-prefix view of a rollout. Both
// projections come from the same bounded disk read: events is the portable
// encoder output and legacyEvents is the exact pre-filter output used only as
// sanitation proof.
type convProjectionSnapshot struct {
	events       []acf.ConversationEvent
	legacyEvents []acf.ConversationEvent
}

type convEncodeCache struct {
	mu         sync.Mutex
	m          map[string]*convEncodeEntry
	maxEntries int
	maxBytes   int64
	totalBytes int64
	clock      uint64
	fullParses uint64
	incParses  uint64
	fullBytes  uint64
	incBytes   uint64

	// afterSnapshotRead is a deterministic test seam for an append that lands
	// after the fixed snapshot boundary has been read. Production adapters leave
	// it nil.
	afterSnapshotRead func(path string, size int64)
}

func newConvEncodeCache(maxEntries int, maxBytes int64) *convEncodeCache {
	if maxEntries <= 0 {
		maxEntries = defaultConvCacheMaxEntries
	}
	return &convEncodeCache{
		m:          make(map[string]*convEncodeEntry),
		maxEntries: maxEntries,
		maxBytes:   maxBytes,
	}
}

func (c *convEncodeCache) entry(path string) *convEncodeEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.m[path]
	if e == nil {
		e = &convEncodeEntry{}
		c.m[path] = e
	}
	c.clock++
	e.tick = c.clock
	e.inUse++
	return e
}

func (c *convEncodeCache) releaseEntry(e *convEncodeEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e.inUse > 0 {
		e.inUse--
	}
	c.evictLocked()
}

// encodeFile returns the full canonical event projection while reading only a
// verified append on warm calls. Prefix verification samples the file itself,
// so it does not require retaining the original raw transcript in memory.
func (c *convEncodeCache) encodeFile(path string) ([]acf.ConversationEvent, error) {
	snapshot, err := c.snapshotFileMode(path, false)
	if err != nil {
		return nil, err
	}
	return snapshot.events, nil
}

// snapshotFile reads at most the size reported by the opened descriptor's
// initial Stat. Growth after that boundary is deliberately left for the next
// call. A partial JSON row retains at most convPendingMaxBytes in memory; after
// an oversized row finally receives its newline (or becomes valid JSON at
// EOF), that one row is reread once for parsing without reconstructing the rest
// of the rollout.
func (c *convEncodeCache) snapshotFile(path string) (convProjectionSnapshot, error) {
	return c.snapshotFileMode(path, true)
}

func (c *convEncodeCache) snapshotFileMode(path string, needLegacy bool) (convProjectionSnapshot, error) {
	e := c.entry(path)
	defer c.releaseEntry(e)
	e.mu.Lock()
	defer e.mu.Unlock()

	f, err := os.Open(path)
	if err != nil {
		return convProjectionSnapshot{}, fmt.Errorf("codex: open conversation: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return convProjectionSnapshot{}, fmt.Errorf("codex: stat conversation: %w", err)
	}

	incremental := e.readLen > 0 && st.Size() >= e.readLen &&
		e.fileInfo != nil && os.SameFile(e.fileInfo, st)
	// Rollouts are defined as append-only agent logs. Sampling the retained
	// prefix protects the hot path from truncation, replacement, and ordinary
	// tail edits without hashing a multi-hundred-megabyte prefix on every small
	// append; an unsampled in-place middle rewrite is outside that contract.
	if incremental {
		head, herr := readSampleAt(f, 0, min64(e.readLen, convHeadSampleBytes))
		tailStart := e.readLen - convTailSampleBytes
		if tailStart < 0 {
			tailStart = 0
		}
		tail, terr := readSampleAt(f, tailStart, e.readLen-tailStart)
		incremental = herr == nil && terr == nil &&
			fnv64a(head) == e.headHash && fnv64a(tail) == e.tailHash
	}
	// Ordinary imports intentionally retain only the portable projection. The
	// legacy projection is needed solely when the residue preflight requests a
	// one-time sanitation proof; if it was not built for this prefix, reconstruct
	// both projections from one full read now.
	if needLegacy && !e.legacyKnown {
		incremental = false
	}

	var (
		readLen          = st.Size()
		prefixLen        int64
		clean            []acf.ConversationEvent
		legacy           []acf.ConversationEvent
		pending          []byte
		pendingOversized bool
		generated        bool
		generatedKnown   bool
		legacyKnown      bool
		bytesRead        int64
	)
	if incremental {
		appended, rerr := io.ReadAll(io.NewSectionReader(f, e.readLen, readLen-e.readLen))
		if rerr != nil {
			return convProjectionSnapshot{}, fmt.Errorf("codex: read conversation tail: %w", rerr)
		}
		bytesRead = int64(len(appended))
		var combined []byte
		if e.pendingOversized {
			newline := bytes.IndexByte(appended, '\n')
			if newline < 0 {
				// json.Decoder accepts a complete value at EOF without a row
				// separator. Once an oversized active row appears capable of being
				// complete, reread and validate that row once so its final event is
				// not stranded forever waiting for a newline that may never arrive.
				if oversizedJSONRowMayBeComplete(appended) {
					rowLen := readLen - e.prefixLen
					row, rowErr := io.ReadAll(io.NewSectionReader(f, e.prefixLen, rowLen))
					if rowErr != nil {
						return convProjectionSnapshot{}, fmt.Errorf("codex: reread completed oversized row: %w", rowErr)
					}
					if json.Valid(row) {
						bytesRead += int64(len(row))
						combined = row
					}
				}
				if combined == nil {
					clean = e.events[:len(e.events):len(e.events)]
					if needLegacy {
						legacy = e.legacyEvents[:len(e.legacyEvents):len(e.legacyEvents)]
						legacyKnown = e.legacyKnown
					}
					prefixLen = e.prefixLen
					pending = append([]byte(nil), e.pending...)
					pendingOversized = true
					generated, generatedKnown = e.generated, e.generatedKnown
					goto snapshotParsed
				}
			} else {
				rowLen := e.readLen + int64(newline) + 1 - e.prefixLen
				row, rowErr := io.ReadAll(io.NewSectionReader(f, e.prefixLen, rowLen))
				if rowErr != nil {
					return convProjectionSnapshot{}, fmt.Errorf("codex: reread completed oversized row: %w", rowErr)
				}
				bytesRead += int64(len(row))
				combined = make([]byte, 0, len(row)+len(appended)-newline-1)
				combined = append(combined, row...)
				combined = append(combined, appended[newline+1:]...)
			}
		} else {
			combined = make([]byte, 0, len(e.pending)+len(appended))
			combined = append(combined, e.pending...)
			combined = append(combined, appended...)
		}
		generated, generatedKnown = e.generated, e.generatedKnown
		if !generatedKnown {
			generated, generatedKnown = generatedCodexSessionState(combined)
		}
		var consumed int64
		clean = e.events[:len(e.events):len(e.events)]
		if needLegacy {
			legacy = e.legacyEvents[:len(e.legacyEvents):len(e.legacyEvents)]
			legacyKnown = e.legacyKnown
		}
		if generatedKnown {
			cleanTail, cleanConsumed := encodePortableCanonicalFromMode(combined, 0, generated)
			consumed = cleanConsumed
			clean = append(clean, cleanTail...)
			if needLegacy {
				legacyTail, legacyConsumed := encodeCanonicalFromPolicy(combined, 0, false, true)
				consumed = min64(cleanConsumed, legacyConsumed)
				legacy = append(legacy, legacyTail...)
				legacyKnown = true
			}
		}
		prefixLen = e.prefixLen + consumed
		pending, pendingOversized = boundedConversationPending(combined[consumed:])
	} else {
		content, rerr := io.ReadAll(io.NewSectionReader(f, 0, readLen))
		if rerr != nil {
			return convProjectionSnapshot{}, fmt.Errorf("codex: read conversation: %w", rerr)
		}
		bytesRead = int64(len(content))
		generated, generatedKnown = generatedCodexSessionState(content)
		var cleanConsumed int64
		if generatedKnown {
			clean, cleanConsumed = encodePortableCanonicalFromMode(content, 0, generated)
			prefixLen = cleanConsumed
			if needLegacy {
				var legacyConsumed int64
				legacy, legacyConsumed = encodeCanonicalFromPolicy(content, 0, false, true)
				prefixLen = min64(cleanConsumed, legacyConsumed)
				legacyKnown = true
			}
		}
		pending, pendingOversized = boundedConversationPending(content[prefixLen:])
	}

snapshotParsed:
	if c.afterSnapshotRead != nil {
		c.afterSnapshotRead(path, readLen)
	}
	pathInfo, err := os.Stat(path)
	if err != nil {
		return convProjectionSnapshot{}, fmt.Errorf("codex: restat conversation: %w", err)
	}
	if !os.SameFile(st, pathInfo) || pathInfo.Size() < readLen {
		return convProjectionSnapshot{}, fmt.Errorf("codex: conversation changed non-append-only during snapshot")
	}
	headHash, tailHash, err := entryHashesAt(f, readLen)
	if err != nil {
		return convProjectionSnapshot{}, err
	}

	e.prefixLen = prefixLen
	e.readLen = readLen
	e.headHash = headHash
	e.tailHash = tailHash
	e.events = clean
	e.legacyEvents = legacy
	e.legacyKnown = legacyKnown
	e.pending = pending
	e.pendingOversized = pendingOversized
	e.generated = generated
	e.generatedKnown = generatedKnown
	e.fileInfo = st
	c.recordParse(e, incremental, bytesRead)
	return convProjectionSnapshot{
		events:       clean[:len(clean):len(clean)],
		legacyEvents: legacy[:len(legacy):len(legacy)],
	}, nil
}

// encode is retained as a deterministic in-memory seam for focused tests.
func (c *convEncodeCache) encode(path string, content []byte) []acf.ConversationEvent {
	e := c.entry(path)
	defer c.releaseEntry(e)
	e.mu.Lock()
	incremental := e.readLen > 0 && int64(len(content)) >= e.readLen &&
		convHeadHash(content, e.readLen) == e.headHash &&
		convTailHash(content, e.readLen) == e.tailHash
	bytesRead := int64(len(content))
	if incremental {
		appended := content[e.readLen:]
		bytesRead = int64(len(appended))
		combined := append(append([]byte(nil), e.pending...), appended...)
		if e.pendingOversized {
			combined = content[e.prefixLen:]
		}
		if !e.generatedKnown {
			e.generated, e.generatedKnown = generatedCodexSessionState(combined)
		}
		if e.generatedKnown {
			tail, consumed := encodePortableCanonicalFromMode(combined, 0, e.generated)
			e.events = append(e.events, tail...)
			e.legacyEvents = nil
			e.legacyKnown = false
			e.prefixLen += consumed
			e.pending, e.pendingOversized = boundedConversationPending(combined[consumed:])
		}
	} else {
		e.generated, e.generatedKnown = generatedCodexSessionState(content)
		if e.generatedKnown {
			e.events, e.prefixLen = encodePortableCanonicalFromMode(content, 0, e.generated)
			e.legacyEvents = nil
			e.legacyKnown = false
		} else {
			e.events, e.legacyEvents, e.prefixLen = nil, nil, 0
			e.legacyKnown = false
		}
		e.pending, e.pendingOversized = boundedConversationPending(content[e.prefixLen:])
	}
	e.readLen = int64(len(content))
	e.headHash = convHeadHash(content, e.readLen)
	e.tailHash = convTailHash(content, e.readLen)
	out := e.events[:len(e.events):len(e.events)]
	e.mu.Unlock()
	c.recordParse(e, incremental, bytesRead)
	return out
}

func oversizedJSONRowMayBeComplete(appended []byte) bool {
	for i := len(appended) - 1; i >= 0; i-- {
		switch appended[i] {
		case ' ', '\t', '\r', '\n':
			continue
		default:
			return appended[i] == '}'
		}
	}
	return false
}

func boundedConversationPending(pending []byte) ([]byte, bool) {
	if len(pending) <= convPendingMaxBytes {
		return append([]byte(nil), pending...), false
	}
	return append([]byte(nil), pending[:convPendingMaxBytes]...), true
}

func (c *convEncodeCache) recordParse(e *convEncodeEntry, incremental bool, bytesRead int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if incremental {
		c.incParses++
		c.incBytes += uint64(bytesRead)
	} else {
		c.fullParses++
		c.fullBytes += uint64(bytesRead)
	}
	retained := convEntryRetainedBytes(e)
	c.totalBytes += retained - e.retainedBytes
	e.retainedBytes = retained
	c.evictLocked()
}

// convEntryRetainedBytes estimates the allocations an entry keeps live after
// parsing. It deliberately over-counts slice/string headers and capacities;
// the cache limit is a safety budget, not a heap profiler. Crucially, readLen
// and the filtered native bytes are absent: the raw rollout is not retained.
func convEntryRetainedBytes(e *convEncodeEntry) int64 {
	if e == nil {
		return 0
	}
	return convEntryRetainedOverhead +
		conversationEventsRetainedBytes(e.events) +
		conversationEventsRetainedBytes(e.legacyEvents) +
		int64(cap(e.pending))
}

func conversationEventsRetainedBytes(events []acf.ConversationEvent) int64 {
	retained := int64(cap(events)) * convEventRetainedOverhead
	for i := range events {
		event := &events[i]
		retained += int64(len(event.Type) + len(event.Role) + len(event.CallID) +
			len(event.ToolName) + len(event.BranchID) + len(event.SourceEventID) +
			len(event.SnapshotState))
		retained += int64(cap(event.Input) + cap(event.NativeExtras))
		retained += int64(cap(event.Content)) * convBlockRetainedOverhead
		for j := range event.Content {
			block := &event.Content[j]
			retained += int64(len(block.Type) + len(block.Text) + len(block.Data))
		}
		retained += int64(cap(event.Tags)) * convStringRetainedOverhead
		for _, tag := range event.Tags {
			retained += int64(len(tag))
		}
		retained += int64(cap(event.MergedBranchIDs)) * convStringRetainedOverhead
		for _, branchID := range event.MergedBranchIDs {
			retained += int64(len(branchID))
		}
	}
	return retained
}

func (c *convEncodeCache) evictLocked() {
	for len(c.m) > c.maxEntries || (c.maxBytes > 0 && c.totalBytes > c.maxBytes && len(c.m) > 1) {
		var victimKey string
		var victim *convEncodeEntry
		for key, entry := range c.m {
			if entry.inUse > 0 {
				continue
			}
			if victim == nil || entry.tick < victim.tick {
				victimKey, victim = key, entry
			}
		}
		if victim == nil {
			return
		}
		c.totalBytes -= victim.retainedBytes
		delete(c.m, victimKey)
	}
}

func entryHashesAt(f *os.File, readLen int64) (uint64, uint64, error) {
	head, err := readSampleAt(f, 0, min64(readLen, convHeadSampleBytes))
	if err != nil {
		return 0, 0, fmt.Errorf("codex: verify conversation head: %w", err)
	}
	tailStart := readLen - convTailSampleBytes
	if tailStart < 0 {
		tailStart = 0
	}
	tail, err := readSampleAt(f, tailStart, readLen-tailStart)
	if err != nil {
		return 0, 0, fmt.Errorf("codex: verify conversation tail: %w", err)
	}
	return fnv64a(head), fnv64a(tail), nil
}

func readSampleAt(f *os.File, offset, n int64) ([]byte, error) {
	if n <= 0 {
		return nil, nil
	}
	b := make([]byte, int(n))
	_, err := f.ReadAt(b, offset)
	if err != nil && err != io.EOF {
		return nil, err
	}
	return b, nil
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func convHeadHash(content []byte, prefixLen int64) uint64 {
	end := min64(prefixLen, convHeadSampleBytes)
	if end < 0 {
		end = 0
	}
	return fnv64a(content[:end])
}

func convTailHash(content []byte, prefixLen int64) uint64 {
	if prefixLen < 0 || prefixLen > int64(len(content)) {
		return 0
	}
	start := prefixLen - convTailSampleBytes
	if start < 0 {
		start = 0
	}
	return fnv64a(content[start:prefixLen])
}

func fnv64a(b []byte) uint64 {
	h := fnv.New64a()
	_, _ = h.Write(b)
	return h.Sum64()
}
