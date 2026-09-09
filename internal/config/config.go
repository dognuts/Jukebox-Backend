package config

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

type Config struct {
	Port          string
	Env           string
	DatabaseURL   string
	RedisURL      string
	SessionTTL    time.Duration
	CORSOrigins   []string
	JWTSecret     string
	ResendAPIKey  string
	FromEmail     string
	FrontendURL   string
	AdminEmail    string
	StripeSecretKey    string
	StripeWebhookSecret string
	StripePlusPriceID  string
	LiveKitURL       string
	LiveKitAPIKey    string
	LiveKitAPISecret string
	TurnstileSecretKey string
	SentryDSN          string
	YouTubeDataAPIKey  string
	// DevPayments enables the no-Stripe instant-activation billing stubs.
	// Explicit opt-in (DEV_PAYMENTS=true) rather than inferred from ENV:
	// ENV defaults to "development" when unset, so keying free purchases
	// off it would FAIL OPEN on any deploy that forgot to set ENV.
	DevPayments bool
	// VerifyHold: email verifications that arrive sooner than this after
	// signup are held for admin review instead of verifying. Bots verify in
	// 12-19s; the fastest human on record took 17 minutes. 0 disables.
	VerifyHold time.Duration
}

func Load() *Config {
	_ = godotenv.Load() // ignore error if no .env file

	ttlHours, _ := strconv.Atoi(getEnv("SESSION_TTL_HOURS", "24"))

	origins := strings.Split(getEnv("CORS_ORIGINS", "http://localhost:3000"), ",")
	for i := range origins {
		origins[i] = strings.TrimSpace(origins[i])
	}

	return &Config{
		Port:        getEnv("PORT", "8080"),
		Env:         getEnv("ENV", "development"),
		DatabaseURL: getEnv("DATABASE_URL", "postgres://jukebox:jukebox@localhost:5432/jukebox?sslmode=disable"),
		RedisURL:    getEnv("REDIS_URL", "redis://localhost:6379/0"),
		SessionTTL:  time.Duration(ttlHours) * time.Hour,
		CORSOrigins: origins,
		JWTSecret:   getEnv("JWT_SECRET", "change-me-in-production-please"),
		ResendAPIKey: getEnv("RESEND_API_KEY", ""),
		FromEmail:   getEnv("FROM_EMAIL", "noreply@jukebox.local"),
		FrontendURL: getEnv("FRONTEND_URL", "http://localhost:3000"),
		AdminEmail:  getEnv("ADMIN_EMAIL", ""),
		StripeSecretKey:    getEnv("STRIPE_SECRET_KEY", ""),
		StripeWebhookSecret: getEnv("STRIPE_WEBHOOK_SECRET", ""),
		StripePlusPriceID:  getEnv("STRIPE_PLUS_PRICE_ID", ""),
		LiveKitURL:       getEnv("LIVEKIT_URL", ""),
		LiveKitAPIKey:    getEnv("LIVEKIT_API_KEY", ""),
		LiveKitAPISecret: getEnv("LIVEKIT_API_SECRET", ""),
		TurnstileSecretKey: getEnv("TURNSTILE_SECRET_KEY", ""),
		SentryDSN:          getEnv("SENTRY_DSN", ""),
		YouTubeDataAPIKey:  getEnv("YOUTUBE_DATA_API_KEY", ""),
		DevPayments:        getEnv("DEV_PAYMENTS", "") == "true",
		VerifyHold:         verifyHoldFromEnv(),
	}
}

const defaultVerifyHoldSeconds = 60

// verifyHoldFromEnv parses VERIFY_HOLD_SECONDS. Unset, non-numeric, or
// negative values fall back to the default; 0 disables the hold.
func verifyHoldFromEnv() time.Duration {
	raw := getEnv("VERIFY_HOLD_SECONDS", "")
	if raw == "" {
		return defaultVerifyHoldSeconds * time.Second
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return defaultVerifyHoldSeconds * time.Second
	}
	return time.Duration(n) * time.Second
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
