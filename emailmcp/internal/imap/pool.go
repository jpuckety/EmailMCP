package imap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/mail"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/jpuckett/EmailMCP/emailmcp/internal/netutil"
	"github.com/jpuckett/EmailMCP/emailmcp/internal/types"
)

const (
	defaultMaxConns    = 4
	defaultIdle        = 4 * time.Minute
	healthCheckTimeout = 8 * time.Second
	imapRetryAttempts  = 2
)

// Config for the IMAP manager.
type Config struct {
	MaxConnsPerAccount int
	IdleTimeout        time.Duration
	Logger             *slog.Logger
	// SkipHostValidation allows loopback IMAP in tests. Never enable in production.
	SkipHostValidation bool
}

// Manager manages per-account IMAP connection pools.
type Manager struct {
	cfg Config

	mu    sync.Mutex
	pools map[string]*accountPool
}

type accountPool struct {
	poolKey   string // ownerUserID + "\x00" + accountID
	ownerID   string
	accountID string
	imapCfg   imapAccountConfig // plaintext only for the life of the pool

	mu       sync.Mutex
	cond     *sync.Cond
	idle     []*pooledConn
	inUse    int
	maxConns int
	idleT    time.Duration
	logger   *slog.Logger
	closed   bool

	// dial, if set, replaces dialAndLogin (tests).
	dial func(context.Context) (*pooledConn, error)
}

// PoolKey returns the map key for a user's account pool.
// Tenants are isolated even when account IDs collide (e.g. both use "default").
func PoolKey(ownerUserID, accountID string) string {
	return ownerUserID + "\x00" + accountID
}

type pooledConn struct {
	client       *imapclient.Client
	lastSelected string
	lastUsed     time.Time
}

// imapAccountConfig holds the connection info (password plaintext only briefly).
type imapAccountConfig struct {
	Host               string
	Port               int
	Username           string
	Password           string
	UseTLS             bool
	SkipHostValidation bool
}

