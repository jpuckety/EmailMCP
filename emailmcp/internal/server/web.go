package server

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/jpuckett/EmailMCP/emailmcp/internal/types"
	"golang.org/x/oauth2"
)

//go:embed all:dist
var distFS embed.FS

// WebUser represents the user info returned to the Angular frontend.
type WebUser struct {
	Subject       string `json:"subject"`
	Email         string `json:"email"`
	Authenticated bool   `json:"authenticated"`
}

// AccountPayload represents the request payload for creating/updating an account.
type AccountPayload struct {
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	IMAPHost          string   `json:"imap_host"`
	IMAPPort          int      `json:"imap_port"`
	IMAPUsername      string   `json:"imap_username"`
	IMAPPassword      string   `json:"imap_password,omitempty"`
	IMAPUseTLS        *bool    `json:"imap_use_tls"`
	SMTPHost          string   `json:"smtp_host"`
	SMTPPort          int      `json:"smtp_port"`
	SMTPUsername      string   `json:"smtp_username,omitempty"`
	SMTPPassword      string   `json:"smtp_password,omitempty"`
	SMTPUseTLS        *bool    `json:"smtp_use_tls"`
	FromAddress       string   `json:"from_address,omitempty"`
	WebhookURL        *string  `json:"webhook_url"`
	WebhookToken      string   `json:"webhook_token,omitempty"`
	ClearWebhookToken bool     `json:"clear_webhook_token,omitempty"`
	WatchFolders      []string `json:"watch_folders"`
}

// AccountResponse represents the account details returned to the frontend.
type AccountResponse struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	IMAPHost        string   `json:"imap_host"`
	IMAPPort        int      `json:"imap_port"`
	IMAPUsername    string   `json:"imap_username"`
	IMAPUseTLS      bool     `json:"imap_use_tls"`
	SMTPHost        string   `json:"smtp_host"`
	SMTPPort        int      `json:"smtp_port"`
	SMTPUsername    string   `json:"smtp_username,omitempty"`
	SMTPUseTLS      bool     `json:"smtp_use_tls"`
	FromAddress     string   `json:"from_address,omitempty"`
	HasPassword     bool     `json:"has_password"`
	WebhookURL      string   `json:"webhook_url,omitempty"`
	HasWebhookToken bool     `json:"has_webhook_token"`
	WatchFolders    []string `json:"watch_folders,omitempty"`
	CreatedAt       string   `json:"created_at,omitempty"`
	UpdatedAt       string   `json:"updated_at,omitempty"`
}

func newAccountResponse(acc *types.Account) AccountResponse {
	if acc == nil {
		return AccountResponse{}
	}
	return AccountResponse{
		ID:              acc.ID,
		Name:            acc.Name,
		IMAPHost:        acc.IMAPHost,
		IMAPPort:        acc.IMAPPort,
		IMAPUsername:    acc.IMAPUsername,
		IMAPUseTLS:      acc.IMAPUseTLS,
		SMTPHost:        acc.SMTPHost,
		SMTPPort:        acc.SMTPPort,
		SMTPUsername:    acc.SMTPUsername,
		SMTPUseTLS:      acc.SMTPUseTLS,
		FromAddress:     acc.FromAddress,
		HasPassword:     acc.IMAPPassword != "" || acc.SMTPPassword != "",
		WebhookURL:      acc.WebhookURL,
		HasWebhookToken: acc.WebhookToken != "",
		WatchFolders:    append([]string(nil), acc.WatchFolders...),
		CreatedAt:       formatTime(acc.CreatedAt),
		UpdatedAt:       formatTime(acc.UpdatedAt),
	}
}

func (s *Server) mountWebRoutes(mux *http.ServeMux) {
	// Web authentication endpoints
	mux.HandleFunc("GET /auth/login", s.handleWebLogin)
	mux.HandleFunc("GET /auth/callback", s.handleWebCallback)
	mux.HandleFunc("POST /api/logout", s.handleAPILogout)
	mux.HandleFunc("GET /api/me", s.handleAPIMe)

	// Account management API endpoints
	mux.HandleFunc("GET /api/accounts", s.handleListAccounts)
	mux.HandleFunc("POST /api/accounts", s.handleCreateAccount)
	mux.HandleFunc("GET /api/accounts/{id}", s.handleGetAccount)
	mux.HandleFunc("PUT /api/accounts/{id}", s.handleUpdateAccount)
	mux.HandleFunc("DELETE /api/accounts/{id}", s.handleDeleteAccount)
	mux.HandleFunc("POST /api/accounts/{id}/verify", s.handleVerifyAccount)

	// User portal static web application
	mux.HandleFunc("/portal", s.handleWebStatic)
	mux.HandleFunc("/portal/", s.handleWebStatic)
}

