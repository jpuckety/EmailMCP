// Package mailbuild builds RFC 5322 / MIME messages for send_email and create_draft.
package mailbuild

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/textproto"
	"strings"
	"time"
	"unicode"

	"github.com/jpuckett/EmailMCP/emailmcp/internal/types"
)

// Attachment is a resolved (decoded) attachment ready to embed in a message.
type Attachment struct {
	Filename    string
	ContentType string
	Content     []byte
	ContentID   string
	Disposition string // "attachment" (default) or "inline"
}

// Message is the complete outgoing email, shared by SMTP send and IMAP APPEND.
type Message struct {
	From        string
	To          []string
	Cc          []string
	Bcc         []string
	Subject     string
	Text        string
	HTML        string
	Attachments []Attachment

	MessageID  string
	InReplyTo  string
	References string
	Date       time.Time

	// IncludeBccHeader writes Bcc into the message (drafts). SMTP copies omit it.
	IncludeBccHeader bool
}

// Limits applied to the encoded message.
type Limits struct {
	MaxBytes     int64
	ProviderMax  int64
	ProviderName string
}

type built struct {
	contentType  string
	extraHeaders [][2]string
	body         []byte
}

// Build renders the MIME message. The same bytes are used for SMTP DATA and IMAP APPEND.
func Build(m Message) ([]byte, error) {
	if err := validate(m); err != nil {
		return nil, err
	}
	m.EnsureDefaults()

	inline, regular := splitAttachments(m.Attachments)
	b, err := buildBody(m, inline, regular)
	if err != nil {
		return nil, err
	}

	var out bytes.Buffer
	writeHeader(&out, "From", sanitizeHeader(m.From))
	if len(m.To) > 0 {
		writeHeader(&out, "To", sanitizeHeader(strings.Join(m.To, ", ")))
	}
	if len(m.Cc) > 0 {
		writeHeader(&out, "Cc", sanitizeHeader(strings.Join(m.Cc, ", ")))
	}
	if m.IncludeBccHeader && len(m.Bcc) > 0 {
		writeHeader(&out, "Bcc", sanitizeHeader(strings.Join(m.Bcc, ", ")))
	}
	writeHeader(&out, "Subject", encodeHeaderValue(sanitizeHeader(m.Subject)))
	writeHeader(&out, "Date", m.Date.Format("Mon, 02 Jan 2006 15:04:05 -0700"))
	writeHeader(&out, "Message-ID", ensureAngle(m.MessageID))
	if m.InReplyTo != "" {
		writeHeader(&out, "In-Reply-To", ensureAngle(m.InReplyTo))
	}
	if m.References != "" {
		writeHeader(&out, "References", sanitizeHeader(m.References))
	}
	writeHeader(&out, "MIME-Version", "1.0")
	for _, h := range b.extraHeaders {
		writeHeader(&out, h[0], h[1])
	}
	writeHeader(&out, "Content-Type", b.contentType)
	out.WriteString("\r\n")
	out.Write(b.body)
	return out.Bytes(), nil
}

// EnsureDefaults fills Date and Message-ID when they are empty.
func (m *Message) EnsureDefaults() {
	if m.Date.IsZero() {
		m.Date = time.Now().UTC()
	}
	if m.MessageID == "" {
		m.MessageID = generateMessageID(m.From)
	}
}

// ExtractHeader returns the first header value from a raw RFC822 message.
func ExtractHeader(raw []byte, name string) string {
	lower := strings.ToLower(name) + ":"
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			return ""
		}
		if strings.HasPrefix(strings.ToLower(line), lower) {
			return strings.TrimSpace(line[len(name)+1:])
		}
	}
	return ""
}

// BuildWithLimits builds the message and rejects it when the encoded size exceeds limits.
func BuildWithLimits(m Message, lim Limits) ([]byte, error) {
	raw, err := Build(m)
	if err != nil {
		return nil, err
	}
	n := int64(len(raw))
	if lim.MaxBytes > 0 && n > lim.MaxBytes {
		return nil, types.SizeLimitExceeded(n, lim.MaxBytes, "configured")
	}
	if lim.ProviderMax > 0 && n > lim.ProviderMax {
		name := lim.ProviderName
		if name == "" {
			name = "provider"
		}
		return nil, types.SizeLimitExceeded(n, lim.ProviderMax, name)
	}
	return raw, nil
}

func validate(m Message) error {
	if strings.TrimSpace(m.From) == "" {
		return types.Validation("from address is required")
	}
	if strings.ContainsAny(m.From, "\r\n") {
		return types.Validation("from address contains invalid characters")
	}
	return nil
}

func splitAttachments(atts []Attachment) (inline, regular []Attachment) {
	for _, a := range atts {
		disp := strings.ToLower(strings.TrimSpace(a.Disposition))
		if disp == "" {
			disp = "attachment"
		}
		a.Disposition = disp
		if a.ContentType == "" {
			a.ContentType = "application/octet-stream"
		}
		if disp == "inline" && strings.TrimSpace(a.ContentID) != "" {
			inline = append(inline, a)
			continue
		}
		regular = append(regular, a)
	}
	return inline, regular
}

