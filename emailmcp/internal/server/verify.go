package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	net_smtp "net/smtp"
	"strconv"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/jpuckett/EmailMCP/emailmcp/internal/netutil"
	mailsmtp "github.com/jpuckett/EmailMCP/emailmcp/internal/smtp"
	"github.com/jpuckett/EmailMCP/emailmcp/internal/types"
)

const (
	verifyAttemptTimeout = 10 * time.Second
	verifyOverallTimeout = 25 * time.Second
	verifyCloseGrace     = 500 * time.Millisecond
)

// ComponentVerification reports verification details for a single protocol.
type ComponentVerification struct {
	Success bool   `json:"success"`
	Message string `json:"message,omitempty"`
	Error   string `json:"error,omitempty"`
}

// VerificationResult reports the combined result of IMAP and SMTP verification.
type VerificationResult struct {
	Success bool                  `json:"success"`
	IMAP    ComponentVerification `json:"imap"`
	SMTP    ComponentVerification `json:"smtp"`
}

// VerifyAccountCredentials verifies that both IMAP and SMTP endpoints are reachable
// and that the provided credentials authenticate successfully.
func (s *Server) VerifyAccountCredentials(ctx context.Context, acc *types.Account) *VerificationResult {
	if acc == nil {
		return &VerificationResult{
			Success: false,
			IMAP:    ComponentVerification{Success: false, Error: "account is required"},
			SMTP:    ComponentVerification{Success: false, Error: "account is required"},
		}
	}

	var (
		imapRes ComponentVerification
		smtpRes ComponentVerification
		wg      sync.WaitGroup
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		imapRes = s.verifyIMAP(ctx, acc)
	}()
	go func() {
		defer wg.Done()
		smtpRes = s.verifySMTP(ctx, acc)
	}()
	wg.Wait()

	return &VerificationResult{
		Success: imapRes.Success && smtpRes.Success,
		IMAP:    imapRes,
		SMTP:    smtpRes,
	}
}

func (s *Server) verifyIMAP(ctx context.Context, acc *types.Account) ComponentVerification {
	if acc.IMAPHost == "" {
		return ComponentVerification{Success: false, Error: "IMAP host is required"}
	}
	if acc.IMAPUsername == "" {
		return ComponentVerification{Success: false, Error: "IMAP username is required"}
	}
	if acc.IMAPPassword == "" {
		return ComponentVerification{Success: false, Error: "IMAP password is required for verification"}
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, verifyAttemptTimeout)
	defer cancel()

	if err := netutil.ValidatePublicHostContext(timeoutCtx, acc.IMAPHost); err != nil {
		return ComponentVerification{Success: false, Error: fmt.Sprintf("IMAP host not allowed: %v", err)}
	}
	if err := netutil.RequireTLSUnlessLocalhost(acc.IMAPHost, acc.IMAPUseTLS, "IMAP"); err != nil {
		return ComponentVerification{Success: false, Error: err.Error()}
	}

	port := acc.IMAPPort
	if port <= 0 {
		if acc.IMAPUseTLS {
			port = 993
		} else {
			port = 143
		}
	}

	addr := net.JoinHostPort(acc.IMAPHost, strconv.Itoa(port))
	conn, err := dialVerifyConn(timeoutCtx, addr, acc.IMAPHost, acc.IMAPUseTLS)
	if err != nil {
		if timeoutCtx.Err() != nil {
			return ComponentVerification{Success: false, Error: fmt.Sprintf("IMAP connection timed out to %s", addr)}
		}
		return ComponentVerification{Success: false, Error: fmt.Sprintf("failed to connect to IMAP %s: %v", addr, err)}
	}

	setConnDeadline(conn, timeoutCtx)
	stopCancel := context.AfterFunc(timeoutCtx, func() {
		closeNetConn(conn)
	})
	defer stopCancel()

	client := imapclient.New(conn, &imapclient.Options{})
	defer closeIMAPClient(client, conn)

	if err := waitWithContext(timeoutCtx, client.WaitGreeting); err != nil {
		if timeoutCtx.Err() != nil {
			return ComponentVerification{Success: false, Error: fmt.Sprintf("IMAP connection timed out to %s", addr)}
		}
		return ComponentVerification{Success: false, Error: fmt.Sprintf("IMAP greeting failed: %v", err)}
	}

	loginErr := waitWithContext(timeoutCtx, func() error {
		return client.Login(acc.IMAPUsername, acc.IMAPPassword).Wait()
	})
	if loginErr != nil {
		if timeoutCtx.Err() != nil {
			return ComponentVerification{Success: false, Error: "IMAP authentication timed out"}
		}
		return ComponentVerification{Success: false, Error: fmt.Sprintf("IMAP authentication failed: %v", loginErr)}
	}

	return ComponentVerification{
		Success: true,
		Message: fmt.Sprintf("Connected to %s and authenticated as %s", addr, acc.IMAPUsername),
	}
}

