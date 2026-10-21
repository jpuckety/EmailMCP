package server

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
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jpuckett/EmailMCP/emailmcp/internal/config"
	"github.com/jpuckett/EmailMCP/emailmcp/internal/types"
	"golang.org/x/oauth2"
)

func newTestWebServerWithSession(t *testing.T) (*Server, string) {
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
	token := "test-web-access-token"
	sess := &Session{
		AccessToken:      token,
		RefreshToken:     "test-web-refresh-token",
		Subject:          "user123",
		Email:            "user123@example.com",
		AccessExpiresAt:  time.Now().Add(time.Hour),
		RefreshExpiresAt: time.Now().Add(24 * time.Hour),
	}
	if err := srv.authenticator.store.PutSession(context.Background(), sess); err != nil {
		t.Fatalf("PutSession failed: %v", err)
	}
	return srv, token
}

func TestWebStatic_ServesIndexAndSPA(t *testing.T) {
	srv, _ := newTestWebServerWithSession(t)
	ts := httptest.NewServer(srv.HTTPHandler())
	defer ts.Close()

	// 1. User Portal root path GET /portal
	resp, err := http.Get(ts.URL + "/portal")
	if err != nil {
		t.Fatalf("GET /portal: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /portal status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
		t.Errorf("Content-Type = %s, want text/html", resp.Header.Get("Content-Type"))
	}
	if !strings.Contains(string(body), "app-root") {
		t.Errorf("expected body to contain 'app-root', got: %s", string(body))
	}
	if !strings.Contains(string(body), `<base href="/portal/">`) {
		t.Errorf("expected body to contain base href /portal/, got: %s", string(body))
	}

	// 2. User Portal trailing slash path GET /portal/
	respSlash, err := http.Get(ts.URL + "/portal/")
	if err != nil {
		t.Fatalf("GET /portal/: %v", err)
	}
	bodySlash, _ := io.ReadAll(respSlash.Body)
	respSlash.Body.Close()

	if respSlash.StatusCode != http.StatusOK {
		t.Fatalf("GET /portal/ status = %d, want 200", respSlash.StatusCode)
	}
	if !strings.Contains(string(bodySlash), "app-root") {
		t.Errorf("expected body to contain 'app-root', got: %s", string(bodySlash))
	}

	// 3. SPA Route GET /portal/accounts/new (should fallback to index.html)
	resp2, err := http.Get(ts.URL + "/portal/accounts/new")
	if err != nil {
		t.Fatalf("GET /portal/accounts/new: %v", err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()

	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("GET /portal/accounts/new status = %d, want 200", resp2.StatusCode)
	}
	if !strings.Contains(string(body2), "app-root") {
		t.Errorf("expected SPA fallback to index.html, got: %s", string(body2))
	}

	// 4. Root path GET / without MCP auth headers routes to MCP handler (401 Unauthorized)
	respRoot, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	respRoot.Body.Close()
	if respRoot.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET / status = %d, want 401 Unauthorized for MCP root", respRoot.StatusCode)
	}
}

func TestWebCallback_Redirects(t *testing.T) {
	srv, _ := newTestWebServerWithSession(t)
	ts := httptest.NewServer(srv.HTTPHandler())
	defer ts.Close()

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// 1. Error param redirect
	respErr, err := client.Get(ts.URL + "/auth/callback?error=access_denied")
	if err != nil {
		t.Fatalf("GET /auth/callback?error: %v", err)
	}
	respErr.Body.Close()
	if respErr.StatusCode != http.StatusFound {
		t.Fatalf("expected 302 redirect, got %d", respErr.StatusCode)
	}
	loc := respErr.Header.Get("Location")
	if loc != "/portal/login?error=access_denied" {
		t.Errorf("Location = %q, want /portal/login?error=access_denied", loc)
	}

	// 2. Missing code/state redirect
	respMissing, err := client.Get(ts.URL + "/auth/callback")
	if err != nil {
		t.Fatalf("GET /auth/callback: %v", err)
	}
	respMissing.Body.Close()
	if respMissing.StatusCode != http.StatusFound {
		t.Fatalf("expected 302 redirect, got %d", respMissing.StatusCode)
	}
	locMissing := respMissing.Header.Get("Location")
	if locMissing != "/portal/login?error=missing_code_or_state" {
		t.Errorf("Location = %q, want /portal/login?error=missing_code_or_state", locMissing)
	}
}

func TestWebLogin_GoogleCallbackIssuesPortalSession(t *testing.T) {
	srv, _ := newTestWebServerWithSession(t)
	srv.oauth.exchangeFn = func(_ context.Context, code string) (*oauth2.Token, error) {
		if code != "google-code" {
			t.Fatalf("unexpected google code %q", code)
		}
		tok := &oauth2.Token{
			AccessToken:  "g-access",
			RefreshToken: "g-refresh",
			Expiry:       time.Now().Add(time.Hour),
		}
		return tok.WithExtra(map[string]any{"id_token": "fake-id-token"}), nil
	}
	srv.oauth.verifyFn = func(_ context.Context, rawToken, audience string) (string, string, time.Time, error) {
		if rawToken != "fake-id-token" {
			return "", "", time.Time{}, errors.New("bad test token")
		}
		if audience != "test-client-id" {
			t.Fatalf("audience = %q", audience)
		}
		return "sub-1", "user@example.com", time.Now().Add(time.Hour), nil
	}

	ts := httptest.NewServer(srv.HTTPHandler())
	defer ts.Close()
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	loginResp, err := client.Get(ts.URL + "/auth/login")
	if err != nil {
		t.Fatalf("GET /auth/login: %v", err)
	}
	loginResp.Body.Close()
	if loginResp.StatusCode != http.StatusFound {
		t.Fatalf("/auth/login status = %d", loginResp.StatusCode)
	}
	googleURL, err := url.Parse(loginResp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse google location: %v", err)
	}
	if googleURL.Query().Get("redirect_uri") != "https://emailmcp.ecg.co/oauth/callback" {
		t.Fatalf("redirect_uri = %q", googleURL.Query().Get("redirect_uri"))
	}
	state := googleURL.Query().Get("state")
	if state == "" {
		t.Fatal("missing google state")
	}

	cbResp, err := client.Get(ts.URL + "/oauth/callback?code=google-code&state=" + url.QueryEscape(state))
	if err != nil {
		t.Fatalf("GET /oauth/callback: %v", err)
	}
	cbResp.Body.Close()
	if cbResp.StatusCode != http.StatusFound {
		t.Fatalf("/oauth/callback status = %d body=%s", cbResp.StatusCode, "")
	}
	loc := cbResp.Header.Get("Location")
	portalURL, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("parse portal location %q: %v", loc, err)
	}
	if portalURL.Path != "/portal/" && portalURL.Path != "/portal" {
		t.Fatalf("Location = %q, want /portal/?token=...", loc)
	}
	token := portalURL.Query().Get("token")
	if token == "" {
		t.Fatalf("Location = %q, missing token", loc)
	}
	var sessionCookie string
	for _, c := range cbResp.Cookies() {
		if c.Name == "emailmcp_session" {
			sessionCookie = c.Value
		}
	}
	if sessionCookie != token {
		t.Fatalf("session cookie = %q, token = %q", sessionCookie, token)
	}
	sess, err := srv.authenticator.store.GetSessionByAccessToken(context.Background(), token)
	if err != nil {
		t.Fatalf("session not stored: %v", err)
	}
	if sess.ClientID != webUIClientID || sess.Subject != "sub-1" || sess.Email != "user@example.com" {
		t.Fatalf("session = %+v", sess)
	}
}

