package model

import "time"

// UserTheme is the room look a host picks once for all of their rooms.
//
// Its own table rather than a column on users: the theme is read only when a
// room is created and written only from the settings page, and keeping it off
// users keeps that row — read on every authenticated request — as narrow as
// it was.
//
// Config is theme.Defaults as JSON, always written through
// theme.SanitizeDefaults.
type UserTheme struct {
	UserID    uint      `gorm:"primaryKey" json:"user_id"`
	Config    string    `gorm:"type:text;not null" json:"config"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
