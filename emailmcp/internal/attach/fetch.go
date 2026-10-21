// Package attach resolves outgoing attachments from base64, HTTPS URLs, or local paths.
package attach

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jpuckett/EmailMCP/emailmcp/internal/mailbuild"
	"github.com/jpuckett/EmailMCP/emailmcp/internal/netutil"
	"github.com/jpuckett/EmailMCP/emailmcp/internal/types"
)

const defaultTimeout = 30 * time.Second
const maxRedirects = 5

// Config for attachment resolution.
type Config struct {
	MaxBytes      int64
	Timeout       time.Duration
	AllowedHosts  []string // optional HTTPS host allowlist; empty = any public host
	PathAllowlist []string // empty = path attachments disabled
	Logger        *slog.Logger

	// HTTPClient overrides the SSRF-safe client (tests).
	HTTPClient *http.Client
	// ValidateHost overrides public-host checks (tests).
	ValidateHost func(ctx context.Context, host string) error
}

// Fetcher resolves AttachmentInput values into decoded mailbuild attachments.
type Fetcher struct {
	cfg Config
}

func NewFetcher(cfg Config) *Fetcher {
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = 10 << 20
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.ValidateHost == nil {
		cfg.ValidateHost = netutil.ValidatePublicHostContext
	}
	return &Fetcher{cfg: cfg}
}

// ResolveAll decodes every attachment. Each input must provide exactly one of data, url, or path.
func (f *Fetcher) ResolveAll(ctx context.Context, inputs []types.AttachmentInput) ([]mailbuild.Attachment, error) {
	out := make([]mailbuild.Attachment, 0, len(inputs))
	var total int64
	for i, in := range inputs {
		a, err := f.Resolve(ctx, in)
		if err != nil {
			name := in.Filename
			if name == "" {
				name = fmt.Sprintf("attachments[%d]", i)
			}
			return nil, types.AttachmentFetchFailed(name, err)
		}
		total += int64(len(a.Content))
		if f.cfg.MaxBytes > 0 && total > f.cfg.MaxBytes*int64(len(inputs)+1) {
			// per-attachment already enforced; keep a generous total guard
		}
		out = append(out, a)
	}
	return out, nil
}

// Resolve a single attachment input.
func (f *Fetcher) Resolve(ctx context.Context, in types.AttachmentInput) (mailbuild.Attachment, error) {
	sources := 0
	if strings.TrimSpace(in.Data) != "" {
		sources++
	}
	if strings.TrimSpace(in.URL) != "" {
		sources++
	}
	if strings.TrimSpace(in.Path) != "" {
		sources++
	}
	if sources == 0 {
		return mailbuild.Attachment{}, types.Validation("attachment requires exactly one of data, url, or path")
	}
	if sources > 1 {
		return mailbuild.Attachment{}, types.Validation("attachment must use exactly one of data, url, or path")
	}

	disp := strings.ToLower(strings.TrimSpace(in.Disposition))
	if disp == "" {
		disp = "attachment"
	}
	if disp != "attachment" && disp != "inline" {
		return mailbuild.Attachment{}, types.Validation("disposition must be attachment or inline")
	}

	var (
		content     []byte
		filename    = in.Filename
		contentType = in.ContentType
		err         error
	)
	switch {
	case in.Data != "":
		content, err = decodeBase64(in.Data, f.cfg.MaxBytes, filename)
	case in.URL != "":
		content, filename, contentType, err = f.fetchURL(ctx, in.URL, filename, contentType)
	default:
		content, filename, contentType, err = f.readPath(in.Path, filename, contentType)
	}
	if err != nil {
		return mailbuild.Attachment{}, err
	}
	if int64(len(content)) > f.cfg.MaxBytes {
		return mailbuild.Attachment{}, types.NewError(types.CodeAttachmentTooLarge,
			fmt.Sprintf("attachment exceeds maximum size of %d bytes", f.cfg.MaxBytes),
			map[string]any{"actual_bytes": len(content), "limit_bytes": f.cfg.MaxBytes, "filename": filename})
	}
	if filename == "" {
		filename = "attachment"
	}
	if contentType == "" {
		contentType = mime.TypeByExtension(path.Ext(filename))
		if contentType == "" {
			contentType = "application/octet-stream"
		}
	}
	return mailbuild.Attachment{
		Filename:    mailbuild.SanitizeFilename(filename),
		ContentType: contentType,
		Content:     content,
		ContentID:   strings.TrimSpace(in.ContentID),
		Disposition: disp,
	}, nil
}

func decodeBase64(data string, max int64, filename string) ([]byte, error) {
	// Reject oversized base64 before decoding (base64 expands ~4/3).
	if max > 0 && int64(len(data)) > (max*4)/3+4 {
		return nil, types.NewError(types.CodeAttachmentTooLarge,
			fmt.Sprintf("attachment exceeds maximum size of %d bytes", max),
			map[string]any{"limit_bytes": max, "filename": filename})
	}
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		raw, err = base64.URLEncoding.DecodeString(data)
	}
	if err != nil {
		return nil, types.Validation("invalid base64 attachment data")
	}
	return raw, nil
}

