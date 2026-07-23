package cache

import (
	"context"
	"log"

	"github.com/kovio/backend/internal/config"
	"github.com/redis/go-redis/v9"
)

var RDB *redis.Client
var ctx = context.Background()

func InitRedis() {
	cfg := config.AppConfig
	RDB = redis.NewClient(&redis.Options{
		Addr:     cfg.RedisAddr,
		Password: cfg.RedisPassword, // Use password from config
		DB:       0,                // use default DB
	})

	// Test connection
	_, err := RDB.Ping(ctx).Result()
	if err != nil {
		log.Fatalf("Failed to connect to Redis: %v", err)
	}

	log.Println("Redis connection established successfully.")
}
