package idempotency

import (
	"bytes"
	"context"
	"sync"
	"time"
)

// MemoryStore is the in-memory store, for the in-memory backend.
//
// It is not a lesser implementation of the same guarantee — it is the same
// guarantee at the scope the memory backend has. That backend is one process
// with one map, so a mutex is exactly as atomic as a transaction is for
// Postgres, and there are no other replicas for a key to be shared with.
//
// It exists so that `go run ./cmd/taskapi all` — no Postgres, no Docker, no
// configuration, which is what the assignment asks to be possible — still
// honours Idempotency-Key rather than silently ignoring it. A header the
// server quietly drops is worse than one it rejects.
type MemoryStore struct {
	mu   sync.Mutex
	keys map[string]*memRecord
	now  func() time.Time
}

type memRecord struct {
	rec     Record
	expires time.Time
}

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{keys: map[string]*memRecord{}, now: time.Now}
}

// Reserve implements Store.
func (s *MemoryStore) Reserve(
	ctx context.Context, key string, fingerprint []byte, ttl time.Duration,
) (Unit, *Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if held, ok := s.keys[key]; ok && held.expires.After(s.now()) {
		rec := held.rec
		rec.Fingerprint = bytes.Clone(rec.Fingerprint)
		rec.Body = bytes.Clone(rec.Body)
		return nil, &rec, nil
	}

	s.keys[key] = &memRecord{
		rec:     Record{Fingerprint: bytes.Clone(fingerprint), State: StateInProgress},
		expires: s.now().Add(ttl),
	}
	return &memUnit{store: s, key: key, ctx: ctx}, nil, nil
}

// Purge implements Store.
func (s *MemoryStore) Purge(context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now, n := s.now(), 0
	for key, held := range s.keys {
		if !held.expires.After(now) {
			delete(s.keys, key)
			n++
		}
	}
	return n, nil
}

// memUnit is one reserved key in the map.
//
// There is no transaction to join, so Context returns the caller's unchanged
// and the repository — memrepo, which has no transactions either — behaves as
// it always does.
type memUnit struct {
	store *MemoryStore
	key   string
	ctx   context.Context
	done  bool
}

// Context implements Unit.
func (u *memUnit) Context() context.Context { return u.ctx }

// Complete implements Unit.
func (u *memUnit) Complete(status int, contentType string, body []byte) error {
	if u.done {
		return nil
	}
	u.done = true

	u.store.mu.Lock()
	defer u.store.mu.Unlock()

	if held, ok := u.store.keys[u.key]; ok {
		held.rec.State = StateCompleted
		held.rec.StatusCode = status
		held.rec.ContentType = contentType
		held.rec.Body = bytes.Clone(body)
	}
	return nil
}

// Abandon implements Unit.
func (u *memUnit) Abandon() {
	if u.done {
		return
	}
	u.done = true

	u.store.mu.Lock()
	defer u.store.mu.Unlock()
	delete(u.store.keys, u.key)
}
