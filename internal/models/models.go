package models

import (
	"time"
)

// ---------- Enums ----------

type TrackSource string

const (
	TrackSourceYouTube    TrackSource = "youtube"
	TrackSourceSoundCloud TrackSource = "soundcloud"
	TrackSourceMP3        TrackSource = "mp3"
)

type RequestPolicy string

const (
	RequestPolicyClosed   RequestPolicy = "closed"
	RequestPolicyOpen     RequestPolicy = "open"
	RequestPolicyApproval RequestPolicy = "approval"
)

type ChatMessageType string

const (
	ChatTypeMessage      ChatMessageType = "message"
	ChatTypeRequest      ChatMessageType = "request"
	ChatTypeAnnouncement ChatMessageType = "announcement"
)

type QueueEntryStatus string

const (
	QueuePending  QueueEntryStatus = "pending"  // awaiting DJ approval
	QueueApproved QueueEntryStatus = "approved" // in the queue
	QueueRejected QueueEntryStatus = "rejected"
	QueuePlayed   QueueEntryStatus = "played"
)

// ---------- Core Models ----------

type Room struct {
	ID             string        `json:"id"`
	Slug           string        `json:"slug"`
	Name           string        `json:"name"`
	Description    string        `json:"description"`
	Genre          string        `json:"genre"`
	Vibes          []string      `json:"vibes"`
	CoverGradient  string        `json:"coverGradient"`
	CoverArtURL    string        `json:"coverArt,omitempty"`
	RequestPolicy  RequestPolicy `json:"requestPolicy"`
	IsLive         bool          `json:"isLive"`
	IsOfficial     bool          `json:"isOfficial"`
	DJKeyHash      string        `json:"-"` // never expose; bcrypt hash of the DJ key
	DJSessionID    string        `json:"-"` // session that created / controls the room
	CreatedAt      time.Time     `json:"createdAt"`
	ScheduledStart *time.Time    `json:"scheduledStart,omitempty"`
	LastActiveAt   *time.Time    `json:"lastActive,omitempty"`
	EndedAt        *time.Time    `json:"endedAt,omitempty"`
	ExpiresAt      *time.Time    `json:"expiresAt,omitempty"`
	IsFeatured     bool          `json:"isFeatured"`

	CreatorUserID  string `json:"creatorUserId"`
	IsAutoplay     bool   `json:"isAutoplay"`

	// Populated at query time, not stored in rooms table
	ListenerCount  int    `json:"listenerCount"`
	DJDisplayName  string `json:"djName"`
	DJAvatarColor  string `json:"djAvatarColor"`
}

type Track struct {
	ID            string      `json:"id"`
	Title         string      `json:"title"`
	Artist        string      `json:"artist"`
	Duration      int         `json:"duration"` // seconds
	Source        TrackSource `json:"source"`
	SourceURL     string      `json:"sourceUrl"`
	AlbumGradient string      `json:"albumGradient"`
	InfoSnippet   string      `json:"infoSnippet,omitempty"`
	CreatedAt     time.Time   `json:"createdAt"`
}

type QueueEntry struct {
	ID          string           `json:"id"`
	RoomID      string           `json:"roomId"`
	Track       Track            `json:"track"`
	SubmittedBy string           `json:"submittedBy"` // display name
	SessionID   string           `json:"-"`            // anonymous session that submitted
	Status      QueueEntryStatus `json:"status"`
	Position    int              `json:"position"`
	CreatedAt   time.Time        `json:"createdAt"`
}

type ChatMessage struct {
	ID          string          `json:"id"`
	RoomID      string          `json:"roomId"`
	SessionID   string          `json:"-"`
	Username    string          `json:"username"`
	AvatarColor string          `json:"avatarColor"`
	Message     string          `json:"message"`
	Type        ChatMessageType `json:"type"`
	Timestamp   time.Time       `json:"timestamp"`
}

// ---------- Session (anonymous identity) ----------

type Session struct {
	ID          string    `json:"id"`
	DisplayName string    `json:"displayName"`
	AvatarColor string    `json:"avatarColor"`
	UserID      string    `json:"userId,omitempty"` // set if logged in
	CreatedAt   time.Time `json:"createdAt"`
	ExpiresAt   time.Time `json:"expiresAt"`
}

