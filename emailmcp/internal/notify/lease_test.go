package notify

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestMemoryLeaseAcquireAndFence(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	key := FolderKey("user", "default", "INBOX")

	ok, err := s.Acquire(ctx, key, "t1", time.Minute)
	if err != nil || !ok {
		t.Fatalf("t1 acquire: ok=%v err=%v", ok, err)
	}
	ok, err = s.Acquire(ctx, key, "t2", time.Minute)
	if err != nil {
		t.Fatalf("t2 acquire err: %v", err)
	}
	if ok {
		t.Fatal("t2 should not steal active lease")
	}
	ok, err = s.Heartbeat(ctx, key, "t2", time.Minute)
	if err != nil || ok {
		t.Fatalf("t2 heartbeat should fail: ok=%v err=%v", ok, err)
	}
	ok, err = s.Heartbeat(ctx, key, "t1", time.Minute)
	if err != nil || !ok {
		t.Fatalf("t1 heartbeat: ok=%v err=%v", ok, err)
	}
	if err := s.Release(ctx, key, "t1"); err != nil {
		t.Fatal(err)
	}
	ok, err = s.Acquire(ctx, key, "t2", time.Minute)
	if err != nil || !ok {
		t.Fatalf("t2 acquire after release: ok=%v err=%v", ok, err)
	}
}

func TestMemoryLeaseSnapshotAndCAS(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	key := FolderKey("user", "default", "INBOX")
	if _, err := s.Acquire(ctx, key, "t1", time.Minute); err != nil {
		t.Fatal(err)
	}
	changed, err := s.Snapshot(ctx, key, "t1", 9, 40)
	if err != nil || !changed {
		t.Fatalf("snapshot: changed=%v err=%v", changed, err)
	}
	changed, err = s.Snapshot(ctx, key, "t1", 9, 1)
	if err != nil || changed {
		t.Fatalf("same uidvalidity must not rewind: changed=%v err=%v", changed, err)
	}
	wm, err := s.GetWatermark(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if !wm.Set || wm.LastUID != 40 || wm.UIDValidity != 9 {
		t.Fatalf("watermark=%+v", wm)
	}

	ok, err := s.ClaimUID(ctx, key, "t1", 9, 41, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	ok, err = s.ClaimUID(ctx, key, "t2", 9, 41, time.Minute)
	if err != nil || ok {
		t.Fatalf("other owner must not claim: ok=%v err=%v", ok, err)
	}
	ok, err = s.AdvanceUID(ctx, key, "t2", 9, 41)
	if err != nil || ok {
		t.Fatalf("cas loser must not advance: ok=%v err=%v", ok, err)
	}
	ok, err = s.AdvanceUID(ctx, key, "t1", 9, 41)
	if err != nil || !ok {
		t.Fatalf("advance: ok=%v err=%v", ok, err)
	}
	wm, _ = s.GetWatermark(ctx, key)
	if wm.LastUID != 41 {
		t.Fatalf("last_uid=%d", wm.LastUID)
	}
}

func TestMemoryLeaseUIDValidityRollover(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	key := "k"
	_, _ = s.Acquire(ctx, key, "t1", time.Minute)
	_, _ = s.Snapshot(ctx, key, "t1", 1, 99)
	changed, err := s.Snapshot(ctx, key, "t1", 2, 5)
	if err != nil || !changed {
		t.Fatalf("rollover snapshot: changed=%v err=%v", changed, err)
	}
	wm, _ := s.GetWatermark(ctx, key)
	if wm.UIDValidity != 2 || wm.LastUID != 5 {
		t.Fatalf("watermark after rollover=%+v", wm)
	}
	ok, err := s.ClaimUID(ctx, key, "t1", 1, 100, time.Minute)
	if err != nil || ok {
		t.Fatalf("old uidvalidity claim: ok=%v err=%v", ok, err)
	}
}

func TestMemoryLeaseAdvanceRace(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	key := "race"
	_, _ = s.Acquire(ctx, key, "t1", time.Minute)
	_, _ = s.Snapshot(ctx, key, "t1", 1, 10)
	ok, err := s.ClaimUID(ctx, key, "t1", 1, 11, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}

	var wins int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := s.AdvanceUID(ctx, key, "t1", 1, 11)
			if err != nil {
				t.Errorf("advance: %v", err)
				return
			}
			if ok {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("expected exactly one cas win, got %d", wins)
	}
}

func TestMemoryLeaseSnapshotRequiresCurrentOwner(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	now := time.Now()
	s.now = func() time.Time { return now }
	key := "fenced-snapshot"

	ok, err := s.Acquire(ctx, key, "old-owner", time.Second)
	if err != nil || !ok {
		t.Fatalf("old owner acquire: ok=%v err=%v", ok, err)
	}
	if changed, err := s.Snapshot(ctx, key, "old-owner", 1, 10); err != nil || !changed {
		t.Fatalf("initial snapshot: changed=%v err=%v", changed, err)
	}
	if err := s.Release(ctx, key, "old-owner"); err != nil {
		t.Fatal(err)
	}
	ok, err = s.Acquire(ctx, key, "new-owner", time.Minute)
	if err != nil || !ok {
		t.Fatalf("new owner acquire: ok=%v err=%v", ok, err)
	}
	if ok, err := s.ClaimUID(ctx, key, "new-owner", 1, 11, time.Minute); err != nil || !ok {
		t.Fatalf("new owner claim: ok=%v err=%v", ok, err)
	}

	if changed, err := s.Snapshot(ctx, key, "old-owner", 2, 0); err != nil || changed {
		t.Fatalf("stale owner snapshot: changed=%v err=%v", changed, err)
	}
	wms, err := s.GetWatermark(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if wms.UIDValidity != 1 || wms.LastUID != 10 {
		t.Fatalf("stale snapshot changed watermark: %+v", wms)
	}
	if advanced, err := s.AdvanceUID(ctx, key, "new-owner", 1, 11); err != nil || !advanced {
		t.Fatalf("stale snapshot cleared current owner's claim: advanced=%v err=%v", advanced, err)
	}
}

func TestMemoryLeaseExpiredAllowsTakeover(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	now := time.Now()
	s.now = func() time.Time { return now }
	key := "k"
	ok, _ := s.Acquire(ctx, key, "t1", time.Second)
	if !ok {
		t.Fatal("acquire")
	}
	s.now = func() time.Time { return now.Add(2 * time.Second) }
	ok, err := s.Acquire(ctx, key, "t2", time.Minute)
	if err != nil || !ok {
		t.Fatalf("takeover: ok=%v err=%v", ok, err)
	}
}
