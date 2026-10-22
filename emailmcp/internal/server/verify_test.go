package server

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jpuckett/EmailMCP/emailmcp/internal/types"
)

func TestVerifyAccountCredentials_Validation(t *testing.T) {
	srv := &Server{}
	ctx := context.Background()

	// 1. Missing password
	acc := &types.Account{
		IMAPHost:     "imap.gmail.com",
		IMAPPort:     993,
		IMAPUsername: "user@gmail.com",
		IMAPPassword: "",
		IMAPUseTLS:   true,
		SMTPHost:     "smtp.gmail.com",
		SMTPPort:     587,
		SMTPUseTLS:   true,
	}
	res := srv.VerifyAccountCredentials(ctx, acc)
	if res.Success {
		t.Fatal("expected verify to fail when password is empty")
	}
	if res.IMAP.Success {
		t.Errorf("expected IMAP to fail with empty password, got success")
	}
	if res.IMAP.Error != "IMAP password is required for verification" {
		t.Errorf("unexpected IMAP error message: %s", res.IMAP.Error)
	}

	// 2. Private host rejection (SSRF)
	acc2 := &types.Account{
		IMAPHost:     "127.0.0.1",
		IMAPPort:     993,
		IMAPUsername: "user@gmail.com",
		IMAPPassword: "password123",
		IMAPUseTLS:   true,
		SMTPHost:     "10.0.0.1",
		SMTPPort:     587,
		SMTPUseTLS:   true,
	}
	res2 := srv.VerifyAccountCredentials(ctx, acc2)
	if res2.Success {
		t.Fatal("expected verify to fail on private hosts")
	}
	if res2.IMAP.Success || res2.SMTP.Success {
		t.Errorf("expected both IMAP and SMTP to fail SSRF check, got: %+v", res2)
	}

	// 3. TLS policy enforcement
	acc3 := &types.Account{
		IMAPHost:     "8.8.8.8",
		IMAPPort:     143,
		IMAPUsername: "user@example.com",
		IMAPPassword: "password123",
		IMAPUseTLS:   false, // Remote without TLS is blocked
		SMTPHost:     "8.8.8.8",
		SMTPPort:     25,
		SMTPUseTLS:   false,
	}
	res3 := srv.VerifyAccountCredentials(ctx, acc3)
	if res3.Success {
		t.Fatal("expected verify to fail when TLS is disabled on remote host")
	}
}

func TestVerifyAccountCredentials_NilAccount(t *testing.T) {
	srv := &Server{}
	res := srv.VerifyAccountCredentials(context.Background(), nil)
	if res == nil {
		t.Fatal("expected non-nil verification result for nil account")
	}
	if res.Success {
		t.Error("expected verification to fail for nil account")
	}
	if res.IMAP.Error != "account is required" {
		t.Errorf("unexpected IMAP error for nil account: %s", res.IMAP.Error)
	}
	if res.SMTP.Error != "account is required" {
		t.Errorf("unexpected SMTP error for nil account: %s", res.SMTP.Error)
	}
}

func TestVerifyAccountCredentials_MissingFields(t *testing.T) {
	srv := &Server{}
	ctx := context.Background()

	// Missing IMAPHost
	res := srv.VerifyAccountCredentials(ctx, &types.Account{
		IMAPUsername: "user",
		IMAPPassword: "pwd",
		SMTPHost:     "smtp.gmail.com",
		SMTPUsername: "user",
		SMTPPassword: "pwd",
	})
	if res.IMAP.Error != "IMAP host is required" {
		t.Errorf("unexpected error for missing IMAP host: %s", res.IMAP.Error)
	}

	// Missing IMAPUsername
	res = srv.VerifyAccountCredentials(ctx, &types.Account{
		IMAPHost:     "imap.gmail.com",
		IMAPPassword: "pwd",
		SMTPHost:     "smtp.gmail.com",
		SMTPUsername: "user",
		SMTPPassword: "pwd",
	})
	if res.IMAP.Error != "IMAP username is required" {
		t.Errorf("unexpected error for missing IMAP username: %s", res.IMAP.Error)
	}

	// Missing SMTPHost
	res = srv.VerifyAccountCredentials(ctx, &types.Account{
		IMAPHost:     "imap.gmail.com",
		IMAPUsername: "user",
		IMAPPassword: "pwd",
		IMAPUseTLS:   true,
	})
	if res.SMTP.Error != "SMTP host is required" {
		t.Errorf("unexpected error for missing SMTP host: %s", res.SMTP.Error)
	}

	// Missing SMTPUsername (when IMAPUsername is empty)
	res = srv.VerifyAccountCredentials(ctx, &types.Account{
		IMAPHost:     "imap.gmail.com",
		IMAPPassword: "pwd",
		IMAPUseTLS:   true,
		SMTPHost:     "smtp.gmail.com",
		SMTPPassword: "pwd",
		SMTPUseTLS:   true,
	})
	if res.SMTP.Error != "SMTP username is required" {
		t.Errorf("unexpected error for missing SMTP username: %s", res.SMTP.Error)
	}
}

func TestVerifyAccountCredentials_SMTPFallsBackToIMAPCredentials(t *testing.T) {
	srv := &Server{}
	res := srv.VerifyAccountCredentials(context.Background(), &types.Account{
		IMAPHost:     "imap.gmail.com",
		IMAPPort:     993,
		IMAPUsername: "user@gmail.com",
		IMAPPassword: "password123",
		IMAPUseTLS:   true,
		SMTPHost:     "10.0.0.1",
		SMTPPort:     587,
		SMTPUseTLS:   true,
	})
	if res.SMTP.Error == "SMTP password is required for verification" {
		t.Fatal("expected empty SMTP password to fall back to IMAP password")
	}
	if res.SMTP.Error == "SMTP username is required" {
		t.Fatal("expected empty SMTP username to fall back to IMAP username")
	}
	if !strings.Contains(res.SMTP.Error, "not allowed") {
		t.Fatalf("expected SSRF error after credential fallback, got %q", res.SMTP.Error)
	}
}

func TestVerifyAccountCredentials_CancelledContext(t *testing.T) {
	srv := &Server{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	acc := &types.Account{
		IMAPHost:     "imap.gmail.com",
		IMAPPort:     993,
		IMAPUsername: "user@gmail.com",
		IMAPPassword: "password123",
		IMAPUseTLS:   true,
		SMTPHost:     "smtp.gmail.com",
		SMTPPort:     587,
		SMTPUsername: "user@gmail.com",
		SMTPPassword: "password123",
		SMTPUseTLS:   true,
	}

	start := time.Now()
	res := srv.VerifyAccountCredentials(ctx, acc)
	if time.Since(start) > time.Second {
		t.Fatalf("cancelled verify took too long: %s", time.Since(start))
	}
	if res.Success {
		t.Fatal("expected verify to fail on canceled context")
	}
	if res.IMAP.Success || res.SMTP.Success {
		t.Fatalf("expected both IMAP and SMTP to fail on canceled context: %+v", res)
	}
}

func TestWaitWithContext_HonorsCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	block := make(chan struct{})
	defer close(block)
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	err := waitWithContext(ctx, func() error {
		<-block
		return nil
	})
	if err == nil {
		t.Fatal("expected context error")
	}
	if time.Since(start) > time.Second {
		t.Fatalf("waitWithContext took too long: %s", time.Since(start))
	}
}

func TestCloseNetConn_DoesNotHang(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	done := make(chan struct{})
	go func() {
		closeNetConn(a)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("closeNetConn hung")
	}
}
