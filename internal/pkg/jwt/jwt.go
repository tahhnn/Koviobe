package jwt

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	jwtv5 "github.com/golang-jwt/jwt/v5"
	"github.com/quizzzone/backend/internal/cache"
	"github.com/quizzzone/backend/internal/config"
)

// Claims for authenticated Host/Admin users.
type Claims struct {
	UserID      uint     `json:"user_id"`
	Email       string   `json:"email"`
	Role        string   `json:"role"`
	Permissions []string `json:"permissions"`
	TokenType   string   `json:"token_type"`
	jwtv5.RegisteredClaims
}

// PlayerClaims for anonymous players who joined a room.
type PlayerClaims struct {
	PlayerID  uint   `json:"player_id"`
	RoomID    uint   `json:"room_id"`
	Nickname  string `json:"nickname"`
	TokenType string `json:"token_type"`
	jwtv5.RegisteredClaims
}

func hs256KeyFunc(secret []byte) jwtv5.Keyfunc {
	return func(token *jwtv5.Token) (interface{}, error) {
		if token.Method != jwtv5.SigningMethodHS256 {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return secret, nil
	}
}

func newJTI() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func GenerateToken(userID uint, email string, role string, permissions []string) (string, error) {
	cfg := config.AppConfig
	claims := Claims{
		UserID:      userID,
		Email:       email,
		Role:        role,
		Permissions: permissions,
		TokenType:   "access",
		RegisteredClaims: jwtv5.RegisteredClaims{
			ExpiresAt: jwtv5.NewNumericDate(time.Now().Add(15 * time.Minute)),
			IssuedAt:  jwtv5.NewNumericDate(time.Now()),
		},
	}

	token := jwtv5.NewWithClaims(jwtv5.SigningMethodHS256, claims)
	return token.SignedString([]byte(cfg.JWTSecret))
}

// GenerateRefreshToken creates a refresh token and stores its JTI in Redis (7-day TTL).
// Old tokens are invalidated when RotateRefreshToken / RevokeRefreshToken is used.
func GenerateRefreshToken(userID uint) (string, error) {
	cfg := config.AppConfig
	jti, err := newJTI()
	if err != nil {
		return "", err
	}

	ttl := 7 * 24 * time.Hour
	claims := Claims{
		UserID:    userID,
		TokenType: "refresh",
		RegisteredClaims: jwtv5.RegisteredClaims{
			ID:        jti,
			ExpiresAt: jwtv5.NewNumericDate(time.Now().Add(ttl)),
			IssuedAt:  jwtv5.NewNumericDate(time.Now()),
		},
	}

	token := jwtv5.NewWithClaims(jwtv5.SigningMethodHS256, claims)
	tokenString, err := token.SignedString([]byte(cfg.JWTSecret))
	if err != nil {
		return "", err
	}

	ctx := context.Background()
	key := refreshKey(jti)
	if err := cache.RDB.Set(ctx, key, fmt.Sprintf("%d", userID), ttl).Err(); err != nil {
		return "", fmt.Errorf("failed to store refresh token: %w", err)
	}

	return tokenString, nil
}

func refreshKey(jti string) string {
	return "refresh:" + jti
}

// RevokeRefreshToken removes a refresh token JTI from Redis (logout / rotation).
func RevokeRefreshToken(tokenString string) {
	claims, err := parseRefreshClaims(tokenString)
	if err != nil || claims.ID == "" {
		return
	}
	_ = cache.RDB.Del(context.Background(), refreshKey(claims.ID)).Err()
}

func parseRefreshClaims(tokenString string) (*Claims, error) {
	cfg := config.AppConfig
	token, err := jwtv5.ParseWithClaims(tokenString, &Claims{}, hs256KeyFunc([]byte(cfg.JWTSecret)))
	if err != nil {
		return nil, err
	}
	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, errors.New("invalid refresh token")
	}
	if claims.TokenType != "refresh" {
		return nil, errors.New("invalid token type: expected refresh token")
	}
	return claims, nil
}

func VerifyToken(tokenString string) (*Claims, error) {
	cfg := config.AppConfig
	token, err := jwtv5.ParseWithClaims(tokenString, &Claims{}, hs256KeyFunc([]byte(cfg.JWTSecret)))
	if err != nil {
		return nil, err
	}

	if claims, ok := token.Claims.(*Claims); ok && token.Valid {
		if claims.TokenType != "access" {
			return nil, errors.New("invalid token type: expected access token")
		}
		return claims, nil
	}
	return nil, errors.New("invalid token")
}

// VerifyRefreshToken validates signature, type, and that the JTI is still active in Redis.
func VerifyRefreshToken(tokenString string) (*Claims, error) {
	claims, err := parseRefreshClaims(tokenString)
	if err != nil {
		return nil, err
	}
	if claims.ID == "" {
		return nil, errors.New("refresh token missing jti")
	}

	ctx := context.Background()
	val, err := cache.RDB.Get(ctx, refreshKey(claims.ID)).Result()
	if err != nil || val == "" {
		return nil, errors.New("refresh token revoked or expired")
	}

	return claims, nil
}

// GeneratePlayerToken creates a shorter-lived JWT for an anonymous player (4 hours).
func GeneratePlayerToken(playerID uint, roomID uint, nickname string) (string, error) {
	cfg := config.AppConfig
	claims := PlayerClaims{
		PlayerID:  playerID,
		RoomID:    roomID,
		Nickname:  nickname,
		TokenType: "player",
		RegisteredClaims: jwtv5.RegisteredClaims{
			ExpiresAt: jwtv5.NewNumericDate(time.Now().Add(4 * time.Hour)),
			IssuedAt:  jwtv5.NewNumericDate(time.Now()),
		},
	}

	token := jwtv5.NewWithClaims(jwtv5.SigningMethodHS256, claims)
	return token.SignedString([]byte(cfg.JWTSecret))
}

func VerifyPlayerToken(tokenString string) (*PlayerClaims, error) {
	cfg := config.AppConfig
	token, err := jwtv5.ParseWithClaims(tokenString, &PlayerClaims{}, hs256KeyFunc([]byte(cfg.JWTSecret)))
	if err != nil {
		return nil, err
	}

	if claims, ok := token.Claims.(*PlayerClaims); ok && token.Valid {
		if claims.TokenType != "player" {
			return nil, errors.New("invalid token type: expected player token")
		}
		return claims, nil
	}
	return nil, errors.New("invalid player token")
}
