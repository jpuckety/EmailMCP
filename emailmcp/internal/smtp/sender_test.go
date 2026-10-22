package smtp

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jpuckett/EmailMCP/emailmcp/internal/types"
)

func TestShouldSaveToSent(t *testing.T) {
	if !ShouldSaveToSent("true", "smtp.gmail.com") {
		t.Fatal("explicit true")
	}
	if ShouldSaveToSent("false", "smtp.mail.us-east-1.awsapps.com") {
		t.Fatal("explicit false")
	}
	if ShouldSaveToSent("auto", "smtp.gmail.com") {
		t.Fatal("gmail auto should skip sent copy")
	}
	if !ShouldSaveToSent("auto", "smtp.mail.us-east-1.awsapps.com") {
		t.Fatal("workmail auto should save")
	}
	if !ShouldSaveToSent("auto", "imap.example.com") {
		t.Fatal("unknown auto should save")
	}
}

func TestProviderMessageLimit_WorkMail(t *testing.T) {
	n, name := ProviderMessageLimit("smtp.mail.us-west-2.awsapps.com")
	if n != WorkMailMIMEMax || name != "Amazon WorkMail" {
		t.Fatalf("%d %s", n, name)
	}
	n, name = ProviderMessageLimit("smtp.gmail.com")
	if n != 0 || name != "" {
		t.Fatalf("%d %s", n, name)
	}
}

func TestSendRaw_TimeoutAborts(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}

	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = c.Write([]byte("220 test ESMTP\r\n"))
		_, _ = io.Copy(io.Discard, c)
	}()

	s := NewSender(Config{
		DefaultTimeout:     200 * time.Millisecond,
		SkipHostValidation: true,
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	acc := &types.Account{
		ID:           "work",
		SMTPHost:     "127.0.0.1",
		SMTPPort:     port,
		SMTPUsername: "user",
		SMTPPassword: "pass",
		SMTPUseTLS:   false,
	}
	start := time.Now()
	_, err = s.SendRaw(context.Background(), acc, "from@example.com", []string{"to@example.com"}, []byte("From: from@example.com\r\n\r\nbody"), "<id@example.com>")
	if err == nil {
		t.Fatal("expected timeout")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "timeout") && !strings.Contains(strings.ToLower(err.Error()), "deadline") {
		t.Fatalf("expected timeout error, got %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("timeout took too long: %s", time.Since(start))
	}
}
