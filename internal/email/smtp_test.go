package email

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSMTPServer is a minimal in-memory SMTP server for tests.
// Captures every message it receives and exposes them via Captured().
type fakeSMTPServer struct {
	listener net.Listener
	addr     string

	mu       sync.Mutex
	captured []capturedMessage

	// responses — flip to inject failure cases
	authShouldFail bool

	wg     sync.WaitGroup
	closed chan struct{}
}

type capturedMessage struct {
	from     string
	rcpts    []string
	data     string // raw DATA payload (headers + body)
	authUser string // username from PLAIN auth
}

func newFakeSMTPServer(t *testing.T) *fakeSMTPServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &fakeSMTPServer{
		listener: ln,
		addr:     ln.Addr().String(),
		closed:   make(chan struct{}),
	}
	s.wg.Add(1)
	go s.acceptLoop()
	t.Cleanup(s.Close)
	return s
}

func (s *fakeSMTPServer) Addr() string { return s.addr }

func (s *fakeSMTPServer) Captured() []capturedMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]capturedMessage, len(s.captured))
	copy(out, s.captured)
	return out
}

func (s *fakeSMTPServer) FailAuth() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.authShouldFail = true
}

func (s *fakeSMTPServer) Close() {
	select {
	case <-s.closed:
		return
	default:
	}
	close(s.closed)
	_ = s.listener.Close()
	s.wg.Wait()
}

func (s *fakeSMTPServer) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go func(c net.Conn) {
			defer s.wg.Done()
			defer func() { _ = c.Close() }()
			_ = c.SetDeadline(time.Now().Add(5 * time.Second))
			s.handle(c)
		}(conn)
	}
}

func (s *fakeSMTPServer) handle(c net.Conn) {
	r := bufio.NewReader(c)
	w := bufio.NewWriter(c)
	write := func(line string) {
		_, _ = w.WriteString(line + "\r\n")
		_ = w.Flush()
	}

	write("220 fake.smtp ESMTP")

	msg := capturedMessage{}

	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(strings.ToUpper(line), "EHLO"), strings.HasPrefix(strings.ToUpper(line), "HELO"):
			write("250-fake.smtp")
			write("250-AUTH PLAIN LOGIN")
			write("250 8BITMIME")
		case strings.HasPrefix(strings.ToUpper(line), "AUTH PLAIN"):
			s.mu.Lock()
			fail := s.authShouldFail
			s.mu.Unlock()
			if fail {
				write("535 auth failed")
				continue
			}
			// AUTH PLAIN <base64> — accept any creds in fake unless RequireAuth set
			parts := strings.SplitN(line, " ", 3)
			if len(parts) == 3 {
				msg.authUser = parts[2]
			}
			write("235 ok")
		case strings.HasPrefix(strings.ToUpper(line), "MAIL FROM:"):
			msg.from = strings.TrimSpace(line[len("MAIL FROM:"):])
			write("250 ok")
		case strings.HasPrefix(strings.ToUpper(line), "RCPT TO:"):
			msg.rcpts = append(msg.rcpts, strings.TrimSpace(line[len("RCPT TO:"):]))
			write("250 ok")
		case strings.EqualFold(line, "DATA"):
			write("354 send data")
			var b strings.Builder
			for {
				dl, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if strings.TrimRight(dl, "\r\n") == "." {
					break
				}
				b.WriteString(dl)
			}
			msg.data = b.String()
			write("250 ok")
		case strings.EqualFold(line, "QUIT"):
			write("221 bye")
			s.mu.Lock()
			s.captured = append(s.captured, msg)
			s.mu.Unlock()
			return
		case strings.EqualFold(line, "RSET"):
			msg = capturedMessage{}
			write("250 ok")
		default:
			write("500 unknown command")
		}
	}
}

// --- tests ---

