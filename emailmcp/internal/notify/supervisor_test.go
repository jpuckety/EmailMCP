package notify

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	imappkg "github.com/jpuckett/EmailMCP/emailmcp/internal/imap"
	"github.com/jpuckett/EmailMCP/emailmcp/internal/types"
)

type fakeAccounts struct {
	mu   sync.Mutex
	accs []*types.Account
}

func (f *fakeAccounts) ListAllConfigs(context.Context) ([]*types.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*types.Account, len(f.accs))
	for i, a := range f.accs {
		out[i] = cloneWatchAccount(a)
	}
	return out, nil
}

func (f *fakeAccounts) set(accs ...*types.Account) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accs = make([]*types.Account, len(accs))
	for i, a := range accs {
		f.accs[i] = cloneWatchAccount(a)
	}
}

type fakeSession struct {
	mu          sync.Mutex
	snap        imappkg.SelectSnapshot
	msgs        []types.EmailSummary
	idle        chan struct{}
	idleErr     error
	refreshIdle bool
	selected    []string
	fetches     int
}

type messageLogHandler struct {
	messages chan string
}

func (h *messageLogHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *messageLogHandler) Handle(_ context.Context, record slog.Record) error {
	select {
	case h.messages <- record.Message:
	default:
	}
	return nil
}

func (h *messageLogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *messageLogHandler) WithGroup(string) slog.Handler { return h }

func (f *fakeSession) Select(_ context.Context, folder string) (imappkg.SelectSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.selected = append(f.selected, folder)
	s := f.snap
	s.Folder = folder
	return s, nil
}

func (f *fakeSession) SupportsIdle() bool { return true }

func (f *fakeSession) Idle(ctx context.Context, _ time.Duration) (bool, error) {
	select {
	case <-f.idle:
		f.mu.Lock()
		err := f.idleErr
		refresh := f.refreshIdle
		f.mu.Unlock()
		return refresh, err
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

func (f *fakeSession) FetchAfter(_ context.Context, lastUID uint32) ([]types.EmailSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetches++
	var out []types.EmailSummary
	for _, m := range f.msgs {
		if m.UID > lastUID {
			out = append(out, m)
		}
	}
	return out, nil
}

func (f *fakeSession) Close() error { return nil }

func (f *fakeSession) ForceClose() {}

func (f *fakeSession) add(msg types.EmailSummary) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.msgs = append(f.msgs, msg)
}

func (f *fakeSession) selectCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.selected)
}

func (f *fakeSession) fetchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fetches
}

func testAccount(url string) *types.Account {
	return &types.Account{
		ID:           "default",
		OwnerUserID:  "user-1",
		IMAPPassword: "secret",
		WebhookURL:   url,
		WebhookToken: "tok",
	}
}

func TestSupervisorNormalizesAndDeduplicatesInbox(t *testing.T) {
	sup := NewSupervisor(Config{MaxFolders: 8})
	got := sup.watchFolders(context.Background(), &types.Account{
		WatchFolders: []string{"inbox", "INBOX", " Sent ", "Sent"},
	})
	want := []string{"INBOX", "Sent"}
	if len(got) != len(want) {
		t.Fatalf("folders=%q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("folders=%q, want %q", got, want)
		}
	}
}

func waitUntil(t *testing.T, timeout time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timeout waiting for condition")
}

func signalIdle(t *testing.T, idle chan struct{}) {
	t.Helper()
	select {
	case idle <- struct{}{}:
	case <-time.After(time.Second):
		t.Fatal("idle not running")
	}
}

func waitForLog(t *testing.T, logs <-chan string, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case message := <-logs:
			if message == want {
				return
			}
		case <-deadline:
			t.Fatalf("timeout waiting for log %q", want)
		}
	}
}

type recordingLeases struct {
	*MemoryStore
	mu       sync.Mutex
	released []string
}

func (r *recordingLeases) Release(ctx context.Context, key, owner string) error {
	r.mu.Lock()
	r.released = append(r.released, key)
	r.mu.Unlock()
	return r.MemoryStore.Release(ctx, key, owner)
}

func (r *recordingLeases) releasedKeys() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.released...)
}

type hbWrap struct {
	LeaseStore
	fail atomic.Bool
	err  atomic.Bool
}

func (h *hbWrap) Heartbeat(ctx context.Context, key, owner string, ttl time.Duration) (bool, error) {
	if h.err.Load() {
		return false, errors.New("dynamodb timeout")
	}
	if h.fail.Load() {
		return false, nil
	}
	return h.LeaseStore.Heartbeat(ctx, key, owner, ttl)
}

