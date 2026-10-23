package imap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/jpuckett/EmailMCP/emailmcp/internal/netutil"
	"github.com/jpuckett/EmailMCP/emailmcp/internal/types"
)

const (
	DefaultIdleRefresh = 25 * time.Minute
	idleDoneWait       = 5 * time.Second
)

// ErrIdleUnsupported is returned when the server does not advertise IDLE.
var ErrIdleUnsupported = errors.New("imap IDLE is not supported")

// SelectSnapshot is UID state after SELECT, used to watermark notify watches.
type SelectSnapshot struct {
	Folder      string
	UIDNext     uint32
	UIDValidity uint32
	NumMessages uint32
}

// IdleConn is a dedicated IMAP connection for IDLE. It must not be returned
// to the request-scoped account pool (whose idle timeout would kill IDLE).
type IdleConn struct {
	mu     sync.Mutex
	client *imapclient.Client
	logger *slog.Logger
	exists chan struct{}
}

// DialAccount opens a dedicated IMAP connection for folder watching.
func DialAccount(ctx context.Context, acc *types.Account, logger *slog.Logger, skipHostValidation bool) (*IdleConn, error) {
	if acc == nil {
		return nil, errors.New("account is required")
	}
	if acc.IMAPPassword == "" {
		return nil, errors.New("imap password is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !skipHostValidation {
		if err := netutil.ValidatePublicHost(acc.IMAPHost); err != nil {
			return nil, fmt.Errorf("imap host not allowed: %w", err)
		}
	}
	if err := netutil.RequireTLSUnlessLocalhost(acc.IMAPHost, acc.IMAPUseTLS, "IMAP"); err != nil {
		return nil, err
	}

	port := acc.IMAPPort
	if port <= 0 {
		if acc.IMAPUseTLS {
			port = 993
		} else {
			port = 143
		}
	}
	addr := fmt.Sprintf("%s:%d", acc.IMAPHost, port)
	exists := make(chan struct{}, 1)
	opts := &imapclient.Options{
		UnilateralDataHandler: &imapclient.UnilateralDataHandler{
			Mailbox: func(data *imapclient.UnilateralDataMailbox) {
				if mailboxExistsUpdate(data) {
					notifyExists(exists)
				}
			},
		},
	}

	var (
		client *imapclient.Client
		err    error
	)
	if acc.IMAPUseTLS {
		client, err = imapclient.DialTLS(addr, opts)
	} else {
		client, err = imapclient.DialInsecure(addr, opts)
	}
	if err != nil {
		return nil, fmt.Errorf("dial imap %s: %w", addr, err)
	}
	if err := client.Login(acc.IMAPUsername, acc.IMAPPassword).Wait(); err != nil {
		client.Close()
		return nil, types.AuthFailed("IMAP", err)
	}
	return &IdleConn{client: client, logger: logger, exists: exists}, nil
}

// Select selects folder and returns UIDNEXT / UIDVALIDITY for watermarking.
func (c *IdleConn) Select(_ context.Context, folder string) (SelectSnapshot, error) {
	client, err := c.alive()
	if err != nil {
		return SelectSnapshot{}, err
	}
	if folder == "" {
		folder = "INBOX"
	}
	data, err := client.Select(folder, nil).Wait()
	if err != nil {
		return SelectSnapshot{}, mapIMAPError(folder, fmt.Errorf("select: %w", err))
	}
	snap := SelectSnapshot{Folder: folder}
	if data != nil {
		snap.UIDNext = uint32(data.UIDNext)
		snap.UIDValidity = data.UIDValidity
		snap.NumMessages = data.NumMessages
	}
	if err := resolveMissingUIDNext(client, folder, &snap); err != nil {
		return SelectSnapshot{}, err
	}
	return snap, nil
}

// SupportsIdle reports whether the server advertised IDLE or IMAP4rev2.
func (c *IdleConn) SupportsIdle() bool {
	client, err := c.alive()
	if err != nil {
		return false
	}
	caps := client.Caps()
	return caps.Has(imap.CapIdle) || caps.Has(imap.CapIMAP4rev2)
}

// Idle blocks until EXISTS, refresh, or ctx cancellation. The caller must
// FetchAfter and then Idle again. Refresh should be ~25 minutes.
// The bool is true when the keep-alive timer fired rather than an EXISTS update.
func (c *IdleConn) Idle(ctx context.Context, refresh time.Duration) (bool, error) {
	client, err := c.alive()
	if err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if !c.SupportsIdle() {
		return false, ErrIdleUnsupported
	}
	if refresh <= 0 {
		refresh = DefaultIdleRefresh
	}
	cmd, err := client.Idle()
	if err != nil {
		return false, err
	}
	timer := time.NewTimer(refresh)
	defer timer.Stop()
	select {
	case <-c.exists:
		if err := cmd.Close(); err != nil {
			return false, err
		}
		return false, waitIdleDone(cmd)
	case <-timer.C:
		if err := cmd.Close(); err != nil {
			return true, err
		}
		return true, waitIdleDone(cmd)
	case <-ctx.Done():
		c.ForceClose()
		return false, ctx.Err()
	}
}

// FetchAfter returns envelope summaries for UIDs lastUID+1:*.
func (c *IdleConn) FetchAfter(ctx context.Context, lastUID uint32) ([]types.EmailSummary, error) {
	client, err := c.alive()
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	set := UIDSetAfter(lastUID)
	data, err := client.UIDSearch(&imap.SearchCriteria{UID: []imap.UIDSet{set}}, nil).Wait()
	if err != nil {
		return nil, fmt.Errorf("uid search: %w", err)
	}
	uids := data.AllUIDs()
	if len(uids) == 0 {
		return []types.EmailSummary{}, nil
	}
	filtered := make([]imap.UID, 0, len(uids))
	for _, u := range uids {
		if uint32(u) > lastUID {
			filtered = append(filtered, u)
		}
	}
	if len(filtered) == 0 {
		return []types.EmailSummary{}, nil
	}
	sort.Slice(filtered, func(i, j int) bool { return filtered[i] < filtered[j] })

	fetchOpts := &imap.FetchOptions{
		Envelope:      true,
		Flags:         true,
		BodyStructure: &imap.FetchItemBodyStructure{},
		UID:           true,
	}
	msgs, err := client.Fetch(imap.UIDSetNum(filtered...), fetchOpts).Collect()
	if err != nil {
		return nil, fmt.Errorf("fetch envelopes: %w", err)
	}
	out := make([]types.EmailSummary, 0, len(msgs))
	for _, msg := range msgs {
		sum := summaryFromBuffer(msg)
		if sum.UID > lastUID {
			out = append(out, sum)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	return out, nil
}

// FindSentFolder resolves the Sent mailbox on this dedicated connection.
func (c *IdleConn) FindSentFolder(_ context.Context) (string, error) {
	client, err := c.alive()
	if err != nil {
		return "", err
	}
	return findSpecialUseFolder(client, imap.MailboxAttrSent, sentNames)
}

// Close logs out and closes the dedicated connection.
func (c *IdleConn) Close() error {
	client := c.takeClient()
	if client == nil {
		return nil
	}
	if err := client.Logout().Wait(); err != nil {
		_ = client.Close()
		return err
	}
	return client.Close()
}

// ForceClose closes the TCP connection without waiting for LOGOUT/IDLE DONE.
func (c *IdleConn) ForceClose() {
	client := c.takeClient()
	if client != nil {
		_ = client.Close()
	}
}

// UIDSetAfter returns the IMAP UID set lastUID+1:*. lastUID 0 means 1:*.
func UIDSetAfter(lastUID uint32) imap.UIDSet {
	var set imap.UIDSet
	start := imap.UID(1)
	if lastUID > 0 {
		if lastUID == ^uint32(0) {
			set.AddNum(imap.UID(lastUID))
			return set
		}
		start = imap.UID(lastUID + 1)
	}
	set.AddRange(start, 0)
	return set
}

func mailboxExistsUpdate(data *imapclient.UnilateralDataMailbox) bool {
	return data != nil && data.NumMessages != nil
}

func notifyExists(ch chan struct{}) {
	if ch == nil {
		return
	}
	select {
	case ch <- struct{}{}:
	default:
	}
}

// SnapshotLastUID returns UIDNEXT-1 for the first-watch / UIDVALIDITY reset
// watermark (no historical backfill).
func SnapshotLastUID(uidNext uint32) uint32 {
	if uidNext == 0 {
		return 0
	}
	return uidNext - 1
}

// ResolvedUIDNext picks UIDNEXT from SELECT, STATUS, or the mailbox max UID.
// Empty mailboxes without UIDNEXT are treated as UIDNEXT=1 so last_uid=0.
func ResolvedUIDNext(selectUIDNext, statusUIDNext, maxUID, numMessages uint32) (uint32, error) {
	if selectUIDNext != 0 {
		return selectUIDNext, nil
	}
	if statusUIDNext != 0 {
		return statusUIDNext, nil
	}
	if maxUID != 0 {
		if maxUID == ^uint32(0) {
			return 0, errors.New("uidnext overflow")
		}
		return maxUID + 1, nil
	}
	if numMessages == 0 {
		return 1, nil
	}
	return 0, errors.New("uidnext unavailable")
}

func resolveMissingUIDNext(client *imapclient.Client, folder string, snap *SelectSnapshot) error {
	if snap.UIDNext != 0 {
		return nil
	}
	var statusNext uint32
	st, err := client.Status(folder, &imap.StatusOptions{UIDNext: true, UIDValidity: true}).Wait()
	if err == nil && st != nil {
		if st.UIDValidity != 0 {
			snap.UIDValidity = st.UIDValidity
		}
		statusNext = uint32(st.UIDNext)
	}
	var maxUID uint32
	if statusNext == 0 {
		data, err := client.UIDSearch(&imap.SearchCriteria{}, &imap.SearchOptions{ReturnMax: true}).Wait()
		if err == nil && data != nil {
			maxUID = data.Max
			if maxUID == 0 {
				for _, u := range data.AllUIDs() {
					if uint32(u) > maxUID {
						maxUID = uint32(u)
					}
				}
			}
		}
	}
	next, err := ResolvedUIDNext(0, statusNext, maxUID, snap.NumMessages)
	if err != nil {
		return fmt.Errorf("mailbox %q: %w", folder, err)
	}
	snap.UIDNext = next
	return nil
}

func (c *IdleConn) alive() (*imapclient.Client, error) {
	if c == nil {
		return nil, errors.New("idle connection is closed")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client == nil {
		return nil, errors.New("idle connection is closed")
	}
	return c.client, nil
}

func (c *IdleConn) takeClient() *imapclient.Client {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	client := c.client
	c.client = nil
	return client
}

func waitIdleDone(cmd *imapclient.IdleCommand) error {
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(idleDoneWait):
		return errors.New("idle wait timed out")
	}
}
