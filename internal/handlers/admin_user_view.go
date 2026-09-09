package handlers

import (
	"time"

	"github.com/jukebox/backend/internal/models"
)

// adminUserView is what /api/admin/users returns: every public User field
// plus the signup forensics that models.User deliberately hides from
// non-admin responses.
type adminUserView struct {
	models.User
	SignupIP        string     `json:"signupIp"`
	SignupUserAgent string     `json:"signupUserAgent"`
	VerifiedAt      *time.Time `json:"verifiedAt"`
	VerifyHeldAt    *time.Time `json:"verifyHeldAt"`
	// SecondsToVerify is verified_at - created_at, rounded down. nil when
	// the account never clicked a verification link.
	SecondsToVerify *int64 `json:"secondsToVerify"`
}

func newAdminUserView(u models.User) adminUserView {
	v := adminUserView{
		User:            u,
		SignupIP:        u.SignupIP,
		SignupUserAgent: u.SignupUserAgent,
		VerifiedAt:      u.VerifiedAt,
		VerifyHeldAt:    u.VerifyHeldAt,
	}
	if u.VerifiedAt != nil {
		secs := int64(u.VerifiedAt.Sub(u.CreatedAt) / time.Second)
		v.SecondsToVerify = &secs
	}
	return v
}

func newAdminUserViews(users []models.User) []adminUserView {
	out := make([]adminUserView, 0, len(users))
	for _, u := range users {
		out = append(out, newAdminUserView(u))
	}
	return out
}
