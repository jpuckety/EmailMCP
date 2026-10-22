package server

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jpuckett/EmailMCP/emailmcp/internal/config"
)

// newTestServerWithSession builds an HTTP-mode server backed by in-memory stores
// and seeds a valid opaque session, returning the server and the bearer token.
func newTestServerWithSession(t *testing.T) (*Server, string) {
	t.Helper()
	cfg := &config.Config{
		Transport:          "http",
		PublicBaseURL:      "https://emailmcp.ecg.co",
		GoogleClientID:     "test-client-id",
		GoogleClientSecret: "test-client-secret",
		ApplicationID:      "emailmcp",
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	token := "opaque-access-token"
	sess := &Session{
		AccessToken:      token,
		RefreshToken:     "opaque-refresh-token",
		Subject:          "user1",
		Email:            "user1@example.com",
		AccessExpiresAt:  time.Now().Add(time.Hour),
		RefreshExpiresAt: time.Now().Add(24 * time.Hour),
	}
	if err := srv.authenticator.store.PutSession(context.Background(), sess); err != nil {
		t.Fatalf("PutSession failed: %v", err)
	}
	return srv, token
}

func doMCP(t *testing.T, url, token, sessionID, body, host string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2025-06-18")
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	if host != "" {
		req.Host = host
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

const initBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"repro","version":"1"}}}`
const toolsCallBody = `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_email_accounts","arguments":{}}}`

// TestAuthenticatedMCPFlow_LoopbackHost exercises the full initialize +
// tools/call sequence the MCP client performs, using the default (loopback)
// Host that httptest supplies.
func TestAuthenticatedMCPFlow_LoopbackHost(t *testing.T) {
	srv, token := newTestServerWithSession(t)
	ts := httptest.NewServer(srv.HTTPHandler())
	defer ts.Close()

	resp1, body1 := doMCP(t, ts.URL+"/", token, "", initBody, "")
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("initialize status = %d, body = %s", resp1.StatusCode, body1)
	}
	sid := resp1.Header.Get("Mcp-Session-Id")
	if sid == "" {
		t.Fatalf("no Mcp-Session-Id returned; body = %s", body1)
	}

	resp2, body2 := doMCP(t, ts.URL+"/", token, sid, toolsCallBody, "")
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("tools/call status = %d, body = %s", resp2.StatusCode, body2)
	}
	if !strings.Contains(body2, `"accounts"`) {
		t.Fatalf("unexpected tools/call body: %s", body2)
	}
}

// TestAuthenticatedMCPFlow_PublicHost is the regression test for the reported
// 403. It reproduces the deployed scenario where the request carries the public
// Host header (emailmcp.ecg.co) while the connection's local address is loopback
// (as behind a same-host proxy hop). Before disabling the SDK's DNS-rebinding /
// localhost protection this returned "403 Forbidden: invalid Host header" on
// every authenticated request; it must now succeed.
func TestAuthenticatedMCPFlow_PublicHost(t *testing.T) {
	srv, token := newTestServerWithSession(t)
	ts := httptest.NewServer(srv.HTTPHandler())
	defer ts.Close()

	const publicHost = "emailmcp.ecg.co"

	resp1, body1 := doMCP(t, ts.URL+"/", token, "", initBody, publicHost)
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("initialize with public Host returned %d (want 200), body = %s", resp1.StatusCode, body1)
	}
	sid := resp1.Header.Get("Mcp-Session-Id")
	if sid == "" {
		t.Fatalf("no Mcp-Session-Id returned; body = %s", body1)
	}

	resp2, body2 := doMCP(t, ts.URL+"/", token, sid, toolsCallBody, publicHost)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("tools/call with public Host returned %d (want 200), body = %s", resp2.StatusCode, body2)
	}
	if !strings.Contains(body2, `"accounts"`) {
		t.Fatalf("unexpected tools/call body: %s", body2)
	}
	if ct := resp2.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("expected JSON response, got Content-Type %q body %s", ct, body2)
	}
}

// TestConcurrentToolCallsSameJSONRPCID reproduces mailbox mixing: two POSTs on
// the same MCP session that reuse jsonrpc id=2. Each HTTP response must contain
// only its own result.
func TestConcurrentToolCallsSameJSONRPCID(t *testing.T) {
	srv, token := newTestServerWithSession(t)
	ts := httptest.NewServer(srv.HTTPHandler())
	defer ts.Close()

	resp1, body1 := doMCP(t, ts.URL+"/", token, "", initBody, "")
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("initialize status = %d, body = %s", resp1.StatusCode, body1)
	}
	sid := resp1.Header.Get("Mcp-Session-Id")
	if sid == "" {
		t.Fatalf("no Mcp-Session-Id returned; body = %s", body1)
	}

	pingBody := `{"jsonrpc":"2.0","id":2,"method":"ping"}`
	type outcome struct {
		status int
		body   string
		err    error
	}
	ch := make(chan outcome, 2)
	post := func(body string) {
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/", bytes.NewReader([]byte(body)))
		if err != nil {
			ch <- outcome{err: err}
			return
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("MCP-Protocol-Version", "2025-06-18")
		req.Header.Set("Mcp-Session-Id", sid)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			ch <- outcome{err: err}
			return
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		ch <- outcome{status: resp.StatusCode, body: string(b)}
	}

	go post(pingBody)
	go post(toolsCallBody)

	var bodies []string
	for i := 0; i < 2; i++ {
		o := <-ch
		if o.err != nil {
			t.Fatalf("request failed: %v", o.err)
		}
		if o.status != http.StatusOK {
			t.Fatalf("status = %d, body = %s", o.status, o.body)
		}
		bodies = append(bodies, o.body)
	}

	hasAccounts, hasPingOnly := 0, 0
	for _, b := range bodies {
		if strings.Contains(b, `"accounts"`) {
			hasAccounts++
			continue
		}
		hasPingOnly++
	}
	if hasAccounts != 1 || hasPingOnly != 1 {
		t.Fatalf("concurrent responses mixed or lost: %v", bodies)
	}
}
