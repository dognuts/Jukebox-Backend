package main

import (
	"context"
	"log"
	"net/http"
	neturl "net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/jukebox/backend/internal/config"
	"github.com/jukebox/backend/internal/email"
	"github.com/jukebox/backend/internal/handlers"
	"github.com/jukebox/backend/internal/antispam"
	"github.com/jukebox/backend/internal/middleware"
	"github.com/jukebox/backend/internal/playback"
	"github.com/jukebox/backend/internal/store"
	"github.com/jukebox/backend/internal/ws"
	"github.com/jukebox/backend/internal/youtube"
)

// hostnamesFromURLs extracts unique lowercase hostnames from a list of
// URLs/origins (e.g. "https://www.jukebox-app.com" → "www.jukebox-app.com").
// Entries that don't parse as URLs with a host are skipped.
func hostnamesFromURLs(urls []string) []string {
	seen := map[string]bool{}
	var hosts []string
	for _, raw := range urls {
		u, err := neturl.Parse(strings.TrimSpace(raw))
		if err != nil || u.Hostname() == "" {
			continue
		}
		h := strings.ToLower(u.Hostname())
		if !seen[h] {
			seen[h] = true
			hosts = append(hosts, h)
		}
	}
	return hosts
}

func main() {
	cfg := config.Load()

	// Refuse to boot with a forgeable JWT secret. The config default is a
	// public string in an open repo — with it live, anyone can sign an
	// HS256 token for any user id (including the admin) and the server
	// will honor it. Unconditional, not ENV-gated: a prod deploy that
	// forgot to set ENV would skip an ENV-gated check exactly where it
	// matters.
	if cfg.JWTSecret == "change-me-in-production-please" || cfg.JWTSecret == "change-me-to-a-random-64-char-string" || len(cfg.JWTSecret) < 32 {
		log.Fatal("FATAL: JWT_SECRET is unset, a known placeholder, or shorter than 32 characters. Set a strong random JWT_SECRET (e.g. `openssl rand -hex 32`) and restart.")
	}

	// ---------- Error monitoring ----------
	if err := middleware.InitSentry(cfg.SentryDSN, cfg.Env); err != nil {
		log.Printf("sentry warning: %v", err)
	}
	defer middleware.FlushSentry()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// ---------- Data stores ----------

	pg, err := store.NewPGStore(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("postgres: %v", err)
	}
	defer pg.Close()

	// Run migrations under their own generous deadline, NOT the shared 10s
	// boot ctx: a legitimately slow migration (index build on a grown table)
	// or an advisory-lock wait behind a migrating peer would otherwise be
	// killed client-side at 10s and, with the fatal exit below, turn every
	// deploy that needs it into a crash loop.
	// A failure is still fatal: booting without the expected schema fails
	// later in stranger ways than a loud restart loop.
	migCtx, migCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	if err := pg.RunMigrations(migCtx, "migrations"); err != nil {
		migCancel()
		log.Fatalf("migrations: %v", err)
	}
	migCancel()

	redis, err := store.NewRedisStore(cfg.RedisURL, cfg.SessionTTL)
	if err != nil {
		log.Fatalf("redis: %v", err)
	}
	defer redis.Close()

	// Clean up stale state from previous server run
	// Rooms may still show as "live" from before a restart
	cleanupCtx := context.Background()
	if err := pg.ResetAllRoomsOffline(cleanupCtx); err != nil {
		log.Printf("startup cleanup warning: %v", err)
	} else {
		log.Println("✓ Reset stale rooms to offline")
	}

	// Bootstrap admin user from ADMIN_EMAIL env var
	if cfg.AdminEmail != "" {
		if err := pg.BootstrapAdmin(cleanupCtx, cfg.AdminEmail); err != nil {
			log.Printf("admin bootstrap warning: %v", err)
		} else {
			log.Printf("✓ Admin user ensured: %s", cfg.AdminEmail)
		}
	}

	// ---------- Services ----------

	hubMgr := ws.NewHubManager(pg, redis)
	syncSvc := playback.NewSyncService(pg, redis, hubMgr)
	idleMon := playback.NewIdleMonitor(pg, redis, hubMgr)
	idleMon.Start()
	emailSvc := email.NewService(cfg.ResendAPIKey, cfg.FromEmail, cfg.FrontendURL, cfg.AdminEmail)

	// Boot autoplay rooms
	syncSvc.StartAutoplayRooms(cleanupCtx)

	// ---------- Anti-spam ----------

	// These protections silently no-op when their keys are unset. Warn
	// unconditionally (not just when ENV=production — a prod deploy that
	// forgot to set ENV would skip the warning exactly where it matters).
	// Captcha tokens are only accepted when solved on our own pages —
	// hostnames derived from CORS_ORIGINS and FRONTEND_URL. This stops
	// token harvesting (see antispam.VerifyTurnstile).
	turnstileHosts := hostnamesFromURLs(append([]string{cfg.FrontendURL}, cfg.CORSOrigins...))
	if cfg.TurnstileSecretKey == "" {
		log.Println("⚠️⚠️⚠️  TURNSTILE_SECRET_KEY is not set — signup CAPTCHA is DISABLED. Set it (and NEXT_PUBLIC_TURNSTILE_SITE_KEY on the frontend) before serving real traffic. ⚠️⚠️⚠️")
	} else {
		log.Printf("✓ Turnstile CAPTCHA enabled (accepted hostnames: %v)", turnstileHosts)
	}
	if cfg.ResendAPIKey == "" {
		log.Println("⚠️⚠️⚠️  RESEND_API_KEY is not set — verification and password-reset emails are NOT delivered (dev mode logs them to console). ⚠️⚠️⚠️")
	}

	signupLimiter := antispam.NewRateLimiter(redis.Client(), 5) // max 5 signups per IP per hour

	// ---------- Handlers ----------

	roomH := handlers.NewRoomHandler(pg, redis, hubMgr, syncSvc, signupLimiter)
	queueH := handlers.NewQueueHandler(pg, redis, hubMgr, signupLimiter)
	sessionH := handlers.NewSessionHandler(redis, signupLimiter)
	wsH := handlers.NewWSHandler(pg, redis, hubMgr, cfg.JWTSecret, cfg.CORSOrigins)
	wsTicketH := handlers.NewWSTicketHandler(pg, redis, signupLimiter)
	authH := handlers.NewAuthHandler(pg, redis, emailSvc, cfg.JWTSecret, cfg.TurnstileSecretKey, turnstileHosts, signupLimiter, cfg.VerifyHold)
	msgH := handlers.NewMessageHandler(pg, signupLimiter)
	plH := handlers.NewPlaylistHandler(pg)
	djH := handlers.NewDJHandler(pg)
	// YouTube search client is optional: nil when YOUTUBE_DATA_API_KEY is unset,
	// which causes the admin bulk-search endpoint to return 503 with a clear message.
	var ytClient *youtube.Client
	if cfg.YouTubeDataAPIKey != "" {
		ytClient = youtube.NewClient(cfg.YouTubeDataAPIKey)
	}
	adminH := handlers.NewAdminHandler(pg, redis, hubMgr, syncSvc, ytClient)
	// Purchases only activate for free when DEV_PAYMENTS=true is set
	// explicitly (local development). Everywhere else they return 503
	// until real (Stripe-webhook-verified) payments exist. Deliberately
	// NOT keyed on ENV, whose default is "development" — that would fail
	// open on a deploy that forgot to set it.
	if cfg.DevPayments {
		log.Println("⚠️  DEV_PAYMENTS=true — billing endpoints grant Plus/Neon/DJ subs WITHOUT payment. Never set this in production.")
	}
	monH := handlers.NewMonetizationHandler(pg, hubMgr, cfg.DevPayments)
	supportH := handlers.NewSupportHandler(pg, signupLimiter, emailSvc)
	lkH := handlers.NewLiveKitHandler(cfg, pg)

	// ---------- Router ----------

	r := chi.NewRouter()

	// Global middleware
	r.Use(chimw.Logger)
	r.Use(chimw.Recoverer)
	r.Use(chimw.RequestID)
	// NOTE: chimw.RealIP is deliberately NOT used — it rewrites RemoteAddr
	// from the leftmost X-Forwarded-For / X-Real-IP entry, both of which a
	// client can spoof. handlers.ClientIP derives the real client IP from
	// the rightmost (proxy-appended) XFF entry instead.
	r.Use(middleware.SentryRecover) // capture panics to Sentry
	r.Use(middleware.SentryMiddleware()) // transaction tracking

	// Security headers
	r.Use(middleware.SecurityHeaders(cfg.CORSOrigins))

	// Route-aware body caps: cover-art-bearing endpoints get megabytes;
	// everything else — including the unauthenticated auth endpoints —
	// gets 64KB, so a flood of fat JSON bodies can't buy cheap memory
	// pressure against login/refresh/forgot-password.
	r.Use(func(next http.Handler) http.Handler {
		const (
			smallBody  = 64 * 1024
			mediumBody = 1024 * 1024
			largeBody  = 10 * 1024 * 1024 // cover art data URLs
		)
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			limit := int64(smallBody)
			p := req.URL.Path
			switch {
			case req.Method == http.MethodPost && p == "/api/rooms",
				strings.HasPrefix(p, "/api/admin/rooms"),
				strings.HasPrefix(p, "/api/admin/autoplay"):
				limit = largeBody
			case strings.HasPrefix(p, "/api/playlists"):
				limit = mediumBody
			}
			if req.Body != nil {
				req.Body = http.MaxBytesReader(w, req.Body, limit)
			}
			next.ServeHTTP(w, req)
		})
	})

	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   cfg.CORSOrigins,
		AllowedMethods:   []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Content-Type", "Authorization", "X-Session-ID", "X-DJ-Key"},
		AllowCredentials: true,
		MaxAge:           300,
	}))

	// CSRF backstop for the cross-site session cookie: mutating JSON
	// endpoints only accept bodies declared as application/json, which a
	// hostile page's form post cannot produce without a CORS preflight.
	// AFTER the CORS handler so its 415 responses carry CORS headers and
	// OPTIONS preflights are answered first.
	r.Use(middleware.RequireJSONContentType)

	// Session middleware (anonymous identity)
	r.Use(middleware.SessionMiddleware(redis))

	// Auth middleware (JWT — adds user to context if valid token present)
	r.Use(middleware.AuthMiddleware(cfg.JWTSecret, pg))

	// Health check
	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})

	// REST API
	r.Route("/api", func(r chi.Router) {
		// Session
		r.Get("/session", sessionH.GetCurrent)
		r.Patch("/session", sessionH.Update)

		// Auth (public)
		r.Post("/auth/signup", authH.Signup)
		r.Post("/auth/login", authH.Login)
		r.Get("/auth/check-stage-name", authH.CheckStageName)
		r.Post("/auth/refresh", authH.RefreshToken)
		r.Post("/auth/forgot-password", authH.ForgotPassword)
		r.Post("/auth/reset-password", authH.ResetPassword)
		r.Post("/auth/verify-email", authH.VerifyEmail)

		// Auth (requires login)
		r.Get("/auth/me", authH.GetMe)
		r.Get("/auth/me/stats", authH.GetMyStats)
		r.Get("/auth/me/favorites", authH.GetMyFavorites)
		r.Patch("/auth/me", authH.UpdateProfile)
		r.Delete("/auth/me", authH.DeleteAccount)
		r.Post("/auth/change-password", authH.ChangePassword)
		r.Post("/auth/logout", authH.Logout)
		r.Post("/auth/resend-verification", authH.ResendVerification)

		// Rooms
		r.Get("/rooms", roomH.List)
		r.Post("/rooms", roomH.Create)
		r.Get("/rooms/{slug}", roomH.Get)
		r.Post("/rooms/{slug}/go-live", roomH.GoLive)
		r.Post("/rooms/{slug}/end", roomH.EndSession)

		// Queue
		r.Get("/rooms/{slug}/queue", queueH.GetQueue)
		r.Post("/rooms/{slug}/queue", queueH.SubmitTrack)
		r.Get("/rooms/{slug}/requests", queueH.GetPendingRequests)
		r.Get("/rooms/{slug}/history", roomH.GetHistory)
		r.Get("/rooms/{slug}/autoplay-tracks", roomH.GetAutoplayTracks)
		r.Post("/rooms/{slug}/save-session", roomH.SaveSession)

		// Direct Messages (requires login)
		r.Get("/messages", msgH.ListConversations)
		r.Get("/messages/{userId}", msgH.GetConversation)
		r.Post("/messages/{userId}", msgH.SendMessage)
		r.Post("/messages/{userId}/read", msgH.MarkRead)

		// Playlists (requires login)
		r.Get("/playlists", plH.List)
		r.Post("/playlists", plH.Create)
		r.Get("/playlists/{id}", plH.Get)
		r.Patch("/playlists/{id}", plH.Update)
		r.Delete("/playlists/{id}", plH.Delete)
		r.Post("/playlists/{id}/tracks", plH.AddTrack)
		r.Delete("/playlists/{id}/tracks/{trackId}", plH.RemoveTrack)

		// Admin (requires login + admin role — checked in handler)
		r.Get("/admin/rooms", adminH.ListRooms)
		r.Post("/admin/rooms", adminH.CreateOfficialRoom)
		r.Patch("/admin/rooms/{id}", adminH.UpdateRoom)
		r.Post("/admin/rooms/{id}/shutdown", adminH.ShutdownRoom)
		r.Delete("/admin/rooms/{id}", adminH.DeleteRoom)
		r.Post("/admin/rooms/{id}/feature", adminH.SetFeatured)
		r.Post("/admin/rooms/{id}/official", adminH.SetOfficial)

		// Admin user management
		r.Get("/admin/users", adminH.ListUsers)
		r.Get("/admin/users/{id}", adminH.GetUser)
		r.Patch("/admin/users/{id}", adminH.UpdateUser)
		r.Delete("/admin/users/{id}", adminH.DeleteUser)

		// Admin autoplay rooms
		r.Post("/admin/autoplay/rooms", adminH.CreateAutoplayRoom)

		// Admin metrics
		r.Get("/admin/metrics", adminH.GetMetrics)
		r.Get("/admin/autoplay/rooms/{id}/playlists", adminH.GetAutoplayPlaylists)
		r.Put("/admin/autoplay/rooms/{id}/staged", adminH.SaveStagedPlaylist)
		r.Patch("/admin/autoplay/rooms/{id}/live/snippets", adminH.UpdateLiveSnippets)
		r.Put("/admin/autoplay/rooms/{id}/live/tracks", adminH.UpdateLiveTracks)
		r.Post("/admin/autoplay/rooms/{id}/activate", adminH.ActivatePlaylist)
		r.Delete("/admin/autoplay/rooms/{id}/staged", adminH.DeleteStagedPlaylist)
		r.Post("/admin/autoplay/rooms/{id}/start", adminH.StartAutoplayRoom)
		r.Post("/admin/autoplay/rooms/{id}/stop", adminH.StopAutoplayRoom)

		// Admin: resolve a free-form query to YouTube Data API top match + alternatives.
		// Used by the /admin/autoplay Bulk mode to skip manual YouTube searches.
		r.Get("/admin/search-track", adminH.SearchTrack)

		// Featured room (public)
		r.Get("/featured", adminH.GetFeatured)

		// DJ profiles (public)
		r.Get("/djs/{username}", djH.GetProfile)

		// Billing / Monetization
		r.Get("/billing/pricing", monH.GetPricing)
		r.Get("/billing/plus/status", monH.PlusStatus)
		r.Post("/billing/plus/subscribe", monH.SubscribePlus)
		r.Post("/billing/plus/cancel", monH.CancelPlus)
		r.Get("/billing/dj/{userId}/settings", monH.GetDJSubSettings)
		r.Post("/billing/dj/settings", monH.UpdateDJSubSettings)
		r.Post("/billing/dj/{userId}/subscribe", monH.SubscribeToDJ)
		r.Get("/billing/dj/{userId}/subscription", monH.GetDJSubscription)
		r.Get("/billing/neon/packs", monH.GetNeonPacks)
		r.Get("/billing/neon/balance", monH.GetNeonBalance)
		r.Post("/billing/neon/buy", monH.BuyNeon)
		r.Post("/billing/neon/send", monH.SendNeon)
		r.Get("/rooms/{roomId}/tube", monH.GetTubeState)
		r.Post("/admin/pool/compute", monH.ComputePool)

		// Support / listener reports (public — anonymous listeners can submit)
		r.Post("/support/listener-report", supportH.CreateListenerReport)

		// LiveKit voice
		r.Post("/livekit/token", lkH.GetToken)

		// WebSocket tickets: single-use, 30s credentials so the ws URL
		// never carries a JWT / DJ key / session id into request logs.
		r.Post("/ws/ticket", wsTicketH.Create)
	})

	// WebSocket
	r.Get("/ws/room/{slug}", wsH.HandleRoomWS)

	// ---------- Server ----------

	srv := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      r,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
		// Defense in depth for header-based floods (e.g. megabyte bearer
		// tokens parsed before signature checks): nothing legitimate sends
		// more than a few KB of headers.
		MaxHeaderBytes: 16 * 1024,
	}

	// Graceful shutdown
	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGTERM)

	go func() {
		log.Printf("🎵 Jukebox server starting on :%s (env=%s)", cfg.Port, cfg.Env)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server: %v", err)
		}
	}()

	<-done
	log.Println("shutting down...")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	syncSvc.Stop()
	idleMon.Stop()
	srv.Shutdown(shutdownCtx)
	log.Println("server stopped")
}
