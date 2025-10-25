package database

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// Existing CreateTables function remains the same...

// ConnectRedis creates a new Redis client connection
func ConnectRedis(ctx context.Context, logger *zap.Logger) (*redis.Client, error) {
	// Redis connection options matching your docker-compose
	rdb := redis.NewClient(&redis.Options{
		Addr:         "localhost:6379", // Redis server address
		Password:     "",               // No password set
		DB:           0,                // Use default DB
		DialTimeout:  5 * time.Second,  // Connection timeout
		ReadTimeout:  3 * time.Second,  // Read timeout
		WriteTimeout: 3 * time.Second,  // Write timeout
		PoolSize:     10,               // Connection pool size
		MinIdleConns: 2,                // Minimum idle connections
	})

	// Test the connection
	_, err := rdb.Ping(ctx).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to connect to Redis: %w", err)
	}

	logger.Info("Successfully connected to Redis",
		zap.String("addr", "localhost:6379"),
		zap.Int("db", 0))

	return rdb, nil
}

// CloseRedis gracefully closes Redis connection
func CloseRedis(rdb *redis.Client, logger *zap.Logger) {
	if err := rdb.Close(); err != nil {
		logger.Error("Failed to close Redis connection", zap.Error(err))
	} else {
		logger.Info("Redis connection closed successfully")
	}
}
