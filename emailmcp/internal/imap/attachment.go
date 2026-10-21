package imap

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"mime/quotedprintable"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/jpuckett/EmailMCP/emailmcp/internal/types"
)

// FetchedAttachment is the decoded bytes of one body part.
type FetchedAttachment struct {
	Filename    string
	ContentType string
	Size        int64
	Content     []byte
	PartID      string
}

// GetAttachment fetches one attachment by part_id / attachment_id / filename.
func (m *Manager) GetAttachment(ctx context.Context, acc *types.Account, folder string, uid uint32, partID, filename string) (*FetchedAttachment, error) {
	if folder == "" {
		folder = "INBOX"
	}
	if uid == 0 {
		return nil, types.Validation("uid is required")
	}
	if partID == "" && filename == "" {
		return nil, types.Validation("attachment_id, part_id, or filename is required")
	}

	logger := m.cfg.Logger.With("op", "get_attachment", "account_id", acc.ID, "folder", folder, "uid", uid)
	start := time.Now()

	msg, err := m.GetEmail(ctx, acc, folder, uid)
	if err != nil {
		return nil, err
	}
	var target *types.Attachment
	for i := range msg.Attachments {
		a := &msg.Attachments[i]
		if partID != "" && (a.PartID == partID || a.ID == partID) {
			target = a
			break
		}
	}
	if target == nil && filename != "" {
		for i := range msg.Attachments {
			a := &msg.Attachments[i]
			if strings.EqualFold(a.Filename, filename) {
				target = a
				break
			}
		}
	}
	if target == nil {
		return nil, types.NewError(types.CodeAttachmentFetch, "attachment not found on message", map[string]any{
			"uid": uid, "part_id": partID, "filename": filename,
		})
	}

	partNums, err := parsePartID(target.PartID)
	if err != nil {
		return nil, err
	}

	conn, err := m.Acquire(ctx, acc)
	if err != nil {
		return nil, err
	}
	hadErr := false
	defer func() { m.Release(acc, conn, hadErr) }()

	_, err = conn.client.Select(folder, nil).Wait()
	if err != nil {
		hadErr = true
		return nil, mapIMAPError(folder, fmt.Errorf("select %s: %w", folder, err))
	}

	opts := &imap.FetchOptions{
		BodyStructure: &imap.FetchItemBodyStructure{Extended: true},
		BodySection: []*imap.FetchItemBodySection{
			{Part: partNums, Peek: true},
		},
	}
	msgs, err := conn.client.Fetch(imap.UIDSetNum(imap.UID(uid)), opts).Collect()
	if err != nil {
		hadErr = true
		return nil, mapIMAPError(folder, fmt.Errorf("fetch attachment: %w", err))
	}
	if len(msgs) == 0 {
		return nil, types.NewError(types.CodeMessageNotFound, "message not found", map[string]any{"uid": uid})
	}
	fetched := msgs[0]
	encoding := partEncoding(fetched.BodyStructure, partNums)

	var raw []byte
	for _, sec := range fetched.BodySection {
		if len(sec.Bytes) > 0 {
			raw = sec.Bytes
			break
		}
	}
	decoded := decodeTransfer(encoding, raw)
	logger.InfoContext(ctx, "fetched attachment",
		"part_id", target.PartID, "size", len(decoded), "elapsed", time.Since(start))
	return &FetchedAttachment{
		Filename:    target.Filename,
		ContentType: target.ContentType,
		Size:        int64(len(decoded)),
		Content:     decoded,
		PartID:      target.PartID,
	}, nil
}

func parsePartID(id string) ([]int, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return []int{1}, nil
	}
	parts := strings.Split(id, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 {
			return nil, types.Validation("invalid part_id")
		}
		out = append(out, n)
	}
	return out, nil
}

func partIDString(path []int) string {
	if len(path) == 0 {
		return "1"
	}
	ss := make([]string, len(path))
	for i, n := range path {
		ss[i] = strconv.Itoa(n)
	}
	return strings.Join(ss, ".")
}

func partEncoding(bs imap.BodyStructure, want []int) string {
	var enc string
	if bs == nil {
		return enc
	}
	bs.Walk(func(path []int, part imap.BodyStructure) bool {
		if !pathEqual(path, want) {
			return true
		}
		if sp, ok := part.(*imap.BodyStructureSinglePart); ok {
			enc = sp.Encoding
		}
		return false
	})
	return enc
}

func pathEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func decodeTransfer(encoding string, data []byte) []byte {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "base64":
		trimmed := bytes.Map(func(r rune) rune {
			if r == '\r' || r == '\n' || r == ' ' || r == '\t' {
				return -1
			}
			return r
		}, data)
		if decoded, err := base64.StdEncoding.DecodeString(string(trimmed)); err == nil {
			return decoded
		}
		return data
	case "quoted-printable":
		decoded, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(data)))
		if err == nil {
			return decoded
		}
		return data
	default:
		return data
	}
}

func extractAttachments(bs imap.BodyStructure) []types.Attachment {
	var atts []types.Attachment
	if bs == nil {
		return atts
	}
	bs.Walk(func(path []int, part imap.BodyStructure) bool {
		sp, ok := part.(*imap.BodyStructureSinglePart)
		if !ok {
			return true
		}
		media := strings.ToLower(sp.MediaType())
		filename := sp.Filename()
		disp := ""
		inline := false
		if d := sp.Disposition(); d != nil {
			disp = strings.ToLower(d.Value)
			inline = disp == "inline"
			if filename == "" && d.Params != nil {
				filename = d.Params["filename"]
			}
		}
		cid := strings.Trim(sp.ID, "<>")
		isBody := (media == "text/plain" || media == "text/html") && filename == "" && disp == "" && cid == ""
		if isBody {
			return true
		}
		isAtt := filename != "" || disp == "attachment" || inline || cid != "" ||
			(!strings.HasPrefix(media, "text/") && !strings.HasPrefix(media, "multipart/"))
		if !isAtt {
			return true
		}
		id := partIDString(path)
		atts = append(atts, types.Attachment{
			ID:          id,
			PartID:      id,
			Filename:    filename,
			ContentType: media,
			Size:        int64(sp.Size),
			Disposition: disp,
			ContentID:   cid,
			Inline:      inline || cid != "",
		})
		return true
	})
	return atts
}

func hasAttachmentStructure(bs imap.BodyStructure) bool {
	return len(extractAttachments(bs)) > 0
}