func TestAPIMe(t *testing.T) {
	srv, token := newTestWebServerWithSession(t)
	ts := httptest.NewServer(srv.HTTPHandler())
	defer ts.Close()

	// 1. Unauthenticated request
	respUnauth, err := http.Get(ts.URL + "/api/me")
	if err != nil {
		t.Fatalf("GET /api/me: %v", err)
	}
	respUnauth.Body.Close()
	if respUnauth.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", respUnauth.StatusCode)
	}

	// 2. Authenticated via Bearer token
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	respAuth, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/me with auth: %v", err)
	}
	defer respAuth.Body.Close()
	if respAuth.StatusCode != http.StatusOK {
		t.Fatalf("authenticated status = %d, want 200", respAuth.StatusCode)
	}

	var user WebUser
	if err := json.NewDecoder(respAuth.Body).Decode(&user); err != nil {
		t.Fatalf("decode WebUser: %v", err)
	}
	if user.Subject != "user123" || user.Email != "user123@example.com" || !user.Authenticated {
		t.Fatalf("unexpected user: %+v", user)
	}

	// 3. Authenticated via session cookie
	reqCookie, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/me", nil)
	reqCookie.AddCookie(&http.Cookie{Name: "emailmcp_session", Value: token})
	respCookie, err := http.DefaultClient.Do(reqCookie)
	if err != nil {
		t.Fatalf("GET /api/me with cookie: %v", err)
	}
	defer respCookie.Body.Close()
	if respCookie.StatusCode != http.StatusOK {
		t.Fatalf("cookie auth status = %d, want 200", respCookie.StatusCode)
	}
}