func TestSendTx_SMTP_Success(t *testing.T) {
	s := newFakeSMTPServer(t)
	host, port := splitHostPort(t, s.Addr())

	client := New(Config{
		SMTPHost:     host,
		SMTPPort:     port,
		SMTPUsername: "user",
		SMTPPassword: "pass",
		SMTPTLS:      "none",
		FromAddress:  "noreply@sendrec.eu",
	})

	err := client.SendConfirmation(context.Background(), "alice@example.com", "Alice", "https://app.sendrec.eu/confirm?token=abc")
	if err != nil {
		t.Fatalf("SendConfirmation: %v", err)
	}

	msgs := waitForMessages(t, s, 1)
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	got := msgs[0]
	if !strings.Contains(got.from, "noreply@sendrec.eu") {
		t.Errorf("unexpected from: %q", got.from)
	}
	if len(got.rcpts) != 1 || !strings.Contains(got.rcpts[0], "alice@example.com") {
		t.Errorf("unexpected rcpts: %v", got.rcpts)
	}
	if !strings.Contains(got.data, "Subject: Confirm your email") {
		t.Errorf("missing subject header in data: %q", got.data)
	}
	if !strings.Contains(got.data, "https://app.sendrec.eu/confirm?token=abc") {
		t.Errorf("missing confirm link in body: %q", got.data)
	}
}

func TestSendTx_SMTP_FromNameInHeaderNotEnvelope(t *testing.T) {
	s := newFakeSMTPServer(t)
	host, port := splitHostPort(t, s.Addr())

	client := New(Config{
		SMTPHost:    host,
		SMTPPort:    port,
		SMTPTLS:     "none",
		FromAddress: "noreply@sendrec.eu",
		FromName:    "Björn & Co",
	})

	if err := client.SendConfirmation(context.Background(), "alice@example.com", "Alice", "https://app.sendrec.eu/confirm?token=abc"); err != nil {
		t.Fatalf("SendConfirmation: %v", err)
	}

	msgs := waitForMessages(t, s, 1)
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	got := msgs[0]
	if !strings.Contains(got.from, "noreply@sendrec.eu") {
		t.Errorf("envelope sender must stay the bare address, got %q", got.from)
	}
	if strings.Contains(got.from, "Björn") || strings.Contains(got.from, "Co") {
		t.Errorf("display name leaked into MAIL FROM: %q", got.from)
	}
	if !strings.Contains(got.data, "From: ") {
		t.Errorf("missing From header: %q", got.data)
	}
	if !strings.Contains(got.data, "noreply@sendrec.eu") {
		t.Errorf("From header missing address: %q", got.data)
	}
	if !strings.Contains(got.data, "=?utf-8?") && !strings.Contains(got.data, "Björn") {
		t.Errorf("expected RFC 2047-encoded or raw display name in From header: %q", got.data)
	}
	if strings.Contains(got.data, "From: noreply@sendrec.eu\r\n") {
		t.Errorf("display name was dropped from From header: %q", got.data)
	}
}

func TestSendTx_SMTP_RejectsCRLFInFromName(t *testing.T) {
	s := newFakeSMTPServer(t)
	host, port := splitHostPort(t, s.Addr())

	client := New(Config{
		SMTPHost:    host,
		SMTPPort:    port,
		SMTPTLS:     "none",
		FromAddress: "noreply@sendrec.eu",
		FromName:    "SendRec\r\nBcc: attacker@evil.com",
	})

	err := client.SendConfirmation(context.Background(), "alice@example.com", "Alice", "https://example.com/confirm")
	if err == nil {
		t.Fatal("expected CRLF rejection on From name, got nil")
	}
	if !strings.Contains(err.Error(), "CR/LF") {
		t.Errorf("expected CR/LF rejection error, got: %v", err)
	}
	if got := s.Captured(); len(got) != 0 {
		t.Errorf("expected no SMTP delivery on CRLF reject, got: %+v", got)
	}
}

func TestSendTx_SMTP_QuotedFromName(t *testing.T) {
	s := newFakeSMTPServer(t)
	host, port := splitHostPort(t, s.Addr())

	client := New(Config{
		SMTPHost:    host,
		SMTPPort:    port,
		SMTPTLS:     "none",
		FromAddress: "noreply@sendrec.eu",
		FromName:    "Acme, Inc",
	})

	if err := client.SendConfirmation(context.Background(), "alice@example.com", "Alice", "https://example.com/confirm"); err != nil {
		t.Fatalf("send: %v", err)
	}
	msgs := waitForMessages(t, s, 1)
	if !strings.Contains(msgs[0].data, `"Acme, Inc"`) {
		t.Errorf("expected quoted display name, got: %q", msgs[0].data)
	}
}

