package repository

import (
	"agro/internal/domain"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

type ChatRepository struct {
	client *redis.Client
}

func NewRedisClient(client *redis.Client) *ChatRepository {
	return &ChatRepository{
		client: client,
	}
}

// HitOnce sets key with TTL if it doesn't exist yet.
// Returns (allowed=true) when key was created; otherwise allowed=false and ttlLeft.
func (r *ChatRepository) HitOnce(ctx context.Context, key string, ttl time.Duration) (allowed bool, ttlLeft time.Duration, err error) {
	ok, err := r.client.SetNX(ctx, key, "1", ttl).Result()
	if err != nil {
		return false, 0, err
	}
	if ok {
		return true, 0, nil
	}
	ttlLeft, err = r.client.TTL(ctx, key).Result()
	if err != nil {
		return false, 0, err
	}
	if ttlLeft < 0 {
		ttlLeft = 0
	}
	return false, ttlLeft, nil
}

// TTL returns remaining TTL (0 if none/expired).
func (r *ChatRepository) TTL(ctx context.Context, key string) (time.Duration, error) {
	d, err := r.client.TTL(ctx, key).Result()
	if err != nil {
		return 0, err
	}
	if d < 0 {
		return 0, nil
	}
	return d, nil
}

// User state methods
func (r *ChatRepository) SaveUserState(ctx context.Context, userID int64, state *domain.UserState) error {
	key := fmt.Sprintf("user_state:%d", userID)

	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("failed to marshal user state: %w", err)
	}

	// Set expiration to 24 hours
	err = r.client.Set(ctx, key, data, 24*time.Hour).Err()
	if err != nil {
		return fmt.Errorf("failed to save user state to redis: %w", err)
	}

	return nil
}

func (r *ChatRepository) GetUserState(ctx context.Context, userID int64) (*domain.UserState, error) {
	key := fmt.Sprintf("user_state:%d", userID)

	data, err := r.client.Get(ctx, key).Result()
	if err == redis.Nil {
		return nil, nil // Key doesn't exist
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get user state from redis: %w", err)
	}

	var state domain.UserState
	err = json.Unmarshal([]byte(data), &state)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal user state: %w", err)
	}

	return &state, nil
}

func (r *ChatRepository) DeleteUserState(ctx context.Context, userID int64) error {
	key := fmt.Sprintf("user_state:%d", userID)

	err := r.client.Del(ctx, key).Err()
	if err != nil {
		return fmt.Errorf("failed to delete user state from redis: %w", err)
	}

	return nil
}

// Admin state methods (using same UserState structure)
func (r *ChatRepository) SaveAdminState(ctx context.Context, adminID int64, state *domain.UserState) error {
	key := fmt.Sprintf("admin_state:%d", adminID)

	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("failed to marshal admin state: %w", err)
	}

	// Set expiration to 24 hours
	err = r.client.Set(ctx, key, data, 24*time.Hour).Err()
	if err != nil {
		return fmt.Errorf("failed to save admin state to redis: %w", err)
	}

	return nil
}

func (r *ChatRepository) GetAdminState(ctx context.Context, adminID int64) (*domain.UserState, error) {
	key := fmt.Sprintf("admin_state:%d", adminID)

	data, err := r.client.Get(ctx, key).Result()
	if err == redis.Nil {
		return nil, nil // Key doesn't exist
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get admin state from redis: %w", err)
	}

	var state domain.UserState
	err = json.Unmarshal([]byte(data), &state)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal admin state: %w", err)
	}

	return &state, nil
}

func (r *ChatRepository) DeleteAdminState(ctx context.Context, adminID int64) error {
	key := fmt.Sprintf("admin_state:%d", adminID)

	err := r.client.Del(ctx, key).Err()
	if err != nil {
		return fmt.Errorf("failed to delete admin state from redis: %w", err)
	}

	return nil
}

// Broadcast state methods
func (r *ChatRepository) SaveBroadcastState(ctx context.Context, adminID int64, broadcastType string) error {
	key := fmt.Sprintf("broadcast_state:%d", adminID)

	// Set expiration to 1 hour for broadcast states
	err := r.client.Set(ctx, key, broadcastType, time.Hour).Err()
	if err != nil {
		return fmt.Errorf("failed to save broadcast state to redis: %w", err)
	}

	return nil
}

