package notify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	imappkg "github.com/jpuckett/EmailMCP/emailmcp/internal/imap"
	"github.com/jpuckett/EmailMCP/emailmcp/internal/types"
)

const (
	DefaultReconcileInterval = 30 * time.Second
	DefaultPollInterval      = 60 * time.Second
	DefaultPermanentBackoff  = time.Minute
	maxPermanentBackoff      = 5 * time.Minute
	maxReconnectBackoff      = time.Minute
	sentCacheTTL             = 15 * time.Minute
	idleDrainWait            = 2 * time.Second
)

var (
	errLeaseLost            = errors.New("lease lost")
	errClaimLost            = errors.New("uid claim lost")
	errUIDValidityMismatch  = errors.New("uidvalidity mismatch")
	errWatermarkUnavailable = errors.New("uidnext unavailable")
)

// Session is a dedicated IMAP connection used by a folder worker.
type Session interface {
	Select(ctx context.Context, folder string) (imappkg.SelectSnapshot, error)
	SupportsIdle() bool
	Idle(ctx context.Context, refresh time.Duration) (refreshed bool, err error)
	FetchAfter(ctx context.Context, lastUID uint32) ([]types.EmailSummary, error)
	Close() error
	ForceClose()
}

// DialFunc opens a dedicated IMAP session (not the request pool).
type DialFunc func(ctx context.Context, acc *types.Account) (Session, error)

// SentFinder resolves the Sent mailbox. Returning an error skips Sent.
type SentFinder func(ctx context.Context, acc *types.Account) (string, error)

// AccountSource loads every account in the application partition.
type AccountSource interface {
	ListAllConfigs(ctx context.Context) ([]*types.Account, error)
}

// Config for the notify supervisor.
type Config struct {
	Logger              *slog.Logger
	OwnerID             string
	Accounts            AccountSource
	Leases              LeaseStore
	Dispatcher          *Dispatcher
	Dial                DialFunc
	FindSent            SentFinder
	ReconcileInterval   time.Duration
	HeartbeatInterval   time.Duration
	LeaseTTL            time.Duration
	IdleRefresh         time.Duration
	PollInterval        time.Duration
	PermanentBackoff    time.Duration
	MaxFolders          int
	SkipWebhookValidate bool
}

// Supervisor reconciles watched folders and runs leased IDLE workers.
type Supervisor struct {
	cfg Config

	mu        sync.Mutex
	workers   map[string]*folderWorker
	sentMu    sync.Mutex
	sentCache map[string]sentCacheEntry
}

type sentCacheEntry struct {
	folder    string
	resolved  bool
	checkedAt time.Time
}

type folderWorker struct {
	key          string
	cancel       context.CancelFunc
	done         chan struct{}
	acc          *types.Account // immutable snapshot for this worker
	folder       string
	loggedNoIdle sync.Once
}

func NewSupervisor(cfg Config) *Supervisor {
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.OwnerID == "" {
		cfg.OwnerID = "notify"
	}
	if cfg.ReconcileInterval <= 0 {
		cfg.ReconcileInterval = DefaultReconcileInterval
	}
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = DefaultHeartbeat
	}
	if cfg.LeaseTTL <= 0 {
		cfg.LeaseTTL = DefaultLeaseTTL
	}
	if cfg.IdleRefresh <= 0 {
		cfg.IdleRefresh = imappkg.DefaultIdleRefresh
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = DefaultPollInterval
	}
	if cfg.PermanentBackoff <= 0 {
		cfg.PermanentBackoff = DefaultPermanentBackoff
	}
	if cfg.MaxFolders <= 0 {
		cfg.MaxFolders = types.MaxWatchFolders
	}
	return &Supervisor{
		cfg:       cfg,
		workers:   make(map[string]*folderWorker),
		sentCache: make(map[string]sentCacheEntry),
	}
}

// WorkerCount returns the number of live folder workers.
func (s *Supervisor) WorkerCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reapLocked()
	return len(s.workers)
}

