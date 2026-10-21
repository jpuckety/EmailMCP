package config

import (
	"testing"
	"time"
)

func TestRemoteParamsNeeded(t *testing.T) {
	tests := []struct {
		name                              string
		cfg                               Config
		wantSession, wantUser, wantNotify bool
	}{
		{"http all unset: no notify lookup", Config{Transport: "http"}, true, true, false},
		{"http tables set: early return", Config{Transport: "http", SessionTableName: "s", UserConfigTableName: "u"}, false, false, false},
		{"stdio never looks up notify", Config{Transport: "stdio"}, true, true, false},
		{"empty transport never looks up notify", Config{}, true, true, false},
		{"notify skips session", Config{Transport: "notify"}, false, true, true},
		{"notify user set", Config{Transport: "notify", UserConfigTableName: "u"}, false, false, true},
		{"notify all set", Config{Transport: "notify", UserConfigTableName: "u", NotifyStateTableName: "n"}, false, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, u, n := tc.cfg.remoteParamsNeeded()
			if s != tc.wantSession || u != tc.wantUser || n != tc.wantNotify {
				t.Fatalf("got session=%v user=%v notify=%v, want %v %v %v", s, u, n, tc.wantSession, tc.wantUser, tc.wantNotify)
			}
		})
	}
}

func TestLoadNotifyDefaultsAndClamps(t *testing.T) {
	t.Setenv("EMAILMCP_NOTIFY_STATE_TABLE", "")
	t.Setenv("EMAILMCP_NOTIFY_OWNER", "")
	t.Setenv("EMAILMCP_NOTIFY_RECONCILE", "")
	t.Setenv("EMAILMCP_NOTIFY_WEBHOOK_TIMEOUT", "")
	t.Setenv("EMAILMCP_NOTIFY_MAX_FOLDERS", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.NotifyReconcileInterval != 30*time.Second {
		t.Fatalf("reconcile = %s, want 30s", cfg.NotifyReconcileInterval)
	}
	if cfg.NotifyWebhookTimeout != 10*time.Second {
		t.Fatalf("webhook timeout = %s, want 10s", cfg.NotifyWebhookTimeout)
	}
	if cfg.NotifyMaxFolders != 8 {
		t.Fatalf("max folders = %d, want 8", cfg.NotifyMaxFolders)
	}
	if cfg.NotifyStateTableName != "" {
		t.Fatalf("state table = %q, want empty", cfg.NotifyStateTableName)
	}

	t.Setenv("EMAILMCP_NOTIFY_STATE_TABLE", "NotifyState")
	t.Setenv("EMAILMCP_NOTIFY_OWNER", "task-1")
	t.Setenv("EMAILMCP_NOTIFY_RECONCILE", "45s")
	t.Setenv("EMAILMCP_NOTIFY_WEBHOOK_TIMEOUT", "8s")
	t.Setenv("EMAILMCP_NOTIFY_MAX_FOLDERS", "3")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load overrides: %v", err)
	}
	if cfg.NotifyStateTableName != "NotifyState" {
		t.Fatalf("state table = %q", cfg.NotifyStateTableName)
	}
	if cfg.NotifyOwnerID != "task-1" {
		t.Fatalf("owner = %q", cfg.NotifyOwnerID)
	}
	if cfg.NotifyReconcileInterval != 45*time.Second {
		t.Fatalf("reconcile = %s, want 45s", cfg.NotifyReconcileInterval)
	}
	if cfg.NotifyWebhookTimeout != 8*time.Second {
		t.Fatalf("webhook timeout = %s, want 8s", cfg.NotifyWebhookTimeout)
	}
	if cfg.NotifyMaxFolders != 3 {
		t.Fatalf("max folders = %d, want 3", cfg.NotifyMaxFolders)
	}

	t.Setenv("EMAILMCP_NOTIFY_RECONCILE", "1s")
	t.Setenv("EMAILMCP_NOTIFY_WEBHOOK_TIMEOUT", "2m")
	t.Setenv("EMAILMCP_NOTIFY_MAX_FOLDERS", "99")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load clamps: %v", err)
	}
	if cfg.NotifyReconcileInterval != 5*time.Second {
		t.Fatalf("reconcile clamp = %s, want 5s", cfg.NotifyReconcileInterval)
	}
	if cfg.NotifyWebhookTimeout != 30*time.Second {
		t.Fatalf("webhook clamp = %s, want 30s", cfg.NotifyWebhookTimeout)
	}
	if cfg.NotifyMaxFolders != 8 {
		t.Fatalf("max folders clamp = %d, want 8", cfg.NotifyMaxFolders)
	}
}
