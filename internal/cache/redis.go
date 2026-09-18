package cache

import (
	"context"
	"log"

	"github.com/quizzzone/backend/internal/config"
	"github.com/quizzzone/backend/internal/pkg/notify"
	"github.com/redis/go-redis/v9"
)

var RDB *redis.Client
var ctx = context.Background()

func InitRedis() {
	cfg := config.AppConfig
	RDB = redis.NewClient(&redis.Options{
		Addr:     cfg.RedisAddr,
		Password: cfg.RedisPassword, // Use password from config
		DB:       0,                 // use default DB
	})

	// Test connection
	_, err := RDB.Ping(ctx).Result()
	if err != nil {
		notify.Fatal("redis_connect", "Không kết nối được Redis (%s): %v — container sẽ exit(1) và restart loop.",
			cfg.RedisAddr, err)
		log.Fatalf("Failed to connect to Redis: %v", err)
	}

	log.Println("Redis connection established successfully.")
}
