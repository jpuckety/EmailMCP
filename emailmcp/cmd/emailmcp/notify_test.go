package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jpuckett/EmailMCP/emailmcp/internal/config"
)

func TestNotifyHealthHandler(t *testing.T) {
	h := notifyHealthHandler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /health status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "ok" {
		t.Fatalf("GET /health body = %q, want ok", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET / status = %d, want 404 (health-only mux)", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/mcp", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /mcp status = %d, want 404", rec.Code)
	}
}

func TestResolveNotifyOwnerNeverSharedDefault(t *testing.T) {
	a := resolveNotifyOwner("")
	b := resolveNotifyOwner("notify")
	if a == "" || b == "" {
		t.Fatal("owner id must not be empty")
	}
	if a == "notify" || b == "notify" {
		t.Fatalf("owner must not use shared default, got %q and %q", a, b)
	}
	if a == b {
		t.Fatalf("generated owners must be unique, both %q", a)
	}
	if got := resolveNotifyOwner("task-abc"); got != "task-abc" {
		t.Fatalf("explicit owner = %q, want task-abc", got)
	}
}

func TestRequireNotifyStateTableFailClosedOnECS(t *testing.T) {
	t.Setenv("ECS_CONTAINER_METADATA_URI_V4", "")
	t.Setenv("ECS_CONTAINER_METADATA_URI", "")
	t.Setenv("AWS_EXECUTION_ENV", "")
	if err := requireNotifyStateTable(""); err != nil {
		t.Fatalf("local empty table should be allowed: %v", err)
	}

	t.Setenv("ECS_CONTAINER_METADATA_URI_V4", "http://169.254.170.2/v4")
	if err := requireNotifyStateTable(""); err == nil {
		t.Fatal("expected fail-closed on ECS without notify state table")
	}
	if err := requireNotifyStateTable("EmailMCPNotifyState"); err != nil {
		t.Fatalf("table set should succeed on ECS: %v", err)
	}
}

func TestRequireNotifyUserConfigTable(t *testing.T) {
	if err := requireNotifyUserConfigTable(""); err == nil {
		t.Fatal("expected error when user config table is unset")
	}
	if err := requireNotifyUserConfigTable("  "); err == nil {
		t.Fatal("expected error for blank user config table")
	}
	if err := requireNotifyUserConfigTable("UserConfig"); err != nil {
		t.Fatalf("table set should succeed: %v", err)
	}
}

func TestRunNotifyFailsWithoutUserConfigTable(t *testing.T) {
	t.Setenv("ECS_CONTAINER_METADATA_URI_V4", "")
	t.Setenv("ECS_CONTAINER_METADATA_URI", "")
	t.Setenv("AWS_EXECUTION_ENV", "")
	cfg := &config.Config{ListenAddr: "127.0.0.1:0"}
	if err := runNotify(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
		t.Fatal("expected runNotify to fail closed without a user config table")
	}
}

func TestRunNotifyHealthWithoutGoogleOAuth(t *testing.T) {
	orig := newUserConfigStore
	newUserConfigStore = func(ctx context.Context, _, appID string, l *slog.Logger) (config.Store, error) {
		return orig(ctx, "", appID, l)
	}
	t.Cleanup(func() { newUserConfigStore = orig })
	t.Setenv("GOOGLE_CLIENT_ID", "")
	t.Setenv("GOOGLE_CLIENT_SECRET", "")
	t.Setenv("ECS_CONTAINER_METADATA_URI_V4", "")
	t.Setenv("ECS_CONTAINER_METADATA_URI", "")
	t.Setenv("AWS_EXECUTION_ENV", "")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	cfg := &config.Config{
		ListenAddr:              addr,
		UserConfigTableName:     "test-user-config",
		NotifyReconcileInterval: time.Hour,
		NotifyWebhookTimeout:    time.Second,
		NotifyMaxFolders:        8,
		NotifyOwnerID:           "test-owner",
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- runNotify(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()

	var resp *http.Response
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp, err = http.Get("http://" + addr + "/health")
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		cancel()
		t.Fatalf("GET /health: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		cancel()
		t.Fatalf("GET /health status=%d body=%q", resp.StatusCode, body)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("runNotify: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runNotify did not stop after cancel")
	}
}

func TestUniqueOwnerID(t *testing.T) {
	seen := map[string]struct{}{}
	for i := 0; i < 8; i++ {
		id := uniqueOwnerID()
		if id == "" || id == "notify" {
			t.Fatalf("invalid owner %q", id)
		}
		if _, ok := seen[id]; ok {
			t.Fatalf("duplicate owner %q", id)
		}
		seen[id] = struct{}{}
	}
}
