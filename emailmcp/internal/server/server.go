package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jpuckett/EmailMCP/emailmcp/internal/attach"
	"github.com/jpuckett/EmailMCP/emailmcp/internal/config"
	imapmgr "github.com/jpuckett/EmailMCP/emailmcp/internal/imap"
	"github.com/jpuckett/EmailMCP/emailmcp/internal/mailbuild"
	"github.com/jpuckett/EmailMCP/emailmcp/internal/netutil"
	"github.com/jpuckett/EmailMCP/emailmcp/internal/smtp"
	"github.com/jpuckett/EmailMCP/emailmcp/internal/types"
)

// Server wraps the MCP server and dependencies.
type Server struct {
	mcpServer     *mcp.Server
	imapMgr       *imapmgr.Manager
	smtpSend      *smtp.Sender
	attachFetch   *attach.Fetcher
	attachStore   attach.ObjectStore
	logger        *slog.Logger
	cfg           *config.Config
	authenticator *Authenticator
	oauth         *OAuthServer
	configStore   config.Store
}

// New creates and configures the EmailMCP server.
func New(ctx context.Context, cfg *config.Config) (*Server, error) {
	if cfg.GoogleClientID == "" {
		return nil, errors.New("GOOGLE_CLIENT_ID is required")
	}

	imapCfg := imapmgr.Config{
		MaxConnsPerAccount: cfg.IMAPMaxConnsPerAccount,
		IdleTimeout:        cfg.IMAPConnIdleTimeout,
		Logger:             slog.Default(),
	}

	imapMgr := imapmgr.NewManager(imapCfg)
	smtpSender := smtp.NewSender(smtp.Config{
		DefaultTimeout:  cfg.SMTPDefaultTimeout,
		Logger:          slog.Default(),
		MaxMessageBytes: cfg.MaxMessageBytes,
	})

	attachFetch := attach.NewFetcher(attach.Config{
		MaxBytes:      cfg.MaxAttachmentBytes,
		Timeout:       cfg.AttachmentURLTimeout,
		AllowedHosts:  cfg.AttachmentAllowedHosts,
		PathAllowlist: cfg.AttachmentPathAllowlist,
		Logger:        slog.Default(),
	})

	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "emailmcp",
		Version: "0.2.0",
	}, &mcp.ServerOptions{Instructions: skillInstructions()})

	logger := slog.Default()

	// The session store persists issued OAuth sessions (opaque access/refresh
	// tokens) and registered clients so they survive restarts and span replicas.
	// It is shared by the OAuth server (which writes sessions) and the
	// authenticator (which resolves them). Falls back to an in-memory store when
	// no DynamoDB session table is configured.
	store, err := NewSessionStore(ctx, cfg.SessionTableName, logger)
	if err != nil {
		return nil, fmt.Errorf("failed to create session store: %w", err)
	}

	// OAuth (HTTP mode) needs a public base URL and Google client secret so MCP
	// clients can complete the authorization code + PKCE flow via Google.
	var oauth *OAuthServer
	if cfg.Transport == "" || cfg.Transport == "http" {
		if cfg.PublicBaseURL == "" {
			return nil, errors.New("PUBLIC_BASE_URL is required for HTTP mode (e.g. https://emailmcp.ecg.co)")
		}
		if cfg.GoogleClientSecret == "" {
			return nil, errors.New("GOOGLE_CLIENT_SECRET is required for HTTP mode OAuth")
		}
		oauth, err = NewOAuthServer(cfg.PublicBaseURL, cfg.GoogleClientID, cfg.GoogleClientSecret, store, logger, cfg.OAuthRedirectAllowlist)
		if err != nil {
			return nil, fmt.Errorf("failed to create oauth server: %w", err)
		}
		if len(cfg.OAuthRedirectAllowlist) > 0 {
			logger.Info("oauth HTTPS redirect allowlist enforced",
				"entries", len(cfg.OAuthRedirectAllowlist),
			)
		} else {
			logger.Warn("oauth HTTPS redirect allowlist empty; any HTTPS redirect_uri is accepted (set OAUTH_REDIRECT_ALLOWLIST to enforce)")
		}
	}

	auth := NewAuthenticator(store, cfg.PublicBaseURL)

	// User email account configurations are read/written directly against the
	// EmailMCPUserConfigs DynamoDB table (the Config API Lambda has been
	// removed). Falls back to an in-memory store when no table is configured.
	configStore, err := config.NewStore(ctx, cfg.UserConfigTableName, cfg.ApplicationID, logger)
	if err != nil {
		return nil, fmt.Errorf("failed to create user config store: %w", err)
	}

	var attachStore attach.ObjectStore
	if cfg.AttachmentS3Bucket != "" {
		store, err := attach.NewS3Store(ctx, cfg.AttachmentS3Bucket, cfg.AttachmentS3Prefix, cfg.AttachmentPresignTTL, logger)
		if err != nil {
			return nil, fmt.Errorf("attachment s3 store: %w", err)
		}
		attachStore = store
	}

	es := &Server{
		mcpServer:     srv,
		imapMgr:       imapMgr,
		smtpSend:      smtpSender,
		attachFetch:   attachFetch,
		attachStore:   attachStore,
		logger:        logger,
		cfg:           cfg,
		authenticator: auth,
		oauth:         oauth,
		configStore:   configStore,
	}

	es.logger.Info("initializing EmailMCP server", "name", "emailmcp", "version", "0.2.0")
	if oauth != nil {
		es.logger.Info("oauth authorization server enabled",
			"issuer", cfg.PublicBaseURL,
			"callback", cfg.PublicBaseURL+"/oauth/callback",
		)
	}
	es.registerTools()
	es.registerResources()
	es.logger.Info("mcp server initialized")

	return es, nil
}

