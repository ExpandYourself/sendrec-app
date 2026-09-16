package email

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	htmltemplate "html/template"
	texttemplate "text/template"
)

// File stems under EMAIL_TEMPLATE_DIR. Each type may supply
// <stem>.subject.tmpl and/or <stem>.html.tmpl; a missing file falls back to
// the built-in for that part only.
const (
	MailKindPasswordReset       = "password-reset"
	MailKindCommentNotification = "comment-notification"
	MailKindViewNotification    = "view-notification"
	MailKindEmailConfirmation   = "email-confirmation"
	MailKindWelcome             = "welcome"
	MailKindOnboardingDay2      = "onboarding-day2"
	MailKindOnboardingDay7      = "onboarding-day7"
	MailKindWeeklyDigest        = "weekly-digest"
	MailKindOrgInvite           = "org-invite"
	MailKindRetentionWarning    = "retention-warning"
)

var mailKinds = []string{
	MailKindPasswordReset,
	MailKindCommentNotification,
	MailKindViewNotification,
	MailKindEmailConfirmation,
	MailKindWelcome,
	MailKindOnboardingDay2,
	MailKindOnboardingDay7,
	MailKindWeeklyDigest,
	MailKindOrgInvite,
	MailKindRetentionWarning,
}

const sendrecGitHubURL = "https://github.com/sendrec/sendrec"

type mailTemplates struct {
	subject *texttemplate.Template
	html    *htmltemplate.Template
}

// PasswordResetData is the template data for password-reset emails.
type PasswordResetData struct {
	Name      string
	ResetLink string
}

// CommentNotificationData is the template data for new-comment emails.
type CommentNotificationData struct {
	Name          string
	VideoTitle    string
	CommentAuthor string
	CommentBody   string
	WatchURL      string
}

// ViewNotificationData is the template data for single-video view emails.
type ViewNotificationData struct {
	Name       string
	VideoTitle string
	WatchURL   string
	ViewCount  int
}

// EmailConfirmationData is the template data for signup confirmation emails.
type EmailConfirmationData struct {
	Name        string
	ConfirmLink string
}

// WelcomeData is the template data for the post-confirmation welcome email.
type WelcomeData struct {
	Name         string
	DashboardURL string
	GitHubURL    string
}

// OnboardingDay2Data is the template data for the day-two onboarding email.
type OnboardingDay2Data struct {
	Name         string
	DashboardURL string
}

// OnboardingDay7Data is the template data for the day-seven onboarding email.
type OnboardingDay7Data struct {
	Name         string
	DashboardURL string
}

// WeeklyDigestData is the template data for the weekly digest email.
type WeeklyDigestData struct {
	Name          string
	TotalViews    int
	TotalComments int
	Videos        []DigestVideoSummary
}

// OrgInviteData is the template data for workspace invitation emails.
type OrgInviteData struct {
	OrgName     string
	InviterName string
	AcceptLink  string
}

// RetentionWarningData is the template data for retention-warning emails.
type RetentionWarningData struct {
	Videos     []RetentionVideoSummary
	ExpiryDate string
}

var builtinSubjects = map[string]string{
	MailKindPasswordReset:       "Reset your password",
	MailKindCommentNotification: "New comment on your video",
	MailKindViewNotification:    "Your video was viewed",
	MailKindEmailConfirmation:   "Confirm your email",
	MailKindWelcome:             "Welcome to SendRec",
	MailKindOnboardingDay2:      "Ready to share your first video?",
	MailKindOnboardingDay7:      "Unlock more with SendRec Pro",
	MailKindWeeklyDigest:        "Your weekly video digest",
	MailKindOrgInvite:           "Join {{.OrgName}} on SendRec",
	MailKindRetentionWarning:    "Videos scheduled for deletion",
}

