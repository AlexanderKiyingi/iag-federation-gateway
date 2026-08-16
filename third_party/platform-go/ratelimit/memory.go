package ratelimit

import (
	"context"
	"math"
	"sync"
	"time"
)

// defaultIdleTTL is how long an untouched bucket is kept before eviction. Any
// bucket idle this long has fully refilled, so dropping it is lossless. Keeps
// the map bounded (unlike a naive per-IP map that grows for the process
// lifetime).
const defaultIdleTTL = time.Hour

// sweepEvery bounds how often eviction scans run (every N Allow calls), so the
// sweep cost is amortised rather than per-request.
const sweepEvery = 4096

// MemoryStore is a process-local token-bucket Store. It is correct for a single
// instance; under horizontal scaling each replica keeps its own buckets, so the
// effective global limit is (rate × replicas) — inject a shared Store (e.g.
// Redis) when that matters.
type MemoryStore struct {
	mu      sync.Mutex
	buckets map[string]*memBucket
	idleTTL time.Duration
	ops     int
	now     func() time.Time // injectable for tests
}

type memBucket struct {
	tokens float64
	last   time.Time
}

// NewMemoryStore returns an in-memory store with the default idle TTL.
func NewMemoryStore() *MemoryStore { return NewMemoryStoreWithTTL(defaultIdleTTL) }

// NewMemoryStoreWithTTL returns an in-memory store that evicts buckets idle for
// longer than idleTTL (<= 0 uses the default).
func NewMemoryStoreWithTTL(idleTTL time.Duration) *MemoryStore {
	if idleTTL <= 0 {
		idleTTL = defaultIdleTTL
	}
	return &MemoryStore{
		buckets: make(map[string]*memBucket),
		idleTTL: idleTTL,
		now:     time.Now,
	}
}

// Allow implements Store using a token bucket: the bucket holds up to
// rate.Requests tokens and refills at rate.Requests/rate.Window per second.
func (s *MemoryStore) Allow(_ context.Context, key string, rate Rate) (bool, time.Duration, error) {
	if !rate.Valid() {
		return true, 0, nil
	}
	burst := float64(rate.Requests)
	perSec := burst / rate.Window.Seconds()

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	s.ops++
	if s.ops >= sweepEvery {
		s.evict(now)
		s.ops = 0
	}

	b := s.buckets[key]
	if b == nil {
		b = &memBucket{tokens: burst, last: now}
		s.buckets[key] = b
	} else {
		elapsed := now.Sub(b.last).Seconds()
		if elapsed > 0 {
			b.tokens = math.Min(burst, b.tokens+elapsed*perSec)
			b.last = now
		}
	}

	if b.tokens >= 1 {
		b.tokens--
		return true, 0, nil
	}
	// Time to accrue the fraction of a token still needed.
	deficit := 1 - b.tokens
	retry := time.Duration(deficit / perSec * float64(time.Second))
	return false, retry, nil
}

// evict drops buckets untouched for longer than idleTTL. Caller holds the lock.
func (s *MemoryStore) evict(now time.Time) {
	for k, b := range s.buckets {
		if now.Sub(b.last) > s.idleTTL {
			delete(s.buckets, k)
		}
	}
}

// Len reports the number of live buckets (for tests / introspection).
func (s *MemoryStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.buckets)
}
