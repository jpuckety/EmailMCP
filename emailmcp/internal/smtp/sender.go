package smtp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/smtp"
	"net/textproto"
	"strings"
	"time"

	"github.com/jpuckett/EmailMCP/emailmcp/internal/mailbuild"
	"github.com/jpuckett/EmailMCP/emailmcp/internal/netutil"
	"github.com/jpuckett/EmailMCP/emailmcp/internal/types"
)

// WorkMailMIMEMax is Amazon WorkMail's hard quota for encoded MIME messages (40 MB).
const WorkMailMIMEMax int64 = 40 << 20

// Config for SMTP sender.
type Config struct {
	DefaultTimeout     time.Duration
	Logger             *slog.Logger
	MaxMessageBytes    int64
	SkipHostValidation bool
}

// Sender manages SMTP operations.
type Sender struct {
	cfg Config
}

// NewSender creates a new SMTP sender.
func NewSender(cfg Config) *Sender {
	if cfg.DefaultTimeout <= 0 {
		cfg.DefaultTimeout = 30 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.MaxMessageBytes <= 0 {
		cfg.MaxMessageBytes = 25 << 20
	}
	return &Sender{cfg: cfg}
}

// SendMessage builds and sends msg, returning Message-ID and recipient status.
func (s *Sender) SendMessage(ctx context.Context, acc *types.Account, msg mailbuild.Message) (*types.SendResult, error) {
	if acc == nil {
		return nil, errors.New("account is required")
	}
	if !s.cfg.SkipHostValidation {
		if err := netutil.ValidatePublicHost(acc.SMTPHost); err != nil {
			return nil, fmt.Errorf("smtp host not allowed: %w", err)
		}
	}
	if err := netutil.RequireTLSUnlessLocalhost(acc.SMTPHost, acc.SMTPUseTLS, "SMTP"); err != nil {
		return nil, err
	}

	smtpPass := acc.SMTPPassword
	if smtpPass == "" {
		smtpPass = acc.IMAPPassword
	}
	if smtpPass == "" {
		return nil, types.AuthFailed("SMTP", errors.New("smtp password is required"))
	}

	if msg.From == "" {
		msg.From = acc.FromAddress
	}
	if msg.From == "" {
		msg.From = acc.SMTPUsername
	}
	if msg.From == "" {
		return nil, types.Validation("no from address available")
	}

	msg.EnsureDefaults()
	provMax, provName := ProviderMessageLimit(acc.SMTPHost)
	raw, err := mailbuild.BuildWithLimits(msg, mailbuild.Limits{
		MaxBytes:     s.cfg.MaxMessageBytes,
		ProviderMax:  provMax,
		ProviderName: provName,
	})
	if err != nil {
		return nil, err
	}
	return s.SendRaw(ctx, acc, msg.From, uniqueRecipients(msg.To, msg.Cc, msg.Bcc), raw, msg.MessageID)
}

// SendRaw transmits a pre-built RFC822 message. raw is the same blob used for IMAP APPEND.
func (s *Sender) SendRaw(ctx context.Context, acc *types.Account, from string, recipients []string, raw []byte, messageID string) (*types.SendResult, error) {
	if acc == nil {
		return nil, errors.New("account is required")
	}
	if !s.cfg.SkipHostValidation {
		if err := netutil.ValidatePublicHost(acc.SMTPHost); err != nil {
			return nil, fmt.Errorf("smtp host not allowed: %w", err)
		}
	}
	if err := netutil.RequireTLSUnlessLocalhost(acc.SMTPHost, acc.SMTPUseTLS, "SMTP"); err != nil {
		return nil, err
	}
	smtpPass := acc.SMTPPassword
	if smtpPass == "" {
		smtpPass = acc.IMAPPassword
	}
	if smtpPass == "" {
		return nil, types.AuthFailed("SMTP", errors.New("smtp password is required"))
	}
	if from == "" {
		from = acc.FromAddress
	}
	if from == "" {
		from = acc.SMTPUsername
	}
	recipients = uniqueRecipients(recipients)
	if len(recipients) == 0 {
		return nil, types.Validation("at least one recipient (to, cc, or bcc) is required")
	}
	if messageID == "" {
		messageID = mailbuild.ExtractHeader(raw, "Message-ID")
	}

	addr := fmt.Sprintf("%s:%d", acc.SMTPHost, acc.SMTPPort)
	auth := smtp.PlainAuth("", acc.SMTPUsername, smtpPass, acc.SMTPHost)

	timeoutCtx, cancel := context.WithTimeout(ctx, s.cfg.DefaultTimeout)
	defer cancel()

	type result struct {
		accepted []string
		rejected []string
		err      error
	}
	done := make(chan result, 1)
	go func() {
		accepted, rejected, sendErr := s.deliver(timeoutCtx, acc, addr, auth, envelopeAddr(from), recipients, raw)
		done <- result{accepted, rejected, sendErr}
	}()

	select {
	case <-timeoutCtx.Done():
		return nil, fmt.Errorf("smtp send timeout: %w", timeoutCtx.Err())
	case r := <-done:
		if r.err != nil {
			return nil, r.err
		}
		if len(r.accepted) == 0 {
			return nil, types.RejectedRecipients(r.accepted, r.rejected)
		}
		s.cfg.Logger.Info("email sent",
			"account_id", acc.ID,
			"accepted", len(r.accepted),
			"rejected", len(r.rejected),
			"size", len(raw),
			"message_id", messageID,
		)
		return &types.SendResult{
			MessageID: messageID,
			Accepted:  r.accepted,
			Rejected:  r.rejected,
		}, nil
	}
}

func (s *Sender) deliver(ctx context.Context, acc *types.Account, addr string, auth smtp.Auth, from string, recipients []string, raw []byte) (accepted, rejected []string, err error) {
	tlsConfig := &tls.Config{
		ServerName: acc.SMTPHost,
		MinVersion: tls.VersionTLS12,
	}

	var conn net.Conn
	dialer := &net.Dialer{Timeout: s.cfg.DefaultTimeout}
	if acc.SMTPUseTLS && acc.SMTPPort == 465 {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, tlsConfig)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("smtp dial: %w", err)
	}

	client, err := smtp.NewClient(conn, acc.SMTPHost)
	if err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("smtp client: %w", err)
	}
	defer client.Close()

	if acc.SMTPUseTLS && acc.SMTPPort != 465 {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(tlsConfig); err != nil {
				return nil, nil, fmt.Errorf("smtp starttls: %w", err)
			}
		}
	}

	if err := client.Auth(auth); err != nil {
		return nil, nil, types.AuthFailed("SMTP", err)
	}
	if err := client.Mail(from); err != nil {
		return nil, nil, classifySMTP(err)
	}

	for _, rcpt := range recipients {
		if err := client.Rcpt(rcpt); err != nil {
			rejected = append(rejected, rcpt+": "+err.Error())
			continue
		}
		accepted = append(accepted, rcpt)
	}
	if len(accepted) == 0 {
		return accepted, rejected, types.RejectedRecipients(accepted, rejected)
	}

	w, err := client.Data()
	if err != nil {
		return accepted, rejected, classifySMTP(err)
	}
	if _, err := w.Write(raw); err != nil {
		_ = w.Close()
		return accepted, rejected, fmt.Errorf("smtp data: %w", err)
	}
	if err := w.Close(); err != nil {
		return accepted, rejected, classifySMTP(err)
	}
	_ = client.Quit()
	return accepted, rejected, nil
}