type denyClaim struct {
	LeaseStore
}

func (d *denyClaim) ClaimUID(context.Context, string, string, uint32, uint32, time.Duration) (bool, error) {
	return false, nil
}

func TestSupervisorSnapshotThenPostAndStop(t *testing.T) {
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sess := &fakeSession{
		snap: imappkg.SelectSnapshot{UIDNext: 50, UIDValidity: 7},
		idle: make(chan struct{}),
	}
	var dials atomic.Int32
	accs := &fakeAccounts{}
	acc := testAccount(srv.URL)
	accs.set(acc)
	leases := NewMemoryStore()
	key := FolderKey(acc.OwnerUserID, acc.ID, "INBOX")

	sup := NewSupervisor(Config{
		Logger:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		OwnerID:             "task-1",
		Accounts:            accs,
		Leases:              leases,
		Dispatcher:          testDispatcher(srv.Client()),
		SkipWebhookValidate: true,
		ReconcileInterval:   40 * time.Millisecond,
		HeartbeatInterval:   time.Hour,
		LeaseTTL:            time.Minute,
		FindSent:            func(context.Context, *types.Account) (string, error) { return "", nil },
		Dial: func(ctx context.Context, a *types.Account) (Session, error) {
			dials.Add(1)
			return sess, nil
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = sup.Run(ctx)
	}()

	waitUntil(t, 2*time.Second, func() bool {
		wm, _ := leases.GetWatermark(context.Background(), key)
		return wm.Set && wm.LastUID == 49 && wm.UIDValidity == 7 && posts.Load() == 0
	})
	if posts.Load() != 0 {
		t.Fatalf("snapshot must not notify existing mail, posts=%d", posts.Load())
	}

	sess.add(types.EmailSummary{UID: 50, Subject: "new"})
	signalIdle(t, sess.idle)
	waitUntil(t, 2*time.Second, func() bool {
		wm, _ := leases.GetWatermark(context.Background(), key)
		return posts.Load() == 1 && wm.LastUID == 50
	})

	accs.set() // webhook/account gone
	waitUntil(t, 2*time.Second, func() bool { return sup.WorkerCount() == 0 })
	cancel()
	<-done
}

func TestSupervisorLogsIdleNewMessageAndWebhook(t *testing.T) {
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	logs := make(chan string, 64)
	logger := slog.New(&messageLogHandler{messages: logs})
	sess := &fakeSession{
		snap: imappkg.SelectSnapshot{UIDNext: 50, UIDValidity: 7},
		idle: make(chan struct{}),
	}
	accs := &fakeAccounts{}
	acc := testAccount(srv.URL)
	accs.set(acc)
	leases := NewMemoryStore()
	key := FolderKey(acc.OwnerUserID, acc.ID, "INBOX")

	sup := NewSupervisor(Config{
		Logger:   logger,
		OwnerID:  "task-logs",
		Accounts: accs,
		Leases:   leases,
		Dispatcher: NewDispatcher(DispatcherConfig{
			Client:             srv.Client(),
			Logger:             logger,
			SkipHostValidation: true,
			MaxAttempts:        1,
			Backoff:            time.Millisecond,
		}),
		SkipWebhookValidate: true,
		ReconcileInterval:   40 * time.Millisecond,
		HeartbeatInterval:   time.Hour,
		LeaseTTL:            time.Minute,
		FindSent:            func(context.Context, *types.Account) (string, error) { return "", nil },
		Dial: func(context.Context, *types.Account) (Session, error) {
			return sess, nil
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = sup.Run(ctx)
	}()

	waitForLog(t, logs, "IMAP IDLE monitoring mailbox", 2*time.Second)
	sess.add(types.EmailSummary{
		UID:     50,
		Subject: "hello",
		From:    []types.Address{{Address: "a@example.com"}},
	})
	signalIdle(t, sess.idle)
	waitForLog(t, logs, "new mailbox message", 2*time.Second)
	waitForLog(t, logs, "webhook delivered", 2*time.Second)
	waitUntil(t, 2*time.Second, func() bool {
		wm, _ := leases.GetWatermark(context.Background(), key)
		return posts.Load() == 1 && wm.LastUID == 50
	})

	cancel()
	<-done
}

func TestSupervisorLogsIdleRefresh(t *testing.T) {
	logs := make(chan string, 64)
	logger := slog.New(&messageLogHandler{messages: logs})
	sess := &fakeSession{
		snap:        imappkg.SelectSnapshot{UIDNext: 10, UIDValidity: 1},
		idle:        make(chan struct{}),
		refreshIdle: true,
	}
	accs := &fakeAccounts{}
	accs.set(testAccount("https://hooks.example.com/mail"))
	sup := NewSupervisor(Config{
		Logger:              logger,
		OwnerID:             "task-idle-refresh",
		Accounts:            accs,
		Leases:              NewMemoryStore(),
		Dispatcher:          testDispatcher(http.DefaultClient),
		SkipWebhookValidate: true,
		ReconcileInterval:   40 * time.Millisecond,
		HeartbeatInterval:   time.Hour,
		LeaseTTL:            time.Minute,
		FindSent:            func(context.Context, *types.Account) (string, error) { return "", nil },
		Dial: func(context.Context, *types.Account) (Session, error) {
			return sess, nil
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = sup.Run(ctx)
	}()

	waitForLog(t, logs, "IMAP IDLE monitoring mailbox", 2*time.Second)
	signalIdle(t, sess.idle)
	waitForLog(t, logs, "IMAP IDLE refreshed", 2*time.Second)

	cancel()
	<-done
}

func TestSupervisorLogsIneligibleAccountGates(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*types.Account)
		reason string
	}{
		{
			name:   "missing webhook",
			mutate: func(a *types.Account) { a.WebhookURL = "" },
			reason: "missing webhook_url",
		},
		{
			name:   "missing imap password",
			mutate: func(a *types.Account) { a.IMAPPassword = "" },
			reason: "missing imap_password",
		},
		{
			name:   "missing owner",
			mutate: func(a *types.Account) { a.OwnerUserID = "" },
			reason: "missing owner or account id",
		},
		{
			name:   "missing account id",
			mutate: func(a *types.Account) { a.ID = "" },
			reason: "missing owner or account id",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			acc := testAccount("https://hooks.example.com/mail")
			tt.mutate(acc)
			accs := &fakeAccounts{}
			accs.set(acc)
			sup := NewSupervisor(Config{
				Logger:              slog.New(slog.NewJSONHandler(&buf, nil)),
				OwnerID:             "task-skip-gates",
				Accounts:            accs,
				Leases:              NewMemoryStore(),
				SkipWebhookValidate: true,
			})
			sup.reconcile(context.Background())
			got := buf.String()
			if !strings.Contains(got, `"msg":"skipping account"`) {
				t.Fatalf("missing skipping account log: %s", got)
			}
			if !strings.Contains(got, `"reason":"`+tt.reason+`"`) {
				t.Fatalf("missing reason %q in log: %s", tt.reason, got)
			}
			if strings.Contains(got, `"msg":"notify worker started"`) {
				t.Fatalf("worker started for ineligible account: %s", got)
			}
		})
	}
}

func TestSupervisorLogsInvalidWebhookGate(t *testing.T) {
	var buf bytes.Buffer
	accs := &fakeAccounts{}
	accs.set(testAccount("http://hooks.example.com/mail"))
	sup := NewSupervisor(Config{
		Logger:   slog.New(slog.NewJSONHandler(&buf, nil)),
		OwnerID:  "task-invalid-webhook",
		Accounts: accs,
		Leases:   NewMemoryStore(),
	})
	sup.reconcile(context.Background())
	got := buf.String()
	if !strings.Contains(got, `"msg":"skipping account with invalid webhook"`) {
		t.Fatalf("missing invalid webhook log: %s", got)
	}
	if strings.Contains(got, `"msg":"notify worker started"`) {
		t.Fatalf("worker started for invalid webhook: %s", got)
	}
}

func TestSupervisorLogsWatchLeaseAndWorkerGates(t *testing.T) {
	acc := testAccount("https://hooks.example.com/mail")
	acc.WatchFolders = []string{"INBOX"}
	key := FolderKey(acc.OwnerUserID, acc.ID, "INBOX")

	t.Run("watching folders and worker started", func(t *testing.T) {
		var buf bytes.Buffer
		accs := &fakeAccounts{}
		accs.set(acc)
		idle := make(chan struct{})
		sup := NewSupervisor(Config{
			Logger:              slog.New(slog.NewJSONHandler(&buf, nil)),
			OwnerID:             "task-watch-gates",
			Accounts:            accs,
			Leases:              NewMemoryStore(),
			Dispatcher:          testDispatcher(http.DefaultClient),
			SkipWebhookValidate: true,
			HeartbeatInterval:   time.Hour,
			FindSent:            func(context.Context, *types.Account) (string, error) { return "", nil },
			Dial: func(context.Context, *types.Account) (Session, error) {
				return &fakeSession{
					snap: imappkg.SelectSnapshot{UIDNext: 1, UIDValidity: 1},
					idle: idle,
				}, nil
			},
		})
		ctx, cancel := context.WithCancel(context.Background())
		sup.reconcile(ctx)
		got := buf.String()
		cancel()
		sup.stopAll()
		for _, want := range []string{
			`"msg":"notify watching folders"`,
			`"folders":"INBOX"`,
			`"msg":"notify worker started"`,
			`"folder":"INBOX"`,
			`"account_id":"default"`,
		} {
			if !strings.Contains(got, want) {
				t.Fatalf("missing %s in log: %s", want, got)
			}
		}
	})

	t.Run("lease not acquired", func(t *testing.T) {
		var buf bytes.Buffer
		accs := &fakeAccounts{}
		accs.set(acc)
		leases := NewMemoryStore()
		ok, err := leases.Acquire(context.Background(), key, "other-owner", time.Minute)
		if err != nil || !ok {
			t.Fatalf("pre-acquire lease: ok=%v err=%v", ok, err)
		}
		sup := NewSupervisor(Config{
			Logger:              slog.New(slog.NewJSONHandler(&buf, nil)),
			OwnerID:             "task-lease-held",
			Accounts:            accs,
			Leases:              leases,
			SkipWebhookValidate: true,
		})
		sup.reconcile(context.Background())
		got := buf.String()
		if !strings.Contains(got, `"msg":"lease not acquired; folder already watched"`) {
			t.Fatalf("missing lease log: %s", got)
		}
		if !strings.Contains(got, `"folder":"INBOX"`) {
			t.Fatalf("missing folder in lease log: %s", got)
		}
		if strings.Contains(got, `"msg":"notify worker started"`) {
			t.Fatalf("worker started without lease: %s", got)
		}
	})
}

func TestSupervisorDefaultFoldersIncludeSent(t *testing.T) {
	sup := NewSupervisor(Config{
		FindSent: func(context.Context, *types.Account) (string, error) { return "Sent", nil },
	})
	acc := testAccount("https://example.com")
	got := sup.watchFolders(context.Background(), acc)
	if len(got) != 2 || got[0] != "INBOX" || got[1] != "Sent" {
		t.Fatalf("folders=%v", got)
	}

	acc.WatchFolders = []string{"INBOX", "Projects"}
	got = sup.watchFolders(context.Background(), acc)
	if len(got) != 2 || got[1] != "Projects" {
		t.Fatalf("explicit folders=%v", got)
	}
}

func TestSupervisorFolderCap(t *testing.T) {
	sup := NewSupervisor(Config{MaxFolders: 8})
	acc := testAccount("https://example.com")
	acc.WatchFolders = []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}
	got := sup.watchFolders(context.Background(), acc)
	if len(got) != 8 {
		t.Fatalf("cap=%d folders=%v", len(got), got)
	}
}

func TestSupervisorFindSentErrorWatchesInbox(t *testing.T) {
	accs := &fakeAccounts{}
	acc := testAccount("https://8.8.8.8/hook")
	accs.set(acc)
	idle := make(chan struct{})
	var dials atomic.Int32
	sup := NewSupervisor(Config{
		OwnerID:             "task-1",
		Accounts:            accs,
		Leases:              NewMemoryStore(),
		Dispatcher:          NewDispatcher(DispatcherConfig{SkipHostValidation: true}),
		SkipWebhookValidate: true,
		ReconcileInterval:   30 * time.Millisecond,
		HeartbeatInterval:   time.Hour,
		FindSent:            func(context.Context, *types.Account) (string, error) { return "", errors.New("no sent") },
		Dial: func(ctx context.Context, a *types.Account) (Session, error) {
			dials.Add(1)
			return &fakeSession{
				snap: imappkg.SelectSnapshot{UIDNext: 1, UIDValidity: 1},
				idle: idle,
			}, nil
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.Run(ctx)
	waitUntil(t, 2*time.Second, func() bool { return sup.WorkerCount() == 1 })
	if dials.Load() != 1 {
		t.Fatalf("expected one INBOX dial, got %d", dials.Load())
	}
}

func TestSupervisorSentCacheKeepsWorkerOnTransientFindSentError(t *testing.T) {
	accs := &fakeAccounts{}
	acc := testAccount("https://8.8.8.8/hook")
	accs.set(acc)
	idle := make(chan struct{})
	var failSent atomic.Bool
	sup := NewSupervisor(Config{
		OwnerID:             "task-1",
		Accounts:            accs,
		Leases:              NewMemoryStore(),
		Dispatcher:          NewDispatcher(DispatcherConfig{SkipHostValidation: true}),
		SkipWebhookValidate: true,
		ReconcileInterval:   25 * time.Millisecond,
		HeartbeatInterval:   time.Hour,
		FindSent: func(context.Context, *types.Account) (string, error) {
			if failSent.Load() {
				return "", errors.New("temp")
			}
			return "Sent", nil
		},
		Dial: func(ctx context.Context, a *types.Account) (Session, error) {
			return &fakeSession{
				snap: imappkg.SelectSnapshot{UIDNext: 1, UIDValidity: 1},
				idle: idle,
			}, nil
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.Run(ctx)
	waitUntil(t, 2*time.Second, func() bool { return sup.WorkerCount() == 2 })
	failSent.Store(true)
	time.Sleep(80 * time.Millisecond)
	if sup.WorkerCount() != 2 {
		t.Fatalf("transient FindSent error dropped Sent worker, count=%d", sup.WorkerCount())
	}
}

func TestSupervisorNonHolderDoesNotDial(t *testing.T) {
	accs := &fakeAccounts{}
	acc := testAccount("https://8.8.8.8/hook")
	accs.set(acc)
	leases := NewMemoryStore()
	idle := make(chan struct{})
	var dials1, dials2 atomic.Int32
	dialer := func(counter *atomic.Int32) DialFunc {
		return func(ctx context.Context, a *types.Account) (Session, error) {
			counter.Add(1)
			return &fakeSession{
				snap: imappkg.SelectSnapshot{UIDNext: 1, UIDValidity: 1},
				idle: idle,
			}, nil
		}
	}
	cfg := Config{
		Accounts:            accs,
		Leases:              leases,
		Dispatcher:          NewDispatcher(DispatcherConfig{SkipHostValidation: true}),
		SkipWebhookValidate: true,
		ReconcileInterval:   30 * time.Millisecond,
		HeartbeatInterval:   time.Hour,
		LeaseTTL:            time.Minute,
		FindSent:            func(context.Context, *types.Account) (string, error) { return "", nil },
	}
	cfg1 := cfg
	cfg1.OwnerID = "t1"
	cfg1.Dial = dialer(&dials1)
	cfg2 := cfg
	cfg2.OwnerID = "t2"
	cfg2.Dial = dialer(&dials2)
	s1 := NewSupervisor(cfg1)
	s2 := NewSupervisor(cfg2)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s1.Run(ctx)
	waitUntil(t, 2*time.Second, func() bool { return s1.WorkerCount() == 1 })
	go s2.Run(ctx)
	time.Sleep(120 * time.Millisecond)
	if s2.WorkerCount() != 0 {
		t.Fatalf("non-holder workers=%d", s2.WorkerCount())
	}
	if dials2.Load() != 0 {
		t.Fatalf("non-holder dialed %d times", dials2.Load())
	}
	if dials1.Load() == 0 {
		t.Fatal("holder never dialed")
	}
	cancel()
}

func TestSupervisorUIDValidityChangeDoesNotReplay(t *testing.T) {
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	sess := &fakeSession{
		snap: imappkg.SelectSnapshot{UIDNext: 20, UIDValidity: 1},
		idle: make(chan struct{}),
		msgs: []types.EmailSummary{{UID: 19, Subject: "old"}},
	}
	accs := &fakeAccounts{}
	acc := testAccount(srv.URL)
	accs.set(acc)
	leases := NewMemoryStore()
	key := FolderKey(acc.OwnerUserID, acc.ID, "INBOX")

	sup := NewSupervisor(Config{
		OwnerID:             "task-1",
		Accounts:            accs,
		Leases:              leases,
		Dispatcher:          testDispatcher(srv.Client()),
		SkipWebhookValidate: true,
		ReconcileInterval:   time.Hour,
		HeartbeatInterval:   time.Hour,
		FindSent:            func(context.Context, *types.Account) (string, error) { return "", nil },
		Dial: func(ctx context.Context, a *types.Account) (Session, error) {
			return sess, nil
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.Run(ctx)
	waitUntil(t, 2*time.Second, func() bool {
		wm, _ := leases.GetWatermark(context.Background(), key)
		return wm.Set && wm.LastUID == 19 && wm.UIDValidity == 1
	})
	if posts.Load() != 0 {
		t.Fatalf("posts=%d", posts.Load())
	}

	sess.mu.Lock()
	sess.snap = imappkg.SelectSnapshot{UIDNext: 20, UIDValidity: 2}
	sess.idleErr = errors.New("disconnect")
	sess.mu.Unlock()
	signalIdle(t, sess.idle)
	waitUntil(t, 2*time.Second, func() bool {
		wm, _ := leases.GetWatermark(context.Background(), key)
		return wm.UIDValidity == 2 && wm.LastUID == 19 && sess.selectCount() >= 2
	})
	if posts.Load() != 0 {
		t.Fatalf("uidvalidity change must not replay, posts=%d", posts.Load())
	}
	cancel()
}

func TestSupervisorWebhookRetryThenAdvance(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sess := &fakeSession{
		snap: imappkg.SelectSnapshot{UIDNext: 50, UIDValidity: 1},
		idle: make(chan struct{}),
	}
	accs := &fakeAccounts{}
	acc := testAccount(srv.URL)
	accs.set(acc)
	leases := NewMemoryStore()
	key := FolderKey(acc.OwnerUserID, acc.ID, "INBOX")
	sup := NewSupervisor(Config{
		OwnerID:             "task-1",
		Accounts:            accs,
		Leases:              leases,
		Dispatcher:          testDispatcher(srv.Client()),
		SkipWebhookValidate: true,
		ReconcileInterval:   time.Hour,
		HeartbeatInterval:   time.Hour,
		FindSent:            func(context.Context, *types.Account) (string, error) { return "", nil },
		Dial:                func(context.Context, *types.Account) (Session, error) { return sess, nil },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.Run(ctx)
	waitUntil(t, 2*time.Second, func() bool {
		wm, _ := leases.GetWatermark(context.Background(), key)
		return wm.Set && wm.LastUID == 49
	})
	sess.add(types.EmailSummary{UID: 50, Subject: "new"})
	signalIdle(t, sess.idle)
	waitUntil(t, 2*time.Second, func() bool {
		wm, _ := leases.GetWatermark(context.Background(), key)
		return wm.LastUID == 50 && n.Load() >= 2
	})
}

func TestSupervisorWebhook400DoesNotAdvance(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	sess := &fakeSession{
		snap: imappkg.SelectSnapshot{UIDNext: 50, UIDValidity: 1},
		idle: make(chan struct{}),
	}
	accs := &fakeAccounts{}
	acc := testAccount(srv.URL)
	accs.set(acc)
	leases := NewMemoryStore()
	key := FolderKey(acc.OwnerUserID, acc.ID, "INBOX")
	sup := NewSupervisor(Config{
		OwnerID:             "task-1",
		Accounts:            accs,
		Leases:              leases,
		Dispatcher:          testDispatcher(srv.Client()),
		SkipWebhookValidate: true,
		ReconcileInterval:   time.Hour,
		HeartbeatInterval:   time.Hour,
		PermanentBackoff:    20 * time.Millisecond,
		FindSent:            func(context.Context, *types.Account) (string, error) { return "", nil },
		Dial:                func(context.Context, *types.Account) (Session, error) { return sess, nil },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.Run(ctx)
	waitUntil(t, 2*time.Second, func() bool {
		wm, _ := leases.GetWatermark(context.Background(), key)
		return wm.Set && wm.LastUID == 49
	})
	sess.add(types.EmailSummary{UID: 50, Subject: "new"})
	signalIdle(t, sess.idle)
	waitUntil(t, 2*time.Second, func() bool { return n.Load() >= 1 })
	wm, _ := leases.GetWatermark(context.Background(), key)
	if wm.LastUID != 49 {
		t.Fatalf("4xx must not advance watermark, last=%d", wm.LastUID)
	}
	if sup.WorkerCount() != 1 {
		t.Fatalf("worker should stay up and back off, count=%d", sup.WorkerCount())
	}
}

func TestSupervisorClaimLoserDoesNotPost(t *testing.T) {
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sess := &fakeSession{
		snap: imappkg.SelectSnapshot{UIDNext: 50, UIDValidity: 1},
		idle: make(chan struct{}),
	}
	accs := &fakeAccounts{}
	acc := testAccount(srv.URL)
	accs.set(acc)
	leases := &denyClaim{LeaseStore: NewMemoryStore()}
	key := FolderKey(acc.OwnerUserID, acc.ID, "INBOX")
	sup := NewSupervisor(Config{
		OwnerID:             "task-1",
		Accounts:            accs,
		Leases:              leases,
		Dispatcher:          testDispatcher(srv.Client()),
		SkipWebhookValidate: true,
		ReconcileInterval:   30 * time.Millisecond,
		HeartbeatInterval:   time.Hour,
		FindSent:            func(context.Context, *types.Account) (string, error) { return "", nil },
		Dial:                func(context.Context, *types.Account) (Session, error) { return sess, nil },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.Run(ctx)
	waitUntil(t, 2*time.Second, func() bool {
		wm, _ := leases.GetWatermark(context.Background(), key)
		return wm.Set && wm.LastUID == 49
	})
	sess.add(types.EmailSummary{UID: 50, Subject: "new"})
	signalIdle(t, sess.idle)
	waitUntil(t, 2*time.Second, func() bool { return sess.fetchCount() >= 2 })
	if posts.Load() != 0 {
		t.Fatalf("claim loser posted %d times", posts.Load())
	}
	wm, _ := leases.GetWatermark(context.Background(), key)
	if wm.LastUID != 49 {
		t.Fatalf("claim loser advanced watermark to %d", wm.LastUID)
	}
}

func TestSupervisorClearsURLReleasesLease(t *testing.T) {
	accs := &fakeAccounts{}
	acc := testAccount("https://8.8.8.8/hook")
	accs.set(acc)
	idle := make(chan struct{})
	leases := &recordingLeases{MemoryStore: NewMemoryStore()}
	key := FolderKey(acc.OwnerUserID, acc.ID, "INBOX")
	sup := NewSupervisor(Config{
		OwnerID:             "task-1",
		Accounts:            accs,
		Leases:              leases,
		Dispatcher:          NewDispatcher(DispatcherConfig{SkipHostValidation: true}),
		SkipWebhookValidate: true,
		ReconcileInterval:   25 * time.Millisecond,
		HeartbeatInterval:   time.Hour,
		FindSent:            func(context.Context, *types.Account) (string, error) { return "", nil },
		Dial: func(context.Context, *types.Account) (Session, error) {
			return &fakeSession{
				snap: imappkg.SelectSnapshot{UIDNext: 1, UIDValidity: 1},
				idle: idle,
			}, nil
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.Run(ctx)
	waitUntil(t, 2*time.Second, func() bool { return sup.WorkerCount() == 1 })
	cleared := cloneWatchAccount(acc)
	cleared.WebhookURL = ""
	accs.set(cleared)
	waitUntil(t, 2*time.Second, func() bool { return sup.WorkerCount() == 0 })
	found := false
	for _, k := range leases.releasedKeys() {
		if k == key {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected Release(%s), got %v", key, leases.releasedKeys())
	}
}

func TestSupervisorLeaseLossReacquired(t *testing.T) {
	accs := &fakeAccounts{}
	acc := testAccount("https://8.8.8.8/hook")
	accs.set(acc)
	idle := make(chan struct{})
	leases := &hbWrap{LeaseStore: NewMemoryStore()}
	var dials atomic.Int32
	sup := NewSupervisor(Config{
		OwnerID:             "task-1",
		Accounts:            accs,
		Leases:              leases,
		Dispatcher:          NewDispatcher(DispatcherConfig{SkipHostValidation: true}),
		SkipWebhookValidate: true,
		ReconcileInterval:   20 * time.Millisecond,
		HeartbeatInterval:   20 * time.Millisecond,
		LeaseTTL:            time.Minute,
		FindSent:            func(context.Context, *types.Account) (string, error) { return "", nil },
		Dial: func(context.Context, *types.Account) (Session, error) {
			dials.Add(1)
			return &fakeSession{
				snap: imappkg.SelectSnapshot{UIDNext: 1, UIDValidity: 1},
				idle: idle,
			}, nil
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.Run(ctx)
	waitUntil(t, 2*time.Second, func() bool { return sup.WorkerCount() == 1 && dials.Load() >= 1 })
	first := dials.Load()
	leases.fail.Store(true)
	waitUntil(t, 2*time.Second, func() bool { return sup.WorkerCount() == 0 })
	leases.fail.Store(false)
	waitUntil(t, 2*time.Second, func() bool { return sup.WorkerCount() == 1 && dials.Load() > first })
}

func TestSupervisorTransientHeartbeatKeepsWorker(t *testing.T) {
	accs := &fakeAccounts{}
	acc := testAccount("https://8.8.8.8/hook")
	accs.set(acc)
	idle := make(chan struct{})
	leases := &hbWrap{LeaseStore: NewMemoryStore()}
	sup := NewSupervisor(Config{
		OwnerID:             "task-1",
		Accounts:            accs,
		Leases:              leases,
		Dispatcher:          NewDispatcher(DispatcherConfig{SkipHostValidation: true}),
		SkipWebhookValidate: true,
		ReconcileInterval:   30 * time.Millisecond,
		HeartbeatInterval:   20 * time.Millisecond,
		LeaseTTL:            time.Minute,
		FindSent:            func(context.Context, *types.Account) (string, error) { return "", nil },
		Dial: func(context.Context, *types.Account) (Session, error) {
			return &fakeSession{
				snap: imappkg.SelectSnapshot{UIDNext: 1, UIDValidity: 1},
				idle: idle,
			}, nil
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.Run(ctx)
	waitUntil(t, 2*time.Second, func() bool { return sup.WorkerCount() == 1 })
	leases.err.Store(true)
	time.Sleep(80 * time.Millisecond)
	if sup.WorkerCount() != 1 {
		t.Fatalf("transient heartbeat error stopped worker, count=%d", sup.WorkerCount())
	}
}

func TestSupervisorRestartsOnIMAPHostChange(t *testing.T) {
	accs := &fakeAccounts{}
	acc := testAccount("https://8.8.8.8/hook")
	acc.IMAPHost = "imap.example.com"
	accs.set(acc)
	idle := make(chan struct{})
	var dials atomic.Int32
	sup := NewSupervisor(Config{
		OwnerID:             "task-1",
		Accounts:            accs,
		Leases:              NewMemoryStore(),
		Dispatcher:          NewDispatcher(DispatcherConfig{SkipHostValidation: true}),
		SkipWebhookValidate: true,
		ReconcileInterval:   20 * time.Millisecond,
		HeartbeatInterval:   time.Hour,
		FindSent:            func(context.Context, *types.Account) (string, error) { return "", nil },
		Dial: func(ctx context.Context, a *types.Account) (Session, error) {
			dials.Add(1)
			return &fakeSession{
				snap: imappkg.SelectSnapshot{UIDNext: 1, UIDValidity: 1},
				idle: idle,
			}, nil
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.Run(ctx)
	waitUntil(t, 2*time.Second, func() bool { return dials.Load() >= 1 })
	updated := cloneWatchAccount(acc)
	updated.IMAPHost = "imap-other.example.com"
	accs.set(updated)
	waitUntil(t, 2*time.Second, func() bool { return dials.Load() >= 2 })
}

func TestSupervisorWebhookTimeoutLogsAndBacksOff(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		time.Sleep(200 * time.Millisecond)
	}))
	defer srv.Close()

	logs := make(chan string, 32)
	logger := slog.New(&messageLogHandler{messages: logs})
	accs := &fakeAccounts{}
	accs.set(testAccount(srv.URL))
	var dials atomic.Int32
	idle := make(chan struct{})
	sup := NewSupervisor(Config{
		Logger:   logger,
		OwnerID:  "task-timeout",
		Accounts: accs,
		Leases:   NewMemoryStore(),
		Dispatcher: NewDispatcher(DispatcherConfig{
			Client:             &http.Client{Timeout: 30 * time.Millisecond},
			MaxAttempts:        1,
			SkipHostValidation: true,
		}),
		SkipWebhookValidate: true,
		ReconcileInterval:   20 * time.Millisecond,
		HeartbeatInterval:   time.Hour,
		FindSent:            func(context.Context, *types.Account) (string, error) { return "", nil },
		Dial: func(context.Context, *types.Account) (Session, error) {
			dials.Add(1)
			return &fakeSession{
				snap: imappkg.SelectSnapshot{UIDNext: 1, UIDValidity: 1},
				msgs: []types.EmailSummary{{UID: 1}},
				idle: idle,
			}, nil
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		sup.Run(ctx)
		close(done)
	}()
	waitUntil(t, time.Second, func() bool { return requests.Load() > 0 })
	deadline := time.After(time.Second)
	for {
		select {
		case message := <-logs:
			if message == "notify watch error; reconnecting" {
				goto logged
			}
		case <-deadline:
			cancel()
			<-done
			t.Fatal("webhook timeout was not logged as a reconnect error")
		}
	}

logged:
	initialDials := dials.Load()
	time.Sleep(100 * time.Millisecond)
	if got := dials.Load(); got != initialDials || got != 1 {
		cancel()
		<-done
		t.Fatalf("timeout did not back off before reconnect: dials=%d, initially=%d", got, initialDials)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("supervisor did not stop after cancellation")
	}
}
