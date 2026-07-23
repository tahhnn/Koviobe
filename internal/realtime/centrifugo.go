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
	"github.com/kovio/backend/internal/config"
)

type CentrifugoClient struct {
	apiURL string
	apiKey string
	secret string
}

var Client *CentrifugoClient

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

	httpClient := &http.Client{Timeout: 5 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		log.Printf("[Centrifugo] Publish request failed (%s): %v", url, err)
		return fmt.Errorf("failed to send request to Centrifugo: %v", err)
	}
	defer resp.Body.Close()

	respBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		log.Printf("[Centrifugo] Publish non-OK status=%d body=%s url=%s", resp.StatusCode, string(respBytes), url)
		return fmt.Errorf("centrifugo api returned status %d: %s", resp.StatusCode, string(respBytes))
	}

	var respBody map[string]interface{}
	if len(respBytes) > 0 {
		if err := json.Unmarshal(respBytes, &respBody); err == nil {
			if centrifugoErr, ok := respBody["error"]; ok && centrifugoErr != nil {
				log.Printf("[Centrifugo] API error on channel %s event %s: %v", channel, event, centrifugoErr)
				return fmt.Errorf("centrifugo api error: %v", centrifugoErr)
			}
		}
	}

	log.Printf("[Centrifugo] Published '%s' → %s", event, channel)
	return nil
}
