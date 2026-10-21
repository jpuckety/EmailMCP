package config

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

// Config holds application configuration.
type Config struct {
	// Server
	ListenAddr string

	// IMAP pool settings
	IMAPMaxConnsPerAccount int
	IMAPConnIdleTimeout    time.Duration

	// SMTP
	SMTPDefaultTimeout time.Duration

	// Logging
	LogLevel string

	// Transport
	Transport string // "http" or "stdio"

	// Auth & remote account storage
	ApplicationID  string
	GoogleClientID string
	// GoogleClientSecret is required for HTTP mode OAuth (authorization code
	// exchange with Google). Not used for ID-token verification itself.
	GoogleClientSecret string
	// PublicBaseURL is the externally reachable origin of this server
	// (e.g. https://emailmcp.ecg.co). Used for OAuth metadata and redirect URIs.
	PublicBaseURL string
	// SessionTableName is the DynamoDB table used to persist OAuth sessions
	// (opaque access/refresh tokens) and registered clients. When empty, an
	// in-memory store is used (sessions will not survive a restart or span
	// replicas). Resolved from EMAILMCP_SESSION_TABLE or the SSM parameter
	// /emailmcp/session-table/name.
	SessionTableName string
	// UserConfigTableName is the DynamoDB table used to persist per-user email
	// account configurations (previously fronted by the Config API Lambda). When
	// empty, an in-memory store is used (accounts will not survive a restart or
	// span replicas). Resolved from EMAILMCP_USER_CONFIG_TABLE or the SSM
	// parameter /emailmcp/user-config-table/name.
	UserConfigTableName string
	// OAuthRedirectAllowlist restricts HTTPS OAuth redirect_uris when non-empty.
	// Entries are comma-separated hostnames (example.com, *.example.com) or
	// https origins/URIs. Empty means the allowlist is not enforced (any HTTPS
	// host is still accepted subject to other redirect rules). Loopback HTTP and
	// custom schemes are always allowed for desktop MCP clients.
	OAuthRedirectAllowlist []string

	// Attachment / message limits
	MaxAttachmentBytes int64
	MaxMessageBytes    int64
	AttachmentURLTimeout time.Duration
	AttachmentAllowedHosts []string
	AttachmentPathAllowlist []string
	AttachmentInlineMaxBytes int64
	AttachmentS3Bucket string
	AttachmentS3Prefix string
	AttachmentPresignTTL time.Duration
	SaveToSentDefault string
}

// Load reads configuration from environment with sensible defaults.
func Load() (*Config, error) {
	cfg := &Config{
		ListenAddr:             getEnv("EMAILMCP_LISTEN_ADDR", ":8080"),
		IMAPMaxConnsPerAccount: getEnvInt("EMAILMCP_IMAP_MAX_CONNS", 4),
		IMAPConnIdleTimeout:    getEnvDuration("EMAILMCP_IMAP_IDLE_TIMEOUT", 5*time.Minute),
		SMTPDefaultTimeout:     getEnvDuration("EMAILMCP_SMTP_TIMEOUT", 30*time.Second),
		LogLevel:               getEnv("EMAILMCP_LOG_LEVEL", "info"),
		Transport:              getEnv("EMAILMCP_TRANSPORT", "http"),
		ApplicationID:          getEnv("APPLICATION_ID", "default"),
		GoogleClientID:         getEnv("GOOGLE_CLIENT_ID", ""),
		GoogleClientSecret:     getEnv("GOOGLE_CLIENT_SECRET", ""),
		PublicBaseURL:          strings.TrimRight(getEnv("PUBLIC_BASE_URL", ""), "/"),
		SessionTableName:       getEnv("EMAILMCP_SESSION_TABLE", ""),
		UserConfigTableName:    getEnv("EMAILMCP_USER_CONFIG_TABLE", ""),
		OAuthRedirectAllowlist: parseCSVEnv("OAUTH_REDIRECT_ALLOWLIST"),
		MaxAttachmentBytes:       getEnvInt64("EMAILMCP_ATTACHMENT_MAX_BYTES", 10<<20),
		MaxMessageBytes:          getEnvInt64("EMAILMCP_MESSAGE_MAX_BYTES", 25<<20),
		AttachmentURLTimeout:     getEnvDuration("EMAILMCP_ATTACHMENT_URL_TIMEOUT", 30*time.Second),
		AttachmentAllowedHosts:   parseCSVEnv("EMAILMCP_ATTACHMENT_ALLOWED_HOSTS"),
		AttachmentPathAllowlist:  parseCSVEnv("EMAILMCP_ATTACHMENT_PATH_ALLOWLIST"),
		AttachmentInlineMaxBytes: getEnvInt64("EMAILMCP_ATTACHMENT_INLINE_MAX", 1<<20),
		AttachmentS3Bucket:       getEnv("EMAILMCP_ATTACHMENT_S3_BUCKET", ""),
		AttachmentS3Prefix:       getEnv("EMAILMCP_ATTACHMENT_S3_PREFIX", "emailmcp-attachments/"),
		AttachmentPresignTTL:     getEnvDuration("EMAILMCP_ATTACHMENT_PRESIGN_TTL", 15*time.Minute),
		SaveToSentDefault:        getEnv("EMAILMCP_SAVE_TO_SENT", "auto"),
	}

	if cfg.IMAPMaxConnsPerAccount < 1 {
		slog.Warn("EMAILMCP_IMAP_MAX_CONNS < 1; clamping to 1",
			"configured", cfg.IMAPMaxConnsPerAccount,
		)
		cfg.IMAPMaxConnsPerAccount = 1
	}

	// Never log GoogleClientSecret or other secrets.
	slog.Info("configuration loaded from environment",
		"listen_addr", cfg.ListenAddr,
		"imap_max_conns", cfg.IMAPMaxConnsPerAccount,
		"imap_idle_timeout", cfg.IMAPConnIdleTimeout,
		"smtp_timeout", cfg.SMTPDefaultTimeout,
		"log_level", cfg.LogLevel,
		"transport", cfg.Transport,
		"application_id", cfg.ApplicationID,
		"google_client_id_set", cfg.GoogleClientID != "",
		"google_client_secret_set", cfg.GoogleClientSecret != "",
		"public_base_url", cfg.PublicBaseURL,
		"session_table_set", cfg.SessionTableName != "",
		"session_table", cfg.SessionTableName,
		"user_config_table_set", cfg.UserConfigTableName != "",
		"user_config_table", cfg.UserConfigTableName,
		"oauth_redirect_allowlist_enforced", len(cfg.OAuthRedirectAllowlist) > 0,
		"oauth_redirect_allowlist_entries", len(cfg.OAuthRedirectAllowlist),
		"max_attachment_bytes", cfg.MaxAttachmentBytes,
		"max_message_bytes", cfg.MaxMessageBytes,
		"attachment_url_timeout", cfg.AttachmentURLTimeout,
		"attachment_allowed_hosts", len(cfg.AttachmentAllowedHosts),
		"attachment_path_allowlist", len(cfg.AttachmentPathAllowlist),
		"attachment_inline_max", cfg.AttachmentInlineMaxBytes,
		"attachment_s3_bucket_set", cfg.AttachmentS3Bucket != "",
		"save_to_sent_default", cfg.SaveToSentDefault,
		"aws_region", os.Getenv("AWS_REGION"),
	)

	return cfg, nil
}

