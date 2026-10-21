package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jpuckett/EmailMCP/emailmcp/internal/config"
	"github.com/jpuckett/EmailMCP/emailmcp/internal/notify"
)

// newUserConfigStore is a seam so tests can avoid DynamoDB.
var newUserConfigStore = config.NewStore

func runNotify(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg == nil {
		return errors.New("config is required")
	}

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := requireNotifyStateTable(cfg.NotifyStateTableName); err != nil {
		return err
	}

	if err := requireNotifyUserConfigTable(cfg.UserConfigTableName); err != nil {
		return err
	}

	owner := resolveNotifyOwner(cfg.NotifyOwnerID)
	accounts, err := newUserConfigStore(ctx, cfg.UserConfigTableName, cfg.ApplicationID, logger)
	if err != nil {
		return fmt.Errorf("user config store: %w", err)
	}
	leases, err := notify.NewLeaseStore(ctx, cfg.NotifyStateTableName, cfg.ApplicationID, logger)
	if err != nil {
		return fmt.Errorf("notify lease store: %w", err)
	}

	dial := notify.IMAPDialer(logger, false)
	sup := notify.NewSupervisor(notify.Config{
		Logger:            logger,
		OwnerID:           owner,
		Accounts:          accounts,
		Leases:            leases,
		Dispatcher:        notify.NewDispatcher(notify.DispatcherConfig{Logger: logger, Timeout: cfg.NotifyWebhookTimeout}),
		Dial:              dial,
		FindSent:          notify.DedicatedSentFinder(dial),
		ReconcileInterval: cfg.NotifyReconcileInterval,
		MaxFolders:        cfg.NotifyMaxFolders,
	})

	addr := cfg.ListenAddr
	if addr == "" {
		addr = ":8080"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("notify listen %s: %w", addr, err)
	}
	httpServer := newNotifyHTTPServer(addr)

	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	supErr := make(chan error, 1)
	go func() {
		supErr <- sup.Run(runCtx)
	}()

	httpErr := make(chan error, 1)
	go func() {
		logger.Info("EmailMCP notify starting",
			"addr", ln.Addr().String(),
			"transport", "notify",
			"owner", owner,
			"notify_state_table_set", cfg.NotifyStateTableName != "",
		)
		httpErr <- httpServer.Serve(ln)
	}()

	var runErr error
	supervisorDone := false
	select {
	case err := <-httpErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			runErr = err
		}
		runCancel()
	case err := <-supErr:
		runErr = err
		supervisorDone = true
		runCancel()
	case <-ctx.Done():
		runCancel()
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("notify http shutdown error", "error", err)
	}
	if !supervisorDone {
		select {
		case err := <-supErr:
			if runErr == nil {
				runErr = err
			}
		case <-shutdownCtx.Done():
			logger.Error("notify supervisor shutdown timed out")
		}
	}
	return runErr
}

func newNotifyHTTPServer(addr string) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           notifyHealthHandler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
}

func notifyHealthHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}

func requireNotifyStateTable(tableName string) error {
	if strings.TrimSpace(tableName) != "" {
		return nil
	}
	if runningInECS() {
		return errors.New("EMAILMCP_NOTIFY_STATE_TABLE is required on ECS (in-memory leases are not allowed in production)")
	}
	return nil
}

// requireNotifyUserConfigTable fails closed when no shared user-config table is
// configured: the watcher has no write path, so an in-memory store would stay
// empty and the process would report healthy while doing nothing.
func requireNotifyUserConfigTable(tableName string) error {
	if strings.TrimSpace(tableName) != "" {
		return nil
	}
	return errors.New("EMAILMCP_USER_CONFIG_TABLE is required for the notify transport (the watcher needs the shared DynamoDB user-config table)")
}

func runningInECS() bool {
	if os.Getenv("ECS_CONTAINER_METADATA_URI_V4") != "" || os.Getenv("ECS_CONTAINER_METADATA_URI") != "" {
		return true
	}
	return strings.HasPrefix(os.Getenv("AWS_EXECUTION_ENV"), "AWS_ECS")
}

func resolveNotifyOwner(configured string) string {
	id := strings.TrimSpace(configured)
	if id != "" && !strings.EqualFold(id, "notify") {
		return id
	}
	return uniqueOwnerID()
}

func uniqueOwnerID() string {
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		host = "host"
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%s-%d-%d", host, os.Getpid(), time.Now().UnixNano())
	}
	return fmt.Sprintf("%s-%d-%x", host, os.Getpid(), b)
}
