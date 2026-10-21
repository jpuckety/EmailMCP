package types

import "time"

// Account represents a unified email account supporting both IMAP and SMTP.
// Credentials are loaded from the Config API (Secrets Manager) as plaintext
// for the lifetime of a request — never logged or persisted by EmailMCP.
type Account struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// OwnerUserID is the authenticated Google subject that owns this account.
	// It is set by EmailMCP after Config API fetch and is never serialized to
	// storage. IMAP connection pools are keyed by (OwnerUserID, ID) so tenants
	// cannot share pools when account IDs collide.
	OwnerUserID string `json:"-"`

	// IMAP settings
	IMAPHost     string `json:"imap_host"`
	IMAPPort     int    `json:"imap_port"`
	IMAPUsername string `json:"imap_username"`
	// IMAPPassword comes from the Config API secret document (JSON field "password").
	IMAPPassword string `json:"password,omitempty"`
	IMAPUseTLS   bool   `json:"imap_use_tls"`

	// SMTP settings
	SMTPHost     string `json:"smtp_host"`
	SMTPPort     int    `json:"smtp_port"`
	SMTPUsername string `json:"smtp_username"`
	// SMTPPassword comes from the Config API secret document; when empty, IMAPPassword is used.
	SMTPPassword string `json:"smtp_password,omitempty"`
	SMTPUseTLS   bool   `json:"smtp_use_tls"`

	// Default sender address (optional). Falls back to SMTPUsername if empty.
	FromAddress string `json:"from_address,omitempty"`

	// SaveToSent controls whether send_email APPENDs a copy to the Sent folder.
	// Values: "auto" (default), "true", "false".
	SaveToSent string `json:"save_to_sent,omitempty"`
}

// AccountSummary is a safe view without any credential material.
type AccountSummary struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	IMAPHost    string    `json:"imap_host"`
	IMAPPort    int       `json:"imap_port"`
	SMTPHost    string    `json:"smtp_host"`
	SMTPPort    int       `json:"smtp_port"`
	FromAddress string    `json:"from_address,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}
