package workflows

import (
	"bytes"
	"context"
	"sync"
)

// Cache only the scalar task group from one fully decoded checkpoint. Every
// lookup still queries the current row and compares all stored bytes and fence
// columns. This preserves Load's format/version checks without repeatedly
// decoding large workflow results for each helper recovery hook.
type checkpointTaskGroupCache struct {
	mu         sync.Mutex
	valid      bool
	run        string
	generation int64
	owner      string
	record     []byte
	snapshot   []byte
	group      string
}

func (s *SQLiteStore) checkpointTaskGroup(ctx context.Context, run string) (string, error) {
	var record, snapshot []byte
	var generation int64
	var owner string
	if err := s.db.QueryRowContext(ctx, "SELECT record,snapshot,generation,owner FROM runs WHERE id=?", run).Scan(&record, &snapshot, &generation, &owner); err != nil {
		return "", err
	}
	cache := s.taskGroupCache
	cache.mu.Lock()
	if cache.valid && cache.run == run && cache.generation == generation && cache.owner == owner &&
		bytes.Equal(cache.record, record) && bytes.Equal(cache.snapshot, snapshot) {
		group := cache.group
		cache.mu.Unlock()
		return group, nil
	}
	cache.mu.Unlock()
	// Misses decode independently so different active runs do not serialize
	// large result parsing behind the cache lock.
	_, snap, err := decodeCheckpoint(record, snapshot, generation, owner)
	if err != nil {
		return "", err
	}
	group := nativeString(snap.Metadata, "task_group")
	// Keep at most one 4 MiB entry. Oversized checkpoints retain full validation
	// on every call without increasing the cache's memory bound.
	if len(record)+len(snapshot) <= 4<<20 {
		cache.mu.Lock()
		cache.valid = true
		cache.run, cache.generation, cache.owner = run, generation, owner
		cache.record, cache.snapshot, cache.group = record, snapshot, group
		cache.mu.Unlock()
	}
	return group, nil
}