func buildBody(m Message, inline, regular []Attachment) (built, error) {
	hasText := m.Text != ""
	hasHTML := m.HTML != ""
	hasInline := len(inline) > 0
	hasRegular := len(regular) > 0

	if !hasText && !hasHTML {
		hasText = true
	}

	relatedOrHTML := func() (built, error) {
		if !hasHTML {
			return textBuilt(false, m.Text), nil
		}
		if !hasInline {
			return textBuilt(true, m.HTML), nil
		}
		body, ct, err := multipartBuffer("related", func(w *multipart.Writer) error {
			if err := writeTextPart(w, true, m.HTML); err != nil {
				return err
			}
			for _, a := range inline {
				if err := writeAttachmentPart(w, a); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return built{}, err
		}
		return built{contentType: ct, body: body}, nil
	}

	alternativeOrBody := func() (built, error) {
		if hasText && hasHTML {
			body, ct, err := multipartBuffer("alternative", func(w *multipart.Writer) error {
				if err := writeTextPart(w, false, m.Text); err != nil {
					return err
				}
				inner, err := relatedOrHTML()
				if err != nil {
					return err
				}
				return writeBuiltPart(w, inner)
			})
			if err != nil {
				return built{}, err
			}
			return built{contentType: ct, body: body}, nil
		}
		if hasHTML {
			return relatedOrHTML()
		}
		return textBuilt(false, m.Text), nil
	}

	if !hasRegular {
		return alternativeOrBody()
	}

	body, ct, err := multipartBuffer("mixed", func(w *multipart.Writer) error {
		inner, err := alternativeOrBody()
		if err != nil {
			return err
		}
		if err := writeBuiltPart(w, inner); err != nil {
			return err
		}
		for _, a := range regular {
			if err := writeAttachmentPart(w, a); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return built{}, err
	}
	return built{contentType: ct, body: body}, nil
}

func textBuilt(html bool, content string) built {
	ct := "text/plain; charset=utf-8"
	if html {
		ct = "text/html; charset=utf-8"
	}
	return built{
		contentType:  ct,
		extraHeaders: [][2]string{{"Content-Transfer-Encoding", "quoted-printable"}},
		body:         qpBytes(content),
	}
}

func writeTextPart(w *multipart.Writer, html bool, content string) error {
	ct := "text/plain; charset=utf-8"
	if html {
		ct = "text/html; charset=utf-8"
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Type", ct)
	h.Set("Content-Transfer-Encoding", "quoted-printable")
	pw, err := w.CreatePart(h)
	if err != nil {
		return err
	}
	_, err = pw.Write(qpBytes(content))
	return err
}

func writeBuiltPart(w *multipart.Writer, b built) error {
	h := textproto.MIMEHeader{}
	h.Set("Content-Type", b.contentType)
	for _, eh := range b.extraHeaders {
		h.Set(eh[0], eh[1])
	}
	pw, err := w.CreatePart(h)
	if err != nil {
		return err
	}
	_, err = pw.Write(b.body)
	return err
}

func multipartBuffer(subtype string, writeParts func(*multipart.Writer) error) ([]byte, string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := writeParts(w); err != nil {
		return nil, "", err
	}
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	ct := fmt.Sprintf("multipart/%s; boundary=%s", subtype, w.Boundary())
	return buf.Bytes(), ct, nil
}

func writeAttachmentPart(w *multipart.Writer, a Attachment) error {
	ct := a.ContentType
	if a.Filename != "" {
		ct = withNameParam(ct, a.Filename)
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Type", ct)
	h.Set("Content-Disposition", formatDisposition(a.Disposition, a.Filename))
	h.Set("Content-Transfer-Encoding", "base64")
	if a.ContentID != "" {
		h.Set("Content-ID", ensureAngle(a.ContentID))
	}
	pw, err := w.CreatePart(h)
	if err != nil {
		return err
	}
	_, err = pw.Write(encodeBase64(a.Content))
	return err
}

func qpBytes(s string) []byte {
	var buf bytes.Buffer
	w := quotedprintable.NewWriter(&buf)
	_, _ = w.Write([]byte(s))
	_ = w.Close()
	return buf.Bytes()
}

func encodeBase64(data []byte) []byte {
	enc := make([]byte, base64.StdEncoding.EncodedLen(len(data)))
	base64.StdEncoding.Encode(enc, data)
	return fold76(enc)
}

func fold76(b []byte) []byte {
	if len(b) <= 76 {
		out := make([]byte, len(b))
		copy(out, b)
		return out
	}
	var buf bytes.Buffer
	for i := 0; i < len(b); i += 76 {
		end := i + 76
		if end > len(b) {
			end = len(b)
		}
		buf.Write(b[i:end])
		buf.WriteString("\r\n")
	}
	return buf.Bytes()
}

func writeHeader(w io.Writer, name, value string) {
	fmt.Fprintf(w, "%s: %s\r\n", name, value)
}

func sanitizeHeader(s string) string {
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", "")
	return s
}

func encodeHeaderValue(s string) string {
	if s == "" {
		return s
	}
	for _, r := range s {
		if r > unicode.MaxASCII || r < 32 {
			return mime.QEncoding.Encode("utf-8", s)
		}
	}
	return s
}

func ensureAngle(s string) string {
	s = sanitizeHeader(strings.TrimSpace(s))
	if s == "" {
		return s
	}
	if !strings.HasPrefix(s, "<") {
		s = "<" + s
	}
	if !strings.HasSuffix(s, ">") {
		s += ">"
	}
	return s
}

func generateMessageID(from string) string {
	domain := "emailmcp.local"
	if at := strings.LastIndex(from, "@"); at >= 0 {
		d := strings.Trim(from[at+1:], ">")
		d = strings.TrimSpace(d)
		if d != "" && !strings.ContainsAny(d, " \t") {
			domain = d
		}
	}
	var b [12]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("<%x@%s>", b, domain)
}

func withNameParam(contentType, filename string) string {
	filename = SanitizeFilename(filename)
	if filename == "" || strings.Contains(contentType, "name=") {
		return contentType
	}
	return contentType + "; name=\"" + asciiFallback(filename) + "\""
}
