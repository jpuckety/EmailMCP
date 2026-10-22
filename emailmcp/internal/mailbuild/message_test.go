package mailbuild

import (
	"bytes"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"
	"testing"

	"github.com/jpuckett/EmailMCP/emailmcp/internal/types"
)

func parseMsg(t *testing.T, raw []byte) *mail.Message {
	t.Helper()
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("parse: %v\n%s", err, raw)
	}
	return msg
}

func TestBuild_TextOnly(t *testing.T) {
	raw, err := Build(Message{From: "a@example.com", To: []string{"b@example.com"}, Subject: "Hi", Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	msg := parseMsg(t, raw)
	if got := msg.Header.Get("From"); got != "a@example.com" {
		t.Fatalf("From=%q", got)
	}
	if !strings.Contains(msg.Header.Get("Content-Type"), "text/plain") {
		t.Fatalf("ctype=%s", msg.Header.Get("Content-Type"))
	}
	body, _ := io.ReadAll(msg.Body)
	if !strings.Contains(string(body), "hello") {
		t.Fatalf("body=%s", body)
	}
	if msg.Header.Get("Message-ID") == "" {
		t.Fatal("expected Message-ID")
	}
}

func TestBuild_HTMLOnly(t *testing.T) {
	raw, err := Build(Message{From: "a@example.com", HTML: "<p>hi</p>"})
	if err != nil {
		t.Fatal(err)
	}
	msg := parseMsg(t, raw)
	if !strings.Contains(msg.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("ctype=%s", msg.Header.Get("Content-Type"))
	}
}

func TestBuild_TextAndHTML(t *testing.T) {
	raw, err := Build(Message{From: "a@example.com", Text: "plain", HTML: "<p>html</p>"})
	if err != nil {
		t.Fatal(err)
	}
	msg := parseMsg(t, raw)
	ctype := msg.Header.Get("Content-Type")
	if !strings.Contains(ctype, "multipart/alternative") {
		t.Fatalf("ctype=%s", ctype)
	}
}

func TestBuild_Attachments(t *testing.T) {
	raw, err := Build(Message{
		From:    "a@example.com",
		Text:    "see attached",
		Subject: "file",
		Attachments: []Attachment{{
			Filename:    "note.txt",
			ContentType: "text/plain",
			Content:     []byte("secret-bytes"),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	msg := parseMsg(t, raw)
	if !strings.Contains(msg.Header.Get("Content-Type"), "multipart/mixed") {
		t.Fatalf("ctype=%s", msg.Header.Get("Content-Type"))
	}
	if !bytes.Contains(raw, []byte("note.txt")) {
		t.Fatal("filename missing")
	}
	enc := base64.StdEncoding.EncodeToString([]byte("secret-bytes"))
	if !bytes.Contains(raw, []byte(enc)) {
		t.Fatal("base64 content missing")
	}
}

func TestBuild_InlineImage(t *testing.T) {
	raw, err := Build(Message{
		From: "a@example.com",
		HTML: `<img src="cid:pic@emailmcp">`,
		Attachments: []Attachment{{
			Filename:    "pic.png",
			ContentType: "image/png",
			Content:     []byte{0x89, 0x50, 0x4e, 0x47},
			ContentID:   "pic@emailmcp",
			Disposition: "inline",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "multipart/related") {
		t.Fatalf("expected multipart/related:\n%s", raw)
	}
	if !strings.Contains(strings.ToLower(string(raw)), "content-id:") || !strings.Contains(string(raw), "pic@emailmcp") {
		t.Fatalf("missing Content-ID:\n%s", raw)
	}
	if !strings.Contains(string(raw), "cid:pic@emailmcp") {
		t.Fatalf("html cid reference missing:\n%s", raw)
	}
}

func TestBuild_NonASCIIFilename(t *testing.T) {
	raw, err := Build(Message{
		From: "a@example.com",
		Text: "x",
		Attachments: []Attachment{{
			Filename:    "résumé.pdf",
			ContentType: "application/pdf",
			Content:     []byte("%PDF"),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "filename*=") {
		t.Fatalf("expected RFC 2231 filename*: %s", raw)
	}
	if strings.Contains(string(raw), "résumé.pdf\r") {
		t.Fatal("raw non-ascii filename in header without encoding")
	}
}

func TestBuild_HeaderInjectionBlocked(t *testing.T) {
	raw, err := Build(Message{
		From:    "a@example.com",
		Subject: "hi\r\nBcc: evil@example.com",
		Text:    "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	msg := parseMsg(t, raw)
	if msg.Header.Get("Bcc") != "" {
		t.Fatal("injected Bcc header")
	}
	if strings.Contains(msg.Header.Get("Subject"), "\n") {
		t.Fatal("newline in subject")
	}
}

func TestBuildWithLimits(t *testing.T) {
	raw, err := Build(Message{From: "a@example.com", Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = BuildWithLimits(Message{From: "a@example.com", Text: "hello"}, Limits{MaxBytes: int64(len(raw) - 1)})
	if err == nil {
		t.Fatal("expected size error")
	}
	var te *types.Error
	if !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("err=%v", err)
	}
	_ = te
}

func TestBuild_MessageIDsHaveAngleBrackets(t *testing.T) {
	raw, err := Build(Message{
		From:       "a@example.com",
		To:         []string{"b@example.com"},
		Subject:    "Hi",
		Text:       "hello",
		MessageID:  "abc@example.com",
		InReplyTo:  "prev@example.com",
		References: "one@example.com two@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	msg := parseMsg(t, raw)
	if got := msg.Header.Get("Message-ID"); got != "<abc@example.com>" {
		t.Fatalf("Message-ID=%q", got)
	}
	if got := msg.Header.Get("In-Reply-To"); got != "<prev@example.com>" {
		t.Fatalf("In-Reply-To=%q", got)
	}
	if got := msg.Header.Get("References"); got != "<one@example.com> <two@example.com>" {
		t.Fatalf("References=%q", got)
	}
	raw2, err := Build(Message{From: "a@example.com", To: []string{"b@example.com"}, Text: "x"})
	if err != nil {
		t.Fatal(err)
	}
	msg2 := parseMsg(t, raw2)
	if msg2.Header.Get("In-Reply-To") != "" || msg2.Header.Get("References") != "" {
		t.Fatalf("new messages must not set reply headers: %v", msg2.Header)
	}
	mid := msg2.Header.Get("Message-ID")
	if !strings.HasPrefix(mid, "<") || !strings.HasSuffix(mid, ">") {
		t.Fatalf("generated Message-ID missing brackets: %q", mid)
	}
}

func TestSanitizeFilename(t *testing.T) {
	if SanitizeFilename("../etc/passwd") != "passwd" {
		t.Fatal(SanitizeFilename("../etc/passwd"))
	}
	if SanitizeFilename("a\nb.txt") != "ab.txt" {
		t.Fatal(SanitizeFilename("a\nb.txt"))
	}
}

func TestMultipartRelatedInsideAlternativeInsideMixed(t *testing.T) {
	raw, err := Build(Message{
		From: "a@example.com",
		Text: "plain",
		HTML: `<img src="cid:x">`,
		Attachments: []Attachment{
			{Filename: "x.png", ContentType: "image/png", Content: []byte("IMG"), ContentID: "x", Disposition: "inline"},
			{Filename: "doc.pdf", ContentType: "application/pdf", Content: []byte("PDF")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if !strings.Contains(s, "multipart/mixed") || !strings.Contains(s, "multipart/alternative") || !strings.Contains(s, "multipart/related") {
		t.Fatalf("structure missing: %s", s)
	}
	_, params, err := mime.ParseMediaType(parseMsg(t, raw).Header.Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	mr := multipart.NewReader(parseMsg(t, raw).Body, params["boundary"])
	part, err := mr.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(part.Header.Get("Content-Type"), "multipart/alternative") {
		t.Fatalf("first mixed part should be alternative, got %s", part.Header.Get("Content-Type"))
	}
}