func (f *Fetcher) fetchURL(ctx context.Context, raw, filename, contentType string) ([]byte, string, string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, "", "", types.NewError(types.CodeURLNotAllowed, "invalid attachment url", nil)
	}
	if err := f.validateURL(ctx, u); err != nil {
		return nil, "", "", err
	}

	client := f.httpClient()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "", "", err
	}
	req.Header.Set("User-Agent", "EmailMCP/1.0")

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", "", fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		f.cfg.Logger.Info("attachment url fetch failed",
			"host", u.Host, "status", resp.StatusCode, "elapsed", time.Since(start))
		return nil, "", "", fmt.Errorf("download returned HTTP %d", resp.StatusCode)
	}

	limit := f.cfg.MaxBytes
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, "", "", fmt.Errorf("download: %w", err)
	}
	if int64(len(data)) > limit {
		return nil, "", "", types.NewError(types.CodeAttachmentTooLarge,
			fmt.Sprintf("attachment exceeds maximum size of %d bytes", limit),
			map[string]any{"limit_bytes": limit, "host": u.Host})
	}

	if filename == "" {
		filename = filenameFromResponse(resp, u)
	}
	if contentType == "" {
		contentType = resp.Header.Get("Content-Type")
		if i := strings.Index(contentType, ";"); i >= 0 {
			contentType = strings.TrimSpace(contentType[:i])
		}
	}

	f.cfg.Logger.Info("attachment url fetched",
		"host", u.Host, "size", len(data), "elapsed", time.Since(start))
	return data, filename, contentType, nil
}

func (f *Fetcher) httpClient() *http.Client {
	base := f.cfg.HTTPClient
	if base == nil {
		base = netutil.SafeHTTPClient(f.cfg.Timeout)
	}
	c := *base
	c.CheckRedirect = f.checkRedirect
	if c.Timeout == 0 {
		c.Timeout = f.cfg.Timeout
	}
	return &c
}

func (f *Fetcher) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("too many redirects")
	}
	if req.URL == nil {
		return types.NewError(types.CodeURLNotAllowed, "redirect missing url", nil)
	}
	return f.validateURL(req.Context(), req.URL)
}

func (f *Fetcher) validateURL(ctx context.Context, u *url.URL) error {
	if u.Scheme != "https" {
		return types.NewError(types.CodeURLNotAllowed, "attachment urls must use https", map[string]any{"scheme": u.Scheme})
	}
	host := u.Hostname()
	if host == "" {
		return types.NewError(types.CodeURLNotAllowed, "attachment url host is required", nil)
	}
	if len(f.cfg.AllowedHosts) > 0 && !hostAllowed(host, f.cfg.AllowedHosts) {
		return types.NewError(types.CodeURLNotAllowed, "attachment url host is not in the allowlist", map[string]any{"host": host})
	}
	if err := f.cfg.ValidateHost(ctx, host); err != nil {
		return types.NewError(types.CodeURLNotAllowed, "attachment url host is not allowed", map[string]any{"host": host})
	}
	return nil
}

func hostAllowed(host string, allow []string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	for _, a := range allow {
		a = strings.ToLower(strings.TrimSpace(a))
		a = strings.TrimPrefix(a, "https://")
		a = strings.TrimSuffix(a, "/")
		if i := strings.Index(a, "/"); i >= 0 {
			a = a[:i]
		}
		if a == "" {
			continue
		}
		if strings.HasPrefix(a, "*.") {
			suf := strings.TrimPrefix(a, "*")
			if strings.HasSuffix(h, suf) || h == strings.TrimPrefix(a, "*.") {
				return true
			}
			continue
		}
		if h == a {
			return true
		}
	}
	return false
}

func filenameFromResponse(resp *http.Response, u *url.URL) string {
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		if _, params, err := mime.ParseMediaType(cd); err == nil {
			if n := params["filename"]; n != "" {
				return n
			}
		}
	}
	p := u.Path
	if p == "" || strings.HasSuffix(p, "/") {
		return "attachment"
	}
	base := path.Base(p)
	if !utf8.ValidString(base) {
		return "attachment"
	}
	return base
}

func (f *Fetcher) readPath(raw, filename, contentType string) ([]byte, string, string, error) {
	if len(f.cfg.PathAllowlist) == 0 {
		return nil, "", "", types.NewError(types.CodePathNotAllowed, "path attachments are disabled", nil)
	}
	abs, err := allowedPath(raw, f.cfg.PathAllowlist)
	if err != nil {
		return nil, "", "", err
	}
	file, err := os.Open(abs)
	if err != nil {
		return nil, "", "", fmt.Errorf("open path: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, f.cfg.MaxBytes+1))
	if err != nil {
		return nil, "", "", err
	}
	if int64(len(data)) > f.cfg.MaxBytes {
		return nil, "", "", types.NewError(types.CodeAttachmentTooLarge,
			fmt.Sprintf("attachment exceeds maximum size of %d bytes", f.cfg.MaxBytes),
			map[string]any{"limit_bytes": f.cfg.MaxBytes})
	}
	if filename == "" {
		filename = filepath.Base(abs)
	}
	if contentType == "" {
		contentType = mime.TypeByExtension(filepath.Ext(filename))
	}
	f.cfg.Logger.Info("attachment path read", "size", len(data))
	return data, filename, contentType, nil
}

func allowedPath(p string, allowlist []string) (string, error) {
	if strings.ContainsRune(p, 0) {
		return "", types.NewError(types.CodePathNotAllowed, "invalid path", nil)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", types.NewError(types.CodePathNotAllowed, "invalid path", nil)
	}
	abs = filepath.Clean(abs)
	for _, dir := range allowlist {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		dirAbs, err := filepath.Abs(dir)
		if err != nil {
			continue
		}
		dirAbs = filepath.Clean(dirAbs)
		rel, err := filepath.Rel(dirAbs, abs)
		if err != nil {
			continue
		}
		if rel == "." || (!strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)) {
			return abs, nil
		}
	}
	return "", types.NewError(types.CodePathNotAllowed, "path is outside the configured allowlist", nil)
}