func classifySMTP(err error) error {
	if err == nil {
		return nil
	}
	var tp *textproto.Error
	if errors.As(err, &tp) {
		switch {
		case tp.Code == 535 || tp.Code == 530 || tp.Code == 454:
			return types.AuthFailed("SMTP", err)
		case tp.Code == 552 || tp.Code == 523 || tp.Code == 534:
			return types.NewError(types.CodeSizeLimit, err.Error(), map[string]any{"smtp_code": tp.Code})
		}
	}
	s := strings.ToLower(err.Error())
	if strings.Contains(s, "auth") || strings.Contains(s, "535") {
		return types.AuthFailed("SMTP", err)
	}
	return fmt.Errorf("send email: %w", err)
}

// ProviderMessageLimit returns a known provider MIME size cap, or 0 if unknown.
func ProviderMessageLimit(smtpHost string) (int64, string) {
	h := strings.ToLower(smtpHost)
	if strings.Contains(h, "awsapps.com") || strings.Contains(h, "workmail") {
		return WorkMailMIMEMax, "Amazon WorkMail"
	}
	return 0, ""
}

// ShouldSaveToSent decides whether to APPEND a Sent copy after SMTP.
// auto: skip Gmail (server already saves); save for WorkMail and unknown providers.
func ShouldSaveToSent(mode, smtpHost string) bool {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "true", "1", "yes":
		return true
	case "false", "0", "no":
		return false
	default:
		h := strings.ToLower(smtpHost)
		if strings.Contains(h, "gmail.com") || strings.Contains(h, "googlemail.com") {
			return false
		}
		return true
	}
}

func uniqueRecipients(lists ...[]string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, list := range lists {
		for _, a := range list {
			a = strings.TrimSpace(a)
			if a == "" {
				continue
			}
			key := strings.ToLower(a)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, a)
		}
	}
	return out
}

func envelopeAddr(from string) string {
	from = strings.TrimSpace(from)
	if i := strings.LastIndex(from, "<"); i >= 0 {
		from = strings.TrimSuffix(from[i+1:], ">")
	}
	return strings.TrimSpace(from)
}
