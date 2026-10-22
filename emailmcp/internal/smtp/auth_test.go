package smtp

import (
	"net/smtp"
	"strings"
	"testing"
)

func TestNewAuth_ImplicitTLSAllowsPlain(t *testing.T) {
	auth := NewAuth("user", "pass", "smtp.example.com", true)
	mech, resp, err := auth.Start(&smtp.ServerInfo{
		Name: "smtp.example.com",
		TLS:  false, // net/smtp leaves this false on implicit TLS
		Auth: []string{"PLAIN"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if mech != "PLAIN" {
		t.Fatalf("mech = %q, want PLAIN", mech)
	}
	if len(resp) == 0 {
		t.Fatal("expected PLAIN initial response")
	}
}

func TestNewAuth_UnencryptedRejected(t *testing.T) {
	auth := NewAuth("user", "pass", "smtp.example.com", false)
	_, _, err := auth.Start(&smtp.ServerInfo{
		Name: "smtp.example.com",
		TLS:  false,
		Auth: []string{"PLAIN"},
	})
	if err == nil || !strings.Contains(err.Error(), "unencrypted connection") {
		t.Fatalf("expected unencrypted connection error, got %v", err)
	}
}

func TestNewAuth_LoginWhenPlainMissing(t *testing.T) {
	auth := NewAuth("user", "pass", "smtp.example.com", true)
	mech, _, err := auth.Start(&smtp.ServerInfo{
		Name: "smtp.example.com",
		TLS:  false,
		Auth: []string{"LOGIN"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if mech != "LOGIN" {
		t.Fatalf("mech = %q, want LOGIN", mech)
	}

	got, err := auth.Next([]byte("Username:"), true)
	if err != nil {
		t.Fatalf("Next username: %v", err)
	}
	if string(got) != "user" {
		t.Fatalf("username response = %q", got)
	}
	got, err = auth.Next([]byte("Password:"), true)
	if err != nil {
		t.Fatalf("Next password: %v", err)
	}
	if string(got) != "pass" {
		t.Fatalf("password response = %q", got)
	}
}

func TestPlainAuth_ImplicitTLSFailsWithoutWrapper(t *testing.T) {
	auth := smtp.PlainAuth("", "user", "pass", "smtp.example.com")
	_, _, err := auth.Start(&smtp.ServerInfo{
		Name: "smtp.example.com",
		TLS:  false,
		Auth: []string{"PLAIN"},
	})
	if err == nil {
		t.Fatal("expected stdlib PlainAuth to reject implicit TLS as unencrypted")
	}
}
