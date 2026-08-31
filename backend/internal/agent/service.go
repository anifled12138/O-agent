package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"time"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/provider"
	"axiom.local/agent/internal/storage"
)

const systemPrompt = `You are Axiom, a local-first engineering agent. Be direct, evidence-driven, and explicit about uncertainty. Do not claim to have executed tools unless tool evidence is present.`

type Service struct {
	store     *storage.Store
	providers *provider.Service
}

func New(store *storage.Store, providers *provider.Service) *Service {
	return &Service{store: store, providers: providers}
}
func (s *Service) List(ctx context.Context, userID string) ([]domain.Conversation, error) {
	return s.store.ListConversations(ctx, userID)
}
func (s *Service) Get(ctx context.Context, userID, id string) (domain.ConversationDetail, error) {
	return s.store.Conversation(ctx, userID, id)
}
func (s *Service) Create(ctx context.Context, userID, title, providerID string) (domain.Conversation, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		title = "New mission"
	}
	if providerID == "" {
		return domain.Conversation{}, domain.ErrInvalid
	}
	now := time.Now().UTC()
	c := domain.Conversation{ID: id("run"), UserID: userID, Title: title, ProviderID: providerID, CreatedAt: now, UpdatedAt: now}
	return c, s.store.CreateConversation(ctx, c)
}
func (s *Service) Turn(ctx context.Context, userID, conversationID, content string) (domain.Message, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return domain.Message{}, domain.ErrInvalid
	}
	now := time.Now().UTC()
	userMessage := domain.Message{ID: id("msg"), ConversationID: conversationID, Role: "user", Content: content, CreatedAt: now}
	if err := s.store.AddMessage(ctx, userID, userMessage); err != nil {
		return domain.Message{}, err
	}
	detail, err := s.store.Conversation(ctx, userID, conversationID)
	if err != nil {
		return domain.Message{}, err
	}
	messages := []provider.ChatMessage{{Role: "system", Content: systemPrompt}}
	for _, m := range detail.Messages {
		messages = append(messages, provider.ChatMessage{Role: m.Role, Content: m.Content})
	}
	reply, err := s.providers.Complete(ctx, userID, detail.ProviderID, messages)
	if err != nil {
		return domain.Message{}, err
	}
	assistant := domain.Message{ID: id("msg"), ConversationID: conversationID, Role: "assistant", Content: reply, CreatedAt: time.Now().UTC()}
	return assistant, s.store.AddMessage(ctx, userID, assistant)
}
func id(prefix string) string {
	raw := make([]byte, 12)
	_, _ = rand.Read(raw)
	return prefix + "_" + hex.EncodeToString(raw)
}
