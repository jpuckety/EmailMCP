package types

import "time"

// Folder represents an IMAP mailbox/folder.
type Folder struct {
	Name        string   `json:"name"`
	Delimiter   string   `json:"delimiter,omitempty"`
	Attributes  []string `json:"attributes,omitempty"`
	NumMessages uint32   `json:"num_messages,omitempty"`
}

// EmailSummary provides header-level information for an email.
type EmailSummary struct {
	AccountID string    `json:"account_id,omitempty"`
	UID       uint32    `json:"uid"`
	SeqNum    uint32    `json:"seq_num,omitempty"`
	Subject   string    `json:"subject"`
	From      []Address `json:"from"`
	To        []Address `json:"to,omitempty"`
	Cc        []Address `json:"cc,omitempty"`
	Date      time.Time `json:"date"`
	Flags     []string  `json:"flags"`
	Size      uint32    `json:"size,omitempty"`
	HasAttach bool      `json:"has_attachments"`
}

// Address is an email address with optional display name.
type Address struct {
	Name    string `json:"name,omitempty"`
	Address string `json:"address"`
}

// EmailMessage is a full email with body content.
type EmailMessage struct {
	EmailSummary
	Folder  string            `json:"folder"`
	Text    string            `json:"text,omitempty"`
	HTML    string            `json:"html,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`

	// Attachments lists metadata; content is fetched with get_attachment.
	Attachments []Attachment `json:"attachments,omitempty"`
}

// Attachment metadata returned by read_email.
type Attachment struct {
	// ID is a stable identifier equal to PartID, suitable for get_attachment.
	ID          string `json:"id,omitempty"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	PartID      string `json:"part_id,omitempty"`
	Disposition string `json:"disposition,omitempty"`
	ContentID   string `json:"content_id,omitempty"`
	Inline      bool   `json:"inline,omitempty"`
}

// SendEmailInput for the send tool. Data for attachments must be base64,
// or a url/path may be supplied instead (exactly one source).
type SendEmailInput struct {
	AccountID string   `json:"account_id"`
	To        []string `json:"to"`
	Cc        []string `json:"cc,omitempty"`
	Bcc       []string `json:"bcc,omitempty"`
	Subject   string   `json:"subject"`
	Text      string   `json:"text,omitempty"`
	HTML      string   `json:"html,omitempty"`
	From      string   `json:"from,omitempty"` // override default

	Attachments []AttachmentInput `json:"attachments,omitempty"`

	ReplyToUID                 uint32 `json:"reply_to_uid,omitempty"`
	ReplyFolder                string `json:"reply_folder,omitempty"`
	ReplyAll                   bool   `json:"reply_all,omitempty"`
	ForwardUID                 uint32 `json:"forward_uid,omitempty"`
	ForwardFolder              string `json:"forward_folder,omitempty"`
	IncludeOriginalAttachments *bool  `json:"include_original_attachments,omitempty"`
	QuoteOriginal              bool   `json:"quote_original,omitempty"`
	SaveToSent                 string `json:"save_to_sent,omitempty"` // auto, true, false
}

// AttachmentInput for sending. Provide exactly one of data, url, or path.
type AttachmentInput struct {
	Filename    string `json:"filename,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	Data        string `json:"data,omitempty"` // base64 encoded
	URL         string `json:"url,omitempty"`  // https URL
	Path        string `json:"path,omitempty"` // local path (disabled unless allowlisted)
	ContentID   string `json:"content_id,omitempty"`
	Disposition string `json:"disposition,omitempty"` // attachment (default) or inline
}

// SendResult is returned after a successful SMTP send.
type SendResult struct {
	MessageID   string   `json:"message_id"`
	Accepted    []string `json:"accepted"`
	Rejected    []string `json:"rejected,omitempty"`
	SavedToSent bool     `json:"saved_to_sent,omitempty"`
	SentFolder  string   `json:"sent_folder,omitempty"`
}

// DraftResult is returned after create_draft.
type DraftResult struct {
	Folder    string `json:"folder"`
	UID       uint32 `json:"uid,omitempty"`
	MessageID string `json:"message_id"`
}

// AttachmentContent is returned by get_attachment.
type AttachmentContent struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	Data        string `json:"data,omitempty"` // base64 when under the inline cap
	URL         string `json:"url,omitempty"`  // presigned download URL
}
