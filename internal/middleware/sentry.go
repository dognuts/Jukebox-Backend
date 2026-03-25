package middleware

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime/debug"
	"time"

	"github.com/getsentry/sentry-go"
	sentryhttp "github.com/getsentry/sentry-go/http"
)

// InitSentry initializes the Sentry SDK. Call once at server startup.
// If dsn is empty, Sentry is disabled (development mode).
func InitSentry(dsn, env string) error {
	if dsn == "" {
		log.Println("⚠ Sentry DSN not set — error monitoring disabled")
		return nil
	}

	err := sentry.Init(sentry.ClientOptions{
		Dsn:              dsn,
		Environment:      env,
		TracesSampleRate: 0.2, // capture 20% of transactions
		EnableTracing:    true,
		BeforeSend: func(event *sentry.Event, hint *sentry.EventHint) *sentry.Event {
			// Strip sensitive headers
			if event.Request != nil {
				delete(event.Request.Headers, "Authorization")
				delete(event.Request.Headers, "Cookie")
				delete(event.Request.Headers, "X-DJ-Key")
			}
			return event
		},
	})
	if err != nil {
		return fmt.Errorf("sentry.Init: %w", err)
	}

	log.Printf("✓ Sentry initialized (env=%s)", env)
	return nil
}

// FlushSentry drains the Sentry buffer. Call before server shutdown.
func FlushSentry() {
	sentry.Flush(2 * time.Second)
}

// SentryMiddleware wraps requests with Sentry transaction tracking and panic recovery.
func SentryMiddleware() func(http.Handler) http.Handler {
	// If Sentry wasn't initialized, return a no-op middleware
	if sentry.CurrentHub().Client() == nil {
		return func(next http.Handler) http.Handler {
			return next
		}
	}

	handler := sentryhttp.New(sentryhttp.Options{
		Repanic: true, // let chi's Recoverer also handle the panic for logging
	})

	return handler.Handle
}

// SentryRecover is a middleware that captures panics to Sentry.
// Use alongside chi's Recoverer for both logging and Sentry reporting.
func SentryRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				// Report to Sentry
				if hub := sentry.GetHubFromContext(r.Context()); hub != nil {
					hub.RecoverWithContext(r.Context(), err)
				} else {
					sentry.CurrentHub().RecoverWithContext(r.Context(), err)
				}

				// Log the stack trace
				log.Printf("[PANIC] %v\n%s", err, debug.Stack())

				// Return 500
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// CaptureError sends an error to Sentry with request context.
func CaptureError(r *http.Request, err error) {
	if hub := sentry.GetHubFromContext(r.Context()); hub != nil {
		hub.CaptureException(err)
	} else {
		sentry.CaptureException(err)
	}
}

// CaptureMessage sends a message to Sentry.
func CaptureMessage(msg string) {
	sentry.CaptureMessage(msg)
}

func init() {
	// Ensure SENTRY_DSN from env is available (fallback for Config.SentryDSN)
	_ = os.Getenv("SENTRY_DSN")
}
