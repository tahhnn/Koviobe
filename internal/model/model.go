package model

import (
	"time"

	"gorm.io/gorm"
)

// Permission represents granular system rights.
type Permission struct {
	ID          uint      `gorm:"primaryKey" json:"id"`
	Name        string    `gorm:"size:100;uniqueIndex;not null" json:"name"` // e.g. "quiz:create", "room:control"
	Description string    `gorm:"size:255" json:"description"`
	CreatedAt   time.Time `json:"created_at"`
}

// Role represents a collection of permissions.
type Role struct {
	ID          uint           `gorm:"primaryKey" json:"id"`
	Name        string         `gorm:"size:100;uniqueIndex;not null" json:"name"` // Account roles: "admin", "host"
	Description string         `gorm:"size:255" json:"description"`
	Permissions []Permission   `gorm:"many2many:role_permissions;" json:"permissions,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
	DeletedAt   gorm.DeletedAt `gorm:"index" json:"-"`
}

// User represents system accounts.
type User struct {
	ID        uint           `gorm:"primaryKey" json:"id"`
	RoleID    *uint          `gorm:"index" json:"role_id,omitempty"` // Links to Role containing permissions
	Email     string         `gorm:"size:255;uniqueIndex;not null" json:"email"`
	Password  string         `gorm:"size:255;not null" json:"-"` // Hashed password
	Nickname  string         `gorm:"size:100" json:"nickname,omitempty"`
	IsActive  bool           `gorm:"default:true" json:"is_active"`
	Role      *Role          `gorm:"foreignKey:RoleID" json:"role,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"-"`
}