// HTTPHandler returns the Streamable HTTP handler wrapped with request/response logging and authentication.
// It includes an unsecured /health endpoint for load balancer liveness probes and MCP OAuth endpoints.
func (s *Server) HTTPHandler() http.Handler {
	mux := http.NewServeMux()

	// Health check endpoint (unsecured)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	// MCP OAuth discovery + authorize/token/register (unsecured).
	if s.oauth != nil {
		s.oauth.Mount(mux)
	}

	// Web authentication and account management REST API.
	s.mountWebRoutes(mux)

	mcpHandler := mcp.NewStreamableHTTPHandler(func(req *http.Request) *mcp.Server {
		return s.mcpServer
	}, &mcp.StreamableHTTPOptions{
		Logger: s.logger,
		// Each HTTP POST gets its own temporary MCP session. The SDK otherwise
		// multiplexes concurrent POSTs on one session by JSON-RPC id, so two
		// tools/call requests that reuse id=2 can receive each other's results
		// (one times out; the other shows the wrong mailbox).
		Stateless: true,
		// Return one JSON-RPC body per HTTP POST rather than an SSE stream.
		JSONResponse: true,
		// DNS-rebinding / "localhost" protection is auto-enabled by the SDK and
		// rejects any request whose connection has a loopback *local* address but
		// a non-loopback Host header with "403 Forbidden: invalid Host header".
		// That protection is meant for locally-bound developer servers reachable
		// from a browser; it is wrong for this deployment, which is intentionally
		// published at a public hostname (PUBLIC_BASE_URL, e.g.
		// https://emailmcp.ecg.co) behind an AWS ALB that terminates TLS and
		// forwards to the pod. When the proxy hop reaches the app over the
		// loopback interface, every authenticated MCP request (initialize,
		// tools/call, ...) would otherwise get a spurious 403 *after* a
		// successful OAuth sign-in. Disable it and rely on the OAuth bearer +
		// SSRF host validation instead.
		DisableLocalhostProtection: true,
	})

	// MCP handler requires Google ID token authentication.
	mcpAuthHandler := s.authenticator.Middleware(mcpHandler)

	// Register root handler for MCP requests.
	mux.Handle("/", mcpAuthHandler)

	// Wrap everything in logging.
	return httpLogging(s.logger, mux)
}

// ServeStdio runs the MCP server on the stdio transport.
// Note: stdio has no HTTP auth middleware; tool handlers still require a user
// context and will fail unless the transport is extended to supply tokens.
func (s *Server) ServeStdio(ctx context.Context) error {
	return s.mcpServer.Run(ctx, &mcp.StdioTransport{})
}

// httpLogging logs every incoming HTTP request/response at Debug level. It does
// NOT log bodies because MCP payloads may contain email content or account
// credentials (see the project's Security Rules in agents.md).
func httpLogging(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		isSSE := r.Header.Get("Accept") == "text/event-stream"
		level := slog.LevelDebug
		if isSSE {
			level = slog.LevelInfo
		}

		// Redact sensitive headers before logging.
		hdrs := make(map[string]string, len(r.Header))
		for k, v := range r.Header {
			switch strings.ToLower(k) {
			case "authorization", "cookie", "proxy-authorization":
				hdrs[k] = "[REDACTED]"
			default:
				hdrs[k] = strings.Join(v, ",")
			}
		}

		logger.Log(r.Context(), level, "http request",
			"method", r.Method,
			"path", r.URL.Path,
			"query", r.URL.RawQuery,
			"remote", r.RemoteAddr,
			"proto", r.Proto,
			"host", r.Host,
			"content_type", r.Header.Get("Content-Type"),
			"accept", r.Header.Get("Accept"),
			"mcp_session_id", r.Header.Get("Mcp-Session-Id"),
			"mcp_protocol_version", r.Header.Get("MCP-Protocol-Version"),
			"headers", hdrs,
		)

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		// Errors (4xx/5xx) are logged at Warn with the response body and
		// connection details. Transport-level rejections (e.g. the SDK's
		// "403 Forbidden: invalid Host header" or a 401 from auth) write their
		// reason to the body, which is otherwise never surfaced in the logs.
		// The body is a short plain-text error here (not an MCP payload), so it
		// is safe to log; successful bodies are never captured.
		respLevel := level
		if rec.status >= http.StatusBadRequest {
			respLevel = slog.LevelWarn
		}
		logger.Log(r.Context(), respLevel, "http response",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"bytes", rec.written,
			"duration_ms", time.Since(start).Milliseconds(),
			"resp_content_type", rec.Header().Get("Content-Type"),
			"resp_mcp_session_id", rec.Header().Get("Mcp-Session-Id"),
			"host", r.Host,
			"local_addr", localAddr(r),
			"error_body", rec.errBody(),
		)
	})
}

// localAddr returns the local (server-side) address of the connection that
// served r, used to diagnose transport-level rejections such as the SDK's
// DNS-rebinding / loopback Host check. Returns "" when unavailable.
func localAddr(r *http.Request) string {
	if a, ok := r.Context().Value(http.LocalAddrContextKey).(interface{ String() string }); ok && a != nil {
		return a.String()
	}
	return ""
}

