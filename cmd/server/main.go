package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
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
)

func main() {
	cfg := config.Load()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// ---------- Data stores ----------

	pg, err := store.NewPGStore(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("postgres: %v", err)
	}
	defer pg.Close()

	// Run migrations
	if err := pg.RunMigrations(ctx, "migrations"); err != nil {
		log.Printf("migrations warning: %v", err)
	}

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
	emailSvc := email.NewService(cfg.ResendAPIKey, cfg.FromEmail, cfg.FrontendURL)

	// Boot autoplay rooms
	syncSvc.StartAutoplayRooms(cleanupCtx)

	// ---------- Anti-spam ----------

	signupLimiter := antispam.NewRateLimiter(redis.Client(), 5) // max 5 signups per IP per hour

	// ---------- Handlers ----------

	roomH := handlers.NewRoomHandler(pg, redis, hubMgr, syncSvc)
	queueH := handlers.NewQueueHandler(pg, redis, hubMgr)
	sessionH := handlers.NewSessionHandler(redis)
	wsH := handlers.NewWSHandler(pg, redis, hubMgr, cfg.JWTSecret, cfg.CORSOrigins)
	authH := handlers.NewAuthHandler(pg, redis, emailSvc, cfg.JWTSecret, cfg.TurnstileSecretKey, signupLimiter)
	msgH := handlers.NewMessageHandler(pg)
	plH := handlers.NewPlaylistHandler(pg)
	adminH := handlers.NewAdminHandler(pg, redis, hubMgr, syncSvc)
	monH := handlers.NewMonetizationHandler(pg, hubMgr)
	lkH := handlers.NewLiveKitHandler(cfg)

	// ---------- Router ----------

	r := chi.NewRouter()

	// Global middleware
	r.Use(chimw.Logger)
	r.Use(chimw.Recoverer)
	r.Use(chimw.RequestID)
	r.Use(chimw.RealIP)

	// Security headers
	r.Use(middleware.SecurityHeaders(cfg.CORSOrigins))

	// Limit request body size to 10MB (covers cover art uploads)
	r.Use(middleware.MaxBodySize(10 * 1024 * 1024))

	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   cfg.CORSOrigins,
		AllowedMethods:   []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Content-Type", "Authorization", "X-Session-ID", "X-DJ-Key"},
		AllowCredentials: true,
		MaxAge:           300,
	}))

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
		r.Post("/admin/autoplay/rooms/{id}/activate", adminH.ActivatePlaylist)
		r.Delete("/admin/autoplay/rooms/{id}/staged", adminH.DeleteStagedPlaylist)
		r.Post("/admin/autoplay/rooms/{id}/stop", adminH.StopAutoplayRoom)

		// Featured room (public)
		r.Get("/featured", adminH.GetFeatured)

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

		// LiveKit voice
		r.Post("/livekit/token", lkH.GetToken)
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
