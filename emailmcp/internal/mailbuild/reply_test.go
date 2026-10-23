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

func TestApplyForward_TextNoteCopiedToHTML(t *testing.T) {
	orig := &types.EmailMessage{
		EmailSummary: types.EmailSummary{
			Subject: "Ticket",
			From:    []types.Address{{Name: "Alice", Address: "alice@example.com"}},
		},
		Text: "original plain",
		HTML: `<!DOCTYPE html><html><head><style>p{color:red}</style></head><body><p>original</p></body></html>`,
	}
	msg := &Message{Text: "Hi James,\nPlease see below."}
	ApplyForward(msg, orig, true)

	if !strings.Contains(msg.Text, "Hi James,") {
		t.Fatalf("plain note missing: %s", msg.Text)
	}
	if !strings.Contains(msg.HTML, "Hi James,") {
		t.Fatalf("html note missing: %s", msg.HTML)
	}
	if !strings.Contains(msg.HTML, "Please see below.") {
		t.Fatalf("html note second line missing: %s", msg.HTML)
	}
	if !strings.Contains(msg.HTML, "<br>") {
		t.Fatalf("expected line break in html note: %s", msg.HTML)
	}
	if strings.Contains(msg.HTML, "<!DOCTYPE") || strings.Contains(strings.ToLower(msg.HTML), "<html") {
		t.Fatalf("nested document chrome: %s", msg.HTML)
	}
	if !strings.Contains(msg.HTML, "<p>original</p>") {
		t.Fatalf("original body missing: %s", msg.HTML)
	}
	if !strings.Contains(msg.HTML, "p{color:red}") {
		t.Fatalf("style missing: %s", msg.HTML)
	}
	if i, j := strings.Index(msg.HTML, "Hi James,"), strings.Index(msg.HTML, "blockquote"); i < 0 || j < 0 || i > j {
		t.Fatalf("note should appear above quote: %s", msg.HTML)
	}
}

func TestApplyForward_ExplicitHTMLNotReplacedByText(t *testing.T) {
	orig := &types.EmailMessage{
		EmailSummary: types.EmailSummary{Subject: "S", From: []types.Address{{Address: "a@x"}}},
		HTML:         "<p>orig</p>",
	}
	msg := &Message{Text: "plain note", HTML: "<p>html note</p>"}
	ApplyForward(msg, orig, true)
	if !strings.Contains(msg.HTML, "<p>html note</p>") {
		t.Fatalf("html note missing: %s", msg.HTML)
	}
	if strings.Contains(msg.HTML, "plain note") {
		t.Fatalf("text should not be copied into html when html was provided: %s", msg.HTML)
	}
	if !strings.Contains(msg.Text, "plain note") {
		t.Fatalf("plain note missing: %s", msg.Text)
	}
}

func TestApplyForward_TextOnlyOriginalStaysPlain(t *testing.T) {
	orig := &types.EmailMessage{
		EmailSummary: types.EmailSummary{Subject: "S", From: []types.Address{{Address: "a@x"}}},
		Text:         "original",
	}
	msg := &Message{Text: "note"}
	ApplyForward(msg, orig, true)
	if msg.HTML != "" {
		t.Fatalf("should not invent an HTML part: %q", msg.HTML)
	}
}

func TestQuoteOriginal_TextNoteCopiedToHTML(t *testing.T) {
	orig := &types.EmailMessage{
		EmailSummary: types.EmailSummary{From: []types.Address{{Address: "alice@example.com"}}},
		HTML:         "<p>orig</p>",
	}
	msg := &Message{Text: "Thanks"}
	QuoteOriginal(msg, orig)
	if !strings.Contains(msg.HTML, "Thanks") {
		t.Fatalf("html note missing: %s", msg.HTML)
	}
	if !strings.Contains(msg.HTML, "<p>orig</p>") {
		t.Fatalf("quoted original missing: %s", msg.HTML)
	}
}

func TestQuoteHTML_FragmentUnchanged(t *testing.T) {
	got := QuoteHTML("a@x", "d", "<p>hi</p>")
	if !strings.Contains(got, "<p>hi</p>") {
		t.Fatalf("%s", got)
	}
	if strings.Contains(got, "<html") {
		t.Fatalf("should not wrap fragment: %s", got)
	}
}

func TestTextToHTML_EscapesAndBreaks(t *testing.T) {
	got := TextToHTML("a <b> & c\nnext")
	if strings.Contains(got, "<b>") || !strings.Contains(got, "&lt;b&gt;") {
		t.Fatalf("escape tags: %s", got)
	}
	if !strings.Contains(got, "&amp;") {
		t.Fatalf("escape amp: %s", got)
	}
	if !strings.Contains(got, "<br>") {
		t.Fatalf("line break: %s", got)
	}
}