// maxErrBodyCapture bounds how many response-body bytes are retained for
// logging on error responses.
const maxErrBodyCapture = 1024

type statusRecorder struct {
	http.ResponseWriter
	status  int
	written int64
	wrote   bool
	// errBuf captures the (short, plain-text) response body for error statuses
	// so the failure reason can be logged; it is left empty for success.
	errBuf []byte
}

// errBody returns the captured error-response body (empty for success).
func (r *statusRecorder) errBody() string {
	return string(r.errBuf)
}

// captureErrBody reports whether the response body for the given status is safe
// and useful to log. It targets transport/auth rejections whose bodies are
// fixed, request-independent reasons (e.g. "Unauthorized: Invalid token",
// "Forbidden: invalid Host header ..."). 400 Bad Request is intentionally
// excluded because the SDK's "malformed payload: %v" body can echo a fragment
// of the request payload, which may contain account credentials (see the
// project's Security Rules); MCP tool errors are returned as JSON-RPC results
// with HTTP 200 and are never captured here.
func captureErrBody(status int) bool {
	switch status {
	case http.StatusUnauthorized, // 401
		http.StatusForbidden,            // 403
		http.StatusNotFound,             // 404
		http.StatusMethodNotAllowed,     // 405
		http.StatusUnsupportedMediaType: // 415
		return true
	}
	return status >= http.StatusInternalServerError // 5xx
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wrote {
		r.status = code
		r.wrote = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wrote {
		r.wrote = true
	}
	// Retain a bounded prefix of error-response bodies for diagnostics.
	if captureErrBody(r.status) && len(r.errBuf) < maxErrBodyCapture {
		remaining := maxErrBodyCapture - len(r.errBuf)
		if remaining > len(b) {
			remaining = len(b)
		}
		r.errBuf = append(r.errBuf, b[:remaining]...)
	}
	n, err := r.ResponseWriter.Write(b)
	r.written += int64(n)
	return n, err
}

// Flush exposes the underlying Flusher so the SDK's streamable transport can
// push SSE events without buffering.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) registerTools() {
	// Account management (Config API)
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name: "add_email_account",
		Description: "Create or replace the authenticated user's email account (IMAP + SMTP). " +
			"Do not send passwords; set IMAP and SMTP passwords in the user portal after the account is created.",
	}, s.addEmailAccount)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name: "list_email_accounts",
		Description: "List the authenticated user's configured email accounts (without credentials). " +
			"Returns an empty list with setup guidance when the user has no accounts.",
	}, s.listEmailAccounts)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "remove_email_account",
		Description: "Remove the authenticated user's email account configuration.",
	}, s.removeEmailAccount)

	// IMAP tools
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "list_folders",
		Description: "List IMAP folders/mailboxes for an account.",
	}, s.listFolders)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name: "search_emails",
		Description: "Search emails in a folder and return summaries. " +
			"Supports basic criteria (query, from, since). " +
			"Use the optional 'limit' parameter to control how many messages are returned (default 50). " +
			"Results are sorted by newest internal date (arrival) by default; " +
			"override with sort_by (arrival, date, from, to, cc, subject, size) and optional sort_reverse.",
	}, s.searchEmails)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name: "read_email",
		Description: "Read a single email by UID including text/HTML body and attachment metadata " +
			"(id/part_id, filename, content_type, size, disposition, content_id, inline). " +
			"Use get_attachment to fetch attachment bytes. " +
			"See email://guides/attachments for metadata vs get_attachment.",
	}, s.readEmail)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "move_emails",
		Description: "Move one or more emails (by UID) to another folder.",
	}, s.moveEmails)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "flag_emails",
		Description: "Add or remove flags (e.g. \\Seen, \\Flagged, \\Deleted) on emails.",
	}, s.flagEmails)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "delete_emails",
		Description: "Delete one or more emails by marking them \\Deleted.",
	}, s.deleteEmails)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name: "get_attachment",
		Description: "Fetch one attachment from a message by attachment_id/part_id or filename. " +
			"Returns base64 data when under the inline size cap, otherwise a short-lived presigned URL if S3 is configured. " +
			"See email://guides/attachments.",
	}, s.getAttachment)

	// SMTP / drafts
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name: "send_email",
		Description: "Send an email now (plain text, HTML, attachments). " +
			"Attachments may be inline base64 (data), an HTTPS URL (url, e.g. a presigned S3 link), or a local path (path, disabled unless allowlisted). " +
			"Use create_draft instead when the user should review or edit before sending — create_draft never sends. " +
			"reply_to_uid sets threading headers (and quote_original quotes the original); forward_uid can also re-attach the original files server-side (include_original_attachments). " +
			"Read email://guides/replying before reply, email://guides/forwarding before forward, email://guides/attachments for files/cid, email://guides/drafts to save instead of send.",
	}, s.sendEmail)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name: "create_draft",
		Description: "Save a complete email to the account Drafts folder with the \\Draft flag. Never sends. " +
			"Use this when the user wants to review or finish the message later. " +
			"Accepts the same body, recipients, attachments (data/url/path), reply, and forward options as send_email. " +
			"Optional folder overrides Drafts discovery (SPECIAL-USE \\Drafts, then Drafts / INBOX.Drafts). " +
			"Read email://guides/drafts; compose options match send_email (email://guides/replying, email://guides/forwarding, email://guides/attachments).",
	}, s.createDraft)
}

// --- Input structs (for schema) ---