// ---------- User (registered account) ----------

type User struct {
	ID             string    `json:"id"`
	Email          string    `json:"email"`
	EmailVerified  bool      `json:"emailVerified"`
	PasswordHash   string    `json:"-"`
	DisplayName    string    `json:"displayName"`
	AvatarColor    string    `json:"avatarColor"`
	AvatarURL      string    `json:"avatarUrl,omitempty"`
	Bio            string    `json:"bio"`
	FavoriteGenres []string  `json:"favoriteGenres"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
	IsAdmin        bool      `json:"isAdmin"`
	City           string    `json:"city"`
	Region         string    `json:"region"`
	Country        string    `json:"country"`
	StageName      string    `json:"stageName"`
	IsPlus         bool      `json:"isPlus"`
	PlusSince      *time.Time `json:"plusSince,omitempty"`
	PlusExpiresAt  *time.Time `json:"plusExpiresAt,omitempty"`
	NeonBalance    int       `json:"neonBalance"`
	StripeCustomerID string  `json:"-"`
	IsBanned         bool    `json:"isBanned"`
}

type EmailVerification struct {
	ID        string     `json:"id"`
	UserID    string     `json:"userId"`
	Token     string     `json:"-"`
	ExpiresAt time.Time  `json:"expiresAt"`
	UsedAt    *time.Time `json:"usedAt,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
}

type PasswordReset struct {
	ID        string     `json:"id"`
	UserID    string     `json:"userId"`
	Token     string     `json:"-"`
	ExpiresAt time.Time  `json:"expiresAt"`
	UsedAt    *time.Time `json:"usedAt,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
}

type RefreshToken struct {
	ID        string     `json:"id"`
	UserID    string     `json:"userId"`
	TokenHash string     `json:"-"`
	ExpiresAt time.Time  `json:"expiresAt"`
	RevokedAt *time.Time `json:"revokedAt,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
}

// ---------- Direct Messages ----------