func TestSendTx_SMTP_CustomTemplateSubjectAndBody(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, MailKindEmailConfirmation+".subject.tmpl"), []byte("Please confirm, {{.Name}}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, MailKindEmailConfirmation+".html.tmpl"), []byte(`<p><a href="{{.ConfirmLink}}">Verify now</a></p>`), 0o644); err != nil {
		t.Fatal(err)
	}

	s := newFakeSMTPServer(t)
	host, port := splitHostPort(t, s.Addr())
	client := New(Config{
		SMTPHost:    host,
		SMTPPort:    port,
		SMTPTLS:     "none",
		FromAddress: "noreply@sendrec.eu",
		TemplateDir: dir,
	})

	if err := client.SendConfirmation(context.Background(), "alice@example.com", "Alice", "https://app.sendrec.eu/confirm?token=abc"); err != nil {
		t.Fatalf("send: %v", err)
	}
	msgs := waitForMessages(t, s, 1)
	if !strings.Contains(msgs[0].data, "Subject: Please confirm, Alice") {
		t.Errorf("missing custom subject: %q", msgs[0].data)
	}
	if !strings.Contains(msgs[0].data, "Verify now") {
		t.Errorf("missing custom body: %q", msgs[0].data)
	}
}

func TestSendTx_SMTP_AuthFailure_ReturnsError(t *testing.T) {
	s := newFakeSMTPServer(t)
	s.FailAuth()
	host, port := splitHostPort(t, s.Addr())

	client := New(Config{
		SMTPHost:     host,
		SMTPPort:     port,
		SMTPUsername: "user",
		SMTPPassword: "wrong",
		SMTPTLS:      "none",
		FromAddress:  "noreply@sendrec.eu",
	})

	err := client.SendConfirmation(context.Background(), "alice@example.com", "Alice", "https://example.com/confirm")
	if err == nil {
		t.Fatalf("expected SMTP auth error, got nil")
	}
}

func TestSendTx_PrefersListmonkOverSMTP(t *testing.T) {
	listmonkHit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/api/tx") {
			listmonkHit = true
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	smtpSrv := newFakeSMTPServer(t)
	host, port := splitHostPort(t, smtpSrv.Addr())

	client := New(Config{
		BaseURL:           srv.URL,
		Username:          "admin",
		Password:          "secret",
		ConfirmTemplateID: 7,
		SMTPHost:          host,
		SMTPPort:          port,
		SMTPUsername:      "u",
		SMTPPassword:      "p",
		SMTPTLS:           "none",
		FromAddress:       "noreply@sendrec.eu",
	})

	if err := client.SendConfirmation(context.Background(), "alice@example.com", "Alice", "https://example.com/confirm"); err != nil {
		t.Fatalf("send: %v", err)
	}

	if !listmonkHit {
		t.Error("expected listmonk to be hit when both backends configured")
	}
	if got := smtpSrv.Captured(); len(got) != 0 {
		t.Errorf("SMTP unexpectedly received message: %+v", got)
	}
}

func TestSendTx_SMTP_ConnectionError_ReturnsError(t *testing.T) {
	client := New(Config{
		SMTPHost:    "127.0.0.1",
		SMTPPort:    1, // closed port
		SMTPTLS:     "none",
		FromAddress: "noreply@sendrec.eu",
	})

	err := client.SendPasswordReset(context.Background(), "alice@example.com", "Alice", "https://example.com/reset")
	if err == nil {
		t.Fatal("expected SMTP connection error, got nil")
	}
}

// fakeNoSTARTTLSServer behaves like fakeSMTPServer but does not advertise STARTTLS in EHLO.
type fakeNoSTARTTLSServer struct {
	listener net.Listener
	addr     string
	wg       sync.WaitGroup
	closed   chan struct{}
}

func newFakeNoSTARTTLSServer(t *testing.T) *fakeNoSTARTTLSServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &fakeNoSTARTTLSServer{listener: ln, addr: ln.Addr().String(), closed: make(chan struct{})}
	s.wg.Add(1)
	go s.acceptLoop()
	t.Cleanup(s.Close)
	return s
}

func (s *fakeNoSTARTTLSServer) Addr() string { return s.addr }

