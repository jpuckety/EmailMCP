package mailbuild

import (
	"strings"
	"testing"

	"github.com/jpuckett/EmailMCP/emailmcp/internal/types"
)

func TestReplySubject_NoStack(t *testing.T) {
	if got := ReplySubject("Re: Re: Hello"); got != "Re: Hello" {
		t.Fatalf("got %q", got)
	}
	if got := ReplySubject("Hello"); got != "Re: Hello" {
		t.Fatalf("got %q", got)
	}
}

func TestForwardSubject_NoStack(t *testing.T) {
	if got := ForwardSubject("Fwd: Hello"); got != "Fwd: Hello" {
		t.Fatalf("got %q", got)
	}
}

func TestBuildReferences(t *testing.T) {
	got := BuildReferences("<a@x>", "<b@x>")
	if got != "<a@x> <b@x>" {
		t.Fatalf("got %q", got)
	}
	got = BuildReferences("", "b@x")
	if got != "<b@x>" {
		t.Fatalf("got %q", got)
	}
}

func TestApplyReply_HeadersAndReplyAll(t *testing.T) {
	orig := &types.EmailMessage{
		EmailSummary: types.EmailSummary{
			Subject: "Hello",
			From:    []types.Address{{Address: "alice@example.com"}},
			To:      []types.Address{{Address: "me@example.com"}, {Address: "bob@example.com"}},
			Cc:      []types.Address{{Address: "carol@example.com"}},
		},
		Headers: map[string]string{
			"Message-ID": "<m1@example.com>",
			"References": "<prev@example.com>",
		},
	}
	msg := &Message{From: "me@example.com"}
	ApplyReply(msg, orig, true, []string{"me@example.com"})
	if msg.Subject != "Re: Hello" {
		t.Fatalf("subject %q", msg.Subject)
	}
	if msg.InReplyTo != "<m1@example.com>" {
		t.Fatalf("in-reply-to %q", msg.InReplyTo)
	}
	if !strings.Contains(msg.References, "<prev@example.com>") || !strings.Contains(msg.References, "<m1@example.com>") {
		t.Fatalf("references %q", msg.References)
	}
	if len(msg.To) != 1 || !strings.Contains(msg.To[0], "alice@example.com") {
		t.Fatalf("to=%v", msg.To)
	}
	joined := strings.Join(msg.Cc, ",")
	if !strings.Contains(joined, "bob@example.com") || !strings.Contains(joined, "carol@example.com") {
		t.Fatalf("cc=%v", msg.Cc)
	}
	if strings.Contains(joined, "me@example.com") {
		t.Fatal("own address in cc")
	}
}

func TestApplyForward_ReattachSubject(t *testing.T) {
	orig := &types.EmailMessage{
		EmailSummary: types.EmailSummary{Subject: "Report", From: []types.Address{{Address: "a@x"}}},
		Text:         "body",
	}
	msg := &Message{Text: "see below"}
	ApplyForward(msg, orig, true)
	if msg.Subject != "Fwd: Report" {
		t.Fatalf("subject %q", msg.Subject)
	}
	if !strings.Contains(msg.Text, "> body") {
		t.Fatalf("quoted text missing: %s", msg.Text)
	}
}
