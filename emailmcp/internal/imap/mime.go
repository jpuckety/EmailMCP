package imap

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"net/textproto"
	"strings"

	"github.com/jpuckett/EmailMCP/emailmcp/internal/types"
)

// applyRFC822 fills headers and decoded text/html bodies from a raw message.
// Multipart attachments are left out of Text/HTML.
func applyRFC822(em *types.EmailMessage, raw []byte) {
	if em == nil || len(raw) == 0 {
		return
	}
	parseMessageHeaders(em, raw)
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return
	}
	ct := msg.Header.Get("Content-Type")
	text, html := extractTextParts(ct, textproto.MIMEHeader(msg.Header), msg.Body)
	if text != "" {
		em.Text = text
	}
	if html != "" {
		em.HTML = html
	}
}

// applyFetchedSections reconstructs an RFC 822 message from IMAP HEADER + TEXT
// fetches and extracts the readable body.
func applyFetchedSections(em *types.EmailMessage, header, body []byte) {
	if len(header) == 0 && len(body) == 0 {
		return
	}
	if len(header) == 0 {
		applyRFC822(em, body)
		return
	}
	raw := header
	if !bytes.Contains(header, []byte("\r\n\r\n")) && !bytes.Contains(header, []byte("\n\n")) {
		if bytes.HasSuffix(raw, []byte("\r\n")) {
			raw = append(append([]byte{}, raw...), []byte("\r\n")...)
		} else {
			raw = append(append([]byte{}, raw...), []byte("\r\n\r\n")...)
		}
	}
	raw = append(raw, body...)
	applyRFC822(em, raw)
}

func extractTextParts(contentType string, hdr textproto.MIMEHeader, r io.Reader) (text, html string) {
	if r == nil {
		return "", ""
	}
	media, params, err := mime.ParseMediaType(strings.TrimSpace(contentType))
	if err != nil || media == "" {
		media = "text/plain"
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return "", ""
	}
	data = decodeTransfer(hdr.Get("Content-Transfer-Encoding"), data)

	if strings.HasPrefix(media, "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			return "", ""
		}
		mr := multipart.NewReader(bytes.NewReader(data), boundary)
		for {
			part, err := mr.NextPart()
			if err != nil {
				break
			}
			t, h := extractTextParts(part.Header.Get("Content-Type"), part.Header, part)
			_ = part.Close()
			if text == "" {
				text = t
			}
			if html == "" {
				html = h
			}
		}
		return text, html
	}

	if isAttachmentPart(contentType, hdr) {
		return "", ""
	}
	switch media {
	case "text/plain":
		return string(data), ""
	case "text/html":
		return "", string(data)
	default:
		return "", ""
	}
}

func angleMessageIDs(s string) string {
	fields := strings.Fields(s)
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if !strings.HasPrefix(f, "<") {
			f = "<" + f
		}
		if !strings.HasSuffix(f, ">") {
			f += ">"
		}
		out = append(out, f)
	}
	return strings.Join(out, " ")
}

func isAttachmentPart(contentType string, hdr textproto.MIMEHeader) bool {
	disp := strings.ToLower(hdr.Get("Content-Disposition"))
	if strings.HasPrefix(disp, "attachment") {
		return true
	}
	filename := ""
	if disp != "" {
		if _, params, err := mime.ParseMediaType(hdr.Get("Content-Disposition")); err == nil {
			filename = params["filename"]
		}
	}
	if filename == "" {
		if _, params, err := mime.ParseMediaType(contentType); err == nil {
			filename = params["name"]
		}
	}
	return filename != ""
}
