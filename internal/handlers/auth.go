package handlers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/mail"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jukebox/backend/internal/antispam"
	"github.com/jukebox/backend/internal/email"
	"github.com/jukebox/backend/internal/middleware"
	"github.com/jukebox/backend/internal/moderation"
	"github.com/jukebox/backend/internal/models"
	"github.com/jukebox/backend/internal/store"
	"golang.org/x/crypto/bcrypt"
)

type AuthHandler struct {
	pg                 *store.PGStore
	redis              *store.RedisStore
	emailSvc           *email.Service
	jwtSecret          string
	turnstileSecret    string
	signupRateLimiter  *antispam.RateLimiter
}

func NewAuthHandler(pg *store.PGStore, redis *store.RedisStore, emailSvc *email.Service, jwtSecret string, turnstileSecret string, rateLimiter *antispam.RateLimiter) *AuthHandler {
	return &AuthHandler{pg: pg, redis: redis, emailSvc: emailSvc, jwtSecret: jwtSecret, turnstileSecret: turnstileSecret, signupRateLimiter: rateLimiter}
}

// POST /api/auth/signup
func (h *AuthHandler) Signup(w http.ResponseWriter, r *http.Request) {
	var req models.SignupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	// --- Anti-spam: honeypot ---
	if req.Website != "" {
		// Bots fill in the hidden honeypot field; real users never see it.
		// Return a fake success so bots don't adapt.
		log.Printf("[antispam] honeypot triggered from %s", r.RemoteAddr)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"status":"ok"}`))
		return
	}

	// --- Anti-spam: rate limiting ---
	ip := ClientIP(r)
	if h.signupRateLimiter != nil {
		allowed, err := h.signupRateLimiter.AllowSignup(r.Context(), ip)
		if err != nil {
			log.Printf("[antispam] rate limit check error: %v", err)
		}
		if !allowed {
			log.Printf("[antispam] rate limit exceeded for IP %s", ip)
			http.Error(w, "too many signup attempts — please try again later", http.StatusTooManyRequests)
			return
		}
	}

	// --- Anti-spam: Turnstile CAPTCHA ---
	if err := antispam.VerifyTurnstile(r.Context(), h.turnstileSecret, req.CaptchaToken, ip); err != nil {
		log.Printf("[antispam] captcha failed from %s: %v", ip, err)
		http.Error(w, "captcha verification failed — please try again", http.StatusBadRequest)
		return
	}

	// Validate email
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	if _, err := mail.ParseAddress(req.Email); err != nil {
		http.Error(w, "invalid email address", http.StatusBadRequest)
		return
	}

	// --- Anti-spam: disposable email (after normalization) ---
	if antispam.IsDisposableEmail(req.Email) {
		log.Printf("[antispam] disposable email blocked: %s", req.Email)
		http.Error(w, "disposable email addresses are not allowed — please use a permanent email", http.StatusBadRequest)
		return
	}

	// Validate password
	if err := validatePassword(req.Password); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Stage name is the primary display name
	req.StageName = strings.TrimSpace(req.StageName)
	if req.StageName == "" {
		// Fall back to displayName for backwards compat
		req.StageName = strings.TrimSpace(req.DisplayName)
	}
	if len(req.StageName) < 2 || len(req.StageName) > 30 {
		http.Error(w, "stage name must be 2-30 characters", http.StatusBadRequest)
		return
	}
	if moderation.ContainsProfanity(req.StageName) {
		http.Error(w, "stage name contains inappropriate language", http.StatusBadRequest)
		return
	}

	ctx := r.Context()

	// Check if stage name is already taken
	taken, err := h.pg.IsStageNameTaken(ctx, req.StageName, "")
	if err != nil {
		log.Printf("[auth] check stage name: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if taken {
		http.Error(w, "this stage name is already taken", http.StatusConflict)
		return
	}

	// Check if email already exists
	existing, _ := h.pg.GetUserByEmail(ctx, req.Email)
	if existing != nil {
		http.Error(w, "an account with this email already exists", http.StatusConflict)
		return
	}

	// Hash password
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Pick an avatar color
	session := middleware.GetSession(ctx)
	avatarColor := "oklch(0.70 0.18 30)"
	if session != nil {
		avatarColor = session.AvatarColor
	}

	user := &models.User{
		ID:             uuid.New().String(),
		Email:          req.Email,
		EmailVerified:  false,
		PasswordHash:   string(hash),
		DisplayName:    req.StageName, // display_name mirrors stage_name
		AvatarColor:    avatarColor,
		Bio:            "",
		FavoriteGenres: []string{},
		StageName:      req.StageName,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}

	if err := h.pg.CreateUser(ctx, user); err != nil {
		log.Printf("[auth] create user: %v", err)
		http.Error(w, "failed to create account", http.StatusInternalServerError)
		return
	}

	// Geolocate from IP (non-blocking — don't fail signup if this errors)
	go func() {
		ip := ClientIP(r)
		if ip == "" || ip == "127.0.0.1" || ip == "::1" || ip == "[" {
			return
		}
		geoCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		city, region, country := geolocateIP(geoCtx, ip)
		if country != "" {
			h.pg.UpdateUserLocation(geoCtx, user.ID, city, region, country)
		}
	}()

	// Create email verification token
	verifyToken := generateSecureToken()
	verification := &models.EmailVerification{
		ID:        uuid.New().String(),
		UserID:    user.ID,
		Token:     verifyToken,
		ExpiresAt: time.Now().Add(24 * time.Hour),
		CreatedAt: time.Now(),
	}
	if err := h.pg.CreateEmailVerification(ctx, verification); err != nil {
		log.Printf("[auth] create verification: %v", err)
	}

	// Send verification email (non-blocking)
	go func() {
		if err := h.emailSvc.SendVerificationEmail(user.Email, verifyToken); err != nil {
			log.Printf("[auth] send verification email: %v", err)
		}
	}()

	// Generate tokens
	accessToken, err := middleware.GenerateAccessToken(user, h.jwtSecret)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	refreshPlain, refreshHash, err := middleware.GenerateRefreshToken()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	rt := &models.RefreshToken{
		ID:        uuid.New().String(),
		UserID:    user.ID,
		TokenHash: refreshHash,
		ExpiresAt: time.Now().Add(30 * 24 * time.Hour), // 30 days
		CreatedAt: time.Now(),
	}
	h.pg.CreateRefreshToken(ctx, rt)

	// Link session to user
	if session != nil {
		h.redis.UpdateSessionUser(ctx, session.ID, user.ID)
	}

	writeJSON(w, http.StatusCreated, models.AuthResponse{
		User:         *user,
		AccessToken:  accessToken,
		RefreshToken: refreshPlain,
	})
}

// GET /api/auth/check-stage-name?name=xxx
func (h *AuthHandler) CheckStageName(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		writeJSON(w, http.StatusOK, map[string]bool{"available": false})
		return
	}
	taken, err := h.pg.IsStageNameTaken(r.Context(), name, "")
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"available": !taken})
}

// POST /api/auth/login
func (h *AuthHandler) Login(w http.ResponseWriter, r *http.Request) {
	var req models.LoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	// --- Anti-spam: rate limit login attempts ---
	ip := ClientIP(r)
	if h.signupRateLimiter != nil {
		allowed, err := h.signupRateLimiter.AllowLogin(r.Context(), ip)
		if err != nil {
			log.Printf("[antispam] login rate limit check error: %v", err)
		}
		if !allowed {
			log.Printf("[antispam] login rate limit exceeded for IP %s", ip)
			http.Error(w, "too many login attempts — please try again later", http.StatusTooManyRequests)
			return
		}
	}

	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	ctx := r.Context()

	user, err := h.pg.GetUserByEmail(ctx, req.Email)
	if err != nil || user == nil {
		http.Error(w, "invalid email or password", http.StatusUnauthorized)
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(req.Password)); err != nil {
		http.Error(w, "invalid email or password", http.StatusUnauthorized)
		return
	}

	accessToken, _ := middleware.GenerateAccessToken(user, h.jwtSecret)
	refreshPlain, refreshHash, _ := middleware.GenerateRefreshToken()

	rt := &models.RefreshToken{
		ID:        uuid.New().String(),
		UserID:    user.ID,
		TokenHash: refreshHash,
		ExpiresAt: time.Now().Add(30 * 24 * time.Hour),
		CreatedAt: time.Now(),
	}
	h.pg.CreateRefreshToken(ctx, rt)

	// Link session
	if session := middleware.GetSession(ctx); session != nil {
		h.redis.UpdateSessionUser(ctx, session.ID, user.ID)
	}

	writeJSON(w, http.StatusOK, models.AuthResponse{
		User:         *user,
		AccessToken:  accessToken,
		RefreshToken: refreshPlain,
	})
}

// POST /api/auth/refresh
func (h *AuthHandler) RefreshToken(w http.ResponseWriter, r *http.Request) {
	var req models.RefreshTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	hash := middleware.HashRefreshToken(req.RefreshToken)

	rt, err := h.pg.GetRefreshTokenByHash(ctx, hash)
	if err != nil || rt == nil {
		http.Error(w, "invalid refresh token", http.StatusUnauthorized)
		return
	}

	if rt.RevokedAt != nil || rt.ExpiresAt.Before(time.Now()) {
		http.Error(w, "refresh token expired or revoked", http.StatusUnauthorized)
		return
	}

	// Revoke old token (rotation)
	h.pg.RevokeRefreshToken(ctx, rt.ID)

	user, _ := h.pg.GetUserByID(ctx, rt.UserID)
	if user == nil {
		http.Error(w, "user not found", http.StatusUnauthorized)
		return
	}

	// Issue new pair
	accessToken, _ := middleware.GenerateAccessToken(user, h.jwtSecret)
	newRefreshPlain, newRefreshHash, _ := middleware.GenerateRefreshToken()

	newRT := &models.RefreshToken{
		ID:        uuid.New().String(),
		UserID:    user.ID,
		TokenHash: newRefreshHash,
		ExpiresAt: time.Now().Add(30 * 24 * time.Hour),
		CreatedAt: time.Now(),
	}
	h.pg.CreateRefreshToken(ctx, newRT)

	writeJSON(w, http.StatusOK, models.AuthResponse{
		User:         *user,
		AccessToken:  accessToken,
		RefreshToken: newRefreshPlain,
	})
}

// POST /api/auth/logout
func (h *AuthHandler) Logout(w http.ResponseWriter, r *http.Request) {
	user := middleware.GetUser(r.Context())
	if user != nil {
		h.pg.RevokeAllUserRefreshTokens(r.Context(), user.ID)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "logged out"})
}

// POST /api/auth/verify-email
func (h *AuthHandler) VerifyEmail(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		// Also accept from body
		var body struct {
			Token string `json:"token"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		token = body.Token
	}

	if token == "" {
		http.Error(w, "token is required", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	v, err := h.pg.GetEmailVerificationByToken(ctx, token)
	if err != nil || v == nil {
		http.Error(w, "invalid verification token", http.StatusBadRequest)
		return
	}

	if v.UsedAt != nil {
		http.Error(w, "token already used", http.StatusBadRequest)
		return
	}

	if v.ExpiresAt.Before(time.Now()) {
		http.Error(w, "verification token expired", http.StatusBadRequest)
		return
	}

	h.pg.MarkEmailVerificationUsed(ctx, v.ID)
	h.pg.SetEmailVerified(ctx, v.UserID)

	writeJSON(w, http.StatusOK, map[string]string{"status": "email verified"})
}

// POST /api/auth/resend-verification
func (h *AuthHandler) ResendVerification(w http.ResponseWriter, r *http.Request) {
	user := middleware.GetUser(r.Context())
	if user == nil {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}

	if user.EmailVerified {
		writeJSON(w, http.StatusOK, map[string]string{"status": "already verified"})
		return
	}

	verifyToken := generateSecureToken()
	verification := &models.EmailVerification{
		ID:        uuid.New().String(),
		UserID:    user.ID,
		Token:     verifyToken,
		ExpiresAt: time.Now().Add(24 * time.Hour),
		CreatedAt: time.Now(),
	}
	h.pg.CreateEmailVerification(r.Context(), verification)

	go func() {
		if err := h.emailSvc.SendVerificationEmail(user.Email, verifyToken); err != nil {
			log.Printf("[auth] resend verification: %v", err)
		}
	}()

	writeJSON(w, http.StatusOK, map[string]string{"status": "verification email sent"})
}

// POST /api/auth/forgot-password
func (h *AuthHandler) ForgotPassword(w http.ResponseWriter, r *http.Request) {
	var req models.ForgotPasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	ctx := r.Context()

	// Always return success to prevent email enumeration
	user, _ := h.pg.GetUserByEmail(ctx, req.Email)
	if user == nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "if that email exists, a reset link has been sent"})
		return
	}

	resetToken := generateSecureToken()
	pr := &models.PasswordReset{
		ID:        uuid.New().String(),
		UserID:    user.ID,
		Token:     resetToken,
		ExpiresAt: time.Now().Add(1 * time.Hour),
		CreatedAt: time.Now(),
	}

	if err := h.pg.CreatePasswordReset(ctx, pr); err != nil {
		log.Printf("[auth] create password reset: %v", err)
	}

	go func() {
		if err := h.emailSvc.SendPasswordResetEmail(user.Email, resetToken); err != nil {
			log.Printf("[auth] send reset email: %v", err)
		}
	}()

	writeJSON(w, http.StatusOK, map[string]string{"status": "if that email exists, a reset link has been sent"})
}