func (s *Server) handleWebStatic(w http.ResponseWriter, r *http.Request) {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		http.Error(w, "UI assets not available", http.StatusInternalServerError)
		return
	}

	cleanPath := strings.TrimPrefix(r.URL.Path, "/portal")
	cleanPath = strings.TrimPrefix(cleanPath, "/")
	if cleanPath == "" || cleanPath == "." {
		cleanPath = "index.html"
	}

	// Check if the requested file exists in the embedded filesystem
	if cleanPath != "index.html" {
		f, err := sub.Open(cleanPath)
		if err == nil {
			stat, statErr := f.Stat()
			_ = f.Close()
			if statErr == nil && !stat.IsDir() {
				// Serve the static asset with proper content-type
				ext := filepath.Ext(cleanPath)
				if mimeType := mime.TypeByExtension(ext); mimeType != "" {
					w.Header().Set("Content-Type", mimeType)
				}
				if ext == ".js" || ext == ".css" || ext == ".ico" || ext == ".woff2" {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				http.StripPrefix("/portal", http.FileServer(http.FS(sub))).ServeHTTP(w, r)
				return
			}
		}
	}

	// Fallback to index.html for SPA client-side routing
	indexFile, err := sub.Open("index.html")
	if err != nil {
		http.Error(w, "index.html not found", http.StatusNotFound)
		return
	}
	defer indexFile.Close()

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	_, _ = io.Copy(w, indexFile)
}

// GET /auth/login - initiates Google OAuth login for web browser
func (s *Server) handleWebLogin(w http.ResponseWriter, r *http.Request) {
	if s.oauth == nil || s.oauth.googleConfig == nil {
		http.Error(w, "Google OAuth is not configured on this server", http.StatusBadRequest)
		return
	}

	state, err := randomToken(24)
	if err != nil {
		http.Error(w, "failed to generate state", http.StatusInternalServerError)
		return
	}

	s.oauth.mu.Lock()
	s.oauth.cleanupLocked()
	s.oauth.pending[state] = &pendingAuth{
		ClientID:    webUIClientID,
		RedirectURI: s.cfg.PublicBaseURL + "/auth/callback",
		ClientState: "web",
		ExpiresAt:   time.Now().Add(oauthPendingTTL),
	}
	s.oauth.mu.Unlock()

	authURL := s.oauth.googleConfig.AuthCodeURL(state,
		oauth2.AccessTypeOffline,
		oauth2.SetAuthURLParam("prompt", "consent"),
	)
	http.Redirect(w, r, authURL, http.StatusFound)
}

// GET /auth/callback - Google OAuth callback for web login
func (s *Server) handleWebCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if errParam := q.Get("error"); errParam != "" {
		http.Redirect(w, r, "/portal/login?error="+errParam, http.StatusFound)
		return
	}
	code := q.Get("code")
	state := q.Get("state")
	if code == "" || state == "" {
		http.Redirect(w, r, "/portal/login?error=missing_code_or_state", http.StatusFound)
		return
	}

	if s.oauth == nil {
		http.Redirect(w, r, "/portal/login?error=oauth_not_configured", http.StatusFound)
		return
	}

	s.oauth.mu.Lock()
	pending, ok := s.oauth.pending[state]
	if ok {
		delete(s.oauth.pending, state)
	}
	s.oauth.mu.Unlock()

	if !ok || time.Now().After(pending.ExpiresAt) {
		http.Redirect(w, r, "/portal/login?error=expired_state", http.StatusFound)
		return
	}

	ctx := r.Context()
	tok, err := s.oauth.exchangeCode(ctx, code)
	if err != nil {
		s.logger.Error("web login google token exchange failed", "error", err)
		http.Redirect(w, r, "/portal/login?error=exchange_failed", http.StatusFound)
		return
	}

	idToken, _ := tok.Extra("id_token").(string)
	if idToken == "" {
		http.Redirect(w, r, "/portal/login?error=missing_id_token", http.StatusFound)
		return
	}

	subject, email, gExpiry, err := s.oauth.verifyIDToken(ctx, idToken, s.oauth.googleConfig.ClientID)
	if err != nil {
		s.logger.Error("web login id token verification failed", "error", err)
		http.Redirect(w, r, "/portal/login?error=invalid_token", http.StatusFound)
		return
	}

	s.oauth.completeWebLogin(w, r, subject, email, idToken, tok.RefreshToken, gExpiry)
}

// GET /api/me - returns current authenticated user information
func (s *Server) handleAPIMe(w http.ResponseWriter, r *http.Request) {
	user, err := s.authenticator.Authenticate(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"authenticated": false,
			"error":         err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, WebUser{
		Subject:       user.Subject,
		Email:         user.Email,
		Authenticated: true,
	})
}

