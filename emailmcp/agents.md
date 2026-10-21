# EmailMCP - AI Agent Guidelines

## Project Overview
EmailMCP is a secure MCP server that exposes IMAP (inbound) and SMTP (outbound) email capabilities to AI agents via the Model Context Protocol using Streamable HTTP. Account configuration is stored remotely in DynamoDB; there is no local database.

## Core Principles
- Security first: Never log credentials or email bodies. Passwords are stored as encrypted attributes in DynamoDB and are fetched per-request.
- Concurrency matters: IMAP connection pooling and SMTP client management must be efficient and safe.
- Keep it idiomatic: Write clean, simple Go. Prefer standard library solutions when reasonable.
- MCP tools should be reliable and well-described — they become the agent's capabilities.

## Security Rules (Non-Negotiable)
- Never log passwords, decrypted credentials, or sensitive email content.
- Credentials are never persisted by the EmailMCP process; they are loaded from DynamoDB for the lifetime of a request/connection.
- IMAP and SMTP passwords are stored in DynamoDB; list APIs must never echo secrets.
- HTTP request/response logging redacts `Authorization`, `Cookie`, and `Proxy-Authorization`.
- IMAP pools are keyed by `(OwnerUserID, accountID)` — always set `Account.OwnerUserID` from the authenticated subject; call `DropPool` on remove/credential change.
- Reject private/link-local/metadata IMAP/SMTP hosts (`internal/netutil`); require TLS except for explicit localhost (loopback is still blocked by host validation).

## Architecture Overview
- `internal/config` — Env config + DynamoDB store (AWS SDK)
- `internal/netutil` — Outbound host validation (SSRF) and TLS policy helpers
- `internal/imap` — IMAP connection pool and operations
- `internal/smtp` — SMTP client management and sending logic
- `internal/server` — MCP server setup, Google auth middleware, tool registration
- `cmd/emailmcp` — Application entrypoint

## Coding Standards
- Use `context.Context` for all long-running or cancellable operations.
- Wrap errors with `%w` for proper error chains.
- Prefer small, focused functions and clear interfaces.
- Use `log/slog` for structured logging with appropriate levels.
- Keep MCP tool handlers relatively thin — move business logic into service packages.

## Adding New MCP Tools
1. Define the tool in the appropriate service package first.
2. Create a clear input/output schema with good descriptions.
3. Register the tool in `internal/server`.
4. Update `agents.md` if the new tool introduces new patterns or constraints.
5. Add basic validation and error handling.

## Working with IMAP
- Always acquire connections from the pool — never create ad-hoc clients.
- Be mindful of folder selection state when reusing connections.
- Respect per-account connection limits.
- Use UID-based operations for stability.
- After errors during use of a pooled connection, release with the error flag so the connection is discarded.

## Working with SMTP
- Reuse SMTP clients when possible but handle connection failures gracefully.
- Support both plain text and HTML bodies.
- Attachment handling should be clean and memory-efficient for reasonable sizes.
- Attachments may be base64 (`data`), HTTPS `url`, or allowlisted `path`. Decode/fetch only when sending or drafting.
- `send_email` and `create_draft` must share `mailbuild` so MIME never drifts. `create_draft` must never SMTP-send.
- Never log message bodies, attachment bytes, or credentials — sizes and IDs only.

## Testing & Quality
- Write tests for the DynamoDB store and connection pool logic.
- Use table-driven tests where appropriate.
- Run `go test ./...` and `go vet ./...` before considering changes complete.
- Ensure `go build ./...` succeeds.

## Project-Specific Patterns
- One email account config per authenticated Google user (keyed by `sub`).
- Account CRUD goes through `config.Store` (GET/PUT/DELETE DynamoDB).
- All IMAP/SMTP operations take a full `*types.Account` with plaintext passwords from DynamoDB.
- HTTP MCP traffic requires an opaque session access token (issued after Google sign-in) resolved against the session store. `GOOGLE_CLIENT_ID`, `GOOGLE_CLIENT_SECRET`, `PUBLIC_BASE_URL`, and `EMAILMCP_USER_CONFIG_TABLE` are required at startup for HTTP mode; `EMAILMCP_SESSION_TABLE` (or SSM `/emailmcp/session-table/name`) and `EMAILMCP_USER_CONFIG_TABLE` (or SSM `/emailmcp/user-config-table/name`) select the DynamoDB tables (in-memory fallback when unset). Google ID tokens are cryptographically verified (signature, aud, iss, exp) before a session is issued.
- `OAUTH_REDIRECT_ALLOWLIST` (optional, ConfigMap): when non-empty, HTTPS OAuth redirect URIs must match the allowlist; when blank, the allowlist is not enforced. Loopback HTTP and custom schemes remain allowed for desktop MCP clients.
- MCP OAuth lives in `internal/server/oauth.go`: protected-resource + AS metadata, DCR, authorize → Google, callback, token. After Google verifies the user, the token endpoint issues an opaque `access_token` (1h, capped by the Google ID token expiry) paired with an opaque refresh token, persisting the session in the `SessionStore` (`internal/server/store.go`). Sessions and registered DCR clients live in DynamoDB; short-lived auth codes and pending authorizations stay in-memory.
- The main server uses Streamable HTTP (`mcp.NewStreamableHTTPHandler`).

## GoLand Specific
- The project uses standard Go modules. GoLand should resolve dependencies cleanly.
- Run configurations should set `EMAILMCP_USER_CONFIG_TABLE`, `GOOGLE_CLIENT_ID`, `GOOGLE_CLIENT_SECRET`, and `PUBLIC_BASE_URL` for HTTP mode.

## Common Pitfalls to Avoid
- Logging plaintext passwords or serializing them into MCP responses.
- Keying IMAP pools by account ID alone (cross-tenant pool collision).
- Creating long-lived IMAP connections outside the pool manager.
- Assuming folder selection state persists across pool acquires.
- Forgetting to close or properly release connections on error paths.
- Using sequence numbers instead of UIDs for operations across sessions.
- Falling back to any local database or file for account storage.
- Trusting a client-supplied user/owner identifier instead of the authenticated session subject when keying account items.
