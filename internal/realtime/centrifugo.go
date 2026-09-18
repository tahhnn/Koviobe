package realtime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/quizzzone/backend/internal/config"
	"github.com/quizzzone/backend/internal/pkg/notify"
)

type CentrifugoClient struct {
	apiURL string
	apiKey string
	secret string
}

var Client *CentrifugoClient

// Publishes happen on every question transition and every answer submission, so
// the client and its connection pool are shared. Building an http.Client per
// call left every publish paying a fresh TCP handshake with no keep-alive reuse.
var publishHTTPClient = &http.Client{
	Timeout: 5 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 100,
		IdleConnTimeout:     90 * time.Second,
	},
}

func InitCentrifugo() {
	cfg := config.AppConfig
	Client = &CentrifugoClient{
		apiURL: strings.TrimRight(cfg.CentrifugoAPIURL, "/"),
		apiKey: cfg.CentrifugoAPIKey,
		secret: cfg.CentrifugoSecret,
	}
}

// GenerateConnectionToken generates a JWT connection token for Centrifugo clients.
// channels lists room channels the client may subscribe to (required when
// allow_subscribe_for_client is false).
func (c *CentrifugoClient) GenerateConnectionToken(userID string, ttlSeconds int64, channels ...string) (string, error) {
	claims := jwt.MapClaims{
		"sub": userID,
	}
	if ttlSeconds > 0 {
		claims["exp"] = time.Now().Add(time.Duration(ttlSeconds) * time.Second).Unix()
	}
	if len(channels) > 0 {
		claims["channels"] = channels
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenString, err := token.SignedString([]byte(c.secret))
	if err != nil {
		return "", fmt.Errorf("failed to sign token: %v", err)
	}

	return tokenString, nil
}

// RoomChannel returns the Centrifugo channel name for a room PIN.
func RoomChannel(pinCode string) string {
	return fmt.Sprintf("rooms:%s", pinCode)
}

// HostChannel carries events only the host consumes.
//
// It sits inside the same "rooms" namespace so it inherits that namespace's
// config (presence, join_leave, allow_subscribe_for_client=false) — a channel
// outside it would fall back to Centrifugo's defaults and let any client
// subscribe.
//
// A host token must be minted for BOTH this and RoomChannel; granting only one
// fails silently, with the socket connected and the events simply never
// arriving. See GetRealtimeToken in internal/handler/auth.go.
func HostChannel(pinCode string) string {
	return fmt.Sprintf("rooms:%s:host", pinCode)
}

// GameEvent defines the structure of real-time messages sent to clients
type GameEvent struct {
	Event   string      `json:"event"`
	Payload interface{} `json:"payload"`
}

func (c *CentrifugoClient) publishURL() string {
	base := c.apiURL
	if strings.HasSuffix(base, "/publish") {
		return base
	}
	// CENTRIFUGO_API_URL is usually http://host:8000/api
	if strings.HasSuffix(base, "/api") {
		return base + "/publish"
	}
	return base + "/api/publish"
}

// Publish sends an event to a Centrifugo channel (Centrifugo v5 HTTP API).
func (c *CentrifugoClient) Publish(channel string, event string, payload interface{}) error {
	if c == nil {
		return fmt.Errorf("centrifugo client not initialized")
	}

	gameEvent := GameEvent{
		Event:   event,
		Payload: payload,
	}

	body, err := json.Marshal(map[string]interface{}{
		"channel": channel,
		"data":    gameEvent,
	})
	if err != nil {
		log.Printf("[Centrifugo] Failed to marshal payload: %v", err)
		return fmt.Errorf("failed to marshal publish request: %v", err)
	}

	url := c.publishURL()
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(body))
	if err != nil {
		return fmt.Errorf("failed to create http request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", c.apiKey)
	// Legacy header still accepted by Centrifugo
	req.Header.Set("Authorization", fmt.Sprintf("apikey %s", c.apiKey))

	resp, err := publishHTTPClient.Do(req)
	if err != nil {
		log.Printf("[Centrifugo] Publish request failed (%s): %v", url, err)
		notify.P0("centrifugo_unreachable", "Không gọi được Centrifugo (%s): %v — phòng đang chơi sẽ đứng, host bấm Next mà người chơi không nhận được gì.", url, err)
		return fmt.Errorf("failed to send request to Centrifugo: %v", err)
	}
	defer resp.Body.Close()

	respBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		log.Printf("[Centrifugo] Publish non-OK status=%d body=%s url=%s", resp.StatusCode, string(respBytes), url)
		notify.P0("centrifugo_status", "Centrifugo trả status=%d khi publish (%s): %s", resp.StatusCode, url, string(respBytes))
		return fmt.Errorf("centrifugo api returned status %d: %s", resp.StatusCode, string(respBytes))
	}

	var respBody map[string]interface{}
	if len(respBytes) > 0 {
		if err := json.Unmarshal(respBytes, &respBody); err == nil {
			if centrifugoErr, ok := respBody["error"]; ok && centrifugoErr != nil {
				log.Printf("[Centrifugo] API error while publishing event %s: %v", event, centrifugoErr)
				notify.P0("centrifugo_api_error", "Centrifugo API báo lỗi khi publish event %q: %v", event, centrifugoErr)
				return fmt.Errorf("centrifugo api error: %v", centrifugoErr)
			}
		}
	}

	log.Printf("[Centrifugo] Published event %q", event)
	return nil
}