// POST /api/logout - logs out the current user session
func (s *Server) handleAPILogout(w http.ResponseWriter, r *http.Request) {
	token := s.authenticator.ExtractToken(r)
	if token != "" {
		_ = s.authenticator.store.DeleteSession(r.Context(), token)
	}

	isSecure := strings.HasPrefix(s.cfg.PublicBaseURL, "https://")
	http.SetCookie(w, &http.Cookie{
		Name:     "emailmcp_session",
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   isSecure,
		SameSite: http.SameSiteLaxMode,
	})

	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

// GET /api/accounts - lists email accounts for the authenticated user
func (s *Server) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	user, err := s.authenticator.Authenticate(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": err.Error()})
		return
	}

	accs, err := s.configStore.ListUserConfigs(r.Context(), user.Subject)
	if err != nil {
		s.logger.Error("failed to list user configs", "error", err, "user", user.Subject)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "Failed to list email accounts"})
		return
	}

	summaries := make([]AccountResponse, 0, len(accs))
	for _, acc := range accs {
		summaries = append(summaries, newAccountResponse(acc))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts": summaries,
	})
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// GET /api/accounts/{id} - gets single email account details
func (s *Server) handleGetAccount(w http.ResponseWriter, r *http.Request) {
	user, err := s.authenticator.Authenticate(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": err.Error()})
		return
	}

	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Account ID is required"})
		return
	}

	acc, err := s.configStore.GetUserConfig(r.Context(), user.Subject, id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": fmt.Sprintf("Account %q not found", id)})
		return
	}

	writeJSON(w, http.StatusOK, newAccountResponse(acc))
}

// POST /api/accounts - creates a new email account
func (s *Server) handleCreateAccount(w http.ResponseWriter, r *http.Request) {
	user, err := s.authenticator.Authenticate(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": err.Error()})
		return
	}

	var payload AccountPayload
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Invalid request body"})
		return
	}

	if payload.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Account name is required"})
		return
	}
	if payload.IMAPHost == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "IMAP host is required"})
		return
	}
	if payload.IMAPUsername == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "IMAP username is required"})
		return
	}
	if payload.IMAPPassword == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "IMAP password is required"})
		return
	}
	if payload.SMTPHost == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "SMTP host is required"})
		return
	}

	id := payload.ID
	if id == "" {
		id = sanitizeAccountID(payload.Name)
	}

	imapPort := payload.IMAPPort
	if imapPort <= 0 {
		imapPort = 993
	}
	smtpPort := payload.SMTPPort
	if smtpPort <= 0 {
		smtpPort = 587
	}

	imapUseTLS := boolDefault(payload.IMAPUseTLS, true)
	smtpUseTLS := boolDefault(payload.SMTPUseTLS, true)

	smtpUsername := payload.SMTPUsername
	if smtpUsername == "" {
		smtpUsername = payload.IMAPUsername
	}

	smtpPassword := payload.SMTPPassword
	if smtpPassword == "" {
		smtpPassword = payload.IMAPPassword
	}

	acc := &types.Account{
		ID:           id,
		OwnerUserID:  user.Subject,
		Name:         payload.Name,
		IMAPHost:     payload.IMAPHost,
		IMAPPort:     imapPort,
		IMAPUsername: payload.IMAPUsername,
		IMAPPassword: payload.IMAPPassword,
		IMAPUseTLS:   imapUseTLS,
		SMTPHost:     payload.SMTPHost,
		SMTPPort:     smtpPort,
		SMTPUsername: smtpUsername,
		SMTPPassword: smtpPassword,
		SMTPUseTLS:   smtpUseTLS,
		FromAddress:  payload.FromAddress,
	}
	applyWebhookPayload(acc, payload, false)

	if err := validateAccountEndpoints(acc); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if err := validateWebhookSettings(acc.WebhookURL, acc.WebhookToken, acc.WatchFolders); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	if err := s.configStore.PutUserConfig(r.Context(), user.Subject, acc); err != nil {
		s.logger.Error("failed to create user config", "error", err, "user", user.Subject, "id", id)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "Failed to store account configuration"})
		return
	}

	s.imapMgr.DropPool(user.Subject, id)

	writeJSON(w, http.StatusCreated, newAccountResponse(acc))
}

