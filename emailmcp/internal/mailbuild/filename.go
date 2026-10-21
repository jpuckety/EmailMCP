package mailbuild

import (
	"fmt"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"
)

// SanitizeFilename strips directory components, CR/LF, and other header-injection
// characters. Empty or dangerous names become "attachment".
func SanitizeFilename(name string) string {
	name = strings.ReplaceAll(name, "\r", "")
	name = strings.ReplaceAll(name, "\n", "")
	name = strings.ReplaceAll(name, "\x00", "")
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return "attachment"
	}
	return name
}

func formatDisposition(disp, filename string) string {
	filename = SanitizeFilename(filename)
	if filename == "" {
		return disp
	}
	if isQuotedSafeASCII(filename) {
		return fmt.Sprintf(`%s; filename="%s"`, disp, filename)
	}
	fallback := asciiFallback(filename)
	return fmt.Sprintf(`%s; filename="%s"; filename*=UTF-8''%s`, disp, fallback, rfc5987(filename))
}

func isQuotedSafeASCII(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < 32 || r > 126 || r == '"' || r == '\\' || r == ';' {
			return false
		}
	}
	return true
}

func asciiFallback(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r > unicode.MaxASCII || r < 32 || r == '"' || r == '\\' {
			b.WriteByte('_')
			continue
		}
		b.WriteRune(r)
	}
	out := b.String()
	if strings.Trim(out, "_") == "" {
		return "attachment"
	}
	return out
}

// rfc5987 percent-encodes a UTF-8 filename for RFC 2231 / 5987 (filename*).
func rfc5987(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			fmt.Fprintf(&b, "%%%02X", s[i])
			i++
			continue
		}
		chunk := s[i : i+size]
		if size == 1 {
			c := chunk[0]
			if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
				c == '.' || c == '-' || c == '_' {
				b.WriteByte(c)
				i += size
				continue
			}
		}
		for j := 0; j < size; j++ {
			fmt.Fprintf(&b, "%%%02X", chunk[j])
		}
		i += size
	}
	return b.String()
}