func TestAPIAccounts_FullCRUD(t *testing.T) {
	srv, token := newTestWebServerWithSession(t)
	ts := httptest.NewServer(srv.HTTPHandler())
	defer ts.Close()

	client := http.DefaultClient

	// 1. Initially empty list
	reqList, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/accounts", nil)
	reqList.Header.Set("Authorization", "Bearer "+token)
	respList, err := client.Do(reqList)
	if err != nil {
		t.Fatalf("GET /api/accounts: %v", err)
	}
	defer respList.Body.Close()
	if respList.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/accounts status = %d, want 200", respList.StatusCode)
	}
	var listRes struct {
		Accounts []types.AccountSummary `json:"accounts"`
	}
	_ = json.NewDecoder(respList.Body).Decode(&listRes)
	if len(listRes.Accounts) != 0 {
		t.Fatalf("expected 0 accounts, got %d", len(listRes.Accounts))
	}

	// 2. Create account (POST /api/accounts)
	useTLS := true
	createPayload := AccountPayload{
		ID:           "work-mail",
		Name:         "Work Fastmail",
		IMAPHost:     "imap.fastmail.com",
		IMAPPort:     993,
		IMAPUsername: "worker@fastmail.com",
		IMAPPassword: "secret-imap-password",
		IMAPUseTLS:   &useTLS,
		SMTPHost:     "smtp.fastmail.com",
		SMTPPort:     587,
		SMTPUsername: "worker@fastmail.com",
		SMTPPassword: "secret-smtp-password",
		SMTPUseTLS:   &useTLS,
		FromAddress:  "Worker <worker@fastmail.com>",
	}
	createJSON, _ := json.Marshal(createPayload)
	reqCreate, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/accounts", bytes.NewReader(createJSON))
	reqCreate.Header.Set("Authorization", "Bearer "+token)
	reqCreate.Header.Set("Content-Type", "application/json")
	respCreate, err := client.Do(reqCreate)
	if err != nil {
		t.Fatalf("POST /api/accounts: %v", err)
	}
	defer respCreate.Body.Close()
	if respCreate.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(respCreate.Body)
		t.Fatalf("POST /api/accounts status = %d (want 201), body = %s", respCreate.StatusCode, string(b))
	}

	var createdAcc AccountResponse
	_ = json.NewDecoder(respCreate.Body).Decode(&createdAcc)
	if createdAcc.ID != "work-mail" || createdAcc.Name != "Work Fastmail" || !createdAcc.HasPassword {
		t.Fatalf("unexpected created account: %+v", createdAcc)
	}

	// 2b. List must return the account without credential material.
	reqListAfter, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/accounts", nil)
	reqListAfter.Header.Set("Authorization", "Bearer "+token)
	respListAfter, err := client.Do(reqListAfter)
	if err != nil {
		t.Fatalf("GET /api/accounts after create: %v", err)
	}
	listBody, _ := io.ReadAll(respListAfter.Body)
	respListAfter.Body.Close()
	if respListAfter.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/accounts after create status = %d, want 200; body = %s", respListAfter.StatusCode, listBody)
	}
	if strings.Contains(string(listBody), "secret-imap-password") || strings.Contains(string(listBody), "secret-smtp-password") {
		t.Fatalf("GET /api/accounts leaked a password: %s", listBody)
	}
	var listed struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if err := json.Unmarshal(listBody, &listed); err != nil {
		t.Fatalf("decode list after create: %v\nbody = %s", err, listBody)
	}
	if len(listed.Accounts) != 1 {
		t.Fatalf("expected 1 listed account, got %d: %s", len(listed.Accounts), listBody)
	}
	for _, key := range []string{"password", "imap_password", "smtp_password"} {
		if _, ok := listed.Accounts[0][key]; ok {
			t.Fatalf("GET /api/accounts included %q: %s", key, listBody)
		}
	}
	if listed.Accounts[0]["id"] != "work-mail" || listed.Accounts[0]["imap_username"] != "worker@fastmail.com" {
		t.Fatalf("unexpected listed account: %s", listBody)
	}
	if listed.Accounts[0]["has_password"] != true {
		t.Fatalf("expected has_password=true, got %s", listBody)
	}

	// 3. Get single account (GET /api/accounts/work-mail)
	reqGet, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/accounts/work-mail", nil)
	reqGet.Header.Set("Authorization", "Bearer "+token)
	respGet, err := client.Do(reqGet)
	if err != nil {
		t.Fatalf("GET /api/accounts/work-mail: %v", err)
	}
	defer respGet.Body.Close()
	if respGet.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/accounts/work-mail status = %d, want 200", respGet.StatusCode)
	}
	getBody, _ := io.ReadAll(respGet.Body)
	if strings.Contains(string(getBody), "secret-imap-password") || strings.Contains(string(getBody), "secret-smtp-password") {
		t.Fatalf("GET /api/accounts/work-mail leaked a password: %s", getBody)
	}
	var gotAcc AccountResponse
	if err := json.Unmarshal(getBody, &gotAcc); err != nil {
		t.Fatalf("decode got account: %v", err)
	}
	if gotAcc.ID != "work-mail" || gotAcc.IMAPHost != "imap.fastmail.com" || !gotAcc.HasPassword {
		t.Fatalf("unexpected got account: %+v", gotAcc)
	}

	// 4. Update account (PUT /api/accounts/work-mail) - change name, keep password blank
	updatePayload := AccountPayload{
		Name:         "Updated Work Mail",
		IMAPHost:     "imap.fastmail.com",
		IMAPPort:     993,
		IMAPUsername: "worker@fastmail.com",
		IMAPPassword: "", // empty - keeps existing password!
		SMTPHost:     "smtp.fastmail.com",
		SMTPPort:     587,
	}
	updateJSON, _ := json.Marshal(updatePayload)
	reqUpdate, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/accounts/work-mail", bytes.NewReader(updateJSON))
	reqUpdate.Header.Set("Authorization", "Bearer "+token)
	reqUpdate.Header.Set("Content-Type", "application/json")
	respUpdate, err := client.Do(reqUpdate)
	if err != nil {
		t.Fatalf("PUT /api/accounts/work-mail: %v", err)
	}
	defer respUpdate.Body.Close()
	if respUpdate.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(respUpdate.Body)
		t.Fatalf("PUT /api/accounts/work-mail status = %d (want 200), body = %s", respUpdate.StatusCode, string(b))
	}

	// Verify password was preserved in store
	stored, err := srv.configStore.GetUserConfig(context.Background(), "user123", "work-mail")
	if err != nil {
		t.Fatalf("GetUserConfig: %v", err)
	}
	if stored.Name != "Updated Work Mail" {
		t.Errorf("Name = %q, want 'Updated Work Mail'", stored.Name)
	}
	if stored.IMAPPassword != "secret-imap-password" {
		t.Errorf("IMAPPassword was lost: got %q, want 'secret-imap-password'", stored.IMAPPassword)
	}

	// 5. Delete account (DELETE /api/accounts/work-mail)
	reqDel, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/accounts/work-mail", nil)
	reqDel.Header.Set("Authorization", "Bearer "+token)
	respDel, err := client.Do(reqDel)
	if err != nil {
		t.Fatalf("DELETE /api/accounts/work-mail: %v", err)
	}
	defer respDel.Body.Close()
	if respDel.StatusCode != http.StatusOK {
		t.Fatalf("DELETE /api/accounts/work-mail status = %d, want 200", respDel.StatusCode)
	}

	// Confirm it's gone
	_, err = srv.configStore.GetUserConfig(context.Background(), "user123", "work-mail")
	if err == nil {
		t.Fatal("expected error getting deleted account, got nil")
	}
}