type DirectMessage struct {
	ID         string     `json:"id"`
	FromUserID string     `json:"fromUserId"`
	ToUserID   string     `json:"toUserId"`
	Message    string     `json:"message"`
	ReadAt     *time.Time `json:"readAt,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
	// Populated at query time
	FromDisplayName string `json:"fromDisplayName,omitempty"`
	FromAvatarColor string `json:"fromAvatarColor,omitempty"`
}

type ConversationSummary struct {
	UserID      string    `json:"userId"`
	DisplayName string    `json:"displayName"`
	AvatarColor string    `json:"avatarColor"`
	LastMessage string    `json:"lastMessage"`
	LastAt      time.Time `json:"lastAt"`
	UnreadCount int       `json:"unreadCount"`
}

// ---------- Listen Tracking ----------

type ListenEvent struct {
	ID              string     `json:"id"`
	UserID          string     `json:"userId"`
	RoomID          string     `json:"roomId"`
	StartedAt       time.Time  `json:"startedAt"`
	EndedAt         *time.Time `json:"endedAt,omitempty"`
	DurationSeconds int        `json:"durationSeconds"`
	TracksHeard     int        `json:"tracksHeard"`
}

type UserStats struct {
	TotalListenMinutes int `json:"totalListenMinutes"`
	RoomsVisited       int `json:"roomsVisited"`
	TracksListened     int `json:"tracksListened"`
}

type FavoriteRoom struct {
	RoomID      string `json:"roomId"`
	RoomName    string `json:"roomName"`
	RoomSlug    string `json:"roomSlug"`
	RoomGenre   string `json:"roomGenre"`
	CoverArtURL string `json:"coverArtUrl"`
	ListenMinutes int  `json:"listenMinutes"`
	VisitCount  int    `json:"visitCount"`
}

// ---------- Playlists ----------

type Playlist struct {
	ID         string          `json:"id"`
	UserID     string          `json:"userId"`
	Name       string          `json:"name"`
	IsLiked    bool            `json:"isLiked"`
	TrackCount int             `json:"trackCount"`
	Tracks     []PlaylistTrack `json:"tracks,omitempty"`
	CreatedAt  time.Time       `json:"createdAt"`
	UpdatedAt  time.Time       `json:"updatedAt"`
}

type PlaylistTrack struct {
	ID            string `json:"id"`
	TrackID       string `json:"trackId"`
	Position      int    `json:"position"`
	AddedAt       string `json:"addedAt"`
	// Joined from tracks table
	Title         string `json:"title,omitempty"`
	Artist        string `json:"artist,omitempty"`
	Duration      int    `json:"duration,omitempty"`
	Source        string `json:"source,omitempty"`
	SourceUrl     string `json:"sourceUrl,omitempty"`
	AlbumGradient string `json:"albumGradient,omitempty"`
}

// ---------- Playback State (lives in Redis) ----------

type PlaybackState struct {
	RoomID        string `json:"roomId"`
	TrackID       string `json:"trackId"`
	StartedAtUnix int64  `json:"startedAt"` // unix millis when track began playing
	IsPlaying     bool   `json:"isPlaying"`
	PausePosition int    `json:"pausePosition"` // seconds into track when paused
}

// ---------- API Request/Response types ----------

type CreateRoomRequest struct {
	Name           string   `json:"name"`
	Description    string   `json:"description"`
	Genre          string   `json:"genre"`
	Vibes          []string `json:"vibes"`
	RequestPolicy  string   `json:"requestPolicy"`
	ScheduledStart string   `json:"scheduledStart,omitempty"` // ISO 8601
	CoverArt       string   `json:"coverArt,omitempty"`       // data URL or external URL
	CoverGradient  string   `json:"coverGradient,omitempty"`
	PlaylistID     string   `json:"playlistId,omitempty"`     // pre-load queue from a user tracklist
}

type CreateRoomResponse struct {
	Room  Room   `json:"room"`
	DJKey string `json:"djKey"` // only returned on creation
}

type SubmitTrackRequest struct {
	Title     string `json:"title"`
	Artist    string `json:"artist"`
	Duration  int    `json:"duration"`
	Source    string `json:"source"`
	SourceURL string `json:"sourceUrl"`
}

type UpdateDisplayNameRequest struct {
	DisplayName string `json:"displayName"`
}

type RoomDetailResponse struct {
	Room          Room          `json:"room"`
	NowPlaying    *Track        `json:"nowPlaying"`
	Queue         []QueueEntry  `json:"queue"`
	RecentChat    []ChatMessage `json:"recentChat"`
	PlaybackState PlaybackState `json:"playbackState"`
}

type GoLiveRequest struct {
	TrackTitle     string `json:"trackTitle"`
	TrackArtist    string `json:"trackArtist"`
	TrackDuration  int    `json:"trackDuration"`
	TrackSource    string `json:"trackSource"`
	TrackSourceURL string `json:"trackSourceUrl"`
}

// ---------- Auth Request/Response types ----------

type SignupRequest struct {
	Email        string `json:"email"`
	Password     string `json:"password"`
	DisplayName  string `json:"displayName"`
	StageName    string `json:"stageName"`
	CaptchaToken string `json:"captchaToken"`
	Website      string `json:"website"` // honeypot — must be empty
}

type LoginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type AuthResponse struct {
	User         User   `json:"user"`
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
}

type ForgotPasswordRequest struct {
	Email string `json:"email"`
}

type ResetPasswordRequest struct {
	Token       string `json:"token"`
	NewPassword string `json:"password"`
}

type ChangePasswordRequest struct {
	CurrentPassword string `json:"currentPassword"`
	NewPassword     string `json:"newPassword"`
}

type UpdateProfileRequest struct {
	DisplayName    *string  `json:"displayName,omitempty"`
	Bio            *string  `json:"bio,omitempty"`
	FavoriteGenres []string `json:"favoriteGenres,omitempty"`
	AvatarColor    *string  `json:"avatarColor,omitempty"`
	StageName      *string  `json:"stageName,omitempty"`
}

type RefreshTokenRequest struct {
	RefreshToken string `json:"refreshToken"`
}

// ---------- Monetization: Plus, DJ Subs, Neon ----------

// Neon Pack definitions (not in DB — static config)
type NeonPack struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	NeonAmount int    `json:"neonAmount"`
	PriceCents int    `json:"priceCents"`
	BonusPct   int    `json:"bonusPct,omitempty"`
}

var NeonPacks = []NeonPack{
	{ID: "starter", Name: "Starter", NeonAmount: 100, PriceCents: 199},
	{ID: "popular", Name: "Popular", NeonAmount: 500, PriceCents: 799, BonusPct: 10},
	{ID: "mega", Name: "Mega", NeonAmount: 1200, PriceCents: 1499, BonusPct: 25},
	{ID: "ultra", Name: "Ultra", NeonAmount: 3000, PriceCents: 2999, BonusPct: 50},
}

type Subscription struct {
	ID                  string     `json:"id"`
	UserID              string     `json:"userId"`
	Type                string     `json:"type"`    // "plus" or "dj_sub"
	TargetUserID        *string    `json:"targetUserId,omitempty"`
	PriceCents          int        `json:"priceCents"`
	Status              string     `json:"status"`  // active, cancelled, expired
	StripeSubID         string     `json:"-"`
	CurrentPeriodStart  time.Time  `json:"currentPeriodStart"`
	CurrentPeriodEnd    time.Time  `json:"currentPeriodEnd"`
	CancelledAt         *time.Time `json:"cancelledAt,omitempty"`
	CreatedAt           time.Time  `json:"createdAt"`
}

type DJSubSettings struct {
	UserID     string    `json:"userId"`
	PriceCents int       `json:"priceCents"`
	IsEnabled  bool      `json:"isEnabled"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

type NeonTube struct {
	RoomID     string    `json:"roomId"`
	Level      int       `json:"level"`
	FillAmount int       `json:"fillAmount"`
	FillTarget int       `json:"fillTarget"`
	TotalNeon  int64     `json:"totalNeon"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// Tube level config
var TubeLevels = []struct {
	Level      int    `json:"level"`
	Color      string `json:"color"`
	FillTarget int    `json:"fillTarget"`
}{
	{1, "cyan", 100},
	{2, "magenta", 250},
	{3, "amber", 500},
	{4, "rainbow", 1000},
	{5, "supernova", 2000},
}

type NeonPurchase struct {
	ID              string    `json:"id"`
	UserID          string    `json:"userId"`
	PackID          string    `json:"packId"`
	NeonAmount      int       `json:"neonAmount"`
	PriceCents      int       `json:"priceCents"`
	StripePaymentID string    `json:"-"`
	CreatedAt       time.Time `json:"createdAt"`
}

type NeonTransaction struct {
	ID           string    `json:"id"`
	FromUserID   string    `json:"fromUserId"`
	ToRoomID     string    `json:"toRoomId"`
	ToDJUserID   *string   `json:"toDjUserId,omitempty"`
	Amount       int       `json:"amount"`
	CreatedAt    time.Time `json:"createdAt"`
}

type CreatorPoolMonth struct {
	ID                    string     `json:"id"`
	Month                 string     `json:"month"`
	TotalPlusRevenueCents int        `json:"totalPlusRevenueCents"`
	PoolPct               int        `json:"poolPct"`
	PoolAmountCents       int        `json:"poolAmountCents"`
	TotalPlusMinutes      int64      `json:"totalPlusMinutes"`
	ComputedAt            *time.Time `json:"computedAt,omitempty"`
}

type CreatorPoolAllocation struct {
	ID             string `json:"id"`
	MonthID        string `json:"monthId"`
	CreatorUserID  string `json:"creatorUserId"`
	ListenMinutes  int64  `json:"listenMinutes"`
	SharePct       float64 `json:"sharePct"`
	EarningsCents  int    `json:"earningsCents"`
}

// ---------- Autoplay ----------

type AutoplayTrack struct {
	Title         string `json:"title"`
	Artist        string `json:"artist"`
	Duration      int    `json:"duration"`
	Source        string `json:"source"`
	SourceURL     string `json:"sourceUrl"`
	AlbumGradient string `json:"albumGradient,omitempty"`
	InfoSnippet   string `json:"infoSnippet,omitempty"`
}

type AutoplayPlaylist struct {
	ID           string          `json:"id"`
	RoomID       string          `json:"roomId"`
	Status       string          `json:"status"` // "live" or "staged"
	Name         string          `json:"name"`
	Tracks       []AutoplayTrack `json:"tracks"`
	CurrentIndex int             `json:"currentIndex"`
	CreatedAt    time.Time       `json:"createdAt"`
	ActivatedAt  *time.Time      `json:"activatedAt,omitempty"`
}
