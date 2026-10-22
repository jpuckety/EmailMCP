package imap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jpuckett/EmailMCP/emailmcp/internal/types"
)

func testManager(t *testing.T) *Manager {
	t.Helper()
	return NewManager(Config{
		MaxConnsPerAccount: 2,
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func TestPoolKeyIsolatesTenants(t *testing.T) {
	a := PoolKey("user-a", "default")
	b := PoolKey("user-b", "default")
	if a == b {
		t.Fatal("pool keys for different owners with same account id must differ")
	}
	if PoolKey("user-a", "work") == PoolKey("user-a", "personal") {
		t.Fatal("pool keys for different account ids must differ")
	}
}

func TestGetOrCreatePool_RequiresOwner(t *testing.T) {
	m := testManager(t)
	_, err := m.getOrCreatePool(&types.Account{
		ID:           "default",
		IMAPHost:     "8.8.8.8",
		IMAPPassword: "secret",
		IMAPUseTLS:   true,
	})
	if err == nil {
		t.Fatal("expected error when OwnerUserID is empty")
	}
}

func TestGetOrCreatePool_BlocksPrivateHost(t *testing.T) {
	m := testManager(t)
	_, err := m.getOrCreatePool(&types.Account{
		ID:           "default",
		OwnerUserID:  "user-1",
		IMAPHost:     "10.0.0.5",
		IMAPPassword: "secret",
		IMAPUseTLS:   true,
	})
	if err == nil {
		t.Fatal("expected private host to be rejected")
	}
}

func TestGetOrCreatePool_RequiresTLSForRemote(t *testing.T) {
	m := testManager(t)
	_, err := m.getOrCreatePool(&types.Account{
		ID:           "default",
		OwnerUserID:  "user-1",
		IMAPHost:     "8.8.8.8",
		IMAPPassword: "secret",
		IMAPUseTLS:   false,
	})
	if err == nil {
		t.Fatal("expected non-TLS remote host to be rejected")
	}
}

func TestGetOrCreatePool_SeparatePoolsPerOwner(t *testing.T) {
	m := testManager(t)
	accA := &types.Account{
		ID:           "default",
		OwnerUserID:  "user-a",
		IMAPHost:     "8.8.8.8",
		IMAPPassword: "secret-a",
		IMAPUseTLS:   true,
	}
	accB := &types.Account{
		ID:           "default",
		OwnerUserID:  "user-b",
		IMAPHost:     "1.1.1.1",
		IMAPPassword: "secret-b",
		IMAPUseTLS:   true,
	}

	pA, err := m.getOrCreatePool(accA)
	if err != nil {
		t.Fatalf("pool A: %v", err)
	}
	pB, err := m.getOrCreatePool(accB)
	if err != nil {
		t.Fatalf("pool B: %v", err)
	}
	if pA == pB {
		t.Fatal("expected distinct pool instances for different owners")
	}
	if pA.imapCfg.Password == pB.imapCfg.Password {
		t.Fatal("pools must not share credentials across tenants")
	}

	// Same owner + account returns the same pool.
	pA2, err := m.getOrCreatePool(accA)
	if err != nil {
		t.Fatalf("pool A2: %v", err)
	}
	if pA != pA2 {
		t.Fatal("expected same pool for same owner+account")
	}
}

func TestDropPool_RemovesPool(t *testing.T) {
	m := testManager(t)
	acc := &types.Account{
		ID:           "default",
		OwnerUserID:  "user-a",
		IMAPHost:     "8.8.8.8",
		IMAPPassword: "secret",
		IMAPUseTLS:   true,
	}
	if _, err := m.getOrCreatePool(acc); err != nil {
		t.Fatalf("create: %v", err)
	}
	m.DropPool("user-a", "default")

	m.mu.Lock()
	_, exists := m.pools[PoolKey("user-a", "default")]
	m.mu.Unlock()
	if exists {
		t.Fatal("pool should be removed after DropPool")
	}

	// DropPool is idempotent.
	m.DropPool("user-a", "default")
}

func TestRelease_WithoutOwnerClosesConn(t *testing.T) {
	m := testManager(t)
	// Should not panic with nil/partial inputs.
	m.Release(nil, nil, false)
	m.Release(&types.Account{ID: "x"}, nil, false)
	_ = context.Background()
}

func TestIsTransientIMAP(t *testing.T) {
	if !isTransientIMAP(io.EOF) {
		t.Fatal("EOF should be transient")
	}
	if !isTransientIMAP(fmt.Errorf("select INBOX: unexpected EOF")) {
		t.Fatal("unexpected EOF should be transient")
	}
	if isTransientIMAP(errors.New("message not found")) {
		t.Fatal("not-found must not retry")
	}
}

func testAccount(id string) *types.Account {
	return &types.Account{
		ID:           id,
		OwnerUserID:  "user-1",
		IMAPHost:     "imap.example.com",
		IMAPPort:     993,
		IMAPUsername: "user",
		IMAPPassword: "secret",
		IMAPUseTLS:   true,
	}
}

func TestAcquire_ConcurrentDialsDoNotSerialize(t *testing.T) {
	m := NewManager(Config{
		MaxConnsPerAccount: 2,
		SkipHostValidation: true,
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	acc := testAccount("work")
	p, err := m.getOrCreatePool(acc)
	if err != nil {
		t.Fatal(err)
	}
	p.dial = func(ctx context.Context) (*pooledConn, error) {
		time.Sleep(200 * time.Millisecond)
		return &pooledConn{lastUsed: time.Now()}, nil
	}

	start := time.Now()
	errCh := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			c, err := m.Acquire(context.Background(), acc)
			if err != nil {
				errCh <- err
				return
			}
			m.Release(acc, c, true)
			errCh <- nil
		}()
	}
	for i := 0; i < 2; i++ {
		if err := <-errCh; err != nil {
			t.Fatal(err)
		}
	}
	elapsed := time.Since(start)
	if elapsed > 350*time.Millisecond {
		t.Fatalf("acquires serialized: elapsed %s (want ~200ms concurrent dials)", elapsed)
	}
}

func TestAcquire_WaitsForRelease(t *testing.T) {
	m := NewManager(Config{
		MaxConnsPerAccount: 1,
		SkipHostValidation: true,
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	acc := testAccount("work")
	p, err := m.getOrCreatePool(acc)
	if err != nil {
		t.Fatal(err)
	}
	p.dial = func(ctx context.Context) (*pooledConn, error) {
		return &pooledConn{lastUsed: time.Now()}, nil
	}

	c1, err := m.Acquire(context.Background(), acc)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		c2, err := m.Acquire(context.Background(), acc)
		if err != nil {
			done <- err
			return
		}
		m.Release(acc, c2, true)
		done <- nil
	}()

	select {
	case err := <-done:
		t.Fatalf("second acquire returned before release: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	m.Release(acc, c1, true)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second acquire did not wake after release")
	}
}

func TestGetOrCreatePool_ReplacesOnCredentialChange(t *testing.T) {
	m := testManager(t)
	m.cfg.SkipHostValidation = true
	acc := testAccount("work")
	p1, err := m.getOrCreatePool(acc)
	if err != nil {
		t.Fatal(err)
	}
	acc.IMAPPassword = "rotated"
	p2, err := m.getOrCreatePool(acc)
	if err != nil {
		t.Fatal(err)
	}
	if p1 == p2 {
		t.Fatal("expected a new pool after password rotation")
	}
}