func (s *fakeNoSTARTTLSServer) Close() {
	select {
	case <-s.closed:
		return
	default:
	}
	close(s.closed)
	_ = s.listener.Close()
	s.wg.Wait()
}

func (s *fakeNoSTARTTLSServer) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go func(c net.Conn) {
			defer s.wg.Done()
			defer func() { _ = c.Close() }()
			_ = c.SetDeadline(time.Now().Add(5 * time.Second))
			r := bufio.NewReader(c)
			w := bufio.NewWriter(c)
			write := func(line string) {
				_, _ = w.WriteString(line + "\r\n")
				_ = w.Flush()
			}
			write("220 fake.smtp ESMTP")
			for {
				line, err := r.ReadString('\n')
				if err != nil {
					return
				}
				line = strings.TrimRight(line, "\r\n")
				up := strings.ToUpper(line)
				switch {
				case strings.HasPrefix(up, "EHLO"), strings.HasPrefix(up, "HELO"):
					write("250-fake.smtp")
					write("250 8BITMIME") // intentionally NO STARTTLS
				case strings.EqualFold(line, "QUIT"):
					write("221 bye")
					return
				default:
					write("500 not supported in this fake")
				}
			}
		}(conn)
	}
}

func TestSendTx_SMTP_StartTLSRequired_FailsIfServerDoesNotOffer(t *testing.T) {
	s := newFakeNoSTARTTLSServer(t)
	host, port := splitHostPort(t, s.Addr())

	client := New(Config{
		SMTPHost:    host,
		SMTPPort:    port,
		SMTPTLS:     "starttls",
		FromAddress: "noreply@sendrec.eu",
	})

	err := client.SendPasswordReset(context.Background(), "alice@example.com", "Alice", "https://example.com/reset")
	if err == nil {
		t.Fatal("expected error when STARTTLS required but server does not offer it")
	}
	if !strings.Contains(err.Error(), "STARTTLS") {
		t.Errorf("expected STARTTLS error message, got: %v", err)
	}
}

func TestSendTx_SMTP_StartTLSDefaultIsRequired(t *testing.T) {
	// Empty SMTPTLS should default to "starttls" (S1 fix).
	s := newFakeNoSTARTTLSServer(t)
	host, port := splitHostPort(t, s.Addr())

	client := New(Config{
		SMTPHost: host,
		SMTPPort: port,
		// SMTPTLS intentionally unset
		FromAddress: "noreply@sendrec.eu",
	})

	err := client.SendPasswordReset(context.Background(), "alice@example.com", "Alice", "https://example.com/reset")
	if err == nil {
		t.Fatal("expected default mode to require STARTTLS and error out when server lacks it")
	}
}

func TestSendTx_SMTP_StalledServer_ContextCancelUnblocks(t *testing.T) {
	// Fake server accepts but never speaks SMTP — caller must not hang.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// Hold the connection open without writing the 220 banner.
			go func(c net.Conn) {
				time.Sleep(2 * time.Second)
				_ = c.Close()
			}(c)
		}
	}()

	host, port := splitHostPort(t, ln.Addr().String())
	client := New(Config{
		SMTPHost:    host,
		SMTPPort:    port,
		SMTPTLS:     "none",
		FromAddress: "noreply@sendrec.eu",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	err = client.SendPasswordReset(ctx, "alice@example.com", "Alice", "https://example.com/reset")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error from stalled SMTP server, got nil")
	}
	if elapsed > 1500*time.Millisecond {
		t.Errorf("call took %v, expected <1.5s — context cancellation not honored", elapsed)
	}
}

func TestSendTx_SMTP_RejectsCRLFInSubject(t *testing.T) {
	s := newFakeSMTPServer(t)
	host, port := splitHostPort(t, s.Addr())

	client := New(Config{
		SMTPHost:    host,
		SMTPPort:    port,
		SMTPTLS:     "none",
		FromAddress: "noreply@sendrec.eu",
	})

	err := client.SendOrgInvite(context.Background(),
		"alice@example.com",
		"Evil Org\r\nBcc: attacker@evil.com",
		"Mallory",
		"https://example.com/invite/abc")
	if err == nil {
		t.Fatal("expected CRLF rejection, got nil")
	}
	if !strings.Contains(err.Error(), "CR/LF") {
		t.Errorf("expected CR/LF rejection error, got: %v", err)
	}
	if got := s.Captured(); len(got) != 0 {
		t.Errorf("expected no SMTP delivery on CRLF reject, got: %+v", got)
	}
}

