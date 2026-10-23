package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jpuckett/EmailMCP/emailmcp/internal/types"
)

func testDispatcher(client *http.Client) *Dispatcher {
	return NewDispatcher(DispatcherConfig{
		Client:             client,
		SkipHostValidation: true,
		MaxAttempts:        3,
		Backoff:            time.Millisecond,
	})
}

func TestDispatcherLogsDeliveryResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	d := NewDispatcher(DispatcherConfig{
		Client:             srv.Client(),
		Logger:             slog.New(slog.NewJSONHandler(&buf, nil)),
		SkipHostValidation: true,
		MaxAttempts:        1,
	})
	if err := d.Post(context.Background(), srv.URL, "", "default", "INBOX", types.EmailSummary{UID: 42}); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	for _, want := range []string{
		`"msg":"webhook delivered"`,
		`"account_id":"default"`,
		`"folder":"INBOX"`,
		`"uid":42`,
		`"status":202`,
		`"attempt":1`,
		`"duration_ms":`,
		`"host":`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %s in log: %s", want, got)
		}
	}
}

func TestDispatcherPostSuccessAndBearer(t *testing.T) {
	var gotAuth string
	var payload EnvelopeEvent
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content-type=%q", r.Header.Get("Content-Type"))
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Errorf("json: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := testDispatcher(srv.Client())
	err := d.Post(context.Background(), srv.URL, "tok", "default", "INBOX", types.EmailSummary{
		UID:     12,
		Subject: "hi",
		From:    []types.Address{{Address: "a@example.com"}},
		Date:    time.Date(2026, 4, 8, 15, 3, 1, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer tok" {
		t.Fatalf("auth=%q", gotAuth)
	}
	if payload.Event != EventEmailReceived || payload.UID != 12 || payload.AccountID != "default" || payload.Folder != "INBOX" {
		t.Fatalf("payload=%+v", payload)
	}
	if payload.Date != "2026-04-08T15:03:01Z" {
		t.Fatalf("date=%q", payload.Date)
	}
	if payload.From == nil || payload.To == nil || payload.Flags == nil {
		t.Fatal("from/to/flags must not be omitted as null")
	}
}

func TestDispatcherRetries500Then200(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := testDispatcher(srv.Client())
	if err := d.Post(context.Background(), srv.URL, "", "a", "INBOX", types.EmailSummary{UID: 1}); err != nil {
		t.Fatal(err)
	}
	if n.Load() != 2 {
		t.Fatalf("attempts=%d", n.Load())
	}
}

func TestDispatcherRetries429(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	d := testDispatcher(srv.Client())
	if err := d.Post(context.Background(), srv.URL, "", "a", "INBOX", types.EmailSummary{UID: 1}); err != nil {
		t.Fatal(err)
	}
	if n.Load() != 3 {
		t.Fatalf("attempts=%d", n.Load())
	}
}

func TestDispatcherNoRetryOn400(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()
	d := testDispatcher(srv.Client())
	err := d.Post(context.Background(), srv.URL, "", "a", "INBOX", types.EmailSummary{UID: 1})
	if !IsPermanent(err) {
		t.Fatalf("err=%v", err)
	}
	if n.Load() != 1 {
		t.Fatalf("attempts=%d", n.Load())
	}
}

func TestDispatcherRejectsNonHTTPSWhenValidating(t *testing.T) {
	d := NewDispatcher(DispatcherConfig{SkipHostValidation: false, MaxAttempts: 1})
	err := d.Post(context.Background(), "http://example.com/hook", "", "a", "INBOX", types.EmailSummary{UID: 1})
	if !IsPermanent(err) {
		t.Fatalf("err=%v", err)
	}
}

func TestSanitizeNetErrorStripsURL(t *testing.T) {
	err := &url.Error{Op: "Post", URL: "https://hooks.example.com/mail?token=secret", Err: errors.New("connection refused")}
	got := sanitizeNetError(err)
	if got == nil {
		t.Fatal("expected wrapped error")
	}
	if strings.Contains(got.Error(), "secret") || strings.Contains(got.Error(), "https://") {
		t.Fatalf("leaked url: %v", got)
	}
}

func TestDispatcherReturnedNetworkErrorOmitsWebhookURL(t *testing.T) {
	d := NewDispatcher(DispatcherConfig{
		Client:             &http.Client{Timeout: time.Second},
		MaxAttempts:        1,
		SkipHostValidation: true,
	})
	err := d.Post(context.Background(), "https://127.0.0.1:1/mail?token=query-secret", "", "a", "INBOX", types.EmailSummary{UID: 1})
	if err == nil {
		t.Fatal("expected connection error")
	}
	if strings.Contains(err.Error(), "query-secret") || strings.Contains(err.Error(), "https://") {
		t.Fatalf("returned error leaked webhook URL: %v", err)
	}
}

func TestDispatcherRequestErrorOmitsWebhookURL(t *testing.T) {
	d := NewDispatcher(DispatcherConfig{
		Client:             http.DefaultClient,
		MaxAttempts:        1,
		SkipHostValidation: true,
	})
	err := d.Post(context.Background(), "https://hooks.example.com/%zz?token=query-secret", "", "a", "INBOX", types.EmailSummary{UID: 1})
	if err == nil {
		t.Fatal("expected request construction error")
	}
	if strings.Contains(err.Error(), "query-secret") || strings.Contains(err.Error(), "https://") {
		t.Fatalf("returned error leaked webhook URL: %v", err)
	}
}
