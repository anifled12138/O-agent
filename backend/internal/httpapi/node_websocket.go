package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"axiom.local/agent/internal/domain"
	"github.com/coder/websocket"
)

const nodeWebSocketSubprotocol = "o-agent-node-v1"

type nodeWakeEvent struct {
	Type   string `json:"type"`
	Reason string `json:"reason"`
}

type nodeWakeSubscription struct {
	wake    chan struct{}
	revoked chan struct{}
}

func (s *Server) nodeWebSocketRoute(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/nodes/connect", s.executionNodeConnect)
}

func (s *Server) executionNodeConnect(w http.ResponseWriter, r *http.Request) {
	nodeID, _ := r.Context().Value(executionNodeContextKey{}).(string)
	if nodeID == "" {
		write(w, http.StatusUnauthorized, map[string]string{"error": "active node credential required"})
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols:    []string{nodeWebSocketSubprotocol},
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return
	}
	if conn.Subprotocol() != nodeWebSocketSubprotocol {
		_ = conn.Close(websocket.StatusPolicyViolation, "required O Agent node protocol was not negotiated")
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(4096)
	wake, revoked, unsubscribe := s.subscribeNodeWake(nodeID)
	defer unsubscribe()
	provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	activeNode, err := s.store.ExecutionNodeForToken(r.Context(), hashToken(provided))
	if errors.Is(err, domain.ErrUnauthorized) || err == nil && activeNode.ID != nodeID {
		_ = conn.Close(websocket.StatusPolicyViolation, "node credential was revoked")
		return
	}
	if err != nil {
		_ = conn.Close(websocket.StatusInternalError, "node credential state could not be verified")
		return
	}

	writeWake := func(reason string) error {
		payload, err := json.Marshal(nodeWakeEvent{Type: "wake", Reason: reason})
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		return conn.Write(ctx, websocket.MessageText, payload)
	}
	if err := writeWake("connected"); err != nil {
		return
	}
	readDone := make(chan error, 1)
	go func() {
		for {
			typ, _, err := conn.Read(r.Context())
			if err != nil {
				readDone <- err
				return
			}
			if typ != websocket.MessageText {
				readDone <- errors.New("node wake socket accepts control frames only")
				return
			}
			readDone <- errors.New("node wake socket does not accept application messages")
			return
		}
	}()
	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			_ = conn.Close(websocket.StatusNormalClosure, "node disconnected")
			return
		case <-revoked:
			_ = conn.Close(websocket.StatusPolicyViolation, "node credential was revoked")
			return
		case err := <-readDone:
			closeStatus := websocket.CloseStatus(err)
			if err != nil && !errors.Is(err, context.Canceled) && closeStatus != websocket.StatusNormalClosure && closeStatus != websocket.StatusGoingAway {
				_ = conn.Close(websocket.StatusPolicyViolation, "unexpected node wake message")
			}
			return
		case <-wake:
			if err := writeWake("task_available"); err != nil {
				return
			}
		case <-keepalive.C:
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			err := conn.Ping(ctx)
			cancel()
			if err != nil {
				return
			}
		}
	}
}

func (s *Server) subscribeNodeWake(nodeID string) (<-chan struct{}, <-chan struct{}, func()) {
	subscription := &nodeWakeSubscription{wake: make(chan struct{}, 1), revoked: make(chan struct{})}
	s.nodeWakeMu.Lock()
	if s.nodeWakeSubs == nil {
		s.nodeWakeSubs = make(map[string]map[*nodeWakeSubscription]struct{})
	}
	if s.nodeWakeSubs[nodeID] == nil {
		s.nodeWakeSubs[nodeID] = make(map[*nodeWakeSubscription]struct{})
	}
	s.nodeWakeSubs[nodeID][subscription] = struct{}{}
	s.nodeWakeMu.Unlock()
	var once sync.Once
	return subscription.wake, subscription.revoked, func() {
		once.Do(func() {
			s.nodeWakeMu.Lock()
			delete(s.nodeWakeSubs[nodeID], subscription)
			if len(s.nodeWakeSubs[nodeID]) == 0 {
				delete(s.nodeWakeSubs, nodeID)
			}
			s.nodeWakeMu.Unlock()
		})
	}
}

func (s *Server) disconnectExecutionNode(nodeID string) {
	if s == nil || nodeID == "" {
		return
	}
	s.nodeWakeMu.Lock()
	for subscription := range s.nodeWakeSubs[nodeID] {
		close(subscription.revoked)
	}
	delete(s.nodeWakeSubs, nodeID)
	s.nodeWakeMu.Unlock()
}

// notifyExecutionNode is a best-effort wake hint after the task has been
// durably read back. The queue itself remains authoritative; a reconnecting
// node receives a wake immediately and then claims from SQLite.
func (s *Server) notifyExecutionNode(nodeID string) {
	if s == nil || nodeID == "" {
		return
	}
	s.nodeWakeMu.Lock()
	for subscription := range s.nodeWakeSubs[nodeID] {
		select {
		case subscription.wake <- struct{}{}:
		default:
		}
	}
	s.nodeWakeMu.Unlock()
}