func (s *Supervisor) reapLocked() {
	for key, w := range s.workers {
		select {
		case <-w.done:
			delete(s.workers, key)
		default:
		}
	}
}

// Run reconciles until ctx is cancelled, then stops workers and releases leases.
func (s *Supervisor) Run(ctx context.Context) error {
	s.reconcile(ctx)
	ticker := time.NewTicker(s.cfg.ReconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.stopAll()
			return nil
		case <-ticker.C:
			s.reconcile(ctx)
		}
	}
}

func (s *Supervisor) stopAll() {
	s.mu.Lock()
	workers := make([]*folderWorker, 0, len(s.workers))
	for _, w := range s.workers {
		workers = append(workers, w)
	}
	s.workers = make(map[string]*folderWorker)
	s.mu.Unlock()
	for _, w := range workers {
		w.cancel()
	}
	for _, w := range workers {
		<-w.done
		_ = s.cfg.Leases.Release(context.Background(), w.key, s.cfg.OwnerID)
	}
}

func (s *Supervisor) reconcile(ctx context.Context) {
	if err := ctx.Err(); err != nil {
		return
	}
	accs, err := s.cfg.Accounts.ListAllConfigs(ctx)
	if err != nil {
		s.cfg.Logger.WarnContext(ctx, "list accounts for notify failed", "error", err)
		return
	}
	desired := make(map[string]*types.Account)
	foldersByKey := make(map[string]string)
	for _, acc := range accs {
		if acc == nil || acc.WebhookURL == "" || acc.IMAPPassword == "" || acc.OwnerUserID == "" || acc.ID == "" {
			continue
		}
		if !s.cfg.SkipWebhookValidate {
			if err := validateWebhookURL(acc.WebhookURL); err != nil {
				s.cfg.Logger.WarnContext(ctx, "skipping account with invalid webhook",
					"account_id", acc.ID, "error", err)
				continue
			}
		}
		folders := s.watchFolders(ctx, acc)
		for _, folder := range folders {
			key := FolderKey(acc.OwnerUserID, acc.ID, folder)
			desired[key] = acc
			foldersByKey[key] = folder
		}
	}

	var stopping []*folderWorker
	s.mu.Lock()
	s.reapLocked()
	for key, w := range s.workers {
		acc, ok := desired[key]
		if !ok || watchConfigChanged(w.acc, acc) {
			delete(s.workers, key)
			stopping = append(stopping, w)
		}
	}
	s.mu.Unlock()

	for _, w := range stopping {
		s.invalidateSentCache(w.acc)
		w.cancel()
	}
	for _, w := range stopping {
		<-w.done
		_ = s.cfg.Leases.Release(context.Background(), w.key, s.cfg.OwnerID)
	}

	for key, acc := range desired {
		s.mu.Lock()
		s.reapLocked()
		_, running := s.workers[key]
		s.mu.Unlock()
		if running {
			continue
		}
		ok, err := s.cfg.Leases.Acquire(ctx, key, s.cfg.OwnerID, s.cfg.LeaseTTL)
		if err != nil {
			s.cfg.Logger.WarnContext(ctx, "lease acquire failed", "error", err, "account_id", acc.ID)
			continue
		}
		if !ok {
			continue
		}
		folder := foldersByKey[key]
		wctx, cancel := context.WithCancel(ctx)
		w := &folderWorker{
			key:    key,
			cancel: cancel,
			done:   make(chan struct{}),
			acc:    cloneWatchAccount(acc),
			folder: folder,
		}
		s.mu.Lock()
		s.reapLocked()
		if _, exists := s.workers[key]; exists {
			s.mu.Unlock()
			cancel()
			close(w.done)
			continue
		}
		s.workers[key] = w
		s.mu.Unlock()
		go s.runWorker(wctx, w)
	}
}