func (r *ChatRepository) GetBroadcastState(ctx context.Context, adminID int64) (string, error) {
	key := fmt.Sprintf("broadcast_state:%d", adminID)

	data, err := r.client.Get(ctx, key).Result()
	if err == redis.Nil {
		return "", nil // Key doesn't exist
	}
	if err != nil {
		return "", fmt.Errorf("failed to get broadcast state from redis: %w", err)
	}

	return data, nil
}

func (r *ChatRepository) DeleteBroadcastState(ctx context.Context, adminID int64) error {
	key := fmt.Sprintf("broadcast_state:%d", adminID)

	err := r.client.Del(ctx, key).Err()
	if err != nil {
		return fmt.Errorf("failed to delete broadcast state from redis: %w", err)
	}

	return nil
}

// Helper method to clear all states for a user (useful for cleanup)
func (r *ChatRepository) ClearAllUserStates(ctx context.Context, userID int64) error {
	keys := []string{
		fmt.Sprintf("user_state:%d", userID),
		fmt.Sprintf("admin_state:%d", userID),
		fmt.Sprintf("broadcast_state:%d", userID),
	}

	err := r.client.Del(ctx, keys...).Err()
	if err != nil {
		return fmt.Errorf("failed to clear all user states from redis: %w", err)
	}

	return nil
}

// Health check method
func (r *ChatRepository) Ping(ctx context.Context) error {
	return r.client.Ping(ctx).Err()
}

func (r *ChatRepository) AddUser(ctx context.Context, userID int64) error {
	key := "chat:users"
	isMember, err := r.client.SIsMember(ctx, key, userID).Result()
	if err != nil {
		return fmt.Errorf("failed to check user membership: %w", err)
	}

	if !isMember {
		if err := r.client.SAdd(ctx, key, userID).Err(); err != nil {
			return fmt.Errorf("failed to add user to set: %w", err)
		}
	}

	return nil
}

func (r *ChatRepository) FindPartner(ctx context.Context, userID int64) (int64, error) {
	key := "chat:users"
	users, err := r.client.SMembers(ctx, key).Result()
	if err != nil {
		return 0, fmt.Errorf("failed to get users from set: %w", err)
	}
	for _, user := range users {
		partnerID := user
		if partnerID != fmt.Sprintf("%d", userID) {
			if err := r.client.SRem(ctx, key, partnerID).Err(); err != nil {
				return 0, fmt.Errorf("failed to remove partner from set: %w", err)
			}
			return parseInt64(partnerID), nil
		}
	}
	return 0, nil
}

func (r *ChatRepository) SetPartner(ctx context.Context, userID, partnerID int64) error {
	key := fmt.Sprintf("chat:partner:%d", userID)
	if err := r.client.Set(ctx, key, partnerID, 0).Err(); err != nil {
		return fmt.Errorf("failed to set partner: %w", err)
	}
	return nil
}

func (r *ChatRepository) GetUserPartner(ctx context.Context, userID int64) (int64, error) {
	key := fmt.Sprintf("chat:partner:%d", userID)
	partnerID, err := r.client.Get(ctx, key).Result()
	if err == redis.Nil {
		return 0, nil // No partner
	} else if err != nil {
		return 0, fmt.Errorf("failed to get partner: %w", err)
	}
	return parseInt64(partnerID), nil
}

func (r *ChatRepository) RemoveUser(ctx context.Context, userID int64) error {
	// Remove user from set
	keyUsers := "chat:users"
	if err := r.client.SRem(ctx, keyUsers, userID).Err(); err != nil {
		return fmt.Errorf("failed to remove user from set: %w", err)
	}

	// Remove partner mapping
	keyPartner := fmt.Sprintf("chat:partner:%d", userID)
	if err := r.client.Del(ctx, keyPartner).Err(); err != nil {
		return fmt.Errorf("failed to delete partner mapping: %w", err)
	}

	return nil
}

func (r *ChatRepository) GetUsers(ctx context.Context) ([]int64, error) {
	key := "chat:users"
	users, err := r.client.SMembers(ctx, key).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get users from set: %w", err)
	}

	var userIDs []int64
	for _, user := range users {
		userIDs = append(userIDs, parseInt64(user))
	}
	return userIDs, nil
}

func (r *ChatRepository) CheckPartnerToEmpty(ctx context.Context, userID int64) (bool, error) {
	key := fmt.Sprintf("chat:partner:%d", userID)
	exists, err := r.client.Exists(ctx, key).Result()
	if err != nil {
		return false, fmt.Errorf("failed to check partner existence: %w", err)
	}
	return exists > 0, nil
}

func parseInt64(s string) int64 {
	var id int64
	fmt.Sscanf(s, "%d", &id)
	return id
}
