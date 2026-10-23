package mailbuild

import (
	"fmt"
	"html"
	"net/mail"
	"regexp"
	"strings"
	"time"

	"github.com/jpuckett/EmailMCP/emailmcp/internal/types"
)

var (
	replyFwdPrefix  = regexp.MustCompile(`(?i)^\s*((re|fwd|fw)\s*(\[\d+\])?\s*:)+\s*`)
	htmlDoctypeRe   = regexp.MustCompile(`(?is)<!DOCTYPE[^>]*>`)
	htmlHeadRe      = regexp.MustCompile(`(?is)<head\b[^>]*>.*?</head>`)
	htmlBodyOpenRe  = regexp.MustCompile(`(?is)<body\b[^>]*>`)
	htmlBodyCloseRe = regexp.MustCompile(`(?is)</body\s*>`)
	htmlOpenRe      = regexp.MustCompile(`(?is)<html\b[^>]*>`)
	htmlCloseRe     = regexp.MustCompile(`(?is)</html\s*>`)
	htmlStyleRe     = regexp.MustCompile(`(?is)<style\b[^>]*>.*?</style>`)
)

// StripSubjectPrefix removes stacked Re:/Fwd:/FW: prefixes.
func StripSubjectPrefix(subject string) string {
	s := strings.TrimSpace(subject)
	for {
		next := replyFwdPrefix.ReplaceAllString(s, "")
		next = strings.TrimSpace(next)
		if next == s {
			return s
		}
		s = next
	}
}

// ReplySubject returns "Re: <original>" without stacking prefixes.
func ReplySubject(original string) string {
	return "Re: " + StripSubjectPrefix(original)
}

// ForwardSubject returns "Fwd: <original>" without stacking prefixes.
func ForwardSubject(original string) string {
	return "Fwd: " + StripSubjectPrefix(original)
}

// BuildReferences appends the original Message-ID to the original References chain.
func BuildReferences(originalRefs, originalMessageID string) string {
	id := strings.TrimSpace(originalMessageID)
	if id == "" {
		return formatMessageIDs(originalRefs)
	}
	id = ensureAngle(id)
	refs := formatMessageIDs(originalRefs)
	if refs == "" {
		return id
	}
	if strings.Contains(refs, strings.Trim(id, "<>")) {
		return refs
	}
	return refs + " " + id
}

// QuotePlain quotes original text for a reply or forward.
func QuotePlain(originalFrom, originalDate, originalText string) string {
	var b strings.Builder
	b.WriteString("\r\n\r\n")
	if originalFrom != "" || !parseTime(originalDate).IsZero() {
		fmt.Fprintf(&b, "On %s, %s wrote:\r\n", originalDate, originalFrom)
	} else {
		b.WriteString("---------- Original Message ----------\r\n")
	}
	if originalText == "" {
		return b.String()
	}
	for _, line := range strings.Split(strings.ReplaceAll(originalText, "\r\n", "\n"), "\n") {
		b.WriteString("> ")
		b.WriteString(line)
		b.WriteString("\r\n")
	}
	return b.String()
}

// TextToHTML escapes plain text and converts line breaks to <br> so a
// text-only note can be placed above a quoted HTML original.
func TextToHTML(text string) string {
	if text == "" {
		return ""
	}
	s := strings.ReplaceAll(text, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = html.EscapeString(s)
	return strings.ReplaceAll(s, "\n", "<br>\r\n")
}

// QuoteHTML wraps the original HTML in a blockquote.
// Full documents are reduced to inner <body> content (plus <style> from <head>)
// so <!DOCTYPE>/<html>/<head> are not nested inside the quote.
func QuoteHTML(originalFrom, originalDate, originalHTML string) string {
	var b strings.Builder
	b.WriteString(`<br><blockquote type="cite">`)
	if originalFrom != "" || originalDate != "" {
		fmt.Fprintf(&b, "<p>On %s, %s wrote:</p>", html.EscapeString(originalDate), html.EscapeString(originalFrom))
	}
	b.WriteString(htmlInnerContent(originalHTML))
	b.WriteString("</blockquote>")
	return b.String()
}

func htmlInnerContent(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return raw
	}
	var styles strings.Builder
	if head := htmlHeadRe.FindString(s); head != "" {
		for _, st := range htmlStyleRe.FindAllString(head, -1) {
			styles.WriteString(st)
		}
	}
	if loc := htmlBodyOpenRe.FindStringIndex(s); loc != nil {
		rest := s[loc[1]:]
		if end := htmlBodyCloseRe.FindStringIndex(rest); end != nil {
			rest = rest[:end[0]]
		}
		return styles.String() + rest
	}
	out := htmlDoctypeRe.ReplaceAllString(s, "")
	out = htmlHeadRe.ReplaceAllString(out, "")
	out = htmlOpenRe.ReplaceAllString(out, "")
	out = htmlCloseRe.ReplaceAllString(out, "")
	return styles.String() + out
}