// NewManager creates an IMAP connection manager.
func NewManager(cfg Config) *Manager {
	if cfg.MaxConnsPerAccount <= 0 {
		cfg.MaxConnsPerAccount = defaultMaxConns
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = defaultIdle
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Manager{
		cfg:   cfg,
		pools: make(map[string]*accountPool),
	}
}

// getOrCreatePool returns the pool for an account.
func (m *Manager) getOrCreatePool(acc *types.Account) (*accountPool, error) {
	if acc == nil {
		return nil, errors.New("account is required")
	}
	if acc.OwnerUserID == "" {
		return nil, errors.New("account owner user id is required for imap pool isolation")
	}
	if acc.ID == "" {
		return nil, errors.New("account id is required")
	}

	key := PoolKey(acc.OwnerUserID, acc.ID)

	if acc.IMAPPassword == "" {
		return nil, errors.New("imap password is required")
	}
	if !m.cfg.SkipHostValidation {
		if err := netutil.ValidatePublicHost(acc.IMAPHost); err != nil {
			return nil, fmt.Errorf("imap host not allowed: %w", err)
		}
	}
	if err := netutil.RequireTLSUnlessLocalhost(acc.IMAPHost, acc.IMAPUseTLS, "IMAP"); err != nil {
		return nil, err
	}

	m.mu.Lock()
	if p, ok := m.pools[key]; ok && p.sameIMAPConfig(acc) {
		m.mu.Unlock()
		return p, nil
	}
	var stale *accountPool
	if p, ok := m.pools[key]; ok {
		delete(m.pools, key)
		stale = p
	}
	m.mu.Unlock()
	if stale != nil {
		m.cfg.Logger.Info("replacing imap pool after account config change",
			"owner_user_id", acc.OwnerUserID, "account_id", acc.ID)
		stale.closeAll()
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.pools[key]; ok {
		return p, nil
	}

	logger := m.cfg.Logger.With(
		"op", "get_or_create_pool",
		"owner_user_id", acc.OwnerUserID,
		"account_id", acc.ID,
	)
	logger.Debug("creating new imap pool", "host", acc.IMAPHost, "port", acc.IMAPPort, "use_tls", acc.IMAPUseTLS)

	p := &accountPool{
		poolKey:   key,
		ownerID:   acc.OwnerUserID,
		accountID: acc.ID,
		imapCfg: imapAccountConfig{
			Host:               acc.IMAPHost,
			Port:               acc.IMAPPort,
			Username:           acc.IMAPUsername,
			Password:           acc.IMAPPassword,
			UseTLS:             acc.IMAPUseTLS,
			SkipHostValidation: m.cfg.SkipHostValidation,
		},
		maxConns: m.cfg.MaxConnsPerAccount,
		idleT:    m.cfg.IdleTimeout,
		logger:   m.cfg.Logger,
		idle:     make([]*pooledConn, 0, m.cfg.MaxConnsPerAccount),
	}
	p.cond = sync.NewCond(&p.mu)

	m.pools[key] = p
	logger.Info("imap pool created", "max_conns", p.maxConns, "idle_timeout", p.idleT)
	return p, nil
}

func (p *accountPool) sameIMAPConfig(acc *types.Account) bool {
	return p.imapCfg.Host == acc.IMAPHost &&
		p.imapCfg.Port == acc.IMAPPort &&
		p.imapCfg.Username == acc.IMAPUsername &&
		p.imapCfg.Password == acc.IMAPPassword &&
		p.imapCfg.UseTLS == acc.IMAPUseTLS
}

// Acquire gets a connection from the pool for the given account.
// Caller must call Release when done.
func (m *Manager) Acquire(ctx context.Context, acc *types.Account) (*pooledConn, error) {
	pool, err := m.getOrCreatePool(acc)
	if err != nil {
		return nil, err
	}
	return pool.acquire(ctx)
}

// Release returns the connection to the pool or closes it on error.
func (m *Manager) Release(acc *types.Account, conn *pooledConn, hadError bool) {
	if conn == nil {
		return
	}
	if acc == nil || acc.OwnerUserID == "" || acc.ID == "" {
		if conn.client != nil {
			m.cfg.Logger.Warn("releasing connection without account identity; closing",
				"had_error", hadError)
			conn.client.Close()
		}
		return
	}
	key := PoolKey(acc.OwnerUserID, acc.ID)
	m.mu.Lock()
	p, ok := m.pools[key]
	m.mu.Unlock()
	if !ok {
		if conn.client != nil {
			m.cfg.Logger.Warn("releasing connection with no matching pool; closing",
				"owner_user_id", acc.OwnerUserID, "account_id", acc.ID, "had_error", hadError)
			conn.client.Close()
		}
		return
	}
	p.release(conn, hadError)
}

// DropPool closes and removes the pool for a user's account (e.g. on remove or
// credential rotation). Safe to call when no pool exists.
func (m *Manager) DropPool(ownerUserID, accountID string) {
	if ownerUserID == "" || accountID == "" {
		return
	}
	key := PoolKey(ownerUserID, accountID)
	m.mu.Lock()
	p, ok := m.pools[key]
	if ok {
		delete(m.pools, key)
	}
	m.mu.Unlock()
	if !ok {
		return
	}
	m.cfg.Logger.Info("dropping imap pool",
		"owner_user_id", ownerUserID, "account_id", accountID)
	p.closeAll()
}

// CloseAll closes all pooled connections (for shutdown).
func (m *Manager) CloseAll() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.cfg.Logger.Info("closing all imap pools", "pool_count", len(m.pools))
	for id, p := range m.pools {
		p.closeAll()
		delete(m.pools, id)
	}
	m.cfg.Logger.Debug("all imap pools closed")
}

// --- accountPool implementation ---

func (p *accountPool) acquire(ctx context.Context) (*pooledConn, error) {
	logger := p.logger.With("op", "pool_acquire", "account_id", p.accountID)

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			logger.WarnContext(ctx, "acquire on closed pool")
			return nil, errors.New("pool closed")
		}

		if c := p.popIdleLocked(); c != nil {
			p.inUse++
			p.mu.Unlock()
			if err := p.healthCheck(ctx, c); err != nil {
				logger.WarnContext(ctx, "idle connection failed health check; discarding", "error", err)
				if c.client != nil {
					c.client.Close()
				}
				p.mu.Lock()
				p.inUse--
				p.cond.Signal()
				p.mu.Unlock()
				continue
			}
			c.lastUsed = time.Now()
			logger.DebugContext(ctx, "reusing idle connection", "in_use", p.inUse)
			return c, nil
		}

		if p.inUse < p.maxConns {
			p.inUse++
			p.mu.Unlock()
			conn, err := p.newConn(ctx)
			if err != nil {
				p.mu.Lock()
				p.inUse--
				p.cond.Signal()
				p.mu.Unlock()
				return nil, err
			}
			logger.DebugContext(ctx, "new connection created", "in_use", p.inUse)
			return conn, nil
		}

		logger.DebugContext(ctx, "waiting for imap connection slot",
			"in_use", p.inUse, "max_conns", p.maxConns)
		if err := p.waitForSlot(ctx); err != nil {
			p.mu.Unlock()
			return nil, err
		}
		p.mu.Unlock()
	}
}

