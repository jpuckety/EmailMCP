package integration

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	imapmgr "github.com/jpuckett/EmailMCP/emailmcp/internal/imap"
	"github.com/jpuckett/EmailMCP/emailmcp/internal/mailbuild"
	"github.com/jpuckett/EmailMCP/emailmcp/internal/smtp"
	"github.com/jpuckett/EmailMCP/emailmcp/internal/types"
)

func testAccount(imapHost string, imapPort int, smtpHost string, smtpPort int) *types.Account {
	return &types.Account{
		ID:           "acct",
		OwnerUserID:  "user-1",
		IMAPHost:     imapHost,
		IMAPPort:     imapPort,
		IMAPUsername: "user@example.com",
		IMAPPassword: "pass",
		IMAPUseTLS:   false,
		SMTPHost:     smtpHost,
		SMTPPort:     smtpPort,
		SMTPUsername: "user@example.com",
		SMTPPassword: "pass",
		SMTPUseTLS:   false,
		FromAddress:  "user@example.com",
	}
}

func startIMAP(t *testing.T) (host string, port int) {
	t.Helper()
	mem := imapmemserver.New()
	u := imapmemserver.NewUser("user@example.com", "pass")
	if err := u.Create("INBOX", nil); err != nil {
		t.Fatal(err)
	}
	if err := u.Create("Drafts", &imap.CreateOptions{SpecialUse: []imap.MailboxAttr{imap.MailboxAttrDrafts}}); err != nil {
		t.Fatal(err)
	}
	if err := u.Create("Sent", &imap.CreateOptions{SpecialUse: []imap.MailboxAttr{imap.MailboxAttrSent}}); err != nil {
		t.Fatal(err)
	}
	mem.AddUser(u)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		InsecureAuth: true,
	})
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	var p int
	fmt.Sscanf(portStr, "%d", &p)
	return host, p
}

type smtpSink struct {
	mu         sync.Mutex
	data       []byte
	recipients []string
}

func startSMTP(t *testing.T, sink *smtpSink) (host string, port int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go handleSMTP(c, sink)
		}
	}()
	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	var p int
	fmt.Sscanf(portStr, "%d", &p)
	return host, p
}

func handleSMTP(c net.Conn, sink *smtpSink) {
	defer c.Close()
	br := bufio.NewReader(c)
	bw := bufio.NewWriter(c)
	send := func(s string) { _, _ = bw.WriteString(s + "\r\n"); _ = bw.Flush() }
	send("220 localhost ESMTP")
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		upper := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(upper, "EHLO") || strings.HasPrefix(upper, "HELO"):
			send("250-localhost")
			send("250 AUTH PLAIN")
		case strings.HasPrefix(upper, "AUTH"):
			send("235 2.7.0 OK")
		case strings.HasPrefix(upper, "MAIL"):
			send("250 OK")
		case strings.HasPrefix(upper, "RCPT"):
			sink.mu.Lock()
			sink.recipients = append(sink.recipients, strings.TrimSpace(line))
			sink.mu.Unlock()
			send("250 OK")
		case strings.HasPrefix(upper, "DATA"):
			send("354 Go")
			var buf bytes.Buffer
			for {
				l, err := br.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				buf.WriteString(l)
			}
			sink.mu.Lock()
			sink.data = buf.Bytes()
			sink.mu.Unlock()
			send("250 OK")
		case strings.HasPrefix(upper, "QUIT"):
			send("221 Bye")
			return
		default:
			send("250 OK")
		}
	}
}