func TestAPIAccounts_Validation(t *testing.T) {
	srv, token := newTestWebServerWithSession(t)
	ts := httptest.NewServer(srv.HTTPHandler())
	defer ts.Close()

	client := http.DefaultClient

	// SSRF: private host rejection
	useTLS := true
	payload := AccountPayload{
		Name:         "Private Host Account",
		IMAPHost:     "192.168.1.1",
		IMAPPort:     993,
		IMAPUsername: "user@private.com",
		IMAPPassword: "pass",
		IMAPUseTLS:   &useTLS,
		SMTPHost:     "smtp.gmail.com",
		SMTPPort:     587,
		SMTPUseTLS:   &useTLS,
	}
	bodyJSON, _ := json.Marshal(payload)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/accounts", bytes.NewReader(bodyJSON))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /api/accounts: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for private host, got %d", resp.StatusCode)
	}
}

func TestAPIAccounts_VerifyUsesSavedAccount(t *testing.T) {
	srv, token := newTestWebServerWithSession(t)
	ts := httptest.NewServer(srv.HTTPHandler())
	defer ts.Close()

	client := http.DefaultClient

	reqMissing, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/accounts/missing/verify", nil)
	reqMissing.Header.Set("Authorization", "Bearer "+token)
	respMissing, err := client.Do(reqMissing)
	if err != nil {
		t.Fatalf("POST /api/accounts/missing/verify: %v", err)
	}
	defer respMissing.Body.Close()
	if respMissing.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for missing account, got %d", respMissing.StatusCode)
	}

	noPass := &types.Account{
		ID:           "no-pass",
		OwnerUserID:  "user123",
		Name:         "No Password",
		IMAPHost:     "imap.gmail.com",
		IMAPPort:     993,
		IMAPUsername: "user@gmail.com",
		IMAPPassword: "",
		IMAPUseTLS:   true,
		SMTPHost:     "smtp.gmail.com",
		SMTPPort:     587,
		SMTPUseTLS:   true,
	}
	if err := srv.configStore.PutUserConfig(context.Background(), "user123", noPass); err != nil {
		t.Fatalf("PutUserConfig no-pass: %v", err)
	}
	reqNoPass, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/accounts/no-pass/verify", nil)
	reqNoPass.Header.Set("Authorization", "Bearer "+token)
	respNoPass, err := client.Do(reqNoPass)
	if err != nil {
		t.Fatalf("POST /api/accounts/no-pass/verify: %v", err)
	}
	defer respNoPass.Body.Close()
	if respNoPass.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for account without stored password, got %d", respNoPass.StatusCode)
	}

	// Seed a stored account with a private host. Verification must use those
	// saved values (and fail SSRF quickly) even if a public-host payload is sent.
	saved := &types.Account{
		ID:           "saved-mail",
		OwnerUserID:  "user123",
		Name:         "Saved Mail",
		IMAPHost:     "127.0.0.1",
		IMAPPort:     993,
		IMAPUsername: "user@local",
		IMAPPassword: "stored-secret",
		IMAPUseTLS:   true,
		SMTPHost:     "10.0.0.1",
		SMTPPort:     587,
		SMTPUsername: "user@local",
		SMTPPassword: "stored-secret",
		SMTPUseTLS:   true,
	}
	if err := srv.configStore.PutUserConfig(context.Background(), "user123", saved); err != nil {
		t.Fatalf("PutUserConfig saved-mail: %v", err)
	}

	ignoredPayload, _ := json.Marshal(AccountPayload{
		ID:           "saved-mail",
		IMAPHost:     "imap.gmail.com",
		IMAPUsername: "attacker@gmail.com",
		IMAPPassword: "attacker-secret",
		SMTPHost:     "smtp.gmail.com",
	})
	reqVerify, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/accounts/saved-mail/verify", bytes.NewReader(ignoredPayload))
	reqVerify.Header.Set("Authorization", "Bearer "+token)
	reqVerify.Header.Set("Content-Type", "application/json")
	respVerify, err := client.Do(reqVerify)
	if err != nil {
		t.Fatalf("POST /api/accounts/saved-mail/verify: %v", err)
	}
	defer respVerify.Body.Close()
	if respVerify.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(respVerify.Body)
		t.Fatalf("expected 200 for saved-account verify, got %d: %s", respVerify.StatusCode, b)
	}

	var res VerificationResult
	if err := json.NewDecoder(respVerify.Body).Decode(&res); err != nil {
		t.Fatalf("decode verify result: %v", err)
	}
	if res.Success {
		t.Fatal("expected stored private-host account verification to fail")
	}
	if res.IMAP.Success || res.SMTP.Success {
		t.Fatalf("expected IMAP and SMTP to fail using stored private hosts, got: %+v", res)
	}
	if !strings.Contains(res.IMAP.Error, "not allowed") {
		t.Fatalf("expected IMAP SSRF error from stored host, got %q", res.IMAP.Error)
	}
}