// FetchRemoteDefaults attempts to load missing configuration from AWS SSM if available.
func (c *Config) FetchRemoteDefaults(ctx context.Context) {
	logger := slog.Default().With("op", "fetch_remote_defaults")

	needSessionTable := c.SessionTableName == ""
	needUserConfigTable := c.UserConfigTableName == ""
	if !needSessionTable && !needUserConfigTable {
		logger.DebugContext(ctx, "skipping SSM lookup; session and user config tables already set")
		return
	}

	region := os.Getenv("AWS_REGION")
	if region == "" {
		logger.DebugContext(ctx, "skipping SSM lookup; AWS_REGION unset")
		return
	}

	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		logger.WarnContext(ctx, "failed to load AWS config for SSM lookup",
			"region", region,
			"error", err,
		)
		return
	}
	client := ssm.NewFromConfig(cfg)

	if needSessionTable {
		if v := getSSMParameter(ctx, client, "/emailmcp/session-table/name", logger); v != "" {
			c.SessionTableName = v
			logger.InfoContext(ctx, "session table resolved from SSM", "session_table", v)
		}
	}
	if needUserConfigTable {
		if v := getSSMParameter(ctx, client, "/emailmcp/user-config-table/name", logger); v != "" {
			c.UserConfigTableName = v
			logger.InfoContext(ctx, "user config table resolved from SSM", "user_config_table", v)
		}
	}
}

// getSSMParameter reads a single SSM parameter, returning "" on any error or
// when the parameter is missing.
func getSSMParameter(ctx context.Context, client *ssm.Client, name string, logger *slog.Logger) string {
	start := time.Now()
	out, err := client.GetParameter(ctx, &ssm.GetParameterInput{
		Name: aws.String(name),
	})
	if err != nil {
		logger.WarnContext(ctx, "SSM GetParameter failed",
			"parameter", name,
			"error", err,
			"elapsed", time.Since(start),
		)
		return ""
	}
	if out.Parameter == nil || out.Parameter.Value == nil {
		logger.WarnContext(ctx, "SSM parameter missing or empty",
			"parameter", name,
			"elapsed", time.Since(start),
		)
		return ""
	}
	return *out.Parameter.Value
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// parseCSVEnv splits a comma-separated env var into trimmed non-empty entries.
func parseCSVEnv(key string) []string {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func getEnvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
		slog.Warn("invalid integer env var; using default",
			"key", key,
			"value", v,
			"default", def,
		)
	}
	return def
}

func getEnvInt64(key string, def int64) int64 {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.ParseInt(v, 10, 64); err == nil {
			return i
		}
		slog.Warn("invalid integer env var; using default",
			"key", key,
			"value", v,
			"default", def,
		)
	}
	return def
}

func getEnvDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		slog.Warn("invalid duration env var; using default",
			"key", key,
			"value", v,
			"default", def,
		)
	}
	return def
}
