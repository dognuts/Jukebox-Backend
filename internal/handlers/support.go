package handlers

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/jukebox/backend/internal/email"
	"github.com/jukebox/backend/internal/middleware"
	"github.com/jukebox/backend/internal/models"
	"github.com/jukebox/backend/internal/store"
)

// ----- Dependencies (interfaces so tests can stub them) -----

// supportReportRateLimiter is the subset of antispam.RateLimiter we use.
// Declared as an interface here so tests don't need Redis.
type supportReportRateLimiter interface {
	AllowSupportReport(ctx context.Context, ip string) (bool, error)
}

// supportReportEmailer is the subset of *email.Service we use.
type supportReportEmailer interface {
	SendListenerReport(ctx email.ListenerReportContext) error
}

// supportReportStore is the subset of *store.PGStore we use for context lookup.
// GetRoomBySlug returns (nil, nil) on miss — the handler treats that as
// "unknown" rather than erroring.
type supportReportStore interface {
	GetRoomBySlug(ctx context.Context, slug string) (*models.Room, error)
}

// ----- Handler -----

type SupportHandler struct {
	pg      supportReportStore
	limiter supportReportRateLimiter
	emailer supportReportEmailer
}

func NewSupportHandler(pg *store.PGStore, limiter supportReportRateLimiter, emailer supportReportEmailer) *SupportHandler {
	return &SupportHandler{pg: pg, limiter: limiter, emailer: emailer}
}

// ----- Request shape -----

type listenerReportRequest struct {
	Category            string  `json:"category"`
	Message             string  `json:"message"`
	ContactEmail        string  `json:"contactEmail"`
	CanContactBack      bool    `json:"canContactBack"`
	OpenedAt            int64   `json:"openedAt"`
	Website             string  `json:"website"`
	RoomSlug            string  `json:"roomSlug"`
	TrackID             string  `json:"trackId"`
	TrackTitle          string  `json:"trackTitle"`
	TrackArtist         string  `json:"trackArtist"`
	PlaybackPositionSec float64 `json:"playbackPositionSec"`
}

// ----- Pure validators -----

// validateListenerReport returns "" if the request is well-formed, else a
// short error message suitable for a 400 response body.
func validateListenerReport(req listenerReportRequest, hasSession bool) string {
	switch req.Category {
	case "gated", "no-audio", "out-of-sync", "other":
	default:
		return "invalid category"
	}
	if n := len(strings.TrimSpace(req.Message)); n < 10 || n > 2000 {
		return "message must be 10–2000 characters"
	}
	if !hasSession {
		if req.ContactEmail == "" {
			return "contactEmail required for anonymous reports"
		}
		if _, err := mail.ParseAddress(req.ContactEmail); err != nil {
			return "invalid contactEmail"
		}
	}
	return ""
}

// isLikelyBot returns true if the honeypot or minimum-submission-time check
// trips. Missing openedAt (zero) or openedAt in the future (>0 skew) is
// treated as a trip to avoid a trivial bypass.
func isLikelyBot(req listenerReportRequest, now time.Time) bool {
	if req.Website != "" {
		return true
	}
	if req.OpenedAt <= 0 {
		return true
	}
	nowMs := now.UnixMilli()
	if req.OpenedAt > nowMs {
		return true
	}
	return nowMs-req.OpenedAt < 2000
}

// ----- HTTP handler -----

// CreateListenerReport handles POST /api/support/listener-report.
func (h *SupportHandler) CreateListenerReport(w http.ResponseWriter, r *http.Request) {
	var req listenerReportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	user := middleware.GetUser(r.Context())
	hasSession := user != nil

	if msg := validateListenerReport(req, hasSession); msg != "" {
		http.Error(w, msg, http.StatusBadRequest)
		return
	}

	// Silent-drop branches: return 200 so the bot can't learn which check it tripped.
	if isLikelyBot(req, time.Now()) {
		writeOK(w)
		return
	}

	ip := ClientIP(r)

	if h.limiter != nil {
		allowed, err := h.limiter.AllowSupportReport(r.Context(), ip)
		if err != nil {
			log.Printf("[support] rate limit check error: %v", err)
			// Fail open — consistent with existing signup limiter behavior.
		}
		if !allowed {
			http.Error(w, "You've sent a lot of reports — please email support@jukebox-app.com directly.", http.StatusTooManyRequests)
			return
		}
	}

	// Resolve context: session > client-supplied track fields > room lookup.
	contactEmail := req.ContactEmail
	userID := ""
	if user != nil {
		contactEmail = user.Email
		userID = user.ID
	}

	roomName := "(unknown)"
	if h.pg != nil && req.RoomSlug != "" {
		if room, err := h.pg.GetRoomBySlug(r.Context(), req.RoomSlug); err == nil && room != nil {
			roomName = room.Name
		}
	}

	trackTitle := req.TrackTitle
	if trackTitle == "" {
		trackTitle = "(unknown)"
	}
	trackArtist := req.TrackArtist
	if trackArtist == "" {
		trackArtist = "(unknown)"
	}

	ctx := email.ListenerReportContext{
		Category:            req.Category,
		Message:             strings.TrimSpace(req.Message),
		ContactEmail:        contactEmail,
		CanContactBack:      req.CanContactBack,
		UserID:              userID,
		RoomSlug:            req.RoomSlug,
		RoomName:            roomName,
		TrackID:             req.TrackID,
		TrackTitle:          trackTitle,
		TrackArtist:         trackArtist,
		PlaybackPositionSec: req.PlaybackPositionSec,
		UserAgent:           r.UserAgent(),
		ClientIP:            ip,
		SubmittedAt:         time.Now().UTC(),
	}

	if err := h.emailer.SendListenerReport(ctx); err != nil {
		log.Printf("[support] SendListenerReport failed: %v", err)
		http.Error(w, "couldn't send — please try again or email support@jukebox-app.com", http.StatusInternalServerError)
		return
	}

	writeOK(w)
}

func writeOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"ok":true}`))
}