type AddAccountInput struct {
	ID           string `json:"id,omitempty" jsonschema:"Optional unique ID for the account; auto-generated from name if empty"`
	Name         string `json:"name" jsonschema:"Human friendly name for the account"`
	IMAPHost     string `json:"imap_host"`
	IMAPPort     int    `json:"imap_port" jsonschema:"default:993"`
	IMAPUsername string `json:"imap_username"`
	// Pointer so JSON omission honors default true (zero-value bool would be false).
	IMAPUseTLS *bool `json:"imap_use_tls,omitempty" jsonschema:"default:true"`

	SMTPHost     string `json:"smtp_host"`
	SMTPPort     int    `json:"smtp_port" jsonschema:"default:587"`
	SMTPUsername string `json:"smtp_username"`
	SMTPUseTLS   *bool  `json:"smtp_use_tls,omitempty" jsonschema:"default:true"`

	FromAddress string `json:"from_address,omitempty"`
	SaveToSent  string `json:"save_to_sent,omitempty" jsonschema:"Whether send_email should IMAP-APPEND a Sent copy: auto (default), true, or false"`
}

type AccountIDInput struct {
	AccountID string `json:"account_id" jsonschema:"Account ID"`
}

type ListFoldersInput struct {
	AccountID string `json:"account_id"`
}

type SearchEmailsInput struct {
	AccountID string `json:"account_id"`
	Folder    string `json:"folder,omitempty" jsonschema:"Folder to search, defaults to INBOX"`
	Query     string `json:"query,omitempty" jsonschema:"Simple text search in subject/body"`
	From      string `json:"from,omitempty"`
	Since     string `json:"since,omitempty" jsonschema:"Date in RFC3339 or YYYY-MM-DD"`
	Limit     int    `json:"limit,omitempty" jsonschema:"Maximum number of message summaries to return (default 50)"`
	// SortBy is one of: arrival (internal date, default), date, from, to, cc, subject, size.
	SortBy string `json:"sort_by,omitempty" jsonschema:"Sort key: arrival (internal date, default), date, from, to, cc, subject, size"`
	// SortReverse overrides default direction. Defaults to true for arrival/date/size, false for string keys.
	SortReverse *bool `json:"sort_reverse,omitempty" jsonschema:"Reverse sort order. Defaults to true for arrival/date/size (newest/largest first), false for from/to/cc/subject"`
}

type ReadEmailInput struct {
	AccountID string `json:"account_id"`
	Folder    string `json:"folder,omitempty"`
	UID       uint32 `json:"uid"`
}

type MoveEmailsInput struct {
	AccountID  string   `json:"account_id"`
	Folder     string   `json:"folder,omitempty"`
	UIDs       []uint32 `json:"uids"`
	DestFolder string   `json:"dest_folder"`
}

type FlagEmailsInput struct {
	AccountID string   `json:"account_id"`
	Folder    string   `json:"folder,omitempty"`
	UIDs      []uint32 `json:"uids"`
	Flags     []string `json:"flags"`
	Add       bool     `json:"add" jsonschema:"true to add flags, false to remove"`
}

type DeleteEmailsInput struct {
	AccountID string   `json:"account_id"`
	Folder    string   `json:"folder,omitempty"`
	UIDs      []uint32 `json:"uids"`
}

type SendEmailToolInput struct {
	AccountID   string                  `json:"account_id"`
	To          []string                `json:"to,omitempty" jsonschema:"Recipients. Optional when reply_to_uid is set."`
	Cc          []string                `json:"cc,omitempty"`
	Bcc         []string                `json:"bcc,omitempty"`
	Subject     string                  `json:"subject,omitempty" jsonschema:"Subject. Defaults to Re:/Fwd: of the original when replying or forwarding."`
	Text        string                  `json:"text,omitempty"`
	HTML        string                  `json:"html,omitempty"`
	From        string                  `json:"from,omitempty"`
	Attachments []types.AttachmentInput `json:"attachments,omitempty" jsonschema:"Each item must provide exactly one of data (base64), url (https), or path (allowlisted local file). Optional content_id + disposition=inline for cid: images."`

	ReplyToUID                 uint32 `json:"reply_to_uid,omitempty" jsonschema:"UID of the message to reply to. Sets In-Reply-To and References."`
	ReplyFolder                string `json:"reply_folder,omitempty" jsonschema:"Folder of the message being replied to (default INBOX)"`
	ReplyAll                   bool   `json:"reply_all,omitempty" jsonschema:"Fill To/Cc from the original minus the sender's own address"`
	ForwardUID                 uint32 `json:"forward_uid,omitempty" jsonschema:"UID of the message to forward"`
	ForwardFolder              string `json:"forward_folder,omitempty"`
	IncludeOriginalAttachments *bool  `json:"include_original_attachments,omitempty" jsonschema:"When forwarding, re-attach original attachments server-side (default true)"`
	QuoteOriginal              bool   `json:"quote_original,omitempty" jsonschema:"Quote the original body below the new text"`
	SaveToSent                 string `json:"save_to_sent,omitempty" jsonschema:"auto (default), true, or false. auto saves a Sent copy except for Gmail which already does."`
}

type CreateDraftInput struct {
	SendEmailToolInput
	Folder string `json:"folder,omitempty" jsonschema:"Override Drafts folder. Default: SPECIAL-USE \\Drafts, then Drafts / INBOX.Drafts"`
}