func (s *Supervisor) watchFolders(ctx context.Context, acc *types.Account) []string {
	if len(acc.WatchFolders) > 0 {
		return normalizeWatchFolders(acc.WatchFolders, s.cfg.MaxFolders)
	}
	folders := []string{"INBOX"}
	if s.cfg.FindSent == nil {
		return normalizeWatchFolders(folders, s.cfg.MaxFolders)
	}
	cacheKey := accountCacheKey(acc)
	s.sentMu.Lock()
	cached, hasCache := s.sentCache[cacheKey]
	fresh := hasCache && cached.resolved && time.Since(cached.checkedAt) < sentCacheTTL
	s.sentMu.Unlock()
	if fresh {
		return normalizeWatchFolders(appendSent(folders, cached.folder), s.cfg.MaxFolders)
	}

	sent, err := s.cfg.FindSent(ctx, acc)
	if err != nil {
		s.cfg.Logger.WarnContext(ctx, "sent folder not resolved; watching INBOX only",
			"account_id", acc.ID, "error", err)
		if hasCache && cached.resolved {
			return normalizeWatchFolders(appendSent(folders, cached.folder), s.cfg.MaxFolders)
		}
		return normalizeWatchFolders(folders, s.cfg.MaxFolders)
	}
	sent = strings.TrimSpace(sent)
	if strings.EqualFold(sent, "INBOX") {
		sent = ""
	}
	s.sentMu.Lock()
	s.sentCache[cacheKey] = sentCacheEntry{folder: sent, resolved: true, checkedAt: time.Now()}
	s.sentMu.Unlock()
	return normalizeWatchFolders(appendSent(folders, sent), s.cfg.MaxFolders)
}

func appendSent(folders []string, sent string) []string {
	if sent == "" {
		return folders
	}
	return append(folders, sent)
}

func normalizeWatchFolders(folders []string, max int) []string {
	out := make([]string, 0, len(folders))
	seen := make(map[string]struct{}, len(folders))
	for _, folder := range folders {
		folder = strings.TrimSpace(folder)
		if folder == "" {
			continue
		}
		if strings.EqualFold(folder, "INBOX") {
			folder = "INBOX"
		}
		if _, exists := seen[folder]; exists {
			continue
		}
		if max > 0 && len(out) >= max {
			break
		}
		seen[folder] = struct{}{}
		out = append(out, folder)
	}
	return out
}

func (s *Supervisor) invalidateSentCache(acc *types.Account) {
	if acc == nil {
		return
	}
	s.sentMu.Lock()
	delete(s.sentCache, accountCacheKey(acc))
	s.sentMu.Unlock()
}

func (s *Supervisor) runWorker(ctx context.Context, w *folderWorker) {
	defer close(w.done)
	logger := s.cfg.Logger.With("account_id", w.acc.ID, "folder", w.folder)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go s.heartbeatLoop(ctx, cancel, w, logger)

	backoff := time.Second
	permBackoff := s.cfg.PermanentBackoff

	for {
		if err := ctx.Err(); err != nil {
			return
		}
		err := s.watchOnce(ctx, w, logger, &permBackoff)
		if err == nil {
			backoff = time.Second
			continue
		}
		if ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
			return
		}
		if errors.Is(err, errLeaseLost) || errors.Is(err, errClaimLost) {
			logger.Info("lost notify lease; stopping worker")
			return
		}
		if IsPermanent(err) {
			timer := time.NewTimer(permBackoff)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			if permBackoff < maxPermanentBackoff {
				permBackoff *= 2
				if permBackoff > maxPermanentBackoff {
					permBackoff = maxPermanentBackoff
				}
			}
			continue
		}
		logger.Warn("notify watch error; reconnecting", "error", err)
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if backoff < maxReconnectBackoff {
			backoff *= 2
			if backoff > maxReconnectBackoff {
				backoff = maxReconnectBackoff
			}
		}
	}
}

func (s *Supervisor) heartbeatLoop(ctx context.Context, cancel context.CancelFunc, w *folderWorker, logger *slog.Logger) {
	ticker := time.NewTicker(s.cfg.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ok, err := s.cfg.Leases.Heartbeat(ctx, w.key, s.cfg.OwnerID, s.cfg.LeaseTTL)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				logger.Warn("notify lease heartbeat failed", "error", err)
				continue
			}
			if !ok {
				logger.Info("lost notify lease; stopping worker")
				cancel()
				return
			}
		}
	}
}

