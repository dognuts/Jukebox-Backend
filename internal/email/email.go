package email

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"log"
	"net/http"
	"strings"
	"time"
)

type Service struct {
	apiKey          string
	fromEmail       string
	frontendURL     string
	supportReportTo string
	devMode         bool
}

func NewService(apiKey, fromEmail, frontendURL, supportReportTo string) *Service {
	devMode := apiKey == ""
	if devMode {
		log.Println("[email] No RESEND_API_KEY set — running in dev mode (emails logged to console)")
	}
	return &Service{
		apiKey:          apiKey,
		fromEmail:       fromEmail,
		frontendURL:     frontendURL,
		supportReportTo: supportReportTo,
		devMode:         devMode,
	}
}

type resendPayload struct {
	From    string `json:"from"`
	To      []string `json:"to"`
	Subject string `json:"subject"`
	HTML    string `json:"html"`
}

func (s *Service) send(to, subject, html string) error {
	if s.devMode {
		log.Printf("[email][DEV] To: %s | Subject: %s\n%s\n", to, subject, html)
		return nil
	}

	payload := resendPayload{
		From:    s.fromEmail,
		To:      []string{to},
		Subject: subject,
		HTML:    html,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequest("POST", "https://api.resend.com/emails", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("resend request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("resend returned status %d", resp.StatusCode)
	}

	return nil
}

// SendVerificationEmail sends an email with a link to verify the user's email address.
func (s *Service) SendVerificationEmail(to, token string) error {
	link := fmt.Sprintf("%s/verify-email?token=%s", s.frontendURL, token)

	html := fmt.Sprintf(`
		<div style="font-family: -apple-system, sans-serif; max-width: 480px; margin: 0 auto; padding: 40px 20px;">
			<h1 style="font-size: 24px; color: #f5f5f5; margin-bottom: 8px;">Welcome to Jukebox 🎵</h1>
			<p style="color: #a0a0a0; font-size: 15px; line-height: 1.6;">
				Thanks for signing up! Click the button below to verify your email address.
			</p>
			<a href="%s"
				style="display: inline-block; margin: 24px 0; padding: 12px 32px; background: #e89a2e; color: #0a0a0a; 
				font-weight: 600; font-size: 15px; text-decoration: none; border-radius: 8px;">
				Verify Email
			</a>
			<p style="color: #666; font-size: 13px;">
				Or copy this link: <br/>
				<span style="color: #888; word-break: break-all;">%s</span>
			</p>
			<p style="color: #555; font-size: 12px; margin-top: 32px;">
				This link expires in 24 hours. If you didn't sign up for Jukebox, ignore this email.
			</p>
		</div>
	`, link, link)

	return s.send(to, "Verify your Jukebox email", html)
}

// SendPasswordResetEmail sends an email with a password reset link.
func (s *Service) SendPasswordResetEmail(to, token string) error {
	link := fmt.Sprintf("%s/reset-password?token=%s", s.frontendURL, token)

	html := fmt.Sprintf(`
		<div style="font-family: -apple-system, sans-serif; max-width: 480px; margin: 0 auto; padding: 40px 20px;">
			<h1 style="font-size: 24px; color: #f5f5f5; margin-bottom: 8px;">Reset your password</h1>
			<p style="color: #a0a0a0; font-size: 15px; line-height: 1.6;">
				We received a request to reset your Jukebox password. Click the button below to set a new one.
			</p>
			<a href="%s"
				style="display: inline-block; margin: 24px 0; padding: 12px 32px; background: #e89a2e; color: #0a0a0a; 
				font-weight: 600; font-size: 15px; text-decoration: none; border-radius: 8px;">
				Reset Password
			</a>
			<p style="color: #666; font-size: 13px;">
				Or copy this link: <br/>
				<span style="color: #888; word-break: break-all;">%s</span>
			</p>
			<p style="color: #555; font-size: 12px; margin-top: 32px;">
				This link expires in 1 hour. If you didn't request a password reset, ignore this email — your password won't change.
			</p>
		</div>
	`, link, link)

	return s.send(to, "Reset your Jukebox password", html)
}

// ListenerReportContext is the data collected for a single listener support
// submission. Compiled by the handler from request body + session + store
// lookups; passed whole to SendListenerReport.
type ListenerReportContext struct {
	Category            string    // "gated" | "no-audio" | "out-of-sync" | "other"
	Message             string
	ContactEmail        string    // account email if logged in, else the form's email field
	CanContactBack      bool
	UserID              string    // empty string if anonymous
	RoomSlug            string
	RoomName            string
	TrackID             string
	TrackTitle          string
	TrackArtist         string
	PlaybackPositionSec float64
	UserAgent           string
	ClientIP            string
	SubmittedAt         time.Time
}

// SendListenerReport emails the listener's support report to supportReportTo.
// If supportReportTo is empty (ADMIN_EMAIL unset), logs a warning and returns
// nil — same spirit as devMode: don't fail the request, just flag it.
func (s *Service) SendListenerReport(ctx ListenerReportContext) error {
	if s.supportReportTo == "" {
		log.Printf("[email] SUPPORT_REPORT_TO not configured — listener report dropped: %s / %s", ctx.RoomName, ctx.TrackTitle)
		return nil
	}

	subject := fmt.Sprintf("[Jukebox Support] Listener report — %s — %s", ctx.Category, ctx.RoomName)
	htmlBody := composeListenerReportHTML(ctx)
	return s.send(s.supportReportTo, subject, htmlBody)
}

// composeListenerReportHTML builds the HTML body of a listener report email.
// Factored out from SendListenerReport so it can be tested without Resend.
// The user's free-text message is HTML-escaped to avoid injecting into the
// admin's HTML-rendering email client.
func composeListenerReportHTML(ctx ListenerReportContext) string {
	contactBack := "no"
	if ctx.CanContactBack {
		contactBack = "yes"
	}

	rows := []struct{ label, value string }{
		{"Category", ctx.Category},
		{"Submitted at", ctx.SubmittedAt.UTC().Format(time.RFC3339)},
		{"User ID", valueOrDash(ctx.UserID)},
		{"Contact email", ctx.ContactEmail},
		{"Room", fmt.Sprintf("%s (%s)", ctx.RoomName, ctx.RoomSlug)},
		{"Track", fmt.Sprintf("%s — %s", ctx.TrackTitle, ctx.TrackArtist)},
		{"Track ID", valueOrDash(ctx.TrackID)},
		{"Playback position", fmt.Sprintf("%.1fs", ctx.PlaybackPositionSec)},
		{"User agent", valueOrDash(ctx.UserAgent)},
		{"Client IP", valueOrDash(ctx.ClientIP)},
	}

	var tableRows strings.Builder
	for _, r := range rows {
		tableRows.WriteString(fmt.Sprintf(
			`<tr><td style="padding:4px 12px 4px 0;color:#888;font-size:13px;">%s</td><td style="padding:4px 0;color:#f5f5f5;font-size:13px;">%s</td></tr>`,
			html.EscapeString(r.label), html.EscapeString(r.value),
		))
	}

	return fmt.Sprintf(`
		<div style="font-family: -apple-system, sans-serif; max-width: 640px; margin: 0 auto; padding: 32px 20px;">
			<h1 style="font-size: 20px; color: #f5f5f5; margin-bottom: 8px;">Listener support report</h1>
			<p style="color: #a0a0a0; font-size: 13px; margin-bottom: 24px;">
				Submitted via /help/listening. Contact back: %s.
			</p>
			<table style="border-collapse: collapse; margin-bottom: 24px;">
				%s
			</table>
			<blockquote style="margin: 0; padding: 12px 16px; border-left: 3px solid #e89a2e; background: rgba(232,154,46,0.08); color: #e8e6ea; font-size: 14px; white-space: pre-wrap;">%s</blockquote>
		</div>
	`, contactBack, tableRows.String(), html.EscapeString(ctx.Message))
}

func valueOrDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