type GetAttachmentInput struct {
	AccountID    string `json:"account_id"`
	Folder       string `json:"folder,omitempty" jsonschema:"Folder, defaults to INBOX"`
	UID          uint32 `json:"uid"`
	AttachmentID string `json:"attachment_id,omitempty" jsonschema:"Stable id from read_email (same as part_id)"`
	PartID       string `json:"part_id,omitempty"`
	Filename     string `json:"filename,omitempty"`
}

// --- Handlers ---

func (s *Server) addEmailAccount(ctx context.Context, req *mcp.CallToolRequest, in AddAccountInput) (*mcp.CallToolResult, any, error) {
	user, err := s.requireAuth(ctx)
	if err != nil {
		return nil, nil, err
	}
	if in.Name == "" || in.IMAPHost == "" || in.IMAPUsername == "" || in.SMTPHost == "" {
		return nil, nil, errors.New("required fields missing: name, imap_host, imap_username, smtp_host")
	}

	imapUseTLS := boolDefault(in.IMAPUseTLS, true)
	smtpUseTLS := boolDefault(in.SMTPUseTLS, true)

	if err := netutil.ValidatePublicHost(in.IMAPHost); err != nil {
		return nil, nil, fmt.Errorf("imap_host not allowed: %w", err)
	}
	if err := netutil.ValidatePublicHost(in.SMTPHost); err != nil {
		return nil, nil, fmt.Errorf("smtp_host not allowed: %w", err)
	}
	if err := netutil.RequireTLSUnlessLocalhost(in.IMAPHost, imapUseTLS, "IMAP"); err != nil {
		return nil, nil, err
	}
	if err := netutil.RequireTLSUnlessLocalhost(in.SMTPHost, smtpUseTLS, "SMTP"); err != nil {
		return nil, nil, err
	}

	smtpUser := in.SMTPUsername
	if smtpUser == "" {
		smtpUser = in.IMAPUsername
	}

	// Generate a slug from name if ID is not provided
	accountID := in.ID
	if accountID == "" {
		accountID = strings.ToLower(strings.ReplaceAll(in.Name, " ", "-"))
	}
	if accountID == "" {
		accountID = "default"
	}

	// Passwords and webhook secrets are set only via the user portal. Preserve
	// any already stored credentials when this tool replaces an existing account.
	var imapPass, smtpPass, saveToSent, webhookURL, webhookToken string
	var watchFolders []string
	if existing, err := s.configStore.GetUserConfig(ctx, user.Subject, accountID); err == nil && existing != nil {
		imapPass = existing.IMAPPassword
		smtpPass = existing.SMTPPassword
		saveToSent = existing.SaveToSent
		webhookURL = existing.WebhookURL
		webhookToken = existing.WebhookToken
		watchFolders = append([]string(nil), existing.WatchFolders...)
	}
	if in.SaveToSent != "" {
		saveToSent = in.SaveToSent
	}

	acc := &types.Account{
		ID:           accountID,
		OwnerUserID:  user.Subject,
		Name:         in.Name,
		IMAPHost:     in.IMAPHost,
		IMAPPort:     defaultPort(in.IMAPPort, 993),
		IMAPUsername: in.IMAPUsername,
		IMAPPassword: imapPass,
		IMAPUseTLS:   imapUseTLS,
		SMTPHost:     in.SMTPHost,
		SMTPPort:     defaultPort(in.SMTPPort, 587),
		SMTPUsername: smtpUser,
		SMTPPassword: smtpPass,
		SMTPUseTLS:   smtpUseTLS,
		FromAddress:  in.FromAddress,
		SaveToSent:   saveToSent,
		WebhookURL:   webhookURL,
		WebhookToken: webhookToken,
		WatchFolders: watchFolders,
	}

	if err := s.configStore.PutUserConfig(ctx, user.Subject, acc); err != nil {
		return nil, nil, fmt.Errorf("store account: %w", err)
	}

	// Drop any existing pool so credential/host changes take effect immediately.
	if s.imapMgr != nil {
		s.imapMgr.DropPool(user.Subject, acc.ID)
	}

	s.logger.Info("account added", "id", acc.ID, "name", acc.Name, "owner", user.Subject)

	portalURL := s.portalURL()
	msg := fmt.Sprintf("Account saved successfully. ID: %s. Provide IMAP and SMTP passwords in the user portal.", acc.ID)
	if portalURL != "" {
		msg = fmt.Sprintf("Account saved successfully. ID: %s. Provide IMAP and SMTP passwords in the user portal at %s.", acc.ID, portalURL)
	}
	out := map[string]any{"id": acc.ID}
	if portalURL != "" {
		out["portal_url"] = portalURL
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
	}, out, nil
}

func (s *Server) listEmailAccounts(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
	user, err := s.requireAuth(ctx)
	if err != nil {
		return nil, nil, err
	}

	accs, err := s.configStore.ListUserConfigs(ctx, user.Subject)
	if err != nil {
		if !errors.Is(err, config.ErrConfigNotFound) {
			return nil, nil, err
		}
		accs = nil
	}

	summaries := make([]types.AccountSummary, 0, len(accs))
	for _, acc := range accs {
		summaries = append(summaries, types.AccountSummary{
			ID:          acc.ID,
			Name:        acc.Name,
			IMAPHost:    acc.IMAPHost,
			IMAPPort:    acc.IMAPPort,
			SMTPHost:    acc.SMTPHost,
			SMTPPort:    acc.SMTPPort,
			FromAddress: acc.FromAddress,
			CreatedAt:   acc.CreatedAt,
			UpdatedAt:   acc.UpdatedAt,
		})
	}

	out := map[string]any{"accounts": summaries}
	if len(summaries) > 0 {
		return nil, out, nil
	}

	portalURL := s.portalURL()
	msg := "No email accounts are configured. Use add_email_account to create one, then set IMAP and SMTP passwords in the user portal."
	if portalURL != "" {
		msg = fmt.Sprintf("No email accounts are configured. Use add_email_account to create one, then set IMAP and SMTP passwords in the user portal at %s.", portalURL)
		out["portal_url"] = portalURL
	}
	out["message"] = msg
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
	}, out, nil
}

