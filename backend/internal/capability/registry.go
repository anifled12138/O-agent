package capability

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

type ScriptExecutor interface {
	Execute(context.Context, string, json.RawMessage, Limits) (json.RawMessage, time.Duration, error)
}

type Fragment struct {
	ID             string         `json:"id"`
	UserID         string         `json:"-"`
	ConversationID string         `json:"conversationId"`
	TurnID         string         `json:"turnId"`
	Contract       Contract       `json:"contract"`
	Program        string         `json:"-"`
	ProgramDigest  string         `json:"programDigest"`
	Evidence       []EvidenceCase `json:"evidence"`
	CreatedAt      time.Time      `json:"createdAt"`
	LastUsedAt     time.Time      `json:"lastUsedAt"`
}

type Registry struct {
	mu       sync.RWMutex
	executor ScriptExecutor
	items    map[string]*Fragment
}

func NewRegistry(executor ScriptExecutor) *Registry {
	return &Registry{executor: executor, items: map[string]*Fragment{}}
}

func (r *Registry) Register(userID, conversationID, turnID string, contract Contract, program string) (Fragment, error) {
	contract.Normalize()
	if contract.Tier != TierFragment || contract.Runtime != RuntimeJavaScript {
		return Fragment{}, errors.New("fragment must use the javascript fragment contract")
	}
	if contract.Scope != ScopeTurn && contract.Scope != ScopeConversation {
		return Fragment{}, errors.New("fragment scope must be turn or conversation")
	}
	if err := contract.Validate(); err != nil {
		return Fragment{}, err
	}
	program = strings.TrimSpace(program)
	if program == "" || len(program) > 32<<10 {
		return Fragment{}, errors.New("fragment program must be between 1 byte and 32 KiB")
	}
	now := time.Now().UTC()
	item := &Fragment{
		ID: newID("frag"), UserID: userID, ConversationID: conversationID, TurnID: turnID,
		Contract: contract, Program: program, ProgramDigest: contract.Digest(program), CreatedAt: now, LastUsedAt: now,
	}
	r.mu.Lock()
	r.items[item.ID] = item
	r.mu.Unlock()
	return cloneFragment(item), nil
}

func (r *Registry) Invoke(ctx context.Context, userID, conversationID, id string, input json.RawMessage) (json.RawMessage, error) {
	r.mu.RLock()
	item := r.items[id]
	if item == nil || item.UserID != userID || item.ConversationID != conversationID {
		r.mu.RUnlock()
		return nil, errors.New("fragment is not available in this conversation")
	}
	contract := item.Contract
	program := item.Program
	r.mu.RUnlock()
	if err := ValidateValue(input, contract.InputSchema); err != nil {
		return nil, fmt.Errorf("fragment input: %w", err)
	}
	output, duration, err := r.executor.Execute(ctx, program, input, contract.Limits)
	if err != nil {
		return nil, err
	}
	if err = ValidateValue(output, contract.OutputSchema); err != nil {
		return nil, fmt.Errorf("fragment output: %w", err)
	}
	r.mu.Lock()
	if current := r.items[id]; current != nil {
		current.LastUsedAt = time.Now().UTC()
		current.Evidence = append(current.Evidence, NewEvidence(newID("evidence"), input, output, duration))
		if len(current.Evidence) > 8 {
			current.Evidence = append([]EvidenceCase(nil), current.Evidence[len(current.Evidence)-8:]...)
		}
	}
	r.mu.Unlock()
	return output, nil
}

func (r *Registry) Get(userID, conversationID, id string) (Fragment, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	item := r.items[id]
	if item == nil || item.UserID != userID || item.ConversationID != conversationID {
		return Fragment{}, errors.New("fragment is not available in this conversation")
	}
	return cloneFragment(item), nil
}

func (r *Registry) List(userID, conversationID string) []Fragment {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := []Fragment{}
	for _, item := range r.items {
		if item.UserID == userID && (conversationID == "" || item.ConversationID == conversationID) {
			result = append(result, cloneFragment(item))
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].LastUsedAt.After(result[j].LastUsedAt) })
	return result
}

func (r *Registry) Drop(userID, conversationID, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	item := r.items[id]
	if item == nil || item.UserID != userID || item.ConversationID != conversationID {
		return errors.New("fragment is not available in this conversation")
	}
	delete(r.items, id)
	return nil
}

func (r *Registry) DropTurn(userID, turnID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, item := range r.items {
		if item.UserID == userID && item.TurnID == turnID && item.Contract.Scope == ScopeTurn {
			delete(r.items, id)
		}
	}
}

func cloneFragment(item *Fragment) Fragment {
	result := *item
	result.Contract.InputSchema = append(json.RawMessage(nil), item.Contract.InputSchema...)
	result.Contract.OutputSchema = append(json.RawMessage(nil), item.Contract.OutputSchema...)
	result.Evidence = append([]EvidenceCase(nil), item.Evidence...)
	return result
}

func newID(prefix string) string {
	raw := make([]byte, 12)
	_, _ = rand.Read(raw)
	return prefix + "_" + hex.EncodeToString(raw)
}
