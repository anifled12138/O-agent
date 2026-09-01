package domain

import (
	"encoding/json"
	"time"
)

var (
	ErrNotFound     = Error("not found")
	ErrConflict     = Error("conflict")
	ErrUnauthorized = Error("unauthorized")
	ErrInvalid      = Error("invalid input")
)

type Error string

func (e Error) Error() string { return string(e) }

type User struct {
	ID          string    `json:"id"`
	Email       string    `json:"email"`
	DisplayName string    `json:"displayName"`
	CreatedAt   time.Time `json:"createdAt"`
}

type Provider struct {
	ID        string    `json:"id"`
	UserID    string    `json:"-"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`
	BaseURL   string    `json:"baseUrl"`
	Model     string    `json:"model"`
	HasAPIKey bool      `json:"hasApiKey"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type Conversation struct {
	ID         string    `json:"id"`
	UserID     string    `json:"-"`
	Title      string    `json:"title"`
	ProviderID string    `json:"providerId"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

type Message struct {
	ID             string    `json:"id"`
	ConversationID string    `json:"conversationId"`
	Role           string    `json:"role"`
	Content        string    `json:"content"`
	CreatedAt      time.Time `json:"createdAt"`
}

type ConversationDetail struct {
	Conversation
	Messages []Message `json:"messages"`
}

type TraceEvent struct {
	ID             string          `json:"id"`
	ConversationID string          `json:"conversationId"`
	TurnID         string          `json:"turnId"`
	Sequence       int             `json:"sequence"`
	Kind           string          `json:"kind"`
	Details        json.RawMessage `json:"details"`
	CreatedAt      time.Time       `json:"createdAt"`
}
