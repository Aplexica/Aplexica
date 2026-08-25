package claudecode

import (
	"bytes"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"sync"

	"github.com/aplexica/aplexica/internal/acf"
)

// convEncodeCache makes canonical conversation encoding INCREMENTAL.
//
// Claude Code session.jsonl files are append-only logs that grow on every
// turn. Without a cache, each watcher settle re-parses the whole file from
// byte 0 (EncodeCanonical), which on a large, actively-written conversation
// dominates idle CPU. This cache remembers, per source path, the events
// already parsed and the byte offset where parsing stopped (resume point). On
// the next import it verifies the file only GREW with its already-parsed
// prefix intact (a head + tail hash of that prefix, O(1)), then parses only
// the freshly-appended tail and appends those events — producing exactly what
// a full EncodeCanonical would, at a fraction of the cost.
//
// A shrink or head/tail sample mismatch falls back to a full re-parse. The
// bounded samples are deliberately only a read/poll optimization: a same-size
// middle rewrite can evade them, so this cache is never write authority.
// Claude session mutation independently re-reads and compares the complete
// file plus inode immediately before its one append.
//
// Memory is bounded by a max-entry count and a max-bytes budget; least-
// recently-used entries are evicted (a future import of an evicted path simply
// full-parses again — cheap, since evicted paths are by definition not the
// actively-appended ones).

const (
	defaultConvCacheMaxEntries = 32
	defaultConvCacheMaxBytes   = 128 << 20 // 128 MiB of parsed-prefix bytes
	convHeadSampleBytes        = 4096      // hash window at the start of the prefix
	convTailSampleBytes        = 256       // hash window at the end of the prefix
)

type convEncodeEntry struct {
	mu        sync.Mutex
	prefixLen int64 // bytes already parsed = resume offset for the next append
	headHash  uint64
	tailHash  uint64
	state     claudeCanonicalState
	bytes     int64  // budget proxy: == prefixLen, mirrored under cache.mu
	tick      uint64 // LRU recency
}

// claudeFileInspection is a content-bounded planning snapshot. Events are the
// canonical projection already required for import; the remaining fields are
// content-free metadata used to select a candidate path and preserve native
// CWD. It never authorizes a write.
type claudeFileInspection struct {
	Events                 []acf.ConversationEvent
	SessionID              string
	LastCWD                string
	HasExplicitThreadStamp bool
	RowsComplete           bool
}

// encodeFile is the disk-streaming form of encode. Warm calls validate two
// tiny samples of the already-parsed prefix and read only bytes appended after
// prefixLen, avoiding a whole-file os.ReadFile for every watcher settle.
func (c *convEncodeCache) encodeFile(path string) ([]acf.ConversationEvent, error) {
	inspection, err := c.inspectFile(path)
	if err != nil {
		return nil, err
	}
	return inspection.Events, nil
}