// PUT /api/accounts/{id} - modifies an existing email account
func (s *Server) handleUpdateAccount(w http.ResponseWriter, r *http.Request) {
	user, err := s.authenticator.Authenticate(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": err.Error()})
		return
	}

	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Account ID is required"})
		return
	}

	existing, err := s.configStore.GetUserConfig(r.Context(), user.Subject, id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": fmt.Sprintf("Account %q not found", id)})
		return
	}

	var payload AccountPayload
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Invalid request body"})
		return
	}

	oldIMAPUsername := existing.IMAPUsername
	oldIMAPPassword := existing.IMAPPassword
	oldSMTPUsername := existing.SMTPUsername
	oldSMTPPassword := existing.SMTPPassword

	if payload.Name != "" {
		existing.Name = payload.Name
	}
	if payload.IMAPHost != "" {
		existing.IMAPHost = payload.IMAPHost
	}
	if payload.IMAPPort > 0 {
		existing.IMAPPort = payload.IMAPPort
	}
	if payload.IMAPUsername != "" {
		existing.IMAPUsername = payload.IMAPUsername
	}
	if payload.IMAPPassword != "" {
		existing.IMAPPassword = payload.IMAPPassword
	}
	if payload.IMAPUseTLS != nil {
		existing.IMAPUseTLS = *payload.IMAPUseTLS
	}

	if payload.SMTPHost != "" {
		existing.SMTPHost = payload.SMTPHost
	}
	if payload.SMTPPort > 0 {
		existing.SMTPPort = payload.SMTPPort
	}
	if payload.SMTPUsername != "" {
		existing.SMTPUsername = payload.SMTPUsername
	} else if payload.IMAPUsername != "" && (oldSMTPUsername == "" || oldSMTPUsername == oldIMAPUsername) {
		// UI: "Leave blank to use IMAP username"
		existing.SMTPUsername = payload.IMAPUsername
	}
	if payload.SMTPPassword != "" {
		existing.SMTPPassword = payload.SMTPPassword
	} else if payload.IMAPPassword != "" && (oldSMTPPassword == "" || oldSMTPPassword == oldIMAPPassword) {
		// UI: "Leave blank to use IMAP password"
		existing.SMTPPassword = payload.IMAPPassword
	}
	if payload.SMTPUseTLS != nil {
		existing.SMTPUseTLS = *payload.SMTPUseTLS
	}
	if payload.FromAddress != "" {
		existing.FromAddress = payload.FromAddress
	}
	applyWebhookPayload(existing, payload, true)

	if err := validateAccountEndpoints(existing); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if err := validateWebhookSettings(existing.WebhookURL, existing.WebhookToken, existing.WatchFolders); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	if err := s.configStore.PutUserConfig(r.Context(), user.Subject, existing); err != nil {
		s.logger.Error("failed to update user config", "error", err, "user", user.Subject, "id", id)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "Failed to update account configuration"})
		return
	}

	s.imapMgr.DropPool(user.Subject, id)

	writeJSON(w, http.StatusOK, newAccountResponse(existing))
}

// DELETE /api/accounts/{id} - deletes an email account
func (s *Server) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	user, err := s.authenticator.Authenticate(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": err.Error()})
		return
	}

	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Account ID is required"})
		return
	}

	if err := s.configStore.DeleteUserConfig(r.Context(), user.Subject, id); err != nil {
		s.logger.Error("failed to delete user config", "error", err, "user", user.Subject, "id", id)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "Failed to delete email account"})
		return
	}

	s.imapMgr.DropPool(user.Subject, id)

	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

// POST /api/accounts/{id}/verify - verifies IMAP & SMTP using previously saved values
func (s *Server) handleVerifyAccount(w http.ResponseWriter, r *http.Request) {
	user, err := s.authenticator.Authenticate(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": err.Error()})
		return
	}

	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Account ID is required"})
		return
	}

	acc, err := s.configStore.GetUserConfig(r.Context(), user.Subject, id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": fmt.Sprintf("Account %q not found", id)})
		return
	}

	if acc.IMAPPassword == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Password is required for connection verification"})
		return
	}

	s.logger.Info("account verification started",
		"user", user.Subject,
		"id", id,
		"imap_host", acc.IMAPHost,
		"smtp_host", acc.SMTPHost,
	)

	ctx, cancel := context.WithTimeout(r.Context(), verifyOverallTimeout)
	defer cancel()
	res := s.VerifyAccountCredentials(ctx, acc)
	if res.Success {
		s.logger.Info("account verification succeeded", "user", user.Subject, "id", id)
	} else {
		s.logger.Warn("account verification failed",
			"user", user.Subject,
			"id", id,
			"imap_ok", res.IMAP.Success,
			"imap_error", res.IMAP.Error,
			"smtp_ok", res.SMTP.Success,
			"smtp_error", res.SMTP.Error,
		)
	}
	writeJSON(w, http.StatusOK, res)
}

func sanitizeAccountID(name string) string {
	id := strings.ToLower(name)
	id = strings.ReplaceAll(id, " ", "-")
	var sb strings.Builder
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			sb.WriteRune(r)
		}
	}
	res := strings.Trim(sb.String(), "-_")
	if res == "" {
		tok, _ := randomToken(4)
		return "account-" + tok
	}
	return res
}