func TestAPIAccounts_WebhookSettings(t *testing.T) {
	srv, token := newTestWebServerWithSession(t)
	ts := httptest.NewServer(srv.HTTPHandler())
	defer ts.Close()

	client := http.DefaultClient
	useTLS := true
	webhookURL := "https://8.8.8.8/notify"
	createPayload := AccountPayload{
		ID:           "notify-mail",
		Name:         "Notify Mail",
		IMAPHost:     "imap.fastmail.com",
		IMAPPort:     993,
		IMAPUsername: "worker@fastmail.com",
		IMAPPassword: "secret-imap-password",
		IMAPUseTLS:   &useTLS,
		SMTPHost:     "smtp.fastmail.com",
		SMTPPort:     587,
		SMTPUseTLS:   &useTLS,
		WebhookURL:   &webhookURL,
		WebhookToken: "webhook-secret",
		WatchFolders: []string{"INBOX", "Sent"},
	}
	createJSON, _ := json.Marshal(createPayload)
	reqCreate, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/accounts", bytes.NewReader(createJSON))
	reqCreate.Header.Set("Authorization", "Bearer "+token)
	reqCreate.Header.Set("Content-Type", "application/json")
	respCreate, err := client.Do(reqCreate)
	if err != nil {
		t.Fatalf("POST /api/accounts: %v", err)
	}
	defer respCreate.Body.Close()
	createBody, _ := io.ReadAll(respCreate.Body)
	if respCreate.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/accounts status = %d (want 201), body = %s", respCreate.StatusCode, createBody)
	}
	if strings.Contains(string(createBody), "webhook-secret") {
		t.Fatalf("create response leaked webhook token: %s", createBody)
	}
	created := mustAccountMap(t, createBody)
	if created["webhook_url"] != webhookURL {
		t.Fatalf("expected webhook_url in create response, got %s", createBody)
	}
	if created["has_webhook_token"] != true {
		t.Fatalf("expected has_webhook_token=true, got %s", createBody)
	}
	if _, ok := created["webhook_token"]; ok {
		t.Fatalf("create response included webhook_token: %s", createBody)
	}

	reqGet, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/accounts/notify-mail", nil)
	reqGet.Header.Set("Authorization", "Bearer "+token)
	respGet, err := client.Do(reqGet)
	if err != nil {
		t.Fatalf("GET /api/accounts/notify-mail: %v", err)
	}
	getBody, _ := io.ReadAll(respGet.Body)
	respGet.Body.Close()
	if respGet.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, body = %s", respGet.StatusCode, getBody)
	}
	if strings.Contains(string(getBody), "webhook-secret") {
		t.Fatalf("GET leaked webhook token: %s", getBody)
	}
	got := mustAccountMap(t, getBody)
	if got["webhook_url"] != webhookURL || got["has_webhook_token"] != true {
		t.Fatalf("unexpected GET account: %s", getBody)
	}
	if _, ok := got["webhook_token"]; ok {
		t.Fatalf("GET included webhook_token: %s", getBody)
	}

	reqList, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/accounts", nil)
	reqList.Header.Set("Authorization", "Bearer "+token)
	respList, err := client.Do(reqList)
	if err != nil {
		t.Fatalf("GET /api/accounts: %v", err)
	}
	listBody, _ := io.ReadAll(respList.Body)
	respList.Body.Close()
	if strings.Contains(string(listBody), "webhook-secret") {
		t.Fatalf("list leaked webhook token: %s", listBody)
	}
	var listed struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if err := json.Unmarshal(listBody, &listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listed.Accounts) != 1 {
		t.Fatalf("expected 1 account, got %s", listBody)
	}
	if _, ok := listed.Accounts[0]["webhook_token"]; ok {
		t.Fatalf("list included webhook_token: %s", listBody)
	}
	if listed.Accounts[0]["has_webhook_token"] != true {
		t.Fatalf("expected has_webhook_token on list, got %s", listBody)
	}

	updatePayload := AccountPayload{
		Name:         "Notify Mail Updated",
		IMAPHost:     "imap.fastmail.com",
		IMAPPort:     993,
		IMAPUsername: "worker@fastmail.com",
		SMTPHost:     "smtp.fastmail.com",
		SMTPPort:     587,
		WebhookURL:   &webhookURL,
		WebhookToken: "",
	}
	updateJSON, _ := json.Marshal(updatePayload)
	reqUpdate, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/accounts/notify-mail", bytes.NewReader(updateJSON))
	reqUpdate.Header.Set("Authorization", "Bearer "+token)
	reqUpdate.Header.Set("Content-Type", "application/json")
	respUpdate, err := client.Do(reqUpdate)
	if err != nil {
		t.Fatalf("PUT /api/accounts/notify-mail: %v", err)
	}
	updateBody, _ := io.ReadAll(respUpdate.Body)
	respUpdate.Body.Close()
	if respUpdate.StatusCode != http.StatusOK {
		t.Fatalf("PUT status = %d, body = %s", respUpdate.StatusCode, updateBody)
	}
	if strings.Contains(string(updateBody), "webhook-secret") {
		t.Fatalf("update response leaked webhook token: %s", updateBody)
	}

	stored, err := srv.configStore.GetUserConfig(context.Background(), "user123", "notify-mail")
	if err != nil {
		t.Fatalf("GetUserConfig: %v", err)
	}
	if stored.WebhookToken != "webhook-secret" {
		t.Fatalf("blank webhook_token on update should keep previous token, got %q", stored.WebhookToken)
	}
	if stored.Name != "Notify Mail Updated" {
		t.Fatalf("name = %q, want updated", stored.Name)
	}
	if len(stored.WatchFolders) != 2 {
		t.Fatalf("omitted watch_folders should keep previous list, got %#v", stored.WatchFolders)
	}

	cleared := ""
	clearPayload := AccountPayload{
		Name:       "Notify Mail Updated",
		IMAPHost:   "imap.fastmail.com",
		IMAPPort:   993,
		SMTPHost:   "smtp.fastmail.com",
		SMTPPort:   587,
		WebhookURL: &cleared,
	}
	clearJSON, _ := json.Marshal(clearPayload)
	reqClear, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/accounts/notify-mail", bytes.NewReader(clearJSON))
	reqClear.Header.Set("Authorization", "Bearer "+token)
	reqClear.Header.Set("Content-Type", "application/json")
	respClear, err := client.Do(reqClear)
	if err != nil {
		t.Fatalf("PUT clear webhook: %v", err)
	}
	clearBody, _ := io.ReadAll(respClear.Body)
	respClear.Body.Close()
	if respClear.StatusCode != http.StatusOK {
		t.Fatalf("PUT clear status = %d, body = %s", respClear.StatusCode, clearBody)
	}
	stored, err = srv.configStore.GetUserConfig(context.Background(), "user123", "notify-mail")
	if err != nil {
		t.Fatalf("GetUserConfig after clear: %v", err)
	}
	if stored.WebhookURL != "" {
		t.Fatalf("expected webhook_url cleared, got %q", stored.WebhookURL)
	}
}