// inspectFile is encodeFile plus the cached content-free metadata needed for
// native-source planning. A warm check reads only two bounded prefix samples
// and the newly-appended tail; materialization revalidates exact bytes before
// it reuses a native path, and native paths are never mutated.
func (c *convEncodeCache) inspectFile(path string) (claudeFileInspection, error) {
	c.mu.Lock()
	e := c.m[path]
	if e == nil {
		e = &convEncodeEntry{}
		c.m[path] = e
	}
	c.clock++
	e.tick = c.clock
	c.mu.Unlock()

	e.mu.Lock()
	defer e.mu.Unlock()
	f, err := os.Open(path)
	if err != nil {
		return claudeFileInspection{}, fmt.Errorf("claude-code: open conversation: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return claudeFileInspection{}, fmt.Errorf("claude-code: stat conversation: %w", err)
	}
	if !st.Mode().IsRegular() {
		return claudeFileInspection{}, fmt.Errorf("claude-code: conversation is not a regular file")
	}
	incremental := e.prefixLen > 0 && st.Size() >= e.prefixLen
	var fullReadBytes, tailReadBytes, probeReadBytes uint64
	rowsComplete := true
	if incremental {
		head, herr := readConvSampleAt(f, 0, minConv64(e.prefixLen, convHeadSampleBytes))
		tailStart := e.prefixLen - convTailSampleBytes
		if tailStart < 0 {
			tailStart = 0
		}
		tail, terr := readConvSampleAt(f, tailStart, e.prefixLen-tailStart)
		probeReadBytes += uint64(len(head) + len(tail))
		incremental = herr == nil && terr == nil && fnv64a(head) == e.headHash && fnv64a(tail) == e.tailHash
	}
	if incremental {
		appended, rerr := io.ReadAll(io.NewSectionReader(f, e.prefixLen, st.Size()-e.prefixLen))
		if rerr != nil {
			return claudeFileInspection{}, fmt.Errorf("claude-code: read conversation tail: %w", rerr)
		}
		state, consumed := encodeCanonicalInto(appended, 0, cloneClaudeCanonicalState(e.state))
		rowsComplete = len(bytes.TrimSpace(appended[consumed:])) == 0
		tailReadBytes += uint64(len(appended))
		e.state = state
		e.prefixLen += consumed
	} else {
		content, rerr := io.ReadAll(io.NewSectionReader(f, 0, st.Size()))
		if rerr != nil {
			return claudeFileInspection{}, fmt.Errorf("claude-code: read conversation: %w", rerr)
		}
		state, consumed := encodeCanonicalInto(content, 0, claudeCanonicalState{})
		rowsComplete = len(bytes.TrimSpace(content[consumed:])) == 0
		e.state, e.prefixLen = state, consumed
		fullReadBytes += uint64(len(content))
	}

	head, err := readConvSampleAt(f, 0, minConv64(e.prefixLen, convHeadSampleBytes))
	if err != nil {
		return claudeFileInspection{}, fmt.Errorf("claude-code: verify conversation head: %w", err)
	}
	tailStart := e.prefixLen - convTailSampleBytes
	if tailStart < 0 {
		tailStart = 0
	}
	tail, err := readConvSampleAt(f, tailStart, e.prefixLen-tailStart)
	if err != nil {
		return claudeFileInspection{}, fmt.Errorf("claude-code: verify conversation tail: %w", err)
	}
	e.headHash, e.tailHash = fnv64a(head), fnv64a(tail)
	probeReadBytes += uint64(len(head) + len(tail))
	out := append([]acf.ConversationEvent(nil), e.state.events...)
	newBytes := e.prefixLen
	inspection := claudeFileInspection{
		Events:                 out,
		SessionID:              e.state.sessionID,
		LastCWD:                e.state.lastCWD,
		HasExplicitThreadStamp: e.state.hasExplicitThreadStamp,
		RowsComplete:           rowsComplete,
	}

	c.mu.Lock()
	if incremental {
		c.incParses++
	} else {
		c.fullParses++
	}
	c.fullReadBytes += fullReadBytes
	c.tailReadBytes += tailReadBytes
	c.probeReadBytes += probeReadBytes
	// The entry can be evicted while its file is being read. Only charge the
	// result if this exact entry is still authoritative for path; otherwise a
	// later lookup has already installed a fresh cache entry.
	if c.m[path] == e {
		c.totalBytes += newBytes - e.bytes
		e.bytes = newBytes
	}
	c.evictLocked()
	c.mu.Unlock()
	return inspection, nil
}

func readConvSampleAt(f *os.File, offset, n int64) ([]byte, error) {
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

func minConv64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

type convEncodeCache struct {
	mu         sync.Mutex
	m          map[string]*convEncodeEntry
	maxEntries int
	maxBytes   int64
	totalBytes int64
	clock      uint64

	// stats (observability + tests)
	fullParses uint64
	incParses  uint64

	fullReadBytes  uint64
	tailReadBytes  uint64
	probeReadBytes uint64
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

// encode returns the full canonical event list for content at path, parsing
// only the appended tail when content is a verified append of what was parsed
// before. The returned slice is a stable snapshot safe to read after return.
func (c *convEncodeCache) encode(path string, content []byte) []acf.ConversationEvent {
	events, _ := c.encodeChecked(path, content)
	return events
}

func (c *convEncodeCache) encodeChecked(path string, content []byte) ([]acf.ConversationEvent, error) {
	c.mu.Lock()
	e := c.m[path]
	if e == nil {
		e = &convEncodeEntry{}
		c.m[path] = e
	}
	c.clock++
	e.tick = c.clock
	c.mu.Unlock()

	e.mu.Lock()
	n := int64(len(content))
	incremental := e.prefixLen > 0 &&
		n >= e.prefixLen &&
		convHeadHash(content, e.prefixLen) == e.headHash &&
		convTailHash(content, e.prefixLen) == e.tailHash

	if incremental {
		state, resume := encodeCanonicalInto(content, e.prefixLen, cloneClaudeCanonicalState(e.state))
		e.state = state
		e.prefixLen = resume
	} else {
		state, resume := encodeCanonicalInto(content, 0, claudeCanonicalState{})
		e.state = state
		e.prefixLen = resume
	}
	e.headHash = convHeadHash(content, e.prefixLen)
	e.tailHash = convTailHash(content, e.prefixLen)
	out := append([]acf.ConversationEvent(nil), e.state.events...) // stable header snapshot under e.mu
	newBytes := e.prefixLen
	c.mu.Lock()
	if incremental {
		c.incParses++
	} else {
		c.fullParses++
	}
	// Keep e.mu through the accounting update so two concurrent encodes of the
	// same path cannot apply their byte deltas out of order. As above, an entry
	// evicted during parsing is no longer charged to the live cache.
	if c.m[path] == e {
		c.totalBytes += newBytes - e.bytes
		e.bytes = newBytes
	}
	c.evictLocked()
	c.mu.Unlock()
	e.mu.Unlock()

	return out, nil
}

// peek reports whether path is currently cached (test/observability helper).
func (c *convEncodeCache) peek(path string) (*convEncodeEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[path]
	return e, ok
}

// evictLocked drops least-recently-used entries until both the entry-count and
// byte budgets are satisfied. The just-touched entry has the highest tick, so
// it is never the victim. A single entry larger than maxBytes is kept (we
// never evict below one entry on the byte budget) — that is the actively-
// appended hot file we most want cached. Caller holds c.mu.
func (c *convEncodeCache) evictLocked() {
	for len(c.m) > c.maxEntries ||
		(c.maxBytes > 0 && c.totalBytes > c.maxBytes && len(c.m) > 1) {
		var victimKey string
		var victim *convEncodeEntry
		for k, e := range c.m {
			if victim == nil || e.tick < victim.tick {
				victim, victimKey = e, k
			}
		}
		if victim == nil {
			break
		}
		c.totalBytes -= victim.bytes
		delete(c.m, victimKey)
	}
}

// convHeadHash hashes up to convHeadSampleBytes at the START of the parsed
// prefix [0:prefixLen). An append leaves these bytes untouched; a head rewrite
// (compaction) changes them.
func convHeadHash(content []byte, prefixLen int64) uint64 {
	end := prefixLen
	if end > convHeadSampleBytes {
		end = convHeadSampleBytes
	}
	if end < 0 {
		end = 0
	}
	return fnv64a(content[:end])
}

// convTailHash hashes up to convTailSampleBytes at the END of the parsed
// prefix [0:prefixLen). Together with the head hash this catches an in-place
// rewrite that happens to preserve the first bytes but changes content near
// the resume point.
func convTailHash(content []byte, prefixLen int64) uint64 {
	start := prefixLen - convTailSampleBytes
	if start < 0 {
		start = 0
	}
	if prefixLen < 0 || prefixLen > int64(len(content)) {
		return 0
	}
	return fnv64a(content[start:prefixLen])
}

func fnv64a(b []byte) uint64 {
	h := fnv.New64a()
	_, _ = h.Write(b)
	return h.Sum64()
}
