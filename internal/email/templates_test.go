package email

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNew_MalformedCustomTemplate_FallsBackWithoutBlockingStart(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, MailKindPasswordReset+".html.tmpl")
	if err := os.WriteFile(bad, []byte(`Hi {{.Name`), 0o644); err != nil {
		t.Fatal(err)
	}

	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	client := New(Config{
		SMTPHost:    "127.0.0.1",
		SMTPPort:    1,
		SMTPTLS:     "none",
		FromAddress: "noreply@sendrec.eu",
		TemplateDir: dir,
	})
	if !client.HasBackend() {
		t.Fatal("expected SMTP backend to remain available after a bad template")
	}

	out := logBuf.String()
	if !strings.Contains(out, "invalid custom email template") {
		t.Errorf("expected startup warning about invalid template, got: %s", out)
	}

	subject, body, err := client.render(MailKindPasswordReset, PasswordResetData{
		Name: "Alice", ResetLink: "https://example.com/reset",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if subject != "Reset your password" {
		t.Errorf("expected built-in subject, got %q", subject)
	}
	if !strings.Contains(body, "Reset password") {
		t.Errorf("expected built-in body, got %q", body)
	}
	if strings.Contains(body, "{{") {
		t.Errorf("malformed custom template leaked into the body: %q", body)
	}
}

func TestNew_UnknownTemplateField_FallsBackWithoutBlockingStart(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, MailKindPasswordReset+".html.tmpl")
	if err := os.WriteFile(bad, []byte(`<p>Hi {{.Nmae}},</p>`), 0o644); err != nil {
		t.Fatal(err)
	}

	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	client := New(Config{
		SMTPHost:    "127.0.0.1",
		SMTPPort:    1,
		SMTPTLS:     "none",
		FromAddress: "noreply@sendrec.eu",
		TemplateDir: dir,
	})
	if !client.HasBackend() {
		t.Fatal("expected SMTP backend to remain available after an unknown template field")
	}

	out := logBuf.String()
	if !strings.Contains(out, "invalid custom email template") {
		t.Errorf("expected startup warning about unknown field, got: %s", out)
	}

	_, body, err := client.render(MailKindPasswordReset, PasswordResetData{
		Name: "Alice", ResetLink: "https://example.com/reset",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(body, "Reset password") {
		t.Errorf("expected built-in body after unknown-field fallback, got %q", body)
	}
	if strings.Contains(body, "{{.Nmae}}") {
		t.Errorf("unknown-field template leaked into the body: %q", body)
	}
}

func TestNew_MissingTemplateDir_FallsBackWithoutBlockingStart(t *testing.T) {
	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	client := New(Config{
		TemplateDir: filepath.Join(t.TempDir(), "does-not-exist"),
	})
	if client.HasBackend() {
		t.Fatal("no backend configured")
	}
	if !strings.Contains(logBuf.String(), "EMAIL_TEMPLATE_DIR is set but cannot be used") {
		t.Errorf("expected missing-dir warning, got: %s", logBuf.String())
	}
}

func TestRender_PartialOverride_SubjectOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, MailKindOrgInvite+".subject.tmpl")
	if err := os.WriteFile(path, []byte("You're invited to {{.OrgName}}"), 0o644); err != nil {
		t.Fatal(err)
	}

	client := New(Config{TemplateDir: dir})
	subject, body, err := client.render(MailKindOrgInvite, OrgInviteData{
		OrgName: "Acme", InviterName: "Bob", AcceptLink: "https://example.com/invite",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if subject != "You're invited to Acme" {
		t.Errorf("expected custom subject, got %q", subject)
	}
	if !strings.Contains(body, "Accept invitation") {
		t.Errorf("expected built-in body when only the subject is overridden, got %q", body)
	}
}

func TestRender_CustomHTMLOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, MailKindPasswordReset+".html.tmpl")
	if err := os.WriteFile(path, []byte(`<p>{{.Name}}</p><a href="{{.ResetLink}}">Set a new password</a>`), 0o644); err != nil {
		t.Fatal(err)
	}

	client := New(Config{TemplateDir: dir})
	subject, body, err := client.render(MailKindPasswordReset, PasswordResetData{
		Name: "Alice", ResetLink: "https://example.com/reset?token=abc",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if subject != "Reset your password" {
		t.Errorf("expected built-in subject, got %q", subject)
	}
	if !strings.Contains(body, "Set a new password") {
		t.Errorf("expected custom body, got %q", body)
	}
	if !strings.Contains(body, "Alice") {
		t.Errorf("expected name in custom body, got %q", body)
	}
}

func TestRender_HTMLEscapesUserContent(t *testing.T) {
	client := New(Config{})
	_, body, err := client.render(MailKindCommentNotification, CommentNotificationData{
		Name:          "Alice",
		VideoTitle:    "<img src=x>",
		CommentAuthor: "<b>Mallory</b>",
		CommentBody:   `<a href="https://evil.example.com">click</a>`,
		WatchURL:      "https://app.sendrec.eu/v/1",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(body, `<a href="https://evil.example.com">`) {
		t.Errorf("comment body reached the mail as markup:\n%s", body)
	}
	if !strings.Contains(body, "&lt;b&gt;Mallory&lt;/b&gt;") {
		t.Errorf("expected escaped author, got:\n%s", body)
	}
}

func TestSendPasswordReset_ListmonkTemplateIDWinsOverFileTemplates(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, MailKindPasswordReset+".html.tmpl"), []byte(`<p>custom</p>`), 0o644); err != nil {
		t.Fatal(err)
	}

	var received txRequest
	var rawPayload []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if handleExistingSubscriber(t, w, r) {
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		rawPayload = body
		if err := json.Unmarshal(body, &received); err != nil {
			t.Fatalf("unmarshal body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := New(Config{
		BaseURL:     srv.URL,
		Username:    "admin",
		Password:    "secret",
		TemplateID:  5,
		TemplateDir: dir,
	})

	if err := client.SendPasswordReset(context.Background(), "alice@example.com", "Alice", "https://example.com/reset"); err != nil {
		t.Fatalf("send: %v", err)
	}
	if received.TemplateID != 5 {
		t.Errorf("expected listmonk template_id=5 to take precedence, got %d", received.TemplateID)
	}
	if !strings.Contains(received.Body, "custom") {
		t.Errorf("expected rendered file body for SMTP/sendmail fallback, got %q", received.Body)
	}
	if strings.Contains(string(rawPayload), `"subject"`) {
		t.Errorf("listmonk payload must not include subject so the listmonk template subject wins; got %s", rawPayload)
	}
}

func TestRender_SubjectDoesNotHTMLEscape(t *testing.T) {
	client := New(Config{})
	subject, _, err := client.render(MailKindOrgInvite, OrgInviteData{
		OrgName: "AT&T", InviterName: "Bob", AcceptLink: "https://example.com/invite",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if subject != "Join AT&T on SendRec" {
		t.Errorf("subject should not HTML-escape, got %q", subject)
	}
}

func TestNew_EmptyCustomTemplate_FallsBackToBuiltin(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, MailKindWelcome+".subject.tmpl"), []byte("   \n"), 0o644); err != nil {
		t.Fatal(err)
	}

	client := New(Config{TemplateDir: dir})
	subject, _, err := client.render(MailKindWelcome, WelcomeData{Name: "Alice", DashboardURL: "https://example.com", GitHubURL: sendrecGitHubURL})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if subject != "Welcome to SendRec" {
		t.Errorf("expected built-in subject for empty override, got %q", subject)
	}
}
