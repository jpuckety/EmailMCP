package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/jpuckett/EmailMCP/emailmcp/internal/netutil"
	"github.com/jpuckett/EmailMCP/emailmcp/internal/types"
)

const (
	EventEmailReceived    = "email.received"
	defaultWebhookTimeout = 10 * time.Second
	defaultMaxAttempts    = 3
)

// EnvelopeEvent is the JSON payload POSTed for each new message.
type EnvelopeEvent struct {
	Event          string          `json:"event"`
	Timestamp      time.Time       `json:"timestamp"`
	AccountID      string          `json:"account_id"`
	Folder         string          `json:"folder"`
	UID            uint32          `json:"uid"`
	Subject        string          `json:"subject"`
	From           []types.Address `json:"from"`
	To             []types.Address `json:"to"`
	Date           string          `json:"date"`
	Flags          []string        `json:"flags"`
	HasAttachments bool            `json:"has_attachments"`
}

// PermanentError is a non-retryable webhook failure (typically 4xx except 429).
type PermanentError struct {
	Status int
}

func (e *PermanentError) Error() string {
	if e == nil {
		return "permanent webhook error"
	}
	return fmt.Sprintf("webhook permanent HTTP %d", e.Status)
}

// IsPermanent reports whether err is a non-retryable webhook failure.
func IsPermanent(err error) bool {
	var p *PermanentError
	return errors.As(err, &p)
}

// Dispatcher POSTs envelope summaries to an account webhook.
type Dispatcher struct {
	client             *http.Client
	logger             *slog.Logger
	maxAttempts        int
	backoff            time.Duration
	skipHostValidation bool
	now                func() time.Time
}

// DispatcherConfig controls outbound webhook POSTs.
type DispatcherConfig struct {
	Client             *http.Client
	Logger             *slog.Logger
	Timeout            time.Duration
	MaxAttempts        int
	Backoff            time.Duration
	SkipHostValidation bool
	Now                func() time.Time
}

func NewDispatcher(cfg DispatcherConfig) *Dispatcher {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultWebhookTimeout
	}
	client := cfg.Client
	if client == nil {
		client = netutil.SafeHTTPClient(timeout)
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	attempts := cfg.MaxAttempts
	if attempts <= 0 {
		attempts = defaultMaxAttempts
	}
	backoff := cfg.Backoff
	if backoff <= 0 {
		backoff = time.Second
	}
	now := cfg.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Dispatcher{
		client:             client,
		logger:             logger,
		maxAttempts:        attempts,
		backoff:            backoff,
		skipHostValidation: cfg.SkipHostValidation,
		now:                now,
	}
}

// Post delivers one envelope. Retryable failures (network, 429, 5xx) are
// retried with backoff. Other 4xx errors are permanent and must not advance
// the UID watermark.
func (d *Dispatcher) Post(ctx context.Context, webhookURL, token, accountID, folder string, summary types.EmailSummary) error {
	if d == nil {
		return errors.New("dispatcher is nil")
	}
	if !d.skipHostValidation {
		if err := validateWebhookURL(webhookURL); err != nil {
			return &PermanentError{Status: 0}
		}
	}
	body, err := json.Marshal(newEnvelopeEvent(accountID, folder, summary, d.now()))
	if err != nil {
		return err
	}

	var last error
	delay := d.backoff
	for attempt := 1; attempt <= d.maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		last = d.postOnce(ctx, webhookURL, token, accountID, folder, summary.UID, body)
		if last == nil {
			return nil
		}
		if IsPermanent(last) {
			return last
		}
		if attempt == d.maxAttempts {
			break
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		delay *= 2
	}
	return last
}

func (d *Dispatcher) postOnce(ctx context.Context, webhookURL, token, accountID, folder string, uid uint32, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(body))
	if err != nil {
		return sanitizeNetError(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := d.client.Do(req)
	if err != nil {
		d.logger.WarnContext(ctx, "webhook post failed",
			"account_id", accountID,
			"folder", folder,
			"uid", uid,
			"host", webhookHost(webhookURL),
			"error", sanitizeNetError(err),
		)
		return sanitizeNetError(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		d.logger.InfoContext(ctx, "webhook delivered",
			"account_id", accountID,
			"folder", folder,
			"uid", uid,
			"status", resp.StatusCode,
		)
		return nil
	}
	d.logger.WarnContext(ctx, "webhook non-success",
		"account_id", accountID,
		"folder", folder,
		"uid", uid,
		"status", resp.StatusCode,
		"host", webhookHost(webhookURL),
	)
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return fmt.Errorf("webhook HTTP %d", resp.StatusCode)
	}
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		return &PermanentError{Status: resp.StatusCode}
	}
	return fmt.Errorf("webhook HTTP %d", resp.StatusCode)
}

func newEnvelopeEvent(accountID, folder string, summary types.EmailSummary, now time.Time) EnvelopeEvent {
	from := summary.From
	if from == nil {
		from = []types.Address{}
	}
	to := summary.To
	if to == nil {
		to = []types.Address{}
	}
	flags := summary.Flags
	if flags == nil {
		flags = []string{}
	}
	date := ""
	if !summary.Date.IsZero() {
		date = summary.Date.UTC().Format(time.RFC3339)
	}
	return EnvelopeEvent{
		Event:          EventEmailReceived,
		Timestamp:      now.UTC(),
		AccountID:      accountID,
		Folder:         folder,
		UID:            summary.UID,
		Subject:        summary.Subject,
		From:           from,
		To:             to,
		Date:           date,
		Flags:          flags,
		HasAttachments: summary.HasAttach,
	}
}

func sanitizeNetError(err error) error {
	if err == nil {
		return nil
	}
	var uerr *url.Error
	if errors.As(err, &uerr) && uerr.Err != nil {
		return uerr.Err
	}
	return err
}