func TestAPIAccounts_WebhookValidation(t *testing.T) {
	srv, token := newTestWebServerWithSession(t)
	ts := httptest.NewServer(srv.HTTPHandler())
	defer ts.Close()

	client := http.DefaultClient
	useTLS := true
	base := AccountPayload{
		Name:         "Notify Mail",
		IMAPHost:     "imap.fastmail.com",
		IMAPPort:     993,
		IMAPUsername: "worker@fastmail.com",
		IMAPPassword: "secret-imap-password",
		IMAPUseTLS:   &useTLS,
		SMTPHost:     "smtp.fastmail.com",
		SMTPPort:     587,
		SMTPUseTLS:   &useTLS,
	}

	post := func(p AccountPayload) *http.Response {
		t.Helper()
		body, _ := json.Marshal(p)
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/accounts", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST /api/accounts: %v", err)
		}
		return resp
	}

	httpURL := "http://8.8.8.8/notify"
	httpPayload := base
	httpPayload.ID = "http-hook"
	httpPayload.WebhookURL = &httpURL
	resp := post(httpPayload)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for http webhook, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	privateURL := "https://192.168.1.1/notify"
	privatePayload := base
	privatePayload.ID = "private-hook"
	privatePayload.WebhookURL = &privateURL
	resp = post(privatePayload)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for private webhook host, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	tooMany := base
	tooMany.ID = "too-many-folders"
	tooMany.WatchFolders = []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}
	resp = post(tooMany)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for more than 8 folders, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	emptyFolders := base
	emptyFolders.ID = "default-folders"
	emptyFolders.WatchFolders = []string{}
	resp = post(emptyFolders)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("empty watch_folders should be accepted, got %d: %s", resp.StatusCode, body)
	}
}