func (s *Supervisor) watchOnce(ctx context.Context, w *folderWorker, logger *slog.Logger, permBackoff *time.Duration) error {
	sess, err := s.cfg.Dial(ctx, w.acc)
	if err != nil {
		return err
	}
	defer func() {
		if sess == nil {
			return
		}
		if ctx.Err() != nil {
			sess.ForceClose()
			return
		}
		_ = sess.Close()
	}()

	snap, err := sess.Select(ctx, w.folder)
	if err != nil {
		return err
	}
	if snap.UIDValidity == 0 {
		return fmt.Errorf("select omitted uidvalidity")
	}
	if snap.UIDNext == 0 {
		return errWatermarkUnavailable
	}

	wm, err := s.cfg.Leases.GetWatermark(ctx, w.key)
	if err != nil {
		return err
	}
	if !wm.Set || wm.UIDValidity != snap.UIDValidity {
		last := imappkg.SnapshotLastUID(snap.UIDNext)
		changed, err := s.cfg.Leases.Snapshot(ctx, w.key, s.cfg.OwnerID, snap.UIDValidity, last)
		if err != nil {
			return err
		}
		if changed {
			logger.Info("notify watermark snapshot", "uidvalidity", snap.UIDValidity, "last_uid", last)
			wm = Watermark{LastUID: last, UIDValidity: snap.UIDValidity, Set: true}
		} else {
			wm, err = s.cfg.Leases.GetWatermark(ctx, w.key)
			if err != nil {
				return err
			}
			if !wm.Set || wm.UIDValidity != snap.UIDValidity {
				return errLeaseLost
			}
		}
	}

	if err := s.deliverNew(ctx, w, sess, snap.UIDValidity, logger, permBackoff); err != nil {
		return err
	}

	loggedIdle := false
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		idleCtx, cancelIdle := context.WithCancel(ctx)
		type idleWait struct {
			err       error
			refreshed bool
		}
		idleDone := make(chan idleWait, 1)
		useIdle := sess.SupportsIdle()
		if useIdle && !loggedIdle {
			logger.Info("IMAP IDLE monitoring mailbox", "refresh", s.cfg.IdleRefresh)
			loggedIdle = true
		}
		go func() {
			if useIdle {
				refreshed, err := sess.Idle(idleCtx, s.cfg.IdleRefresh)
				idleDone <- idleWait{err: err, refreshed: refreshed}
				return
			}
			w.loggedNoIdle.Do(func() {
				logger.Warn("IMAP IDLE unsupported; polling folder")
			})
			timer := time.NewTimer(s.cfg.PollInterval)
			defer timer.Stop()
			select {
			case <-idleCtx.Done():
				idleDone <- idleWait{err: idleCtx.Err()}
			case <-timer.C:
				idleDone <- idleWait{}
			}
		}()

		select {
		case <-ctx.Done():
			cancelIdle()
			sess.ForceClose()
			select {
			case <-idleDone:
			case <-time.After(idleDrainWait):
			}
			return ctx.Err()
		case result := <-idleDone:
			cancelIdle()
			err := result.err
			if err != nil && !errors.Is(err, context.Canceled) {
				if errors.Is(err, imappkg.ErrIdleUnsupported) {
					if err := sleepCtx(ctx, s.cfg.PollInterval); err != nil {
						return err
					}
				} else {
					return err
				}
			}
			if result.refreshed && err == nil {
				logger.Info("IMAP IDLE refreshed")
			}
			if err := s.deliverNew(ctx, w, sess, snap.UIDValidity, logger, permBackoff); err != nil {
				return err
			}
		}
	}
}

