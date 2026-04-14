package middleware

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jukebox/backend/internal/models"
	"github.com/jukebox/backend/internal/store"
)

const UserKey contextKey = "user"

var (
	ErrInvalidToken = errors.New("invalid or expired token")
	ErrNoToken      = errors.New("no authorization token provided")
)

type JWTClaims struct {
	UserID      string `json:"uid"`
	Email       string `json:"email"`
	DisplayName string `json:"name"`
	jwt.RegisteredClaims
}

// GenerateAccessToken creates a short-lived JWT (15 minutes).
func GenerateAccessToken(user *models.User, secret string) (string, error) {
	claims := JWTClaims{
		UserID:      user.ID,
		Email:       user.Email,
		DisplayName: user.DisplayName,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(15 * time.Minute)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			Subject:   user.ID,
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(secret))
}

// GenerateRefreshToken creates a cryptographically random refresh token string.
// Returns the plain token (sent to client) and its SHA-256 hash (stored in DB).
func GenerateRefreshToken() (plain string, hash string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	plain = hex.EncodeToString(b)
	h := sha256.Sum256([]byte(plain))
	hash = hex.EncodeToString(h[:])
	return plain, hash, nil
}

// HashRefreshToken computes SHA-256 hash of a plain refresh token for DB lookup.
func HashRefreshToken(plain string) string {
	h := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(h[:])
}

// ValidateAccessToken parses and validates a JWT access token.
func ValidateAccessToken(tokenStr, secret string) (*JWTClaims, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &JWTClaims{}, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unexpected signing method")
		}
		return []byte(secret), nil
	})
	if err != nil {
		return nil, ErrInvalidToken
	}

	claims, ok := token.Claims.(*JWTClaims)
	if !ok || !token.Valid {
		return nil, ErrInvalidToken
	}
	return claims, nil
}

// AuthMiddleware extracts and validates the JWT from the Authorization header.
// If valid, sets the User in context. If missing/invalid, the request continues
// without a user (endpoints that require auth should check for nil user).
func AuthMiddleware(secret string, pg *store.PGStore) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()

			authHeader := r.Header.Get("Authorization")
			if authHeader != "" && strings.HasPrefix(authHeader, "Bearer ") {
				tokenStr := strings.TrimPrefix(authHeader, "Bearer ")
				claims, err := ValidateAccessToken(tokenStr, secret)
				if err == nil {
					// Cache lookup first — most requests within the short
					// cache TTL skip Postgres entirely. Mutating endpoints
					// call InvalidateCachedUser(uid) after writes so the
					// next request sees fresh data.
					user := getCachedUser(claims.UserID)
					if user == nil {
						user, err = pg.GetUserByID(ctx, claims.UserID)
						if err == nil && user != nil {
							putCachedUser(claims.UserID, user)
						}
					}
					if user != nil {
						ctx = context.WithValue(ctx, UserKey, user)
					}
				}
			}

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// GetUser retrieves the authenticated user from request context (nil if not logged in).
func GetUser(ctx context.Context) *models.User {
	u, _ := ctx.Value(UserKey).(*models.User)
	return u
}

// RequireAuth is a middleware that returns 401 if no authenticated user is present.
func RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if GetUser(r.Context()) == nil {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
