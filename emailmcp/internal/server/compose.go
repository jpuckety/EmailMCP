package server

import (
	"context"
	"encoding/base64"

	"github.com/jpuckett/EmailMCP/emailmcp/internal/mailbuild"
	"github.com/jpuckett/EmailMCP/emailmcp/internal/smtp"
	"github.com/jpuckett/EmailMCP/emailmcp/internal/types"
)

// ComposeInput is the shared input for send_email and create_draft.
type ComposeInput struct {
	AccountID   string
	To          []string
	Cc          []string
	Bcc         []string
	Subject     string
	Text        string
	HTML        string
	From        string
	Attachments []types.AttachmentInput
	Folder      string // draft folder override

	ReplyToUID                 uint32
	ReplyFolder                string
	ReplyAll                   bool
	ForwardUID                 uint32
	ForwardFolder              string
	IncludeOriginalAttachments *bool
	QuoteOriginal              bool
	SaveToSent                 string
}

func (s *Server) resolveFrom(acc *types.Account, from string) string {
	if from != "" {
		return from
	}
	if acc.FromAddress != "" {
		return acc.FromAddress
	}
	return acc.SMTPUsername
}

func (s *Server) composeMessage(ctx context.Context, acc *types.Account, in ComposeInput, includeBcc bool) (mailbuild.Message, error) {
	msg := mailbuild.Message{
		From:             s.resolveFrom(acc, in.From),
		To:               in.To,
		Cc:               in.Cc,
		Bcc:              in.Bcc,
		Subject:          in.Subject,
		Text:             in.Text,
		HTML:             in.HTML,
		IncludeBccHeader: includeBcc,
	}

	if s.attachFetch != nil && len(in.Attachments) > 0 {
		atts, err := s.attachFetch.ResolveAll(ctx, in.Attachments)
		if err != nil {
			return msg, err
		}
		msg.Attachments = atts
	}

	if in.ReplyToUID != 0 {
		folder := in.ReplyFolder
		orig, err := s.imapMgr.GetEmail(ctx, acc, folder, in.ReplyToUID)
		if err != nil {
			return msg, err
		}
		mailbuild.ApplyReply(&msg, orig, in.ReplyAll, mailbuild.OwnAddresses(acc))
		if in.QuoteOriginal {
			from := ""
			if len(orig.From) > 0 {
				from = orig.From[0].Address
			}
			date := ""
			if !orig.Date.IsZero() {
				date = orig.Date.Format("Mon, 02 Jan 2006 15:04:05 -0700")
			}
			if orig.Text != "" || msg.Text != "" {
				msg.Text += mailbuild.QuotePlain(from, date, orig.Text)
			}
			if orig.HTML != "" || msg.HTML != "" {
				msg.HTML += mailbuild.QuoteHTML(from, date, orig.HTML)
			}
		}
	}

	if in.ForwardUID != 0 {
		folder := in.ForwardFolder
		orig, err := s.imapMgr.GetEmail(ctx, acc, folder, in.ForwardUID)
		if err != nil {
			return msg, err
		}
		includeAtt := true
		if in.IncludeOriginalAttachments != nil {
			includeAtt = *in.IncludeOriginalAttachments
		}
		mailbuild.ApplyForward(&msg, orig, in.QuoteOriginal)
		if includeAtt {
			for _, meta := range orig.Attachments {
				if meta.Inline {
					continue
				}
				fetched, err := s.imapMgr.GetAttachment(ctx, acc, orig.Folder, orig.UID, meta.PartID, meta.Filename)
				if err != nil {
					return msg, types.AttachmentFetchFailed(meta.Filename, err)
				}
				msg.Attachments = append(msg.Attachments, mailbuild.Attachment{
					Filename:    fetched.Filename,
					ContentType: fetched.ContentType,
					Content:     fetched.Content,
					Disposition: "attachment",
				})
			}
		}
	}

	msg.EnsureDefaults()
	return msg, nil
}

func (s *Server) buildLimits(acc *types.Account) mailbuild.Limits {
	max := int64(25 << 20)
	if s.cfg != nil && s.cfg.MaxMessageBytes > 0 {
		max = s.cfg.MaxMessageBytes
	}
	provMax, provName := smtp.ProviderMessageLimit(acc.SMTPHost)
	return mailbuild.Limits{MaxBytes: max, ProviderMax: provMax, ProviderName: provName}
}

func (s *Server) saveToSentMode(acc *types.Account, override string) string {
	if override != "" {
		return override
	}
	if acc != nil && acc.SaveToSent != "" {
		return acc.SaveToSent
	}
	if s.cfg != nil && s.cfg.SaveToSentDefault != "" {
		return s.cfg.SaveToSentDefault
	}
	return "auto"
}

func encodeInline(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}