func (s *Server) verifySMTP(ctx context.Context, acc *types.Account) ComponentVerification {
	if acc.SMTPHost == "" {
		return ComponentVerification{Success: false, Error: "SMTP host is required"}
	}

	smtpUser := acc.SMTPUser()
	if smtpUser == "" {
		return ComponentVerification{Success: false, Error: "SMTP username is required"}
	}

	smtpPass := acc.SMTPPass()
	if smtpPass == "" {
		return ComponentVerification{Success: false, Error: "SMTP password is required for verification"}
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, verifyAttemptTimeout)
	defer cancel()

	if err := netutil.ValidatePublicHostContext(timeoutCtx, acc.SMTPHost); err != nil {
		return ComponentVerification{Success: false, Error: fmt.Sprintf("SMTP host not allowed: %v", err)}
	}
	if err := netutil.RequireTLSUnlessLocalhost(acc.SMTPHost, acc.SMTPUseTLS, "SMTP"); err != nil {
		return ComponentVerification{Success: false, Error: err.Error()}
	}

	port := acc.SMTPPort
	if port <= 0 {
		if acc.SMTPUseTLS {
			port = 587
		} else {
			port = 25
		}
	}

	addr := net.JoinHostPort(acc.SMTPHost, strconv.Itoa(port))

	err := waitWithContext(timeoutCtx, func() error {
		return s.dialAndAuthSMTP(timeoutCtx, addr, acc.SMTPHost, port, smtpUser, smtpPass, acc.SMTPUseTLS)
	})
	if err != nil {
		if timeoutCtx.Err() != nil {
			return ComponentVerification{Success: false, Error: fmt.Sprintf("SMTP connection timed out to %s", addr)}
		}
		return ComponentVerification{Success: false, Error: err.Error()}
	}

	return ComponentVerification{
		Success: true,
		Message: fmt.Sprintf("Connected to %s and authenticated as %s", addr, smtpUser),
	}
}

func (s *Server) dialAndAuthSMTP(ctx context.Context, addr, host string, port int, user, pass string, useTLS bool) error {
	implicitTLS := useTLS && port == 465
	conn, err := dialVerifyConn(ctx, addr, host, implicitTLS)
	if err != nil {
		if implicitTLS {
			return fmt.Errorf("TLS connection to %s failed: %w", addr, err)
		}
		return fmt.Errorf("connection to %s failed: %w", addr, err)
	}

	setConnDeadline(conn, ctx)
	stopCancel := context.AfterFunc(ctx, func() {
		closeNetConn(conn)
	})
	defer stopCancel()
	defer closeNetConn(conn)

	client, err := net_smtp.NewClient(conn, host)
	if err != nil {
		return fmt.Errorf("SMTP handshake with %s failed: %w", addr, err)
	}
	defer func() {
		_ = client.Close()
	}()

	if useTLS && port != 465 {
		if ok, _ := client.Extension("STARTTLS"); ok {
			tlsConfig := &tls.Config{
				ServerName: host,
				MinVersion: tls.VersionTLS12,
			}
			if err := client.StartTLS(tlsConfig); err != nil {
				return fmt.Errorf("STARTTLS negotiation failed: %w", err)
			}
		} else {
			return errors.New("server does not support STARTTLS")
		}
	}

	auth := mailsmtp.NewAuth(user, pass, host, implicitTLS)
	if err := client.Auth(auth); err != nil {
		return fmt.Errorf("SMTP authentication failed: %w", err)
	}

	if err := client.Quit(); err != nil {
		_ = err
	}

	return nil
}

func dialVerifyConn(ctx context.Context, addr, serverName string, useTLS bool) (net.Conn, error) {
	dialer := &net.Dialer{}
	rawConn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	if !useTLS {
		return rawConn, nil
	}
	tlsConn := tls.Client(rawConn, &tls.Config{
		ServerName: serverName,
		MinVersion: tls.VersionTLS12,
	})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		closeNetConn(rawConn)
		return nil, err
	}
	return tlsConn, nil
}

func waitWithContext(ctx context.Context, fn func() error) error {
	errCh := make(chan error, 1)
	go func() {
		errCh <- fn()
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errCh:
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
}

func setConnDeadline(conn net.Conn, ctx context.Context) {
	if conn == nil {
		return
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
		return
	}
	_ = conn.SetDeadline(time.Now().Add(verifyAttemptTimeout))
}

func closeNetConn(conn net.Conn) {
	if conn == nil {
		return
	}
	_ = conn.SetDeadline(time.Now())
	_ = conn.Close()
}

func closeIMAPClient(client *imapclient.Client, conn net.Conn) {
	closeNetConn(conn)
	if client == nil {
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = client.Close()
	}()
	select {
	case <-done:
	case <-time.After(verifyCloseGrace):
	}
}
