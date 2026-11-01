// Package generator implements the raw SMTP client used for load testing.
//
// Transaction model (persistent profile):
//
//	TCP -> greeting -> EHLO -> STARTTLS -> TLS -> EHLO -> AUTH -> (MAIL/RCPT/DATA)*
//
// Churn profile performs the full sequence per message and disconnects.
package generator

import (
	"bufio"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"strings"
	"time"
)

// smtpConn is a single SMTP session. Not safe for concurrent use;
// the engine guarantees one goroutine owns a persistent connection.
type smtpConn struct {
	c       net.Conn
	r       *bufio.Reader
	w       *bufio.Writer
	tlsUsed bool
}

// dialSMTP performs steps 1-7 of the brief: TCP, greeting, EHLO, STARTTLS,
// TLS negotiation, EHLO again, AUTH. On success the connection is ready for
// MAIL FROM. tlsHandshake is true when a TLS handshake was performed so the
// caller can count smtp_tls_handshakes_total accurately.
func dialSMTP(host string, port int, serverName string, tlsVerify bool, user, pass string, timeout time.Duration) (*smtpConn, bool, error) {
	addr := net.JoinHostPort(host, fmt.Sprint(port))
	raw, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, false, fmt.Errorf("tcp dial: %w", err)
	}
	sc := &smtpConn{c: raw, r: bufio.NewReader(raw), w: bufio.NewWriter(raw)}
	_ = sc.c.SetDeadline(time.Now().Add(timeout))

	if err := sc.expect(220); err != nil {
		sc.Close()
		return nil, false, fmt.Errorf("greeting: %w", err)
	}
	hostname := "load-generator"
	if err := sc.cmd("EHLO "+hostname, 250); err != nil {
		sc.Close()
		return nil, false, fmt.Errorf("ehlo: %w", err)
	}
	// STARTTLS is mandatory in this test plan.
	if err := sc.cmd("STARTTLS", 220); err != nil {
		sc.Close()
		return nil, false, fmt.Errorf("starttls not accepted: %w", err)
	}
	tlsCfg := &tls.Config{ServerName: serverName, InsecureSkipVerify: !tlsVerify} //nolint:gosec
	if serverName == "" {
		tlsCfg.ServerName = host
	}
	tc := tls.Client(raw, tlsCfg)
	if err := tc.Handshake(); err != nil {
		raw.Close()
		return nil, false, fmt.Errorf("tls handshake: %w", err)
	}
	sc.c = tc
	sc.r = bufio.NewReader(tc)
	sc.w = bufio.NewWriter(tc)
	sc.tlsUsed = true
	_ = sc.c.SetDeadline(time.Now().Add(timeout))

	if err := sc.cmd("EHLO "+hostname, 250); err != nil {
		sc.Close()
		return nil, false, fmt.Errorf("ehlo after tls: %w", err)
	}
	if err := sc.auth(user, pass); err != nil {
		sc.Close()
		return nil, true, fmt.Errorf("auth: %w", err)
	}
	return sc, true, nil
}

// sendOne performs steps 8-12: MAIL FROM, RCPT TO, DATA, transmission,
// final acceptance. Caller supplies a fully-formed RFC5322 message.
func (s *smtpConn) sendOne(from string, to string, msg []byte, timeout time.Duration) error {
	_ = s.c.SetDeadline(time.Now().Add(timeout))
	if err := s.cmd("MAIL FROM:<"+from+">", 250); err != nil {
		return fmt.Errorf("mail from: %w", err)
	}
	if err := s.cmd("RCPT TO:<"+to+">", 250, 251); err != nil {
		return fmt.Errorf("rcpt to: %w", err)
	}
	if err := s.cmd("DATA", 354); err != nil {
		return fmt.Errorf("data: %w", err)
	}
	dotStuffed := dotStuff(msg)
	if _, err := s.w.Write(dotStuffed); err != nil {
		return fmt.Errorf("body write: %w", err)
	}
	if _, err := s.w.WriteString("\r\n.\r\n"); err != nil {
		return fmt.Errorf("data terminator: %w", err)
	}
	if err := s.w.Flush(); err != nil {
		return fmt.Errorf("flush: %w", err)
	}
	return s.expect(250)
}

func (s *smtpConn) auth(user, pass string) error {
	if err := s.cmd("AUTH PLAIN "+base64.StdEncoding.EncodeToString([]byte("\x00"+user+"\x00"+pass)), 235); err == nil {
		return nil
	}
	// Fallback to AUTH LOGIN for servers without PLAIN.
	if err := s.cmd("AUTH LOGIN", 334); err != nil {
		return err
	}
	if err := s.cmd(base64.StdEncoding.EncodeToString([]byte(user)), 334); err != nil {
		return err
	}
	return s.cmd(base64.StdEncoding.EncodeToString([]byte(pass)), 235)
}

func (s *smtpConn) cmd(format string, want ...int) error {
	if _, err := s.w.WriteString(format + "\r\n"); err != nil {
		return err
	}
	if err := s.w.Flush(); err != nil {
		return err
	}
	return s.expect(want...)
}

// expect reads a full (possibly multiline) SMTP reply and checks its code.
func (s *smtpConn) expect(want ...int) error {
	for {
		line, err := s.r.ReadString('\n')
		if err != nil {
			return err
		}
		if len(line) < 4 {
			return fmt.Errorf("short reply: %q", strings.TrimSpace(line))
		}
		var code int
		if _, err := fmt.Sscanf(line[:3], "%d", &code); err != nil {
			return fmt.Errorf("bad reply code %q", strings.TrimSpace(line))
		}
		if len(line) > 3 && line[3] == '-' {
			continue // multiline, keep reading
		}
		for _, w := range want {
			if code == w {
				return nil
			}
		}
		return fmt.Errorf("smtp code %d (want %v): %q", code, want, strings.TrimSpace(line))
	}
}

// Quit sends QUIT and closes the socket. Errors are ignored on teardown.
func (s *smtpConn) Quit() {
	_ = s.c.SetDeadline(time.Now().Add(3 * time.Second))
	_, _ = s.w.WriteString("QUIT\r\n")
	_ = s.w.Flush()
	s.Close()
}

// Close hard-closes the socket.
func (s *smtpConn) Close() { _ = s.c.Close() }

// dotStuff escapes leading dots per RFC 5321 section 4.5.2.
func dotStuff(msg []byte) []byte {
	lines := strings.Split(string(msg), "\n")
	for i, l := range lines {
		l = strings.TrimSuffix(l, "\r")
		if strings.HasPrefix(l, ".") {
			lines[i] = "." + l
		} else {
			lines[i] = l
		}
	}
	return []byte(strings.Join(lines, "\r\n"))
}