// POST /api/auth/reset-password
func (h *AuthHandler) ResetPassword(w http.ResponseWriter, r *http.Request) {
	var req models.ResetPasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	if err := validatePassword(req.NewPassword); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	ctx := r.Context()

	pr, err := h.pg.GetPasswordResetByToken(ctx, req.Token)
	if err != nil || pr == nil {
		http.Error(w, "invalid or expired reset token", http.StatusBadRequest)
		return
	}

	if pr.UsedAt != nil {
		http.Error(w, "reset token already used", http.StatusBadRequest)
		return
	}

	if pr.ExpiresAt.Before(time.Now()) {
		http.Error(w, "reset token expired", http.StatusBadRequest)
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	h.pg.MarkPasswordResetUsed(ctx, pr.ID)
	h.pg.UpdateUserPassword(ctx, pr.UserID, string(hash))
	h.pg.RevokeAllUserRefreshTokens(ctx, pr.UserID) // force re-login

	writeJSON(w, http.StatusOK, map[string]string{"status": "password reset successfully"})
}

// GET /api/auth/me
func (h *AuthHandler) GetMe(w http.ResponseWriter, r *http.Request) {
	user := middleware.GetUser(r.Context())
	if user == nil {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	writeJSON(w, http.StatusOK, user)
}

// GET /api/auth/me/stats
func (h *AuthHandler) GetMyStats(w http.ResponseWriter, r *http.Request) {
	user := middleware.GetUser(r.Context())
	if user == nil {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	stats, _ := h.pg.GetUserStats(r.Context(), user.ID)
	writeJSON(w, http.StatusOK, stats)
}

// GET /api/auth/me/favorites
func (h *AuthHandler) GetMyFavorites(w http.ResponseWriter, r *http.Request) {
	user := middleware.GetUser(r.Context())
	if user == nil {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	favs, err := h.pg.GetUserFavoriteRooms(r.Context(), user.ID, 10)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if favs == nil {
		favs = []models.FavoriteRoom{}
	}
	writeJSON(w, http.StatusOK, favs)
}

// PATCH /api/auth/me
func (h *AuthHandler) UpdateProfile(w http.ResponseWriter, r *http.Request) {
	user := middleware.GetUser(r.Context())
	if user == nil {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}

	var req models.UpdateProfileRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	// Apply partial updates
	displayName := user.DisplayName
	if req.DisplayName != nil {
		name := strings.TrimSpace(*req.DisplayName)
		if len(name) < 2 || len(name) > 30 {
			http.Error(w, "display name must be 2-30 characters", http.StatusBadRequest)
			return
		}
		displayName = name
	}

	bio := user.Bio
	if req.Bio != nil {
		b := strings.TrimSpace(*req.Bio)
		if len(b) > 300 {
			http.Error(w, "bio must be 300 characters or less", http.StatusBadRequest)
			return
		}
		bio = b
	}

	avatarColor := user.AvatarColor
	if req.AvatarColor != nil {
		avatarColor = *req.AvatarColor
	}

	genres := user.FavoriteGenres
	if req.FavoriteGenres != nil {
		genres = req.FavoriteGenres
		if len(genres) > 5 {
			genres = genres[:5]
		}
	}

	stageName := user.StageName
	if req.StageName != nil {
		sn := strings.TrimSpace(*req.StageName)
		if len(sn) < 2 || len(sn) > 30 {
			http.Error(w, "stage name must be 2-30 characters", http.StatusBadRequest)
			return
		}
		if moderation.ContainsProfanity(sn) {
			http.Error(w, "stage name contains inappropriate language", http.StatusBadRequest)
			return
		}
		// Check uniqueness if changed
		if !strings.EqualFold(sn, user.StageName) {
			taken, err := h.pg.IsStageNameTaken(r.Context(), sn, user.ID)
			if err != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			if taken {
				http.Error(w, "this stage name is already taken", http.StatusConflict)
				return
			}
		}
		stageName = sn
		displayName = sn // keep display_name in sync
	}

	ctx := r.Context()
	if err := h.pg.UpdateUserProfile(ctx, user.ID, displayName, bio, avatarColor, genres, stageName); err != nil {
		log.Printf("[auth] update profile: %v", err)
		http.Error(w, "failed to update profile", http.StatusInternalServerError)
		return
	}

	updated, _ := h.pg.GetUserByID(ctx, user.ID)
	if updated == nil {
		updated = user
	}

	writeJSON(w, http.StatusOK, updated)
}

// POST /api/auth/change-password
func (h *AuthHandler) ChangePassword(w http.ResponseWriter, r *http.Request) {
	user := middleware.GetUser(r.Context())
	if user == nil {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}

	var req models.ChangePasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	// Verify current password
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(req.CurrentPassword)); err != nil {
		http.Error(w, "current password is incorrect", http.StatusUnauthorized)
		return
	}

	if err := validatePassword(req.NewPassword); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	ctx := r.Context()
	h.pg.UpdateUserPassword(ctx, user.ID, string(hash))

	// Revoke all refresh tokens except current session
	h.pg.RevokeAllUserRefreshTokens(ctx, user.ID)

	// Issue new tokens
	accessToken, _ := middleware.GenerateAccessToken(user, h.jwtSecret)
	refreshPlain, refreshHash, _ := middleware.GenerateRefreshToken()
	rt := &models.RefreshToken{
		ID:        uuid.New().String(),
		UserID:    user.ID,
		TokenHash: refreshHash,
		ExpiresAt: time.Now().Add(30 * 24 * time.Hour),
		CreatedAt: time.Now(),
	}
	h.pg.CreateRefreshToken(ctx, rt)

	writeJSON(w, http.StatusOK, models.AuthResponse{
		User:         *user,
		AccessToken:  accessToken,
		RefreshToken: refreshPlain,
	})
}

// DELETE /api/auth/me
func (h *AuthHandler) DeleteAccount(w http.ResponseWriter, r *http.Request) {
	user := middleware.GetUser(r.Context())
	if user == nil {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}

	// Require password confirmation
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Password == "" {
		http.Error(w, "password confirmation required", http.StatusBadRequest)
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(body.Password)); err != nil {
		http.Error(w, "incorrect password", http.StatusUnauthorized)
		return
	}

	ctx := r.Context()
	h.pg.RevokeAllUserRefreshTokens(ctx, user.ID)
	if err := h.pg.DeleteUser(ctx, user.ID); err != nil {
		log.Printf("[auth] delete user: %v", err)
		http.Error(w, "failed to delete account", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "account deleted"})
}

// ---------- Helpers ----------

func validatePassword(password string) error {
	if len(password) < 8 {
		return &validationError{"password must be at least 8 characters"}
	}
	if len(password) > 128 {
		return &validationError{"password must be 128 characters or less"}
	}

	var hasUpper, hasLower, hasDigit bool
	for _, c := range password {
		switch {
		case unicode.IsUpper(c):
			hasUpper = true
		case unicode.IsLower(c):
			hasLower = true
		case unicode.IsDigit(c):
			hasDigit = true
		}
	}
	if !hasUpper || !hasLower || !hasDigit {
		return &validationError{"password must contain uppercase, lowercase, and a digit"}
	}
	return nil
}

type validationError struct {
	msg string
}

func (e *validationError) Error() string {
	return e.msg
}

func generateSecureToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// geolocateIP uses ip-api.com (free, no key required) to get location from IP.
func geolocateIP(ctx context.Context, ip string) (city, region, country string) {
	req, err := http.NewRequestWithContext(ctx, "GET", "http://ip-api.com/json/"+ip+"?fields=city,regionName,country", nil)
	if err != nil {
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var result struct {
		City       string `json:"city"`
		RegionName string `json:"regionName"`
		Country    string `json:"country"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return
	}
	return result.City, result.RegionName, result.Country
}
