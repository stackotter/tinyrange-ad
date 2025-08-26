package main

import (
	"time"
)

type Session struct {
	ID        int
	Token     string
	UserID    int
	ExpiresAt time.Time
}

const SessionValidDuration time.Duration = 4 * 24 * time.Hour
