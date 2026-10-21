package imap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	defaultMaxConns = 4
	defaultIdle     = 4 * time.Minute
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
	idle     []*pooledConn
	inUse    int
	maxConns int
	idleT    time.Duration
	logger   *slog.Logger
	closed   bool
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
	Host     string
	Port     int
	Username string
	Password string
	UseTLS   bool
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

	m.pools[key] = p
	logger.Info("imap pool created", "max_conns", p.maxConns, "idle_timeout", p.idleT)
	return p, nil
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
	p.mu.Lock()
	defer p.mu.Unlock()

	logger := p.logger.With("op", "pool_acquire", "account_id", p.accountID)

	if p.closed {
		logger.WarnContext(ctx, "acquire on closed pool")
		return nil, errors.New("pool closed")
	}

	logger.DebugContext(ctx, "acquiring connection",
		"idle", len(p.idle), "in_use", p.inUse, "max_conns", p.maxConns)

	// Try idle first
	for i := len(p.idle) - 1; i >= 0; i-- {
		c := p.idle[i]
		p.idle = p.idle[:i]
		if time.Since(c.lastUsed) > p.idleT {
			logger.DebugContext(ctx, "discarding idle connection past idle timeout",
				"idle_for", time.Since(c.lastUsed))
			c.client.Close()
			continue
		}
		// Quick health check
		if err := c.client.Noop().Wait(); err != nil {
			logger.WarnContext(ctx, "idle connection failed NOOP; discarding", "error", err)
			c.client.Close()
			continue
		}
		p.inUse++
		c.lastUsed = time.Now()
		logger.DebugContext(ctx, "reusing idle connection", "in_use", p.inUse)
		return c, nil
	}

	// Create new if capacity allows
	if p.inUse >= p.maxConns {
		logger.WarnContext(ctx, "connection limit reached",
			"in_use", p.inUse, "max_conns", p.maxConns)
		return nil, errors.New("imap connection limit reached for account")
	}

	conn, err := p.dialAndLogin(ctx)
	if err != nil {
		return nil, err
	}
	p.inUse++
	logger.DebugContext(ctx, "new connection created", "in_use", p.inUse)
	return conn, nil
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

	if hadError || p.closed || conn.client == nil {
		if conn.client != nil {
			logger.Debug("closing connection on release",
				"had_error", hadError, "pool_closed", p.closed, "in_use", p.inUse)
			conn.client.Close()
		}
		return
	}

	// Return to idle
	conn.lastUsed = time.Now()
	p.idle = append(p.idle, conn)
	logger.Debug("connection returned to idle pool",
		"idle", len(p.idle), "in_use", p.inUse)

	// Trim excess idle connections (keep at most maxConns)
	for len(p.idle) > p.maxConns {
		c := p.idle[0]
		p.idle = p.idle[1:]
		logger.Debug("trimming excess idle connection", "idle", len(p.idle))
		c.client.Close()
	}
}

func (p *accountPool) closeAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true

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

// --- High level operations ---