func TestAPIAccounts_WebhookPUTSemanticsAndValidation(t *testing.T) {
	srv, token := newTestWebServerWithSession(t)
	ts := httptest.NewServer(srv.HTTPHandler())
	defer ts.Close()

	useTLS := true
	webhookURL := "https://8.8.8.8/hook"
	createPayload := AccountPayload{
		ID:           "put-notify",
		Name:         "PUT Notify",
		IMAPHost:     "8.8.8.8",
		IMAPPort:     993,
		IMAPUsername: "worker@example.com",
		IMAPPassword: "imap-secret",
		IMAPUseTLS:   &useTLS,
		SMTPHost:     "1.1.1.1",
		SMTPPort:     587,
		SMTPUseTLS:   &useTLS,
		WebhookURL:   &webhookURL,
		WebhookToken: "old-token",
		WatchFolders: []string{"INBOX", "Sent"},
	}
	createBody, err := json.Marshal(createPayload)
	if err != nil {
		t.Fatal(err)
	}
	createReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/accounts", bytes.NewReader(createBody))
	createReq.Header.Set("Authorization", "Bearer "+token)
	createReq.Header.Set("Content-Type", "application/json")
	createResp, err := http.DefaultClient.Do(createReq)
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	createResp.Body.Close()
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", createResp.StatusCode)
	}

	put := func(payload AccountPayload) (int, []byte) {
		t.Helper()
		body, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal PUT payload: %v", err)
		}
		req, err := http.NewRequest(http.MethodPut, ts.URL+"/api/accounts/put-notify", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("build PUT request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("PUT account: %v", err)
		}
		defer resp.Body.Close()
		responseBody, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read PUT response: %v", err)
		}
		return resp.StatusCode, responseBody
	}

	invalidURLs := []struct {
		name string
		url  string
	}{
		{name: "http scheme", url: "http://8.8.8.8/hook"},
		{name: "private host", url: "https://192.168.1.1/hook"},
		{name: "userinfo", url: "https://user:pass@8.8.8.8/hook"},
	}
	for _, tc := range invalidURLs {
		t.Run("reject "+tc.name, func(t *testing.T) {
			badURL := tc.url
			status, body := put(AccountPayload{WebhookURL: &badURL})
			if status != http.StatusBadRequest {
				t.Fatalf("PUT status = %d, want 400: %s", status, body)
			}
			stored, err := srv.configStore.GetUserConfig(context.Background(), "user123", "put-notify")
			if err != nil {
				t.Fatalf("GetUserConfig after rejected PUT: %v", err)
			}
			if stored.WebhookURL != webhookURL || stored.WebhookToken != "old-token" || !reflect.DeepEqual(stored.WatchFolders, []string{"INBOX", "Sent"}) {
				t.Fatalf("rejected PUT changed stored webhook settings: URL=%q token=%q folders=%#v", stored.WebhookURL, stored.WebhookToken, stored.WatchFolders)
			}
		})
	}

	tooManyFolders := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}
	status, body := put(AccountPayload{WatchFolders: tooManyFolders})
	if status != http.StatusBadRequest {
		t.Fatalf("too-many-folders PUT status = %d, want 400: %s", status, body)
	}
	stored, err := srv.configStore.GetUserConfig(context.Background(), "user123", "put-notify")
	if err != nil {
		t.Fatal(err)
	}
	if stored.WebhookURL != webhookURL || stored.WebhookToken != "old-token" || !reflect.DeepEqual(stored.WatchFolders, []string{"INBOX", "Sent"}) {
		t.Fatalf("rejected folder PUT changed stored settings: URL=%q token=%q folders=%#v", stored.WebhookURL, stored.WebhookToken, stored.WatchFolders)
	}

	status, body = put(AccountPayload{WebhookToken: "replacement-token", WatchFolders: []string{" INBOX ", "inbox", "Sent", " Sent "}})
	if status != http.StatusOK {
		t.Fatalf("replace token PUT status = %d, want 200: %s", status, body)
	}
	stored, err = srv.configStore.GetUserConfig(context.Background(), "user123", "put-notify")
	if err != nil {
		t.Fatal(err)
	}
	if stored.WebhookToken != "replacement-token" {
		t.Fatalf("nonblank token should replace previous token, got %q", stored.WebhookToken)
	}
	if !reflect.DeepEqual(stored.WatchFolders, []string{"INBOX", "Sent"}) {
		t.Fatalf("folders should be trimmed and deduplicated, got %#v", stored.WatchFolders)
	}

	status, body = put(AccountPayload{WatchFolders: []string{}})
	if status != http.StatusOK {
		t.Fatalf("empty folders PUT status = %d, want 200: %s", status, body)
	}
	stored, err = srv.configStore.GetUserConfig(context.Background(), "user123", "put-notify")
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.WatchFolders) != 0 {
		t.Fatalf("empty watch_folders should reset to defaults, got %#v", stored.WatchFolders)
	}

	status, body = put(AccountPayload{ClearWebhookToken: true})
	if status != http.StatusOK {
		t.Fatalf("explicit token clear PUT status = %d, want 200: %s", status, body)
	}
	stored, err = srv.configStore.GetUserConfig(context.Background(), "user123", "put-notify")
	if err != nil {
		t.Fatal(err)
	}
	if stored.WebhookURL != webhookURL || stored.WebhookToken != "" {
		t.Fatalf("explicit clear should retain URL and clear token, got URL=%q token=%q", stored.WebhookURL, stored.WebhookToken)
	}

	status, body = put(AccountPayload{WebhookToken: "origin-token"})
	if status != http.StatusOK {
		t.Fatalf("set token PUT status = %d, want 200: %s", status, body)
	}
	newOriginURL := "https://1.1.1.1/new-hook"
	status, body = put(AccountPayload{WebhookURL: &newOriginURL})
	if status != http.StatusOK {
		t.Fatalf("change origin PUT status = %d, want 200: %s", status, body)
	}
	stored, err = srv.configStore.GetUserConfig(context.Background(), "user123", "put-notify")
	if err != nil {
		t.Fatal(err)
	}
	if stored.WebhookURL != newOriginURL || stored.WebhookToken != "" {
		t.Fatalf("origin change should clear stale token, got URL=%q token=%q", stored.WebhookURL, stored.WebhookToken)
	}

	status, body = put(AccountPayload{WebhookToken: "clear-on-remove"})
	if status != http.StatusOK {
		t.Fatalf("set token before removal PUT status = %d, want 200: %s", status, body)
	}
	clearURL := ""
	status, body = put(AccountPayload{WebhookURL: &clearURL})
	if status != http.StatusOK {
		t.Fatalf("clear URL PUT status = %d, want 200: %s", status, body)
	}
	stored, err = srv.configStore.GetUserConfig(context.Background(), "user123", "put-notify")
	if err != nil {
		t.Fatal(err)
	}
	if stored.WebhookURL != "" || stored.WebhookToken != "" {
		t.Fatalf("clearing URL should clear token, got URL=%q token=%q", stored.WebhookURL, stored.WebhookToken)
	}
}