// QuoteOriginal appends a quoted copy of orig below msg's note.
// When an HTML part will be produced and msg.HTML is empty, a text-only note
// is converted to HTML so clients that show the HTML part still see it.
func QuoteOriginal(msg *Message, orig *types.EmailMessage) {
	if msg == nil || orig == nil {
		return
	}
	from := strings.Join(addressesToStrings(orig.From), ", ")
	date := ""
	if !orig.Date.IsZero() {
		date = orig.Date.Format("Mon, 02 Jan 2006 15:04:05 -0700")
	}
	appendQuote(msg, from, date, orig.Text, orig.HTML)
}

func appendQuote(msg *Message, originalFrom, originalDate, originalText, originalHTML string) {
	if msg == nil {
		return
	}
	noteText, noteHTML := msg.Text, msg.HTML
	if noteText != "" || originalText != "" {
		msg.Text = noteText + QuotePlain(originalFrom, originalDate, originalText)
	}
	if noteHTML != "" || originalHTML != "" {
		prefix := noteHTML
		if prefix == "" && noteText != "" {
			prefix = TextToHTML(noteText)
		}
		msg.HTML = prefix + QuoteHTML(originalFrom, originalDate, originalHTML)
	}
}

func parseTime(s string) time.Time {
	t, err := mail.ParseDate(s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// OwnAddresses are addresses that should be removed from reply-all recipients.
func OwnAddresses(acc *types.Account) []string {
	if acc == nil {
		return nil
	}
	var out []string
	for _, a := range []string{acc.FromAddress, acc.SMTPUsername, acc.IMAPUsername} {
		if addr := extractAddr(a); addr != "" {
			out = append(out, addr)
		}
	}
	return out
}

func extractAddr(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if parsed, err := mail.ParseAddress(s); err == nil && parsed.Address != "" {
		return strings.ToLower(parsed.Address)
	}
	if at := strings.LastIndex(s, "<"); at >= 0 {
		s = strings.TrimSuffix(s[at+1:], ">")
	}
	return strings.ToLower(strings.TrimSpace(s))
}

func addrKey(s string) string {
	return extractAddr(s)
}

func inList(list []string, addr string) bool {
	k := addrKey(addr)
	if k == "" {
		return false
	}
	for _, a := range list {
		if addrKey(a) == k {
			return true
		}
	}
	return false
}

func addressesToStrings(addrs []types.Address) []string {
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		if a.Address == "" {
			continue
		}
		if a.Name != "" {
			out = append(out, fmt.Sprintf("%s <%s>", a.Name, a.Address))
			continue
		}
		out = append(out, a.Address)
	}
	return out
}

// ApplyReply sets In-Reply-To / References, default subject, and optionally reply-all recipients.
// Caller-supplied To/Cc/Subject are preserved when non-empty.
func ApplyReply(msg *Message, orig *types.EmailMessage, replyAll bool, own []string) {
	if orig == nil || msg == nil {
		return
	}
	origID := origMessageID(orig)
	if origID != "" {
		msg.InReplyTo = origID
		msg.References = BuildReferences(origHeader(orig, "References"), origID)
	}
	if strings.TrimSpace(msg.Subject) == "" {
		msg.Subject = ReplySubject(orig.Subject)
	}
	if len(msg.To) > 0 || len(msg.Cc) > 0 {
		return
	}
	replyTo := addressesToStrings(orig.From)
	if v := origHeader(orig, "Reply-To"); v != "" {
		replyTo = []string{v}
	}
	msg.To = filterOwn(replyTo, own)
	if !replyAll {
		return
	}
	seen := append([]string{}, own...)
	seen = append(seen, msg.To...)
	var cc []string
	for _, a := range append(addressesToStrings(orig.To), addressesToStrings(orig.Cc)...) {
		if inList(seen, a) {
			continue
		}
		cc = append(cc, a)
		seen = append(seen, a)
	}
	msg.Cc = cc
}

// ApplyForward sets the default Fwd: subject and optionally quotes the original body.
func ApplyForward(msg *Message, orig *types.EmailMessage, quote bool) {
	if orig == nil || msg == nil {
		return
	}
	if strings.TrimSpace(msg.Subject) == "" {
		msg.Subject = ForwardSubject(orig.Subject)
	}
	if !quote {
		return
	}
	QuoteOriginal(msg, orig)
}

func origMessageID(orig *types.EmailMessage) string {
	if orig.Headers != nil {
		if v := orig.Headers["Message-ID"]; v != "" {
			return ensureAngle(v)
		}
		if v := orig.Headers["Message-Id"]; v != "" {
			return ensureAngle(v)
		}
	}
	return ""
}

func origHeader(orig *types.EmailMessage, name string) string {
	if orig.Headers == nil {
		return ""
	}
	if v := orig.Headers[name]; v != "" {
		return v
	}
	for k, v := range orig.Headers {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

func filterOwn(addrs, own []string) []string {
	var out []string
	for _, a := range addrs {
		if inList(own, a) {
			continue
		}
		out = append(out, a)
	}
	return out
}