func TestSendTx_SMTP_RejectsCRLFInFrom(t *testing.T) {
	s := newFakeSMTPServer(t)
	host, port := splitHostPort(t, s.Addr())

	client := New(Config{
		SMTPHost:    host,
		SMTPPort:    port,
		SMTPTLS:     "none",
		FromAddress: "noreply@sendrec.eu\r\nBcc: attacker@evil.com",
	})

	err := client.SendConfirmation(context.Background(), "alice@example.com", "Alice", "https://example.com/confirm")
	if err == nil {
		t.Fatal("expected CRLF rejection on From, got nil")
	}
}

func TestNormalizeSMTPTLS(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"", "starttls"},
		{"  ", "starttls"},
		{"starttls", "starttls"},
		{"STARTTLS", "starttls"},
		{" StartTLS ", "starttls"},
		{"tls", "tls"},
		{"auto", "auto"},
		{"none", "none"},
		{"start_tls", "starttls"}, // typo -> safe default
		{"starttsl", "starttls"},  // typo -> safe default
		{"plain", "starttls"},     // unsupported -> safe default
		{"insecure", "starttls"},  // unsupported -> safe default
	}
	for _, tt := range tests {
		if got := normalizeSMTPTLS(tt.in); got != tt.want {
			t.Errorf("normalizeSMTPTLS(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestSendTx_SMTP_UnknownTLSMode_DoesNotPlainTextLeak(t *testing.T) {
	// Server only advertises STARTTLS; client must require it because typo
	// values normalize to "starttls", not silently fall through to plaintext.
	s := newFakeNoSTARTTLSServer(t)
	host, port := splitHostPort(t, s.Addr())

	client := New(Config{
		SMTPHost:    host,
		SMTPPort:    port,
		SMTPTLS:     "start_tls", // typo
		FromAddress: "noreply@sendrec.eu",
	})

	err := client.SendPasswordReset(context.Background(), "alice@example.com", "Alice", "https://example.com/reset")
	if err == nil {
		t.Fatal("expected typo SMTP_TLS to be normalized to starttls and fail when server lacks it")
	}
	if !strings.Contains(err.Error(), "STARTTLS") {
		t.Errorf("expected STARTTLS error, got: %v", err)
	}
}

func TestNew_SendmailEnabledButMissingBinary_DoesNotCountAsBackend(t *testing.T) {
	// PATH cleared so exec.LookPath("sendmail") fails inside New().
	t.Setenv("PATH", "")

	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	c := New(Config{SendmailEnabled: true})
	if c.HasBackend() {
		t.Error("HasBackend() must be false when sendmail is enabled but binary is missing")
	}

	out := logBuf.String()
	if !strings.Contains(out, "email backend: none") {
		t.Errorf("expected startup log to say 'email backend: none', got: %s", out)
	}
	if strings.Contains(out, "email backend: sendmail") {
		t.Errorf("startup log incorrectly claims sendmail backend when binary is missing: %s", out)
	}
}

func TestHasBackend(t *testing.T) {
	tests := []struct {
		name   string
		config Config
		want   bool
	}{
		{"no backend", Config{}, false},
		{"listmonk only", Config{BaseURL: "https://listmonk.example"}, true},
		{"smtp only", Config{SMTPHost: "smtp.example", SMTPPort: 587}, true},
		// "sendmail only" depends on exec.LookPath at runtime — covered separately by
		// TestNew_SendmailEnabledButMissingBinary_DoesNotCountAsBackend and a positive
		// case set via the unexported field below.
		{"both listmonk and smtp", Config{BaseURL: "https://listmonk.example", SMTPHost: "smtp.example"}, true},
		{"smtp host alone", Config{SMTPHost: "smtp.example"}, true}, // SMTPPort defaults inside sendViaSMTP
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := New(tt.config)
			if got := c.HasBackend(); got != tt.want {
				t.Errorf("HasBackend() = %v, want %v", got, tt.want)
			}
		})
	}

	// Positive sendmail case: simulate the binary being available regardless of host.
	t.Run("sendmail enabled and available", func(t *testing.T) {
		c := New(Config{SendmailEnabled: true})
		c.sendmailAvailable = true
		if !c.HasBackend() {
			t.Error("expected HasBackend()=true when SendmailEnabled and sendmail is on PATH")
		}
	})
}

