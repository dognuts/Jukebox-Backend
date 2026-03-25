package store

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"time"

	"github.com/google/uuid"
	"github.com/jukebox/backend/internal/models"
	"github.com/redis/go-redis/v9"
)

var avatarColors = []string{
	"oklch(0.70 0.18 30)",
	"oklch(0.65 0.20 250)",
	"oklch(0.72 0.15 150)",
	"oklch(0.68 0.22 80)",
	"oklch(0.60 0.18 320)",
	"oklch(0.75 0.12 200)",
}

var adjectives = []string{
	"Cosmic", "Neon", "Velvet", "Electric", "Midnight", "Golden",
	"Crystal", "Stellar", "Sonic", "Lunar", "Vivid", "Dreamy",
	"Mellow", "Funky", "Groovy", "Astral", "Hazy", "Radiant",
}

var nouns = []string{
	"Listener", "Vibe", "Beat", "Wave", "Echo", "Pulse",
	"Rhythm", "Note", "Melody", "Tune", "Groove", "Flow",
	"Drift", "Spark", "Glow", "Haze", "Breeze", "Storm",
}

type RedisStore struct {
	client     *redis.Client
	sessionTTL time.Duration
}

func NewRedisStore(redisURL string, sessionTTL time.Duration) (*RedisStore, error) {
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("parse redis URL: %w", err)
	}
	client := redis.NewClient(opts)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("redis ping: %w", err)
	}

	return &RedisStore{client: client, sessionTTL: sessionTTL}, nil
}

func (r *RedisStore) Close() error {
	return r.client.Close()
}

func (r *RedisStore) Client() *redis.Client {
	return r.client
}

// ==================== Sessions ====================

func (r *RedisStore) CreateSession(ctx context.Context) (*models.Session, error) {
	s := &models.Session{
		ID:          uuid.New().String(),
		DisplayName: generateDisplayName(),
		AvatarColor: avatarColors[rand.Intn(len(avatarColors))],
		CreatedAt:   time.Now(),
		ExpiresAt:   time.Now().Add(r.sessionTTL),
	}

	data, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}

	key := "session:" + s.ID
	if err := r.client.Set(ctx, key, data, r.sessionTTL).Err(); err != nil {
		return nil, err
	}
	return s, nil
}

func (r *RedisStore) GetSession(ctx context.Context, id string) (*models.Session, error) {
	data, err := r.client.Get(ctx, "session:"+id).Bytes()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s models.Session
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func (r *RedisStore) UpdateSessionName(ctx context.Context, id, displayName string) error {
	key := "session:" + id
	// Use a transaction to prevent race conditions with UpdateSessionUser
	return r.updateSessionField(ctx, key, func(s *models.Session) {
		s.DisplayName = displayName
	})
}

func (r *RedisStore) RefreshSession(ctx context.Context, id string) error {
	return r.client.Expire(ctx, "session:"+id, r.sessionTTL).Err()
}

// UpdateSessionUser links an anonymous session to a registered user.
func (r *RedisStore) UpdateSessionUser(ctx context.Context, sessionID, userID string) error {
	key := "session:" + sessionID
	return r.updateSessionField(ctx, key, func(s *models.Session) {
		s.UserID = userID
	})
}

// updateSessionField atomically updates a session using Redis WATCH/MULTI/EXEC.
func (r *RedisStore) updateSessionField(ctx context.Context, key string, mutate func(*models.Session)) error {
	// Retry up to 3 times on WATCH conflicts
	for i := 0; i < 3; i++ {
		err := r.client.Watch(ctx, func(tx *redis.Tx) error {
			data, err := tx.Get(ctx, key).Bytes()
			if err != nil {
				return fmt.Errorf("session not found")
			}
			var s models.Session
			if err := json.Unmarshal(data, &s); err != nil {
				return err
			}

			mutate(&s)

			newData, err := json.Marshal(s)
			if err != nil {
				return err
			}

			ttl := time.Until(s.ExpiresAt)
			if ttl <= 0 {
				ttl = r.sessionTTL
			}

			_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				pipe.Set(ctx, key, newData, ttl)
				return nil
			})
			return err
		}, key)

		if err == nil {
			return nil
		}
		if err == redis.TxFailedErr {
			continue // retry on WATCH conflict
		}
		return err
	}
	return fmt.Errorf("session update failed after retries")
}

// ==================== Playback State ====================

func (r *RedisStore) SetPlaybackState(ctx context.Context, state *models.PlaybackState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return r.client.Set(ctx, "playback:"+state.RoomID, data, 0).Err()
}

func (r *RedisStore) GetPlaybackState(ctx context.Context, roomID string) (*models.PlaybackState, error) {
	data, err := r.client.Get(ctx, "playback:"+roomID).Bytes()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s models.PlaybackState
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func (r *RedisStore) ClearPlaybackState(ctx context.Context, roomID string) error {
	return r.client.Del(ctx, "playback:"+roomID).Err()
}

// ==================== Listener Counts ====================

func (r *RedisStore) AddListener(ctx context.Context, roomID, sessionID string) (int64, error) {
	key := "listeners:" + roomID
	r.client.SAdd(ctx, key, sessionID)
	return r.client.SCard(ctx, key).Result()
}

func (r *RedisStore) RemoveListener(ctx context.Context, roomID, sessionID string) (int64, error) {
	key := "listeners:" + roomID
	r.client.SRem(ctx, key, sessionID)
	return r.client.SCard(ctx, key).Result()
}

func (r *RedisStore) GetListenerCount(ctx context.Context, roomID string) (int64, error) {
	return r.client.SCard(ctx, "listeners:"+roomID).Result()
}

func (r *RedisStore) ClearListeners(ctx context.Context, roomID string) error {
	return r.client.Del(ctx, "listeners:"+roomID).Err()
}

// ==================== Pub/Sub ====================

func (r *RedisStore) Publish(ctx context.Context, channel string, payload interface{}) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return r.client.Publish(ctx, channel, data).Err()
}

func (r *RedisStore) Subscribe(ctx context.Context, channel string) *redis.PubSub {
	return r.client.Subscribe(ctx, channel)
}

// ==================== Helpers ====================

func generateDisplayName() string {
	adj := adjectives[rand.Intn(len(adjectives))]
	noun := nouns[rand.Intn(len(nouns))]
	num := rand.Intn(99) + 1
	return fmt.Sprintf("%s%s%d", adj, noun, num)
}