var builtinHTML = map[string]string{
	MailKindPasswordReset:       `<p>Hi {{.Name}},</p><p>Click the link below to reset your password:</p><p><a href="{{.ResetLink}}">Reset password</a></p>`,
	MailKindCommentNotification: `<p>Hi {{.Name}},</p><p><strong>{{.CommentAuthor}}</strong> commented on your video <strong>{{.VideoTitle}}</strong>:</p><blockquote>{{.CommentBody}}</blockquote><p><a href="{{.WatchURL}}">View video</a></p>`,
	MailKindViewNotification:    `<p>Hi {{.Name}},</p><p>Your video <strong>{{.VideoTitle}}</strong> has been viewed {{.ViewCount}} time(s).</p><p><a href="{{.WatchURL}}">View video</a></p>`,
	MailKindEmailConfirmation:   `<p>Hi {{.Name}},</p><p>Please confirm your email address by clicking the link below:</p><p><a href="{{.ConfirmLink}}">Confirm email</a></p>`,
	MailKindWelcome: `<p>Hi {{.Name}},</p><p>Welcome to SendRec! Your account is ready.</p><p><a href="{{.DashboardURL}}">Go to dashboard</a></p>` +
		`<p style="margin-top:16px;font-size:13px;color:#64748b;">SendRec is open source. If you find it useful, <a href="{{.GitHubURL}}">star us on GitHub</a>!</p>`,
	MailKindOnboardingDay2:   `<p>Hi {{.Name}},</p><p>Ready to share your first video? Record and share in seconds.</p><p><a href="{{.DashboardURL}}">Get started</a></p>`,
	MailKindOnboardingDay7:   `<p>Hi {{.Name}},</p><p>Unlock more with SendRec Pro — longer recordings, custom branding, and more.</p><p><a href="{{.DashboardURL}}">Learn more</a></p>`,
	MailKindWeeklyDigest:     `<p>Hi {{.Name}},</p><p>Your videos received {{.TotalViews}} view(s) and {{.TotalComments}} comment(s) this week.</p>`,
	MailKindOrgInvite:        `<p>Hi,</p><p><strong>{{.InviterName}}</strong> has invited you to join <strong>{{.OrgName}}</strong> on SendRec.</p><p><a href="{{.AcceptLink}}">Accept invitation</a></p>`,
	MailKindRetentionWarning: `<p>Hi,</p><p>The following videos will be deleted on <strong>{{.ExpiryDate}}</strong>: {{range $i, $v := .Videos}}{{if $i}}, {{end}}{{$v.Title}}{{end}}.</p><p>Upgrade your plan to keep them.</p>`,
}

func mustText(name, src string) *texttemplate.Template {
	return texttemplate.Must(texttemplate.New(name).Parse(src))
}

func mustHTML(name, src string) *htmltemplate.Template {
	return htmltemplate.Must(htmltemplate.New(name).Parse(src))
}

func builtinMail(kind string) mailTemplates {
	return mailTemplates{
		subject: mustText(kind+".subject", builtinSubjects[kind]),
		html:    mustHTML(kind+".html", builtinHTML[kind]),
	}
}

func (c *Client) loadTemplates() {
	c.templates = make(map[string]mailTemplates, len(mailKinds))
	for _, kind := range mailKinds {
		c.templates[kind] = builtinMail(kind)
	}

	dir := strings.TrimSpace(c.config.TemplateDir)
	c.config.TemplateDir = dir
	if dir == "" {
		return
	}

	info, err := os.Stat(dir)
	if err != nil {
		slog.Warn("EMAIL_TEMPLATE_DIR is set but cannot be used; using built-in templates", "dir", dir, "error", err)
		return
	}
	if !info.IsDir() {
		slog.Warn("EMAIL_TEMPLATE_DIR is not a directory; using built-in templates", "dir", dir)
		return
	}

	overrides := 0
	for _, kind := range mailKinds {
		current := c.templates[kind]
		if parsed, ok := loadCustomText(dir, kind+".subject.tmpl"); ok {
			current.subject = parsed
			overrides++
		}
		if parsed, ok := loadCustomHTML(dir, kind+".html.tmpl"); ok {
			current.html = parsed
			overrides++
		}
		c.templates[kind] = current
	}
	if overrides > 0 {
		slog.Info("custom email templates loaded", "dir", dir, "overrides", overrides)
	}
}

func loadCustomText(dir, filename string) (*texttemplate.Template, bool) {
	path := filepath.Join(dir, filename)
	raw, ok := readTemplateFile(path)
	if !ok {
		return nil, false
	}
	tmpl, err := texttemplate.New(filename).Parse(raw)
	if err != nil {
		slog.Warn("invalid custom email template; using built-in", "file", path, "error", err)
		return nil, false
	}
	return tmpl, true
}

func loadCustomHTML(dir, filename string) (*htmltemplate.Template, bool) {
	path := filepath.Join(dir, filename)
	raw, ok := readTemplateFile(path)
	if !ok {
		return nil, false
	}
	tmpl, err := htmltemplate.New(filename).Parse(raw)
	if err != nil {
		slog.Warn("invalid custom email template; using built-in", "file", path, "error", err)
		return nil, false
	}
	return tmpl, true
}

func readTemplateFile(path string) (string, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("could not read custom email template; using built-in", "file", path, "error", err)
		}
		return "", false
	}
	src := string(raw)
	if strings.TrimSpace(src) == "" {
		slog.Warn("custom email template is empty; using built-in", "file", path)
		return "", false
	}
	return src, true
}

func (c *Client) render(kind string, data any) (subject, body string, err error) {
	t, ok := c.templates[kind]
	if !ok {
		return "", "", fmt.Errorf("unknown email template %q", kind)
	}

	var subjBuf bytes.Buffer
	if err := t.subject.Execute(&subjBuf, data); err != nil {
		return "", "", fmt.Errorf("render %s subject: %w", kind, err)
	}
	var bodyBuf bytes.Buffer
	if err := t.html.Execute(&bodyBuf, data); err != nil {
		return "", "", fmt.Errorf("render %s body: %w", kind, err)
	}

	subject = strings.TrimRight(subjBuf.String(), "\r\n")
	return subject, bodyBuf.String(), nil
}
