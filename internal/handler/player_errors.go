package handler

import (
	"log"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/quizzzone/backend/internal/db"
	"github.com/quizzzone/backend/internal/model"
	"github.com/quizzzone/backend/internal/realtime"
)

// Machine-readable codes for player-facing failures. The "error" text is
// rewritten into the caller's language by middleware.Localize, so the player
// client must branch on these instead of the message. They are protocol
// values: never translate or rename one without updating the frontend.
const (
	codeRoomNotFound    = "ROOM_NOT_FOUND"
	codeJoinClosed      = "JOIN_CLOSED"
	codeRoomFinished    = "ROOM_FINISHED"
	codeRoomFull        = "ROOM_FULL"
	codeNicknameTaken   = "NICKNAME_TAKEN"
	codeSessionInvalid  = "SESSION_INVALID"
	codePlayerNotFound  = "PLAYER_NOT_FOUND"
	codeAlreadyAnswered = "ALREADY_ANSWERED"
	codeLate            = "LATE"
	codeRateLimited     = "RATE_LIMITED"
)

// playerError answers a player request with a message and a code, and logs
// the reason. Gin's access log only records the status, and the gateway keeps
// no bodies, so without this line a 400 during a live game is unexplainable
// afterwards — B.FEST 2026-10-03 had 37 submit 400s nobody could classify.
func playerError(c *gin.Context, status int, msg, code string) {
	log.Printf("[player-4xx] %d %s %s code=%s ip=%s msg=%q",
		status, c.Request.Method, c.FullPath(), code, c.ClientIP(), msg)
	c.JSON(status, gin.H{"error": msg, "code": code})
}

// reconnectPlayer marks a player connected again after their page unloaded.
//
// The play page reports beforeunload as a leave, and a page refresh fires
// beforeunload too, so a reload must not cost the player their seat: LeaveRoom
// only flags the row, and the first authenticated call from the reloaded page
// lands here and flips it back. The conditional UPDATE keeps this to one
// cheap statement only on the rare call that actually reconnects.
func reconnectPlayer(p *model.Player, room *model.Room) {
	if p.IsConnected {
		return
	}
	res := db.DB.Model(&model.Player{}).
		Where("id = ? AND is_connected = ?", p.ID, false).
		Update("is_connected", true)
	if res.Error != nil {
		log.Printf("[reconnectPlayer] player=%d room=%d: %v", p.ID, room.ID, res.Error)
		return
	}
	p.IsConnected = true
	// The lobby dropped this player on player:left; put them back on the
	// host's list. A running game never removed them from the scoreboard.
	if res.RowsAffected > 0 && room.Status == "waiting" {
		if err := realtime.Client.Publish(realtime.RoomChannel(room.PinCode), "player:joined", gin.H{
			"player_id": p.ID,
			"nickname":  p.Nickname,
		}); err != nil {
			log.Printf("[reconnectPlayer] publish failed room=%d: %v", room.ID, err)
		}
	}
}

// sessionInvalid is the answer for a player token whose player row is gone:
// the client must stop polling and send the player back to the join screen.
func sessionInvalid(c *gin.Context) {
	playerError(c, http.StatusUnauthorized, "Player session is no longer active", codeSessionInvalid)
}
