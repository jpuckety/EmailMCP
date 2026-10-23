# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Overview

This repository contains **EmailMCP** — a production-grade MCP (Model Context Protocol) server written in Go that exposes IMAP and SMTP email capabilities as MCP tools for AI agents. All code lives in `emailmcp/`. Account configuration is loaded exclusively from DynamoDB (no local database).

## Commands

All Go commands must run from within `emailmcp/`, or use `run.sh` from the repo root.

```bash
# From emailmcp/
go build -o emailmcp ./cmd/emailmcp   # build native binary
go test ./...                          # run all tests
go test ./internal/config/...          # run a single package's tests
go vet ./...                           # vet
go build ./...                         # verify everything compiles

# From repo root via run.sh
./run.sh test          # go test ./...
./run.sh run           # load .env and start the HTTP server
./run.sh run-stdio     # stdio MCP mode
./run.sh check         # test + vet + build
./run.sh docker-build  # repo-root Dockerfile
./run.sh synth         # CDK synth
./run.sh clean         # remove binaries
```

The server requires `GOOGLE_CLIENT_ID` to start. HTTP mode also requires `GOOGLE_CLIENT_SECRET` and `PUBLIC_BASE_URL`. The `run.sh` script loads them from `emailmcp/.env` automatically. If `EMAILMCP_USER_CONFIG_TABLE` is unset, startup may resolve it from SSM parameter `/emailmcp/user-config-table/name` when `AWS_REGION` and credentials are available. ECS injects table names as environment variables.

## Architecture

```
emailmcp/
  cmd/emailmcp/         Entry point — wires together config and server
  internal/
    config/             Env-based config + DynamoDB store (AWS SDK)
    imap/               Per-account connection pool + all IMAP operations (go-imap/v2)
    smtp/               Stateless per-send SMTP client (jordan-wright/email)
    server/             MCP server setup, Google auth, tool registration and handlers
    types/              Shared domain types (Account, EmailSummary, EmailMessage, etc.)
```

**Request flow:** MCP client (Google ID token) → Streamable HTTP → auth middleware → `server.go` handler → `config.Store` for account → `imap.Manager` or `smtp.Sender`.

### MCP Tool Registration

All tools are registered in `server.go:registerTools()` using `mcp.AddTool`. Handler functions live in `server.go` and delegate to service packages. Tool input structs use `jsonschema` tags for the MCP schema.

### Account storage

There is **no local SQLite or other local account store**. Each authenticated Google user has account configurations stored in DynamoDB. IMAP/SMTP passwords are application-encrypted with KMS before storage; table-level encryption is also enabled. Tools:

- `add_email_account` → PUT DynamoDB
- `list_email_accounts` → GET DynamoDB (0 or 1 account)
- `remove_email_account` → DELETE DynamoDB

### IMAP Connection Pool

`internal/imap/pool.go` maintains a per-account pool of `*imapclient.Client` connections. The pattern for every IMAP operation:

```go
conn, err := m.Acquire(ctx, acc) // acc.OwnerUserID must be set (tenant isolation)
hadErr := false
defer func() { m.Release(acc, conn, hadErr) }()
// ... use conn.client ...
// on error: hadErr = true (connection is discarded, not returned to pool)
// on remove_email_account / credential change: m.DropPool(ownerUserID, accountID)
```

Pools are keyed by `(OwnerUserID, accountID)` so two users with the same account slug cannot share connections. Connections track `lastSelected` folder to reduce churn but always re-`SELECT` before operations. Always use UID-based operations (`UIDSearch`, `UIDSetNum`) — never sequence numbers. IMAP/SMTP hosts must resolve to public addresses (private/link-local/metadata ranges are blocked). Non-TLS is refused except for explicit localhost (which is still blocked by host validation in normal use).

### Credential Handling

Passwords are decrypted from DynamoDB into `types.Account` (`IMAPPassword` / `SMTPPassword`) for the lifetime of the operation. Legacy unprefixed values remain readable and are encrypted the next time the account is written. Never log passwords. When `SMTPPassword` is empty, SMTP falls back to `IMAPPassword`.

### Transports

The server supports two MCP transports selected at startup:
- **HTTP** (default): Streamable HTTP on `:8080`, served via `mcp.NewStreamableHTTPHandler`, with Google ID token auth and MCP OAuth endpoints (`/.well-known/*`, `/oauth/*`). Logs are written to stderr; SSE connections are logged at Info, regular requests at Debug.
- **stdio**: `mcp.StdioTransport` for Claude Desktop integration. Stdout is reserved for the protocol; all logging goes to stderr. Tool handlers still require authenticated user context (HTTP auth middleware is not applied on stdio).

## Adding New MCP Tools

1. Implement the operation in the appropriate service package (`imap/` or `smtp/`).
2. Define an input struct with `json` and `jsonschema` tags in `server.go`.
3. Write a thin handler method on `*Server` that calls the service.
4. Register with `mcp.AddTool` in `registerTools()`.
5. Update `emailmcp/agents.md` if new patterns or constraints are introduced.

## Security Rules

- Never log passwords or email body content.
- Do not reintroduce local credential storage or encryption keys.
- HTTP request/response logging in `httpLogging()` redacts `Authorization`, `Cookie`, and `Proxy-Authorization` headers and never logs bodies.
