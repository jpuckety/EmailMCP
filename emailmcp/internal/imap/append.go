package imap

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/jpuckett/EmailMCP/emailmcp/internal/types"
)

// AppendResult is the UIDPLUS data from APPEND, if the server provided it.
type AppendResult struct {
	Folder      string
	UID         uint32
	UIDValidity uint32
}

// AppendMessage APPENDs raw RFC822 bytes to folder with the given flags.
func (m *Manager) AppendMessage(ctx context.Context, acc *types.Account, folder string, raw []byte, flags []imap.Flag) (*AppendResult, error) {
	if folder == "" {
		return nil, types.Validation("folder is required")
	}
	if len(raw) == 0 {
		return nil, types.Validation("message is empty")
	}

	logger := m.cfg.Logger.With("op", "append_message", "account_id", acc.ID, "folder", folder, "size", len(raw))
	start := time.Now()

	var res *AppendResult
	err := m.withConn(ctx, acc, func(conn *pooledConn) error {
		cmd := conn.client.Append(folder, int64(len(raw)), &imap.AppendOptions{
			Flags: flags,
			Time:  time.Now(),
		})
		if _, err := io.Copy(cmd, bytes.NewReader(raw)); err != nil {
			_ = cmd.Close()
			return mapIMAPError(folder, fmt.Errorf("append write: %w", err))
		}
		if err := cmd.Close(); err != nil {
			return mapIMAPError(folder, fmt.Errorf("append: %w", err))
		}
		data, err := cmd.Wait()
		if err != nil {
			return mapIMAPError(folder, fmt.Errorf("append: %w", err))
		}

		res = &AppendResult{Folder: folder}
		if data != nil {
			res.UID = uint32(data.UID)
			res.UIDValidity = data.UIDValidity
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	logger.InfoContext(ctx, "appended message", "uid", res.UID, "elapsed", time.Since(start))
	return res, nil
}

// AppendDraft stores a message in Drafts with the \Draft flag. It never sends.
func (m *Manager) AppendDraft(ctx context.Context, acc *types.Account, folder string, raw []byte) (*AppendResult, error) {
	var err error
	folder, err = m.FindDraftsFolder(ctx, acc, folder)
	if err != nil {
		return nil, err
	}
	return m.AppendMessage(ctx, acc, folder, raw, []imap.Flag{imap.FlagDraft})
}

// AppendSent stores a copy of a sent message in the Sent folder with \Seen.
func (m *Manager) AppendSent(ctx context.Context, acc *types.Account, raw []byte) (*AppendResult, error) {
	folder, err := m.FindSentFolder(ctx, acc)
	if err != nil {
		return nil, err
	}
	return m.AppendMessage(ctx, acc, folder, raw, []imap.Flag{imap.FlagSeen})
}