// Quiz represents a questionnaire created by a Host.
type Quiz struct {
	ID          uint           `gorm:"primaryKey" json:"id"`
	HostID      uint           `gorm:"index;not null" json:"host_id"`
	Title       string         `gorm:"size:255;not null" json:"title"`
	Description string         `gorm:"type:text" json:"description"`
	ThemeConfig string         `gorm:"type:text" json:"theme_config"` // JSON string containing custom theme colors/bg for this quiz
	Questions   []Question     `gorm:"foreignKey:QuizID;constraint:OnDelete:CASCADE" json:"questions,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
	DeletedAt   gorm.DeletedAt `gorm:"index" json:"-"`
}

// Question represents an individual query within a Quiz.
type Question struct {
	ID            uint   `gorm:"primaryKey" json:"id"`
	QuizID        uint   `gorm:"index;not null" json:"quiz_id"`
	Content       string `gorm:"type:text;not null" json:"content"`
	Type          string `gorm:"size:50;not null;default:'multiple_choice'" json:"type"` // multiple_choice, true_false
	Options       string `gorm:"type:text;not null" json:"options"`                      // JSON array of options
	CorrectAnswer string `gorm:"size:50;not null" json:"correct_answer"`                 // e.g. "A"
	Duration      int    `gorm:"default:20" json:"duration"`
	Points        int    `gorm:"default:1000" json:"points"`
	Order         int    `gorm:"default:0" json:"order"`
	// Explanation is the answer-explanation slide, stored as a JSON document
	// ({v, layout, bg, overlay, elements[]}). Empty means the question has no
	// slide, and every explanation step is skipped for it. It reveals the
	// answer, so it must never be served before the player has submitted.
	Explanation string    `gorm:"type:text" json:"explanation"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Template is a reusable question-bank pack (snapshot of questions).
// Rooms always run from Quiz; banks are copied/imported into quizzes.
type Template struct {
	ID          uint           `gorm:"primaryKey" json:"id"`
	HostID      uint           `gorm:"index;not null" json:"host_id"` // Owner of the template
	Title       string         `gorm:"size:255;not null" json:"title"`
	Description string         `gorm:"type:text" json:"description"`
	Questions   string         `gorm:"type:text;not null" json:"questions"` // JSON block of questions configuration
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
	DeletedAt   gorm.DeletedAt `gorm:"index" json:"-"`
}

// Room represents a live game instance.
type Room struct {
	ID                   uint           `gorm:"primaryKey" json:"id"`
	PinCode              string         `gorm:"size:10;uniqueIndex;not null" json:"pin_code"`
	QuizID               uint           `gorm:"not null" json:"quiz_id"`
	HostID               uint           `gorm:"not null" json:"host_id"`
	Status               string         `gorm:"size:50;default:'waiting'" json:"status"` // waiting, active, finished
	// EndedReason is why the room closed: "" (the host ended it, or solo mode
	// finished), license_expired, license_revoked. No `default:` tag — "" is the
	// meaningful normal case, and GORM omits zero values on columns that have one.
	EndedReason          string         `gorm:"size:32" json:"ended_reason,omitempty"`
	IsPrivate            bool           `gorm:"default:true;not null" json:"is_private"` // private = PIN-only; false = listed on open lobby
	ThemeConfig          string         `gorm:"type:text" json:"theme_config"`           // Copied from Quiz or overridden for this room
	CurrentQuestionID    *uint          `json:"current_question_id,omitempty"`
	CurrentQuestionIndex int            `gorm:"default:-1" json:"current_question_index"`
	QuestionActiveUntil  *time.Time     `json:"question_active_until,omitempty"`
	Quiz                 Quiz           `gorm:"foreignKey:QuizID" json:"quiz,omitempty"`
	CreatedAt            time.Time      `json:"created_at"`
	UpdatedAt            time.Time      `json:"updated_at"`
	DeletedAt            gorm.DeletedAt `gorm:"index" json:"-"`
}

// Player represents a participant in a Room.
type Player struct {
	ID                  uint       `gorm:"primaryKey" json:"id"`
	RoomID              uint       `gorm:"uniqueIndex:idx_room_nickname;not null" json:"room_id"`
	UserID              *uint      `gorm:"index" json:"user_id,omitempty"` // Optional: null for Guest/Anonymous players
	Nickname            string     `gorm:"size:100;uniqueIndex:idx_room_nickname;not null" json:"nickname"`
	Score               int        `gorm:"default:0" json:"score"`
	IsConnected         bool       `gorm:"default:true" json:"is_connected"`
	CurrentQuestionID   *uint      `json:"current_question_id,omitempty"`
	QuestionActiveUntil *time.Time `json:"question_active_until,omitempty"`
	CorrectAnswers      int        `gorm:"-" json:"correct_answers"`
	// AnsweredCount is how many questions this player has submitted, correct or
	// not. Solo mode needs it: every player sits on a different question, so the
	// host screen has no single "question 3 of 10" to show — progress is per row.
	AnsweredCount int       `gorm:"-" json:"answered_count"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// AnswerLog records the submissions of players.
type AnswerLog struct {
	ID             uint      `gorm:"primaryKey" json:"id"`
	RoomID         uint      `gorm:"index;not null" json:"room_id"`
	PlayerID       uint      `gorm:"uniqueIndex:idx_player_question;not null" json:"player_id"`
	QuestionID     uint      `gorm:"uniqueIndex:idx_player_question;not null" json:"question_id"`
	SelectedOption string    `gorm:"size:50;not null" json:"selected_option"`
	IsCorrect      bool      `json:"is_correct"`
	PointsEarned   int       `json:"points_earned"`
	ResponseTimeMs int       `json:"response_time_ms"`
	CreatedAt      time.Time `json:"created_at"`
}

// GameSession records the final result of a completed game room.
// [C-4 FIX] Replaces the misuse of AnswerLog for game-end archiving.
type GameSession struct {
	ID          uint      `gorm:"primaryKey" json:"id"`
	RoomID      uint      `gorm:"uniqueIndex;not null" json:"room_id"` // One session per room
	HostID      uint      `gorm:"index;not null" json:"host_id"`
	QuizID      uint      `gorm:"index;not null" json:"quiz_id"`
	Rankings    string    `gorm:"type:text;not null" json:"rankings"` // JSON array of {nickname, score}
	PlayerCount int       `gorm:"default:0" json:"player_count"`
	EndedAt     time.Time `json:"ended_at"`
	CreatedAt   time.Time `json:"created_at"`
}

// AuditLog records Host/Admin actions for security monitoring.
type AuditLog struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	UserID    uint      `gorm:"index;not null" json:"user_id"`
	Action    string    `gorm:"size:100;not null" json:"action"`   // e.g., "create_quiz", "delete_template"
	Resource  string    `gorm:"size:255;not null" json:"resource"` // e.g., "quiz_12", "template_5"
	IPAddress string    `gorm:"size:45" json:"ip_address"`
	CreatedAt time.Time `json:"created_at"`
}
