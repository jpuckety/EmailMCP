package attach

import (
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jpuckett/EmailMCP/emailmcp/internal/types"
)

func testFetcher(t *testing.T, client *http.Client, allowHost bool) *Fetcher {
	t.Helper()
	cfg := Config{
		MaxBytes:   64,
		Timeout:    2 * time.Second,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		HTTPClient: client,
	}
	if allowHost {
		cfg.ValidateHost = func(context.Context, string) error { return nil }
	}
	return NewFetcher(cfg)
}

func TestResolve_Base64(t *testing.T) {
	f := testFetcher(t, nil, false)
	data := base64.StdEncoding.EncodeToString([]byte("hello"))
	a, err := f.Resolve(context.Background(), types.AttachmentInput{
		Filename: "a.txt", Data: data, ContentType: "text/plain",
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(a.Content) != "hello" {
		t.Fatalf("got %q", a.Content)
	}
}

func TestResolve_ExactlyOneSource(t *testing.T) {
	f := testFetcher(t, nil, false)
	_, err := f.Resolve(context.Background(), types.AttachmentInput{Data: "YQ==", URL: "https://example.com/x"})
	if err == nil {
		t.Fatal("expected error")
	}
	_, err = f.Resolve(context.Background(), types.AttachmentInput{})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestFetchURL_SuccessAndInfer(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte("PNGDATA"))
	}))
	defer srv.Close()

	f := testFetcher(t, srv.Client(), true)
	a, err := f.Resolve(context.Background(), types.AttachmentInput{URL: srv.URL + "/photos/cat.png"})
	if err != nil {
		t.Fatal(err)
	}
	if string(a.Content) != "PNGDATA" {
		t.Fatalf("content %q", a.Content)
	}
	if a.Filename != "cat.png" {
		t.Fatalf("filename %q", a.Filename)
	}
	if a.ContentType != "image/png" {
		t.Fatalf("ctype %q", a.ContentType)
	}
}

func TestFetchURL_RejectHTTP(t *testing.T) {
	f := testFetcher(t, http.DefaultClient, true)
	_, err := f.Resolve(context.Background(), types.AttachmentInput{URL: "http://example.com/x"})
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("err=%v", err)
	}
}

func TestFetchURL_SSRFBlocked(t *testing.T) {
	f := NewFetcher(Config{MaxBytes: 64, Timeout: time.Second, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	for _, u := range []string{
		"https://127.0.0.1/secret",
		"https://localhost/secret",
		"https://169.254.169.254/latest/meta-data/",
		"https://10.0.0.1/x",
	} {
		_, err := f.Resolve(context.Background(), types.AttachmentInput{URL: u})
		if err == nil {
			t.Fatalf("expected SSRF block for %s", u)
		}
	}
}

func TestFetchURL_RedirectBlocked(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://127.0.0.1/secret", http.StatusFound)
	}))
	defer srv.Close()
	f := testFetcher(t, srv.Client(), true)
	_, err := f.Resolve(context.Background(), types.AttachmentInput{URL: srv.URL})
	if err == nil {
		t.Fatal("expected redirect to loopback to fail")
	}
}

func TestFetchURL_SizeCap(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(bytesOf(100))
	}))
	defer srv.Close()
	f := testFetcher(t, srv.Client(), true)
	_, err := f.Resolve(context.Background(), types.AttachmentInput{URL: srv.URL})
	if err == nil {
		t.Fatal("expected size cap")
	}
}

func TestFetchURL_Timeout(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.Write([]byte("late"))
	}))
	defer srv.Close()
	client := srv.Client()
	client.Timeout = 50 * time.Millisecond
	f := testFetcher(t, client, true)
	f.cfg.Timeout = 50 * time.Millisecond
	_, err := f.Resolve(context.Background(), types.AttachmentInput{URL: srv.URL})
	if err == nil {
		t.Fatal("expected timeout")
	}
}

func TestPath_DisabledByDefault(t *testing.T) {
	f := testFetcher(t, nil, false)
	_, err := f.Resolve(context.Background(), types.AttachmentInput{Path: "/tmp/x"})
	if err == nil {
		t.Fatal("expected disabled")
	}
}

func TestPath_AllowlistAndTraversal(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ok.txt"), []byte("yes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "no.txt"), []byte("no"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := NewFetcher(Config{
		MaxBytes:      64,
		PathAllowlist: []string{dir},
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	a, err := f.Resolve(context.Background(), types.AttachmentInput{Path: filepath.Join(dir, "ok.txt")})
	if err != nil {
		t.Fatal(err)
	}
	if string(a.Content) != "yes" {
		t.Fatalf("got %q", a.Content)
	}
	_, err = f.Resolve(context.Background(), types.AttachmentInput{Path: filepath.Join(dir, "..", filepath.Base(outside), "no.txt")})
	if err == nil {
		t.Fatal("expected traversal reject")
	}
}

func bytesOf(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'x'
	}
	return b
}
