package imap

import (
	"context"
	"fmt"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/jpuckett/EmailMCP/emailmcp/internal/types"
)

var (
	draftsNames = []string{"Drafts", "INBOX.Drafts", "INBOX/Drafts"}
	sentNames   = []string{"Sent", "Sent Items", "SentItems", "INBOX.Sent", "INBOX/Sent"}
)

// FindSpecialUseFolder locates a mailbox by SPECIAL-USE attribute, then by common names.
func (m *Manager) FindSpecialUseFolder(ctx context.Context, acc *types.Account, attr imap.MailboxAttr, fallbacks []string) (string, error) {
	logger := m.cfg.Logger.With("op", "find_special_use", "account_id", acc.ID, "attr", string(attr))
	var name string
	err := m.withConn(ctx, acc, func(conn *pooledConn) error {
		var err error
		name, err = findSpecialUseFolder(conn.client, attr, fallbacks)
		return err
	})
	if err != nil {
		return "", err
	}
	logger.InfoContext(ctx, "found special-use folder", "folder", name)
	return name, nil
}

func findSpecialUseFolder(client *imapclient.Client, attr imap.MailboxAttr, fallbacks []string) (string, error) {
	if client == nil {
		return "", fmt.Errorf("imap client is required")
	}
	cmd := client.List("", "*", &imap.ListOptions{ReturnSpecialUse: true})
	mailboxes, err := cmd.Collect()
	if err != nil {
		return "", mapIMAPError("", fmt.Errorf("list mailboxes: %w", err))
	}

	want := strings.ToLower(string(attr))
	for _, mb := range mailboxes {
		for _, a := range mb.Attrs {
			if strings.ToLower(string(a)) == want {
				return mb.Mailbox, nil
			}
		}
	}

	names := map[string]string{}
	for _, mb := range mailboxes {
		names[strings.ToLower(mb.Mailbox)] = mb.Mailbox
	}
	for _, candidate := range fallbacks {
		if got, ok := names[strings.ToLower(candidate)]; ok {
			return got, nil
		}
	}

	return "", types.FolderNotFound(string(attr), fmt.Errorf("no mailbox with %s and none of %v", attr, fallbacks))
}

// FindDraftsFolder returns the Drafts mailbox, or folder if override is set.
func (m *Manager) FindDraftsFolder(ctx context.Context, acc *types.Account, override string) (string, error) {
	if override != "" {
		return override, nil
	}
	return m.FindSpecialUseFolder(ctx, acc, imap.MailboxAttrDrafts, draftsNames)
}

// FindSentFolder returns the Sent mailbox.
func (m *Manager) FindSentFolder(ctx context.Context, acc *types.Account) (string, error) {
	return m.FindSpecialUseFolder(ctx, acc, imap.MailboxAttrSent, sentNames)
}

func mapIMAPError(folder string, err error) error {
	if err == nil {
		return nil
	}
	s := strings.ToLower(err.Error())
	if strings.Contains(s, "auth") || strings.Contains(s, "login") || strings.Contains(s, "authentication failed") {
		return types.AuthFailed("IMAP", err)
	}
	if folder != "" && (strings.Contains(s, "no such mailbox") || strings.Contains(s, "nonexistent") ||
		strings.Contains(s, "trycreate") || strings.Contains(s, "doesn't exist") || strings.Contains(s, "does not exist")) {
		return types.FolderNotFound(folder, err)
	}
	return err
}