func splitHostPort(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}
	var port int
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		t.Fatalf("port parse: %v", err)
	}
	return host, port
}

func waitForMessages(t *testing.T, s *fakeSMTPServer, n int) []capturedMessage {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		msgs := s.Captured()
		if len(msgs) >= n {
			return msgs
		}
		time.Sleep(10 * time.Millisecond)
	}
	return s.Captured()
}

// Names, titles and comment bodies come from users. They land in an HTML mail
// body, so markup in them must arrive as text rather than as markup.
func TestSendTx_SMTP_EscapesUserContentInBody(t *testing.T) {
	cases := []struct {
		name     string
		send     func(*Client) error
		injected string
	}{
		{
			name: "comment author and body",
			send: func(c *Client) error {
				return c.SendCommentNotification(context.Background(), "alice@example.com", "Alice",
					"<img src=x onerror=alert(1)>", "<b>Mallory</b>",
					`<a href="https://evil.example.com">click</a>`, "https://app.sendrec.eu/v/1")
			},
			injected: `<a href="https://evil.example.com">`,
		},
		{
			name: "recipient name",
			send: func(c *Client) error {
				return c.SendPasswordReset(context.Background(), "alice@example.com",
					`<script>alert(1)</script>`, "https://app.sendrec.eu/reset?token=abc")
			},
			injected: "<script>",
		},
		{
			name: "workspace and inviter names",
			send: func(c *Client) error {
				return c.SendOrgInvite(context.Background(), "alice@example.com",
					`<b>Acme</b>`, `<a href="https://evil.example.com">Bob</a>`,
					"https://app.sendrec.eu/invites/accept?token=abc")
			},
			injected: `<a href="https://evil.example.com">`,
		},
		{
			name: "video title in view notification",
			send: func(c *Client) error {
				return c.SendViewNotification(context.Background(), "alice@example.com", "Alice",
					`<iframe src="https://evil.example.com"></iframe>`, "https://app.sendrec.eu/v/1", 3)
			},
			injected: "<iframe",
		},
		{
			name: "video titles in retention warning",
			send: func(c *Client) error {
				return c.SendRetentionWarning(context.Background(), "alice@example.com",
					[]RetentionVideoSummary{{Title: `<script>alert(1)</script>`, WatchURL: "https://app.sendrec.eu/v/1"}},
					"2026-10-01")
			},
			injected: "<script>",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newFakeSMTPServer(t)
			host, port := splitHostPort(t, s.Addr())
			client := New(Config{
				SMTPHost: host, SMTPPort: port, SMTPTLS: "none", FromAddress: "noreply@sendrec.eu",
			})

			if err := tc.send(client); err != nil {
				t.Fatalf("send: %v", err)
			}

			msgs := waitForMessages(t, s, 1)
			if strings.Contains(msgs[0].data, tc.injected) {
				t.Errorf("user content reached the body as markup (%q):\n%s", tc.injected, msgs[0].data)
			}
		})
	}
}

// Links SendRec builds itself carry query separators, which belong in an href
// as entities so mail clients follow the whole URL.
func TestSendTx_SMTP_EscapesAmpersandInLinks(t *testing.T) {
	s := newFakeSMTPServer(t)
	host, port := splitHostPort(t, s.Addr())
	client := New(Config{
		SMTPHost: host, SMTPPort: port, SMTPTLS: "none", FromAddress: "noreply@sendrec.eu",
	})

	link := "https://app.sendrec.eu/confirm-email?token=abc&redirect=%2Finvites%2Faccept"
	if err := client.SendConfirmation(context.Background(), "alice@example.com", "Alice", link); err != nil {
		t.Fatalf("SendConfirmation: %v", err)
	}

	msgs := waitForMessages(t, s, 1)
	if !strings.Contains(msgs[0].data, "token=abc&amp;redirect=") {
		t.Errorf("expected the link's ampersand escaped in href, got:\n%s", msgs[0].data)
	}
}
