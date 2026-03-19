package email

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
)

type Service struct {
	apiKey      string
	fromEmail   string
	frontendURL string
	devMode     bool
}

func NewService(apiKey, fromEmail, frontendURL string) *Service {
	devMode := apiKey == ""
	if devMode {
		log.Println("[email] No RESEND_API_KEY set — running in dev mode (emails logged to console)")
	}
	return &Service{
		apiKey:      apiKey,
		fromEmail:   fromEmail,
		frontendURL: frontendURL,
		devMode:     devMode,
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