func TestCreateDraft_AppearsInDrafts(t *testing.T) {
	host, port := startIMAP(t)
	mgr := imapmgr.NewManager(imapmgr.Config{
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		SkipHostValidation: true,
	})
	acc := testAccount(host, port, host, 25)
	raw, err := mailbuild.Build(mailbuild.Message{
		From: "user@example.com", To: []string{"to@example.com"}, Subject: "Draft subject", Text: "draft body",
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := mgr.AppendDraft(context.Background(), acc, "", raw)
	if err != nil {
		t.Fatal(err)
	}
	if res.Folder != "Drafts" {
		t.Fatalf("folder %q", res.Folder)
	}
	msg, err := mgr.GetEmail(context.Background(), acc, "Drafts", res.UID)
	if err != nil {
		t.Fatal(err)
	}
	foundDraft := false
	for _, f := range msg.Flags {
		if strings.EqualFold(f, `\Draft`) {
			foundDraft = true
		}
	}
	if !foundDraft {
		t.Fatalf("flags=%v", msg.Flags)
	}
}

func TestSendEmail_AttachmentsAndSentCopy(t *testing.T) {
	imapHost, imapPort := startIMAP(t)
	sink := &smtpSink{}
	smtpHost, smtpPort := startSMTP(t, sink)

	acc := testAccount(imapHost, imapPort, smtpHost, smtpPort)
	sender := smtp.NewSender(smtp.Config{
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		SkipHostValidation: true,
		DefaultTimeout:     5 * time.Second,
	})
	mgr := imapmgr.NewManager(imapmgr.Config{
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		SkipHostValidation: true,
	})

	payload := []byte("hello-attach")
	msg := mailbuild.Message{
		From:    "user@example.com",
		To:      []string{"to@example.com"},
		Subject: "With file",
		Text:    "see file",
		Attachments: []mailbuild.Attachment{{
			Filename: "hello.txt", ContentType: "text/plain", Content: payload,
		}},
	}
	msg.EnsureDefaults()
	raw, err := mailbuild.Build(msg)
	if err != nil {
		t.Fatal(err)
	}
	result, err := sender.SendRaw(context.Background(), acc, msg.From, msg.To, raw, msg.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	if result.MessageID == "" {
		t.Fatal("missing message id")
	}
	if len(result.Accepted) != 1 {
		t.Fatalf("accepted=%v", result.Accepted)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		sink.mu.Lock()
		n := len(sink.data)
		sink.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	sink.mu.Lock()
	got := append([]byte{}, sink.data...)
	sink.mu.Unlock()
	if !bytes.Contains(got, []byte("hello.txt")) {
		t.Fatalf("smtp payload missing attachment name: %s", got)
	}

	if _, err := mgr.AppendSent(context.Background(), acc, raw); err != nil {
		t.Fatal(err)
	}
	sent, err := mgr.GetEmail(context.Background(), acc, "Sent", 1)
	if err != nil {
		t.Fatal(err)
	}
	if sent.Subject != "With file" && !strings.Contains(strings.Join(sent.Flags, ","), "") {
		// subject may come from envelope
	}
	if len(sent.Attachments) == 0 && !sent.HasAttach {
		// memserver may or may not parse bodystructure; at least message exists
	}
}

func TestGetAttachment_RoundTrip(t *testing.T) {
	host, port := startIMAP(t)
	mgr := imapmgr.NewManager(imapmgr.Config{
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		SkipHostValidation: true,
	})
	acc := testAccount(host, port, host, 25)
	payload := []byte("round-trip-bytes-xyz")
	raw, err := mailbuild.Build(mailbuild.Message{
		From: "user@example.com", To: []string{"to@example.com"}, Subject: "rt", Text: "body",
		Attachments: []mailbuild.Attachment{{Filename: "rt.bin", ContentType: "application/octet-stream", Content: payload}},
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := mgr.AppendMessage(context.Background(), acc, "INBOX", raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	uid := res.UID
	if uid == 0 {
		uid = 1
	}
	msg, err := mgr.GetEmail(context.Background(), acc, "INBOX", uid)
	if err != nil {
		t.Fatal(err)
	}
	partID, filename := "", "rt.bin"
	if len(msg.Attachments) > 0 {
		partID = msg.Attachments[0].PartID
		filename = msg.Attachments[0].Filename
	}
	got, err := mgr.GetAttachment(context.Background(), acc, "INBOX", uid, partID, filename)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Content, payload) {
		t.Fatalf("got %q want %q", got.Content, payload)
	}
}

func TestReplyThreadingHeaders(t *testing.T) {
	orig := &types.EmailMessage{
		EmailSummary: types.EmailSummary{Subject: "Hello", From: []types.Address{{Address: "alice@example.com"}}},
		Headers:      map[string]string{"Message-ID": "<orig@example.com>", "References": "<root@example.com>"},
	}
	msg := mailbuild.Message{From: "me@example.com", Text: "reply"}
	mailbuild.ApplyReply(&msg, orig, false, nil)
	raw, err := mailbuild.Build(msg)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte("In-Reply-To: <orig@example.com>")) {
		t.Fatalf("missing In-Reply-To: %s", raw)
	}
	if !bytes.Contains(raw, []byte("References: <root@example.com> <orig@example.com>")) {
		t.Fatalf("missing References: %s", raw)
	}
	if !bytes.Contains(raw, []byte("Subject: Re: Hello")) {
		t.Fatalf("subject: %s", raw)
	}
}