func (p *accountPool) popIdleLocked() *pooledConn {
	for len(p.idle) > 0 {
		i := len(p.idle) - 1
		c := p.idle[i]
		p.idle = p.idle[:i]
		if c == nil {
			continue
		}
		if time.Since(c.lastUsed) > p.idleT {
			client := c.client
			go func() {
				if client != nil {
					client.Close()
				}
			}()
			continue
		}
		return c
	}
	return nil
}

func (p *accountPool) newConn(ctx context.Context) (*pooledConn, error) {
	type result struct {
		c   *pooledConn
		err error
	}
	done := make(chan result, 1)
	go func() {
		if p.dial != nil {
			c, err := p.dial(ctx)
			done <- result{c, err}
			return
		}
		c, err := p.dialAndLogin(ctx)
		done <- result{c, err}
	}()
	select {
	case <-ctx.Done():
		go func() {
			r := <-done
			if r.c != nil && r.c.client != nil {
				r.c.client.Close()
			}
		}()
		return nil, ctx.Err()
	case r := <-done:
		return r.c, r.err
	}
}

func (p *accountPool) healthCheck(ctx context.Context, c *pooledConn) error {
	if c == nil || c.client == nil {
		return errors.New("nil imap client")
	}
	ctx, cancel := context.WithTimeout(ctx, healthCheckTimeout)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- c.client.Noop().Wait()
	}()
	select {
	case <-ctx.Done():
		c.client.Close()
		return fmt.Errorf("imap health check: %w", ctx.Err())
	case err := <-done:
		return err
	}
}

// waitForSlot waits until a connection may be available. p.mu must be held.
func (p *accountPool) waitForSlot(ctx context.Context) error {
	waitDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			p.mu.Lock()
			p.cond.Broadcast()
			p.mu.Unlock()
		case <-waitDone:
		}
	}()
	p.cond.Wait()
	close(waitDone)
	return ctx.Err()
}

