package antispam

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const turnstileVerifyURL = "https://challenges.cloudflare.com/turnstile/v0/siteverify"

type TurnstileResponse struct {
	Success    bool     `json:"success"`
	ErrorCodes []string `json:"error-codes"`
	Challenge  string   `json:"challenge_ts"`
	Hostname   string   `json:"hostname"`
}

// VerifyTurnstile validates a Cloudflare Turnstile token server-side.
// Returns nil if valid, an error otherwise.
// If secretKey is empty, verification is skipped (for local dev).
func VerifyTurnstile(ctx context.Context, secretKey, token, remoteIP string) error {
	if secretKey == "" {
		// No secret configured — skip verification (development mode)
		return nil
	}
	if token == "" {
		return fmt.Errorf("captcha verification required")
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	form := url.Values{
		"secret":   {secretKey},
		"response": {token},
	}
	if remoteIP != "" {
		form.Set("remoteip", remoteIP)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", turnstileVerifyURL, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("captcha verification failed: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("captcha verification failed: %w", err)
	}
	defer resp.Body.Close()

	var result TurnstileResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("captcha verification failed: %w", err)
	}

	if !result.Success {
		return fmt.Errorf("captcha verification failed: %v", result.ErrorCodes)
	}

	return nil
}
