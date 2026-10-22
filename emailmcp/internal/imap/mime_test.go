package imap

import (
	"strings"
	"testing"

	"github.com/jpuckett/EmailMCP/emailmcp/internal/types"
)

func TestApplyRFC822_MultipartSkipsAttachment(t *testing.T) {
	raw := []byte("From: a@example.com\r\n" +
		"To: b@example.com\r\n" +
		"Subject: hi\r\n" +
		"Message-ID: id@example.com\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=bound\r\n" +
		"\r\n" +
		"--bound\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"hello body\r\n" +
		"--bound\r\n" +
		"Content-Type: text/plain; name=\"note.txt\"\r\n" +
		"Content-Disposition: attachment; filename=\"note.txt\"\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"\r\n" +
		"aGVsbG8gd29ybGQ=\r\n" +
		"--bound--\r\n")
	em := &types.EmailMessage{}
	applyRFC822(em, raw)
	if em.Text != "hello body" {
		t.Fatalf("text=%q", em.Text)
	}
	if strings.Contains(em.Text, "bound") || strings.Contains(em.Text, "aGVsbG8") {
		t.Fatalf("raw mime leaked into text: %q", em.Text)
	}
	if em.Headers["Message-ID"] != "<id@example.com>" {
		t.Fatalf("message-id=%q", em.Headers["Message-ID"])
	}
}

func TestApplyFetchedSections_PlainText(t *testing.T) {
	header := []byte("From: a@x\r\nContent-Type: text/plain; charset=utf-8\r\n")
	body := []byte("just the body")
	em := &types.EmailMessage{}
	applyFetchedSections(em, header, body)
	if em.Text != "just the body" {
		t.Fatalf("text=%q", em.Text)
	}
}

func TestAngleMessageIDs(t *testing.T) {
	if got := angleMessageIDs("abc@x def@x"); got != "<abc@x> <def@x>" {
		t.Fatalf("got %q", got)
	}
	if got := angleMessageIDs("<abc@x>"); got != "<abc@x>" {
		t.Fatalf("got %q", got)
	}
}
