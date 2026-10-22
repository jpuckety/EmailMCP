package smtp

import (
	"errors"
	"fmt"
	"net/smtp"
	"strings"
)

// NewAuth returns an smtp.Auth that works for STARTTLS and implicit TLS (port 465).
//
// net/smtp.PlainAuth refuses to send credentials unless ServerInfo.TLS is true.
// That flag is only set after STARTTLS, not when the TCP connection is already
// TLS-wrapped, so AUTH PLAIN on port 465 would fail with "unencrypted connection"
// even though the session is encrypted. implicitTLS treats the session as TLS.
//
// When the server advertises LOGIN but not PLAIN, LOGIN is used instead.
func NewAuth(username, password, host string, implicitTLS bool) smtp.Auth {
	return &smtpAuth{
		username:    username,
		password:    password,
		host:        host,
		implicitTLS: implicitTLS,
	}
}

type smtpAuth struct {
	username, password, host string
	implicitTLS              bool
	inner                    smtp.Auth
}

func (a *smtpAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	if server == nil {
		return "", nil, errors.New("missing SMTP server info")
	}
	info := *server
	if a.implicitTLS {
		info.TLS = true
	}
	if !info.TLS && !isLocalhost(info.Name) {
		return "", nil, errors.New("unencrypted connection")
	}
	a.inner = pickAuth(info.Auth, a.username, a.password, a.host)
	return a.inner.Start(&info)
}

func (a *smtpAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if a.inner == nil {
		return nil, errors.New("smtp auth not started")
	}
	return a.inner.Next(fromServer, more)
}

func pickAuth(mechs []string, username, password, host string) smtp.Auth {
	hasPLAIN, hasLOGIN := false, false
	for _, mech := range mechs {
		switch strings.ToUpper(mech) {
		case "PLAIN":
			hasPLAIN = true
		case "LOGIN":
			hasLOGIN = true
		}
	}
	if hasLOGIN && !hasPLAIN {
		return loginAuth{username: username, password: password}
	}
	return smtp.PlainAuth("", username, password, host)
}

type loginAuth struct {
	username, password string
}

func (a loginAuth) Start(_ *smtp.ServerInfo) (string, []byte, error) {
	return "LOGIN", nil, nil
}

func (a loginAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	switch strings.ToLower(strings.TrimSpace(string(fromServer))) {
	case "username:":
		return []byte(a.username), nil
	case "password:":
		return []byte(a.password), nil
	default:
		return nil, fmt.Errorf("unexpected LOGIN challenge: %q", fromServer)
	}
}

func isLocalhost(name string) bool {
	return name == "localhost" || name == "127.0.0.1" || name == "::1"
}