func (p *accountPool) dialAndLogin(ctx context.Context) (*pooledConn, error) {
	// Re-check at dial time so a pool cannot be abused if validation rules tighten.
	if !p.imapCfg.SkipHostValidation {
		if err := netutil.ValidatePublicHost(p.imapCfg.Host); err != nil {
			return nil, fmt.Errorf("imap host not allowed: %w", err)
		}
	}
	if err := netutil.RequireTLSUnlessLocalhost(p.imapCfg.Host, p.imapCfg.UseTLS, "IMAP"); err != nil {
		return nil, err
	}

	addr := fmt.Sprintf("%s:%d", p.imapCfg.Host, p.imapCfg.Port)
	logger := p.logger.With("op", "dial_and_login",
		"owner_user_id", p.ownerID, "account_id", p.accountID,
		"host", p.imapCfg.Host, "port", p.imapCfg.Port, "use_tls", p.imapCfg.UseTLS)

	start := time.Now()
	logger.DebugContext(ctx, "dialing imap server")

	var client *imapclient.Client
	var err error

	if p.imapCfg.UseTLS {
		client, err = imapclient.DialTLS(addr, &imapclient.Options{})
	} else {
		// Non-TLS is only permitted for localhost (checked above).
		client, err = imapclient.DialInsecure(addr, &imapclient.Options{})
	}
	if err != nil {
		logger.ErrorContext(ctx, "imap dial failed", "error", err, "elapsed", time.Since(start))
		return nil, fmt.Errorf("dial imap %s: %w", addr, err)
	}

	logger.DebugContext(ctx, "dial complete; logging in",
		"username", p.imapCfg.Username, "elapsed", time.Since(start))

	// Set deadline aware if possible (library mostly uses Wait())
	loginCmd := client.Login(p.imapCfg.Username, p.imapCfg.Password)
	if err := loginCmd.Wait(); err != nil {
		logger.ErrorContext(ctx, "imap login failed",
			"error", err, "username", p.imapCfg.Username, "elapsed", time.Since(start))
		client.Close()
		return nil, types.AuthFailed("IMAP", err)
	}

	logger.InfoContext(ctx, "imap connection established",
		"username", p.imapCfg.Username, "elapsed", time.Since(start))

	return &pooledConn{
		client:   client,
		lastUsed: time.Now(),
	}, nil
}

func (p *accountPool) release(conn *pooledConn, hadError bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	logger := p.logger.With("op", "pool_release", "account_id", p.accountID)

	p.inUse--
	if p.inUse < 0 {
		p.inUse = 0
	}
	if p.cond != nil {
		p.cond.Signal()
	}

	if hadError || p.closed || conn.client == nil {
		if conn.client != nil {
			logger.Debug("closing connection on release",
				"had_error", hadError, "pool_closed", p.closed, "in_use", p.inUse)
			conn.client.Close()
		}
		return
	}

	conn.lastUsed = time.Now()
	p.idle = append(p.idle, conn)
	logger.Debug("connection returned to idle pool",
		"idle", len(p.idle), "in_use", p.inUse)

	for len(p.idle) > p.maxConns {
		c := p.idle[0]
		p.idle = p.idle[1:]
		logger.Debug("trimming excess idle connection", "idle", len(p.idle))
		if c.client != nil {
			c.client.Close()
		}
	}
}

func (p *accountPool) closeAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	if p.cond != nil {
		p.cond.Broadcast()
	}

	logger := p.logger.With("op", "pool_close_all", "account_id", p.accountID)
	logger.Info("closing account pool", "idle", len(p.idle), "in_use", p.inUse)

	for _, c := range p.idle {
		if c.client != nil {
			if err := c.client.Logout().Wait(); err != nil {
				logger.Debug("imap logout returned error during close", "error", err)
			}
			c.client.Close()
		}
	}
	p.idle = nil
}

// withConn runs fn with an exclusive pooled connection and retries once if the
// connection dies mid-command (EOF, reset, closed).
func (m *Manager) withConn(ctx context.Context, acc *types.Account, fn func(*pooledConn) error) error {
	var last error
	for i := 1; i <= imapRetryAttempts; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		conn, err := m.Acquire(ctx, acc)
		if err != nil {
			return err
		}
		err = fn(conn)
		m.Release(acc, conn, err != nil)
		if err == nil || !isTransientIMAP(err) || i == imapRetryAttempts {
			return err
		}
		last = err
		m.cfg.Logger.WarnContext(ctx, "imap connection lost; retrying with a new connection",
			"account_id", acc.ID, "attempt", i, "error", err)
	}
	return last
}