func (s *Server) removeEmailAccount(ctx context.Context, req *mcp.CallToolRequest, in AccountIDInput) (*mcp.CallToolResult, any, error) {
	user, err := s.requireAuth(ctx)
	if err != nil {
		return nil, nil, err
	}
	if in.AccountID == "" {
		return nil, nil, errors.New("account_id is required")
	}

	if err := s.configStore.DeleteUserConfig(ctx, user.Subject, in.AccountID); err != nil {
		if errors.Is(err, config.ErrConfigNotFound) {
			return nil, nil, errors.New("account not found")
		}
		return nil, nil, err
	}
	// Close any live IMAP connections for this tenant account.
	if s.imapMgr != nil {
		s.imapMgr.DropPool(user.Subject, in.AccountID)
	}
	s.logger.Info("account removed", "id", in.AccountID, "owner", user.Subject)
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Account removed"}}}, nil, nil
}

func (s *Server) listFolders(ctx context.Context, req *mcp.CallToolRequest, in ListFoldersInput) (*mcp.CallToolResult, any, error) {
	acc, err := s.getAccount(ctx, in.AccountID)
	if err != nil {
		return nil, nil, err
	}
	folders, err := s.imapMgr.ListFolders(ctx, acc)
	if err != nil {
		return nil, nil, err
	}
	return nil, map[string]any{"folders": folders}, nil
}

func (s *Server) searchEmails(ctx context.Context, req *mcp.CallToolRequest, in SearchEmailsInput) (*mcp.CallToolResult, any, error) {
	acc, err := s.getAccount(ctx, in.AccountID)
	if err != nil {
		return nil, nil, err
	}

	crit := imap.SearchCriteria{}
	if in.Query != "" {
		crit.Text = []string{in.Query}
	}
	if in.From != "" {
		crit.Header = append(crit.Header, imap.SearchCriteriaHeaderField{Key: "From", Value: in.From})
	}
	if in.Since != "" {
		if t, err := parseDate(in.Since); err == nil {
			crit.Since = t
		}
	}

	limit := in.Limit
	if limit <= 0 {
		limit = 50
	}

	sortKey, err := imapmgr.ParseSearchSortKey(in.SortBy)
	if err != nil {
		return nil, nil, err
	}

	folder := in.Folder
	summaries, err := s.imapMgr.SearchEmails(ctx, acc, folder, crit, limit, imapmgr.SearchSort{
		Key:     sortKey,
		Reverse: in.SortReverse,
	})
	if err != nil {
		return nil, nil, err
	}
	return nil, map[string]any{"account_id": acc.ID, "emails": summaries}, nil
}

func (s *Server) readEmail(ctx context.Context, req *mcp.CallToolRequest, in ReadEmailInput) (*mcp.CallToolResult, any, error) {
	acc, err := s.getAccount(ctx, in.AccountID)
	if err != nil {
		return nil, nil, err
	}
	msg, err := s.imapMgr.GetEmail(ctx, acc, in.Folder, in.UID)
	if err != nil {
		return nil, nil, err
	}
	return nil, msg, nil
}