func TestValidateWebhookSettings(t *testing.T) {
	if err := validateWebhookSettings("", "", nil); err != nil {
		t.Fatalf("empty webhook should be valid: %v", err)
	}
	if err := validateWebhookSettings("", "", []string{}); err != nil {
		t.Fatalf("empty folders should be valid: %v", err)
	}
	if err := validateWebhookSettings("https://8.8.8.8/hook", "", []string{"INBOX"}); err != nil {
		t.Fatalf("public https webhook should be valid: %v", err)
	}
	if err := validateWebhookSettings("http://8.8.8.8/hook", "", nil); err == nil {
		t.Fatal("expected error for http webhook")
	}
	if err := validateWebhookSettings("https://192.168.0.5/hook", "", nil); err == nil {
		t.Fatal("expected error for private webhook host")
	}
	if err := validateWebhookSettings("https://user:pass@8.8.8.8/hook", "", nil); err == nil {
		t.Fatal("expected error for URL userinfo")
	}
	if err := validateWebhookSettings("", "", []string{"1", "2", "3", "4", "5", "6", "7", "8", "9"}); err == nil {
		t.Fatal("expected error for more than 8 folders")
	}
	if err := validateWebhookSettings("", "secret", nil); err == nil {
		t.Fatal("expected error for token without URL")
	}
}

func TestAPIAccounts_WebhookOriginAndRetention(t *testing.T) {
	srv, token := newTestWebServerWithSession(t)
	ts := httptest.NewServer(srv.HTTPHandler())
	defer ts.Close()

	do := func(method, path string, payload AccountPayload) (int, []byte) {
		t.Helper()
		b, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		req, err := http.NewRequest(method, ts.URL+path, bytes.NewReader(b))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, body
	}
	stored := func() *types.Account {
		t.Helper()
		acc, err := srv.configStore.GetUserConfig(context.Background(), "user123", "origin-test")
		if err != nil {
			t.Fatalf("GetUserConfig: %v", err)
		}
		return acc
	}

	useTLS := true
	url1 := "https://8.8.8.8:443/hook"
	status, body := do(http.MethodPost, "/api/accounts", AccountPayload{
		ID:           "origin-test",
		Name:         "Origin Test",
		IMAPHost:     "1.1.1.1",
		IMAPPort:     993,
		IMAPUsername: "u",
		IMAPPassword: "p",
		IMAPUseTLS:   &useTLS,
		SMTPHost:     "1.1.1.1",
		SMTPPort:     587,
		SMTPUseTLS:   &useTLS,
		WebhookURL:   &url1,
		WebhookToken: " my-token ",
	})
	if status != http.StatusCreated {
		t.Fatalf("POST status = %d, body = %s", status, body)
	}
	if got := stored().WebhookToken; got != "my-token" {
		t.Fatalf("token should be trimmed on create, got %q", got)
	}

	// Same origin (default port 443 == explicit 443), different path: token kept.
	url2 := "https://8.8.8.8/other"
	status, body = do(http.MethodPut, "/api/accounts/origin-test", AccountPayload{WebhookURL: &url2})
	if status != http.StatusOK {
		t.Fatalf("PUT same-origin status = %d, body = %s", status, body)
	}
	if got := stored(); got.WebhookURL != url2 || got.WebhookToken != "my-token" {
		t.Fatalf("same-origin update should keep token, got URL=%q token=%q", got.WebhookURL, got.WebhookToken)
	}

	// Rejected: token without URL. Nothing may change.
	url3 := ""
	status, _ = do(http.MethodPut, "/api/accounts/origin-test", AccountPayload{WebhookURL: &url3, WebhookToken: "new-token"})
	if status != http.StatusBadRequest {
		t.Fatalf("expected 400 for token without URL, got %d", status)
	}
	if got := stored(); got.WebhookURL != url2 || got.WebhookToken != "my-token" {
		t.Fatalf("rejected PUT changed config: URL=%q token=%q", got.WebhookURL, got.WebhookToken)
	}

	// Different port is a different origin: token cleared.
	url4 := "https://8.8.8.8:8443/x"
	status, body = do(http.MethodPut, "/api/accounts/origin-test", AccountPayload{WebhookURL: &url4})
	if status != http.StatusOK {
		t.Fatalf("PUT port change status = %d, body = %s", status, body)
	}
	if got := stored(); got.WebhookURL != url4 || got.WebhookToken != "" {
		t.Fatalf("port change should clear token, got URL=%q token=%q", got.WebhookURL, got.WebhookToken)
	}

	// POST with token and no URL is rejected and not stored.
	status, _ = do(http.MethodPost, "/api/accounts", AccountPayload{
		ID: "no-url", Name: "No URL", IMAPHost: "1.1.1.1", IMAPPort: 993, IMAPUsername: "u",
		IMAPPassword: "p", IMAPUseTLS: &useTLS, SMTPHost: "1.1.1.1", SMTPPort: 587,
		SMTPUseTLS: &useTLS, WebhookToken: "orphan",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("expected 400 for POST token without URL, got %d", status)
	}
	if _, err := srv.configStore.GetUserConfig(context.Background(), "user123", "no-url"); err == nil {
		t.Fatal("rejected POST must not persist account")
	}
}

func mustAccountMap(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode account JSON: %v\nbody = %s", err, body)
	}
	return got
}

func TestAPILogout(t *testing.T) {
	srv, token := newTestWebServerWithSession(t)
	ts := httptest.NewServer(srv.HTTPHandler())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/logout", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /api/logout: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// Session should be deleted
	_, err = srv.authenticator.store.GetSessionByAccessToken(context.Background(), token)
	if err == nil {
		t.Fatal("expected session to be deleted, got nil error")
	}
}