func (s *Supervisor) deliverNew(ctx context.Context, w *folderWorker, sess Session, uidValidity uint32, logger *slog.Logger, permBackoff *time.Duration) error {
	wm, err := s.cfg.Leases.GetWatermark(ctx, w.key)
	if err != nil {
		return err
	}
	if !wm.Set {
		return fmt.Errorf("watermark missing")
	}
	if wm.UIDValidity != uidValidity {
		return errUIDValidityMismatch
	}
	msgs, err := sess.FetchAfter(ctx, wm.LastUID)
	if err != nil {
		return err
	}
	for _, msg := range msgs {
		if msg.UID <= wm.LastUID {
			continue
		}
		from := ""
		if len(msg.From) > 0 {
			from = msg.From[0].Address
		}
		logger.Info("new mailbox message", "uid", msg.UID, "subject", msg.Subject, "from", from)
		ok, err := s.cfg.Leases.Heartbeat(ctx, w.key, s.cfg.OwnerID, s.cfg.LeaseTTL)
		if err != nil {
			return err
		}
		if !ok {
			return errLeaseLost
		}
		claimed, err := s.cfg.Leases.ClaimUID(ctx, w.key, s.cfg.OwnerID, uidValidity, msg.UID, s.cfg.LeaseTTL)
		if err != nil {
			return err
		}
		if !claimed {
			logger.Info("uid claim lost; stopping delivery", "uid", msg.UID)
			return errClaimLost
		}
		err = s.cfg.Dispatcher.Post(ctx, w.acc.WebhookURL, w.acc.WebhookToken, w.acc.ID, w.folder, msg)
		if err != nil {
			if IsPermanent(err) {
				_ = s.cfg.Leases.ClearClaim(ctx, w.key, s.cfg.OwnerID)
				return err
			}
			return err
		}
		advanced, err := s.cfg.Leases.AdvanceUID(ctx, w.key, s.cfg.OwnerID, uidValidity, msg.UID)
		if err != nil {
			return err
		}
		if !advanced {
			logger.Info("uid cas lost after post", "uid", msg.UID)
			return errClaimLost
		}
		wm.LastUID = msg.UID
		*permBackoff = s.cfg.PermanentBackoff
	}
	return nil
}

// IMAPDialer returns a DialFunc that opens dedicated IDLE connections.
func IMAPDialer(logger *slog.Logger, skipHostValidation bool) DialFunc {
	return func(ctx context.Context, acc *types.Account) (Session, error) {
		return imappkg.DialAccount(ctx, acc, logger, skipHostValidation)
	}
}

// DedicatedSentFinder resolves Sent on a short-lived dedicated IMAP connection.
func DedicatedSentFinder(dial DialFunc) SentFinder {
	return func(ctx context.Context, acc *types.Account) (string, error) {
		if dial == nil {
			return "", errors.New("imap dialer is required")
		}
		sess, err := dial(ctx, acc)
		if err != nil {
			return "", err
		}
		defer func() { _ = sess.Close() }()
		type sentResolver interface {
			FindSentFolder(context.Context) (string, error)
		}
		r, ok := sess.(sentResolver)
		if !ok {
			return "", errors.New("imap session cannot resolve sent folder")
		}
		return r.FindSentFolder(ctx)
	}
}

func watchConfigChanged(old, neu *types.Account) bool {
	if old == nil || neu == nil {
		return true
	}
	return old.WebhookURL != neu.WebhookURL ||
		old.WebhookToken != neu.WebhookToken ||
		old.IMAPPassword != neu.IMAPPassword ||
		old.IMAPHost != neu.IMAPHost ||
		old.IMAPPort != neu.IMAPPort ||
		old.IMAPUsername != neu.IMAPUsername ||
		old.IMAPUseTLS != neu.IMAPUseTLS
}

func cloneWatchAccount(a *types.Account) *types.Account {
	if a == nil {
		return nil
	}
	cp := *a
	if a.WatchFolders != nil {
		cp.WatchFolders = append([]string(nil), a.WatchFolders...)
	}
	return &cp
}

func accountCacheKey(acc *types.Account) string {
	if acc == nil {
		return ""
	}
	return acc.OwnerUserID + "#" + acc.ID
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
