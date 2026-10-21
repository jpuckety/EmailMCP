package notify

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/jpuckett/EmailMCP/emailmcp/internal/netutil"
)

func validateWebhookURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fmt.Errorf("webhook_url is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("webhook_url is not a valid URL")
	}
	if u.Scheme != "https" {
		return fmt.Errorf("webhook_url must be https")
	}
	if u.User != nil {
		return fmt.Errorf("webhook_url must not include user information")
	}
	if u.Hostname() == "" {
		return fmt.Errorf("webhook_url host is required")
	}
	if err := netutil.ValidatePublicHost(u.Hostname()); err != nil {
		return fmt.Errorf("webhook_url not allowed: %w", err)
	}
	return nil
}

func webhookHost(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return u.Host
}
