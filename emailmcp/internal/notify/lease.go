package notify

import (
	"context"
	"sync"
	"time"
)

const (
	DefaultLeaseTTL  = 90 * time.Second
	DefaultHeartbeat = 30 * time.Second
	defaultClaimTTL  = 90 * time.Second
)

// Watermark is the last successfully notified UID for a folder.
type Watermark struct {
	LastUID     uint32
	UIDValidity uint32
	Set         bool
}

// LeaseStore holds per-folder leases, UID watermarks, and delivery claims.
type LeaseStore interface {
	Acquire(ctx context.Context, key, owner string, ttl time.Duration) (bool, error)
	Heartbeat(ctx context.Context, key, owner string, ttl time.Duration) (bool, error)
	Release(ctx context.Context, key, owner string) error
	GetWatermark(ctx context.Context, key string) (Watermark, error)
	Snapshot(ctx context.Context, key, owner string, uidValidity, lastUID uint32) (bool, error)
	ClaimUID(ctx context.Context, key, owner string, uidValidity, uid uint32, ttl time.Duration) (bool, error)
	AdvanceUID(ctx context.Context, key, owner string, uidValidity, uid uint32) (bool, error)
	ClearClaim(ctx context.Context, key, owner string) error
}

// FolderKey is the NotifyState sort key: ownerUserId#accountId#folder.
func FolderKey(ownerUserID, accountID, folder string) string {
	return ownerUserID + "#" + accountID + "#" + folder
}

type memoryItem struct {
	lastUID      uint32
	uidValidity  uint32
	set          bool
	leaseOwner   string
	leaseUntil   time.Time
	claimedUID   uint32
	claimedOwner string
	claimedUntil time.Time
}

// MemoryStore is an in-memory LeaseStore for tests and local runs.
type MemoryStore struct {
	mu    sync.Mutex
	items map[string]*memoryItem
	now   func() time.Time
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		items: make(map[string]*memoryItem),
		now:   func() time.Time { return time.Now() },
	}
}

func (s *MemoryStore) getLocked(key string) *memoryItem {
	it, ok := s.items[key]
	if !ok {
		it = &memoryItem{}
		s.items[key] = it
	}
	return it
}

func (s *MemoryStore) Acquire(_ context.Context, key, owner string, ttl time.Duration) (bool, error) {
	if ttl <= 0 {
		ttl = DefaultLeaseTTL
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	it := s.getLocked(key)
	if it.leaseOwner != "" && it.leaseUntil.After(now) && it.leaseOwner != owner {
		return false, nil
	}
	it.leaseOwner = owner
	it.leaseUntil = now.Add(ttl)
	return true, nil
}

func (s *MemoryStore) Heartbeat(_ context.Context, key, owner string, ttl time.Duration) (bool, error) {
	if ttl <= 0 {
		ttl = DefaultLeaseTTL
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	it, ok := s.items[key]
	if !ok || it.leaseOwner != owner || !it.leaseUntil.After(now) {
		return false, nil
	}
	it.leaseUntil = now.Add(ttl)
	return true, nil
}

func (s *MemoryStore) Release(_ context.Context, key, owner string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, ok := s.items[key]
	if !ok || it.leaseOwner != owner {
		return nil
	}
	it.leaseOwner = ""
	it.leaseUntil = time.Time{}
	it.claimedUID = 0
	it.claimedOwner = ""
	it.claimedUntil = time.Time{}
	return nil
}

func (s *MemoryStore) GetWatermark(_ context.Context, key string) (Watermark, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, ok := s.items[key]
	if !ok || !it.set {
		return Watermark{}, nil
	}
	return Watermark{LastUID: it.lastUID, UIDValidity: it.uidValidity, Set: true}, nil
}

func (s *MemoryStore) Snapshot(_ context.Context, key, owner string, uidValidity, lastUID uint32) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it := s.getLocked(key)
	if !s.holdsLeaseLocked(it, owner, s.now()) {
		return false, nil
	}
	if it.set && it.uidValidity == uidValidity {
		return false, nil
	}
	it.uidValidity = uidValidity
	it.lastUID = lastUID
	it.set = true
	it.claimedUID = 0
	it.claimedOwner = ""
	it.claimedUntil = time.Time{}
	return true, nil
}

func (s *MemoryStore) holdsLeaseLocked(it *memoryItem, owner string, now time.Time) bool {
	return it.leaseOwner == owner && it.leaseUntil.After(now)
}

func (s *MemoryStore) ClaimUID(_ context.Context, key, owner string, uidValidity, uid uint32, ttl time.Duration) (bool, error) {
	if ttl <= 0 {
		ttl = defaultClaimTTL
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	it, ok := s.items[key]
	if !ok || !it.set || it.uidValidity != uidValidity || uid <= it.lastUID {
		return false, nil
	}
	if !s.holdsLeaseLocked(it, owner, now) {
		return false, nil
	}
	if it.claimedUID != 0 && it.claimedUntil.After(now) &&
		!(it.claimedOwner == owner && it.claimedUID == uid) {
		return false, nil
	}
	it.claimedUID = uid
	it.claimedOwner = owner
	it.claimedUntil = now.Add(ttl)
	return true, nil
}

func (s *MemoryStore) AdvanceUID(_ context.Context, key, owner string, uidValidity, uid uint32) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	it, ok := s.items[key]
	if !ok || !it.set || it.uidValidity != uidValidity || uid <= it.lastUID {
		return false, nil
	}
	if !s.holdsLeaseLocked(it, owner, now) {
		return false, nil
	}
	if it.claimedOwner != owner || it.claimedUID != uid {
		return false, nil
	}
	it.lastUID = uid
	it.claimedUID = 0
	it.claimedOwner = ""
	it.claimedUntil = time.Time{}
	return true, nil
}

func (s *MemoryStore) ClearClaim(_ context.Context, key, owner string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, ok := s.items[key]
	if !ok || it.claimedOwner != owner {
		return nil
	}
	it.claimedUID = 0
	it.claimedOwner = ""
	it.claimedUntil = time.Time{}
	return nil
}