func isTransientIMAP(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "eof") ||
		strings.Contains(s, "broken pipe") ||
		strings.Contains(s, "connection reset") ||
		strings.Contains(s, "use of closed") ||
		strings.Contains(s, "i/o timeout") ||
		strings.Contains(s, "connection lost") ||
		strings.Contains(s, "connection refused")
}

// --- High level operations ---

// ListFolders lists available mailboxes for the account.
func (m *Manager) ListFolders(ctx context.Context, acc *types.Account) ([]types.Folder, error) {
	logger := m.cfg.Logger.With("op", "list_folders", "account_id", acc.ID)
	logger.DebugContext(ctx, "listing folders")

	start := time.Now()

	var folders []types.Folder
	err := m.withConn(ctx, acc, func(conn *pooledConn) error {
		logger.DebugContext(ctx, "issuing LIST command", "reference", "", "pattern", "%")
		cmd := conn.client.List("", "%", nil)
		mailboxes, err := cmd.Collect()
		if err != nil {
			logger.ErrorContext(ctx, "LIST command failed",
				"error", err,
				"elapsed", time.Since(start))
			return fmt.Errorf("list mailboxes: %w", err)
		}

		folders = make([]types.Folder, 0, len(mailboxes))
		for _, mb := range mailboxes {
			f := types.Folder{
				Name:      mb.Mailbox,
				Delimiter: string(mb.Delim),
			}
			for _, attr := range mb.Attrs {
				f.Attributes = append(f.Attributes, string(attr))
			}
			folders = append(folders, f)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	logger.InfoContext(ctx, "listed folders",
		"folder_count", len(folders),
		"elapsed", time.Since(start))

	return folders, nil
}

// SearchEmails searches a folder using simple criteria.
// Results are ordered by sort (default: newest internal date / arrival, reverse).
// When the server supports the IMAP SORT extension, ordering is done server-side
// before limit is applied. Otherwise a best-effort client-side path is used.
func (m *Manager) SearchEmails(ctx context.Context, acc *types.Account, folder string, criteria imap.SearchCriteria, limit int, sortOpt SearchSort) ([]types.EmailSummary, error) {
	if folder == "" {
		folder = "INBOX"
	}
	sortKey, reverse := sortOpt.Resolve()

	logger := m.cfg.Logger.With(
		"op", "search_emails",
		"account_id", acc.ID,
		"folder", folder,
		"limit", limit,
		"sort_by", string(sortKey),
		"sort_reverse", reverse,
	)
	logger.DebugContext(ctx, "searching emails")
	start := time.Now()

	var summaries []types.EmailSummary
	var totalMatches int
	err := m.withConn(ctx, acc, func(conn *pooledConn) error {
		_, err := conn.client.Select(folder, nil).Wait()
		if err != nil {
			logger.ErrorContext(ctx, "SELECT failed", "error", err, "elapsed", time.Since(start))
			return fmt.Errorf("select folder %s: %w", folder, err)
		}
		conn.lastSelected = folder

		uidList, total, usedServerSort, err := searchSortedUIDs(conn.client, &criteria, sortKey, reverse)
		if err != nil {
			logger.ErrorContext(ctx, "search/sort failed", "error", err, "elapsed", time.Since(start))
			return err
		}
		totalMatches = total
		if len(uidList) == 0 {
			summaries = []types.EmailSummary{}
			return nil
		}

		limitBeforeFetch := usedServerSort || sortKey == SortKeyArrival
		if limitBeforeFetch && limit > 0 && len(uidList) > limit {
			uidList = uidList[:limit]
		}
		logger.DebugContext(ctx, "fetching summaries",
			"total_matches", totalMatches,
			"fetching", len(uidList),
			"server_sort", usedServerSort,
		)

		fetchOpts := &imap.FetchOptions{
			Envelope:      true,
			Flags:         true,
			BodyStructure: &imap.FetchItemBodyStructure{},
			BodySection: []*imap.FetchItemBodySection{
				{Specifier: imap.PartSpecifierHeader},
			},
			RFC822Size: true,
		}

		msgs, err := conn.client.Fetch(imap.UIDSetNum(uidList...), fetchOpts).Collect()
		if err != nil {
			logger.ErrorContext(ctx, "FETCH summaries failed", "error", err, "elapsed", time.Since(start))
			return fmt.Errorf("fetch summaries: %w", err)
		}

		summaries = make([]types.EmailSummary, 0, len(msgs))
		for _, msg := range msgs {
			summaries = append(summaries, summaryFromBuffer(msg))
		}

		if !limitBeforeFetch {
			sortSummaries(summaries, sortKey, reverse)
			if limit > 0 && len(summaries) > limit {
				summaries = summaries[:limit]
			}
		} else {
			summaries = orderSummariesByUIDs(summaries, uidList)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	logger.InfoContext(ctx, "search complete",
		"total_matches", totalMatches, "returned", len(summaries), "elapsed", time.Since(start))
	return summaries, nil
}

// searchSortedUIDs returns matching UIDs in sort order.
// usedServerSort is true when IMAP SORT produced the order.
func searchSortedUIDs(client *imapclient.Client, criteria *imap.SearchCriteria, sortKey SearchSortKey, reverse bool) (uids []imap.UID, total int, usedServerSort bool, err error) {
	if client.Caps().Has(imap.CapSort) {
		nums, sortErr := client.UIDSort(&imapclient.SortOptions{
			SearchCriteria: criteria,
			SortCriteria: []imapclient.SortCriterion{{
				Key:     toIMAPSortKey(sortKey),
				Reverse: reverse,
			}},
		}).Wait()
		if sortErr == nil {
			uids = uidsToUIDs(nums)
			return uids, len(uids), true, nil
		}
		// Fall through to SEARCH + client-side ordering.
	}

	searchCmd := client.UIDSearch(criteria, nil)
	data, err := searchCmd.Wait()
	if err != nil {
		return nil, 0, false, fmt.Errorf("search: %w", err)
	}
	uids = data.AllUIDs()
	total = len(uids)
	if total == 0 {
		return uids, 0, false, nil
	}

	// Without SORT, approximate ARRIVAL using UID order (usually ascending by receipt).
	if sortKey == SortKeyArrival {
		if reverse {
			reverseUIDs(uids)
		}
		return uids, total, false, nil
	}

	// Other keys require FETCH of all matches before sorting (caller handles that).
	return uids, total, false, nil
}

// GetEmail fetches a full email by UID.
func (m *Manager) GetEmail(ctx context.Context, acc *types.Account, folder string, uid uint32) (*types.EmailMessage, error) {
	if folder == "" {
		folder = "INBOX"
	}

	logger := m.cfg.Logger.With("op", "get_email", "account_id", acc.ID, "folder", folder, "uid", uid)
	logger.DebugContext(ctx, "fetching email")
	start := time.Now()

	var em *types.EmailMessage
	err := m.withConn(ctx, acc, func(conn *pooledConn) error {
		_, err := conn.client.Select(folder, nil).Wait()
		if err != nil {
			logger.ErrorContext(ctx, "SELECT failed", "error", err, "elapsed", time.Since(start))
			return mapIMAPError(folder, fmt.Errorf("select %s: %w", folder, err))
		}
		conn.lastSelected = folder

		opts := &imap.FetchOptions{
			Envelope:      true,
			Flags:         true,
			BodyStructure: &imap.FetchItemBodyStructure{Extended: true},
			BodySection: []*imap.FetchItemBodySection{
				{Specifier: imap.PartSpecifierHeader, Peek: true},
				{Specifier: imap.PartSpecifierText, Peek: true},
			},
			RFC822Size: true,
		}

		msgs, err := conn.client.Fetch(imap.UIDSetNum(imap.UID(uid)), opts).Collect()
		if err != nil {
			logger.ErrorContext(ctx, "FETCH failed", "error", err, "elapsed", time.Since(start))
			return fmt.Errorf("fetch message: %w", err)
		}
		if len(msgs) == 0 {
			logger.WarnContext(ctx, "message not found", "elapsed", time.Since(start))
			return errors.New("message not found")
		}
		msg := msgs[0]

		em = &types.EmailMessage{
			EmailSummary: types.EmailSummary{
				UID:   uint32(msg.UID),
				Flags: flagStrings(msg.Flags),
				Size:  uint32(msg.RFC822Size),
			},
			Folder: folder,
		}

		em.Headers = map[string]string{}
		if msg.Envelope != nil {
			em.Subject = msg.Envelope.Subject
			em.From = convertAddresses(msg.Envelope.From)
			em.To = convertAddresses(msg.Envelope.To)
			em.Cc = convertAddresses(msg.Envelope.Cc)
			em.Date = msg.Envelope.Date
			if msg.Envelope.MessageID != "" {
				em.Headers["Message-ID"] = msg.Envelope.MessageID
			}
			if len(msg.Envelope.InReplyTo) > 0 {
				em.Headers["In-Reply-To"] = strings.Join(msg.Envelope.InReplyTo, " ")
			}
		}

		for _, bs := range msg.BodySection {
			if len(bs.Bytes) == 0 {
				continue
			}
			if bs.Section != nil && bs.Section.Specifier == imap.PartSpecifierHeader {
				parseMessageHeaders(em, bs.Bytes)
				continue
			}
			content := string(bs.Bytes)
			if em.HTML == "" && looksLikeHTML(content) {
				em.HTML = content
			} else if em.Text == "" {
				em.Text = content
			}
		}

		if msg.BodyStructure != nil {
			em.Attachments = extractAttachments(msg.BodyStructure)
			if hasAttachmentStructure(msg.BodyStructure) {
				em.HasAttach = true
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	logger.InfoContext(ctx, "fetched email",
		"size", em.Size, "has_attach", em.HasAttach,
		"attachments", len(em.Attachments), "elapsed", time.Since(start))
	return em, nil
}

// MoveEmails moves messages by UIDs to destination folder.
func (m *Manager) MoveEmails(ctx context.Context, acc *types.Account, folder string, uids []uint32, destFolder string) error {
	if len(uids) == 0 {
		return nil
	}
	if folder == "" {
		folder = "INBOX"
	}

	logger := m.cfg.Logger.With("op", "move_emails", "account_id", acc.ID,
		"folder", folder, "dest_folder", destFolder, "uid_count", len(uids))
	logger.DebugContext(ctx, "moving emails")
	start := time.Now()

	err := m.withConn(ctx, acc, func(conn *pooledConn) error {
		_, err := conn.client.Select(folder, nil).Wait()
		if err != nil {
			logger.ErrorContext(ctx, "SELECT source failed", "error", err, "elapsed", time.Since(start))
			return fmt.Errorf("select source: %w", err)
		}

		uidSet := imap.UIDSetNum(uidsToUIDs(uids)...)
		_, err = conn.client.Move(uidSet, destFolder).Wait()
		if err != nil {
			logger.ErrorContext(ctx, "MOVE failed", "error", err, "elapsed", time.Since(start))
			return fmt.Errorf("move: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}

	logger.InfoContext(ctx, "moved emails", "elapsed", time.Since(start))
	return nil
}

// FlagEmails adds or removes flags.
func (m *Manager) FlagEmails(ctx context.Context, acc *types.Account, folder string, uids []uint32, flags []string, add bool) error {
	if len(uids) == 0 {
		return nil
	}
	if folder == "" {
		folder = "INBOX"
	}

	logger := m.cfg.Logger.With("op", "flag_emails", "account_id", acc.ID,
		"folder", folder, "uid_count", len(uids), "flags", flags, "add", add)
	logger.DebugContext(ctx, "updating flags")
	start := time.Now()

	err := m.withConn(ctx, acc, func(conn *pooledConn) error {
		_, err := conn.client.Select(folder, nil).Wait()
		if err != nil {
			logger.ErrorContext(ctx, "SELECT failed", "error", err, "elapsed", time.Since(start))
			return fmt.Errorf("select: %w", err)
		}

		imapFlags := make([]imap.Flag, len(flags))
		for i, f := range flags {
			imapFlags[i] = imap.Flag(f)
		}

		storeOp := imap.StoreFlagsAdd
		if !add {
			storeOp = imap.StoreFlagsDel
		}

		uidSet := imap.UIDSetNum(uidsToUIDs(uids)...)
		storeCmd := conn.client.Store(uidSet, &imap.StoreFlags{
			Op:     storeOp,
			Silent: true,
			Flags:  imapFlags,
		}, nil)
		if err := storeCmd.Close(); err != nil {
			logger.ErrorContext(ctx, "STORE flags failed", "error", err, "elapsed", time.Since(start))
			return fmt.Errorf("store flags: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}

	logger.InfoContext(ctx, "flags updated", "elapsed", time.Since(start))
	return nil
}

// DeleteEmails marks messages deleted (they may still need EXPUNGE in some servers).
func (m *Manager) DeleteEmails(ctx context.Context, acc *types.Account, folder string, uids []uint32) error {
	return m.FlagEmails(ctx, acc, folder, uids, []string{string(imap.FlagDeleted)}, true)
}

// Helper functions

func summaryFromBuffer(msg *imapclient.FetchMessageBuffer) types.EmailSummary {
	if msg == nil {
		return types.EmailSummary{}
	}
	summary := types.EmailSummary{
		UID:   uint32(msg.UID),
		Flags: flagStrings(msg.Flags),
		Size:  uint32(msg.RFC822Size),
	}
	if msg.Envelope != nil {
		summary.Subject = msg.Envelope.Subject
		summary.From = convertAddresses(msg.Envelope.From)
		summary.To = convertAddresses(msg.Envelope.To)
		summary.Cc = convertAddresses(msg.Envelope.Cc)
		summary.Date = msg.Envelope.Date
	}
	if msg.BodyStructure != nil {
		summary.HasAttach = hasAttachmentStructure(msg.BodyStructure)
	}
	return summary
}

func flagStrings(flags []imap.Flag) []string {
	out := make([]string, len(flags))
	for i, f := range flags {
		out[i] = string(f)
	}
	return out
}

func convertAddresses(addrs []imap.Address) []types.Address {
	out := make([]types.Address, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, types.Address{Name: a.Name, Address: a.Addr()})
	}
	return out
}

func uidsToUIDs(uids []uint32) []imap.UID {
	res := make([]imap.UID, len(uids))
	for i, u := range uids {
		res[i] = imap.UID(u)
	}
	return res
}

func looksLikeHTML(content string) bool {
	s := strings.TrimSpace(content)
	if s == "" {
		return false
	}
	l := strings.ToLower(s)
	return strings.HasPrefix(l, "<html") || strings.HasPrefix(l, "<!doctype") ||
		strings.Contains(l, "<html") || strings.Contains(l, "<body") || strings.Contains(l, "<div")
}

func parseMessageHeaders(em *types.EmailMessage, raw []byte) {
	if em.Headers == nil {
		em.Headers = map[string]string{}
	}
	// Header block only; add a dummy body so net/mail can parse.
	if !bytes.Contains(raw, []byte("\r\n\r\n")) && !bytes.Contains(raw, []byte("\n\n")) {
		raw = append(append([]byte{}, raw...), []byte("\r\n\r\n")...)
	}
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return
	}
	for _, k := range []string{"Message-ID", "In-Reply-To", "References", "Reply-To"} {
		if v := msg.Header.Get(k); v != "" {
			em.Headers[k] = v
		}
	}
}
