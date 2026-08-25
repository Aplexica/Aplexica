package syncd

import (
	"hash/fnv"
	"path/filepath"
	"sync"
)

// pathLockSet is a fixed pool of mutexes keyed by file path. It serializes
// work per path with bounded memory: no per-path map entries to grow or leak,
// at the cost of occasional false sharing when two distinct paths hash to the
// same shard (harmless — they briefly serialize).
//
// pathLockShardCount balances false-sharing odds against footprint: 256
// mutexes cost ~2KB and make two hot paths colliding on one shard unlikely.
const pathLockShardCount = 256

// The zero value is ready to use.
type pathLockSet struct {
	shards [pathLockShardCount]sync.Mutex
}

// lock acquires the shard mutex for path and returns the matching unlock.
// Paths are cleaned first so trivially different spellings of the same file
// (trailing slash, redundant separators) share a lock. Not reentrant: a
// goroutine must not call lock for the same (or colliding) path while it
// already holds it — handleEvent, the only user, never nests.
func (s *pathLockSet) lock(path string) func() {
	h := fnv.New32a()
	_, _ = h.Write([]byte(filepath.Clean(path)))
	m := &s.shards[h.Sum32()%uint32(len(s.shards))]
	m.Lock()
	return m.Unlock
}