func (s *Server) moveEmails(ctx context.Context, req *mcp.CallToolRequest, in MoveEmailsInput) (*mcp.CallToolResult, any, error) {
	acc, err := s.getAccount(ctx, in.AccountID)
	if err != nil {
		return nil, nil, err
	}
	if err := s.imapMgr.MoveEmails(ctx, acc, in.Folder, in.UIDs, in.DestFolder); err != nil {
		return nil, nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Emails moved"}}}, nil, nil
}

func (s *Server) flagEmails(ctx context.Context, req *mcp.CallToolRequest, in FlagEmailsInput) (*mcp.CallToolResult, any, error) {
	acc, err := s.getAccount(ctx, in.AccountID)
	if err != nil {
		return nil, nil, err
	}
	if err := s.imapMgr.FlagEmails(ctx, acc, in.Folder, in.UIDs, in.Flags, in.Add); err != nil {
		return nil, nil, err
	}
	action := "added"
	if !in.Add {
		action = "removed"
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Flags %s", action)}}}, nil, nil
}

func (s *Server) deleteEmails(ctx context.Context, req *mcp.CallToolRequest, in DeleteEmailsInput) (*mcp.CallToolResult, any, error) {
	acc, err := s.getAccount(ctx, in.AccountID)
	if err != nil {
		return nil, nil, err
	}
	if err := s.imapMgr.DeleteEmails(ctx, acc, in.Folder, in.UIDs); err != nil {
		return nil, nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Emails marked for deletion"}}}, nil, nil
}

func (s *Server) sendEmail(ctx context.Context, req *mcp.CallToolRequest, in SendEmailToolInput) (*mcp.CallToolResult, any, error) {
	acc, err := s.getAccount(ctx, in.AccountID)
	if err != nil {
		return nil, nil, err
	}

	msg, err := s.composeMessage(ctx, acc, composeFromSend(in), false)
	if err != nil {
		return nil, nil, err
	}
	raw, err := mailbuild.BuildWithLimits(msg, s.buildLimits(acc))
	if err != nil {
		return nil, nil, err
	}

	recipients := append(append([]string{}, msg.To...), msg.Cc...)
	recipients = append(recipients, msg.Bcc...)
	result, err := s.smtpSend.SendRaw(ctx, acc, msg.From, recipients, raw, msg.MessageID)
	if err != nil {
		return nil, nil, err
	}

	if smtp.ShouldSaveToSent(s.saveToSentMode(acc, in.SaveToSent), acc.SMTPHost) {
		s.enqueueSaveToSent(acc, raw)
	}

	text := fmt.Sprintf("Email sent. Message-ID: %s", result.MessageID)
	if len(result.Rejected) > 0 {
		text += fmt.Sprintf(" (%d recipient(s) rejected)", len(result.Rejected))
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, result, nil
}

const saveToSentTimeout = 45 * time.Second

func (s *Server) enqueueSaveToSent(acc *types.Account, raw []byte) {
	if s == nil || s.imapMgr == nil || acc == nil || len(raw) == 0 {
		return
	}
	accCopy := *acc
	rawCopy := bytes.Clone(raw)
	logger := s.logger
	imapMgr := s.imapMgr
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), saveToSentTimeout)
		defer cancel()
		if _, err := imapMgr.AppendSent(ctx, &accCopy, rawCopy); err != nil {
			logger.Info("save to sent failed", "account_id", accCopy.ID, "error", err)
			return
		}
		logger.Info("save to sent complete", "account_id", accCopy.ID)
	}()
}

func (s *Server) createDraft(ctx context.Context, req *mcp.CallToolRequest, in CreateDraftInput) (*mcp.CallToolResult, any, error) {
	acc, err := s.getAccount(ctx, in.AccountID)
	if err != nil {
		return nil, nil, err
	}

	msg, err := s.composeMessage(ctx, acc, composeFromSend(in.SendEmailToolInput), true)
	if err != nil {
		return nil, nil, err
	}
	raw, err := mailbuild.BuildWithLimits(msg, s.buildLimits(acc))
	if err != nil {
		return nil, nil, err
	}

	app, err := s.imapMgr.AppendDraft(ctx, acc, in.Folder, raw)
	if err != nil {
		return nil, nil, err
	}
	out := types.DraftResult{
		Folder:    app.Folder,
		UID:       app.UID,
		MessageID: msg.MessageID,
	}
	s.logger.Info("draft created", "account_id", acc.ID, "folder", out.Folder, "uid", out.UID, "size", len(raw))
	text := fmt.Sprintf("Draft saved to %s", out.Folder)
	if out.UID != 0 {
		text += fmt.Sprintf(" (UID %d)", out.UID)
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, out, nil
}

func (s *Server) getAttachment(ctx context.Context, req *mcp.CallToolRequest, in GetAttachmentInput) (*mcp.CallToolResult, any, error) {
	acc, err := s.getAccount(ctx, in.AccountID)
	if err != nil {
		return nil, nil, err
	}
	partID := in.AttachmentID
	if partID == "" {
		partID = in.PartID
	}
	fetched, err := s.imapMgr.GetAttachment(ctx, acc, in.Folder, in.UID, partID, in.Filename)
	if err != nil {
		return nil, nil, err
	}

	out := types.AttachmentContent{
		Filename:    fetched.Filename,
		ContentType: fetched.ContentType,
		Size:        fetched.Size,
	}
	inlineMax := int64(1 << 20)
	if s.cfg != nil && s.cfg.AttachmentInlineMaxBytes > 0 {
		inlineMax = s.cfg.AttachmentInlineMaxBytes
	}
	if fetched.Size <= inlineMax {
		out.Data = encodeInline(fetched.Content)
		return nil, out, nil
	}
	if s.attachStore != nil {
		url, err := s.attachStore.PutAndPresign(ctx, fetched.Filename, fetched.ContentType, fetched.Content)
		if err != nil {
			return nil, nil, types.AttachmentFetchFailed(fetched.Filename, err)
		}
		out.URL = url
		return nil, out, nil
	}
	return nil, nil, types.NewError(types.CodeAttachmentTooLarge,
		fmt.Sprintf("attachment is %d bytes which exceeds the inline cap of %d bytes; configure EMAILMCP_ATTACHMENT_S3_BUCKET for a download URL", fetched.Size, inlineMax),
		map[string]any{"actual_bytes": fetched.Size, "limit_bytes": inlineMax, "filename": fetched.Filename})
}

func composeFromSend(in SendEmailToolInput) ComposeInput {
	return ComposeInput{
		AccountID:                  in.AccountID,
		To:                         in.To,
		Cc:                         in.Cc,
		Bcc:                        in.Bcc,
		Subject:                    in.Subject,
		Text:                       in.Text,
		HTML:                       in.HTML,
		From:                       in.From,
		Attachments:                in.Attachments,
		ReplyToUID:                 in.ReplyToUID,
		ReplyFolder:                in.ReplyFolder,
		ReplyAll:                   in.ReplyAll,
		ForwardUID:                 in.ForwardUID,
		ForwardFolder:              in.ForwardFolder,
		IncludeOriginalAttachments: in.IncludeOriginalAttachments,
		QuoteOriginal:              in.QuoteOriginal,
		SaveToSent:                 in.SaveToSent,
	}
}

// --- Helpers ---

func (s *Server) requireAuth(ctx context.Context) (*UserInfo, error) {
	userInfo, ok := UserFromContext(ctx)
	if !ok {
		return nil, errors.New("authentication required: provide a valid session token")
	}
	return userInfo, nil
}

// getAccount loads an authenticated user's email account from the config store.
func (s *Server) getAccount(ctx context.Context, accountID string) (*types.Account, error) {
	userInfo, err := s.requireAuth(ctx)
	if err != nil {
		return nil, err
	}

	if accountID == "" {
		return nil, errors.New("account_id is required")
	}

	s.logger.Debug("fetching config from store", "user", userInfo.Email, "account_id", accountID)
	acc, err := s.configStore.GetUserConfig(ctx, userInfo.Subject, accountID)
	if err != nil {
		if errors.Is(err, config.ErrConfigNotFound) {
			return nil, fmt.Errorf("account %q not found; call add_email_account first", accountID)
		}
		return nil, fmt.Errorf("fetch config from store: %w", err)
	}

	// Bind ownership for pool isolation and re-validate host/TLS policy.
	acc.OwnerUserID = userInfo.Subject
	if err := validateAccountEndpoints(acc); err != nil {
		return nil, err
	}
	return acc, nil
}

// validateAccountEndpoints enforces SSRF and TLS policy on stored config before dial.
func validateAccountEndpoints(acc *types.Account) error {
	if err := netutil.ValidatePublicHost(acc.IMAPHost); err != nil {
		return fmt.Errorf("imap_host not allowed: %w", err)
	}
	if err := netutil.ValidatePublicHost(acc.SMTPHost); err != nil {
		return fmt.Errorf("smtp_host not allowed: %w", err)
	}
	if err := netutil.RequireTLSUnlessLocalhost(acc.IMAPHost, acc.IMAPUseTLS, "IMAP"); err != nil {
		return err
	}
	if err := netutil.RequireTLSUnlessLocalhost(acc.SMTPHost, acc.SMTPUseTLS, "SMTP"); err != nil {
		return err
	}
	return nil
}

func applyWebhookPayload(acc *types.Account, payload AccountPayload, keepExistingToken bool) {
	previousURL := acc.WebhookURL
	if payload.WebhookURL != nil {
		acc.WebhookURL = strings.TrimSpace(*payload.WebhookURL)
	}
	token := strings.TrimSpace(payload.WebhookToken)
	if token != "" {
		acc.WebhookToken = token
	} else if payload.ClearWebhookToken || !keepExistingToken || (acc.WebhookURL == "" && token == "") || (acc.WebhookURL != "" && webhookOrigin(previousURL) != webhookOrigin(acc.WebhookURL) && token == "") {
		acc.WebhookToken = ""
	}
	if payload.WatchFolders != nil {
		acc.WatchFolders = normalizeWatchFolders(payload.WatchFolders)
	}
}

func normalizeWatchFolders(folders []string) []string {
	result := make([]string, 0, len(folders))
	seen := make(map[string]struct{}, len(folders))
	for _, folder := range folders {
		folder = strings.TrimSpace(folder)
		key := folder
		if strings.EqualFold(folder, "INBOX") {
			folder = "INBOX"
			key = "INBOX"
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, folder)
	}
	return result
}

func webhookOrigin(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Hostname() == "" {
		return ""
	}
	port := u.Port()
	if port == "" {
		if strings.EqualFold(u.Scheme, "https") {
			port = "443"
		} else if strings.EqualFold(u.Scheme, "http") {
			port = "80"
		}
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Hostname()) + ":" + port
}

func validateWebhookSettings(webhookURL string, webhookToken string, folders []string) error {
	if len(folders) > types.MaxWatchFolders {
		return fmt.Errorf("watch_folders: at most %d folders allowed", types.MaxWatchFolders)
	}
	for i, folder := range folders {
		if strings.TrimSpace(folder) == "" {
			return fmt.Errorf("watch_folders[%d]: folder name must not be empty", i)
		}
	}
	webhookURL = strings.TrimSpace(webhookURL)
	if webhookURL == "" {
		if strings.TrimSpace(webhookToken) != "" {
			return fmt.Errorf("webhook_token supplied without webhook_url")
		}
		return nil
	}
	u, err := url.Parse(webhookURL)
	if err != nil || u.Host == "" {
		return fmt.Errorf("webhook_url is not a valid URL")
	}
	if u.Scheme != "https" {
		return fmt.Errorf("webhook_url must be https")
	}
	if u.User != nil {
		return fmt.Errorf("webhook_url must not include user information")
	}
	if err := netutil.ValidatePublicHost(u.Hostname()); err != nil {
		return fmt.Errorf("webhook_url not allowed: %w", err)
	}
	return nil
}

func (s *Server) portalURL() string {
	if s == nil || s.cfg == nil || s.cfg.PublicBaseURL == "" {
		return ""
	}
	return strings.TrimRight(s.cfg.PublicBaseURL, "/") + "/portal"
}

func defaultPort(p, def int) int {
	if p > 0 {
		return p
	}
	return def
}

// boolDefault returns *b when set, otherwise def. Used so omitted JSON bools
// honor schema defaults (e.g. imap_use_tls / smtp_use_tls default true).
func boolDefault(b *bool, def bool) bool {
	if b == nil {
		return def
	}
	return *b
}

func parseDate(s string) (time.Time, error) {
	layouts := []string{time.RFC3339, "2006-01-02", time.DateOnly}
	for _, l := range layouts {
		if t, err := time.Parse(l, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized date format: %s", s)
}