// ListFolders lists available mailboxes for the account.
func (m *Manager) ListFolders(ctx context.Context, acc *types.Account) ([]types.Folder, error) {
	logger := m.cfg.Logger.With("op", "list_folders", "account_id", acc.ID)
	logger.DebugContext(ctx, "listing folders")

	start := time.Now()

	conn, err := m.Acquire(ctx, acc)
	if err != nil {
		logger.ErrorContext(ctx, "failed to acquire imap connection", "error", err)
		return nil, err
	}
	released := false
	defer func() {
		if !released {
			m.Release(acc, conn, false)
		}
	}()

	logger.DebugContext(ctx, "issuing LIST command", "reference", "", "pattern", "%")
	cmd := conn.client.List("", "%", nil)
	mailboxes, err := cmd.Collect()
	if err != nil {
		logger.ErrorContext(ctx, "LIST command failed",
			"error", err,
			"elapsed", time.Since(start))
		m.Release(acc, conn, true)
		released = true
		return nil, fmt.Errorf("list mailboxes: %w", err)
	}

	folders := make([]types.Folder, 0, len(mailboxes))
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

	conn, err := m.Acquire(ctx, acc)
	if err != nil {
		logger.ErrorContext(ctx, "failed to acquire imap connection", "error", err)
		return nil, err
	}
	hadErr := false
	defer func() {
		m.Release(acc, conn, hadErr)
	}()

	// Select folder (we track to reduce churn but still select each time for safety)
	_, err = conn.client.Select(folder, nil).Wait()
	if err != nil {
		hadErr = true
		logger.ErrorContext(ctx, "SELECT failed", "error", err, "elapsed", time.Since(start))
		return nil, fmt.Errorf("select folder %s: %w", folder, err)
	}
	conn.lastSelected = folder

	uidList, totalMatches, usedServerSort, err := searchSortedUIDs(conn.client, &criteria, sortKey, reverse)
	if err != nil {
		hadErr = true
		logger.ErrorContext(ctx, "search/sort failed", "error", err, "elapsed", time.Since(start))
		return nil, err
	}
	if len(uidList) == 0 {
		logger.InfoContext(ctx, "search returned no results", "elapsed", time.Since(start))
		return []types.EmailSummary{}, nil
	}

	// Server SORT (or arrival UID heuristic) can limit before FETCH.
	// Other client-side sorts need envelopes for all matches first.
	limitBeforeFetch := usedServerSort || sortKey == SortKeyArrival
	if limitBeforeFetch && limit > 0 && len(uidList) > limit {
		uidList = uidList[:limit]
	}
	logger.DebugContext(ctx, "fetching summaries",
		"total_matches", totalMatches,
		"fetching", len(uidList),
		"server_sort", usedServerSort,
	)

	// Fetch summaries (Envelope + Flags + RFC822.SIZE)
	fetchOpts := &imap.FetchOptions{
		Envelope:      true,
		Flags:         true,
		BodyStructure: &imap.FetchItemBodyStructure{},
		BodySection: []*imap.FetchItemBodySection{
			{Specifier: imap.PartSpecifierHeader},
		},
		RFC822Size: true,
	}

	fetchCmd := conn.client.Fetch(imap.UIDSetNum(uidList...), fetchOpts)

	msgs, err := fetchCmd.Collect()
	if err != nil {
		hadErr = true
		logger.ErrorContext(ctx, "FETCH summaries failed", "error", err, "elapsed", time.Since(start))
		return nil, fmt.Errorf("fetch summaries: %w", err)
	}

	summaries := make([]types.EmailSummary, 0, len(msgs))
	for _, msg := range msgs {
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
		// Basic detection of attachments via body structure if present
		if msg.BodyStructure != nil {
			summary.HasAttach = hasAttachmentStructure(msg.BodyStructure)
		}
		summaries = append(summaries, summary)
	}

	if !limitBeforeFetch {
		sortSummaries(summaries, sortKey, reverse)
		if limit > 0 && len(summaries) > limit {
			summaries = summaries[:limit]
		}
	} else {
		// FETCH does not guarantee response order — re-apply search/sort order.
		summaries = orderSummariesByUIDs(summaries, uidList)
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

	conn, err := m.Acquire(ctx, acc)
	if err != nil {
		logger.ErrorContext(ctx, "failed to acquire imap connection", "error", err)
		return nil, err
	}
	hadErr := false
	defer func() {
		m.Release(acc, conn, hadErr)
	}()

	_, err = conn.client.Select(folder, nil).Wait()
	if err != nil {
		hadErr = true
		logger.ErrorContext(ctx, "SELECT failed", "error", err, "elapsed", time.Since(start))
		return nil, mapIMAPError(folder, fmt.Errorf("select %s: %w", folder, err))
	}
	conn.lastSelected = folder

	// Fetch full body + envelope + structure + headers (Message-ID, References).
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
		hadErr = true
		logger.ErrorContext(ctx, "FETCH failed", "error", err, "elapsed", time.Since(start))
		return nil, fmt.Errorf("fetch message: %w", err)
	}
	if len(msgs) == 0 {
		logger.WarnContext(ctx, "message not found", "elapsed", time.Since(start))
		return nil, errors.New("message not found")
	}
	msg := msgs[0]

	em := &types.EmailMessage{
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

	// Extract attachments info from body structure (best effort)
	if msg.BodyStructure != nil {
		em.Attachments = extractAttachments(msg.BodyStructure)
		if hasAttachmentStructure(msg.BodyStructure) {
			em.HasAttach = true
		}
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

	conn, err := m.Acquire(ctx, acc)
	if err != nil {
		logger.ErrorContext(ctx, "failed to acquire imap connection", "error", err)
		return err
	}
	hadErr := false
	defer func() { m.Release(acc, conn, hadErr) }()

	_, err = conn.client.Select(folder, nil).Wait()
	if err != nil {
		hadErr = true
		logger.ErrorContext(ctx, "SELECT source failed", "error", err, "elapsed", time.Since(start))
		return fmt.Errorf("select source: %w", err)
	}

	uidSet := imap.UIDSetNum(uidsToUIDs(uids)...)
	_, err = conn.client.Move(uidSet, destFolder).Wait()
	if err != nil {
		hadErr = true
		logger.ErrorContext(ctx, "MOVE failed", "error", err, "elapsed", time.Since(start))
		return fmt.Errorf("move: %w", err)
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

	conn, err := m.Acquire(ctx, acc)
	if err != nil {
		logger.ErrorContext(ctx, "failed to acquire imap connection", "error", err)
		return err
	}
	hadErr := false
	defer func() { m.Release(acc, conn, hadErr) }()

	_, err = conn.client.Select(folder, nil).Wait()
	if err != nil {
		hadErr = true
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
		hadErr = true
		logger.ErrorContext(ctx, "STORE flags failed", "error", err, "elapsed", time.Since(start))
		return fmt.Errorf("store flags: %w", err)
	}

	logger.InfoContext(ctx, "flags updated", "elapsed", time.Since(start))
	return nil
}

// DeleteEmails marks messages deleted (they may still need EXPUNGE in some servers).
func (m *Manager) DeleteEmails(ctx context.Context, acc *types.Account, folder string, uids []uint32) error {
	return m.FlagEmails(ctx, acc, folder, uids, []string{string(imap.FlagDeleted)}, true)
}

// Helper functions

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
