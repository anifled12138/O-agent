package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"axiom.local/agent/internal/domain"
	"github.com/coder/websocket"
)

const nodeWakeSubprotocol = "o-agent-node-v1"

type nodeWakeMessage struct {
	Type   string `json:"type"`
	Reason string `json:"reason"`
}

func (c *nodeAPIClient) connectWake(ctx context.Context) (*websocket.Conn, error) {
	base, err := url.Parse(c.baseURL)
	if err != nil {
		return nil, err
	}
	switch base.Scheme {
	case "https":
		base.Scheme = "wss"
	case "http":
		base.Scheme = "ws"
	default:
		return nil, fmt.Errorf("node wake connection requires HTTP(S) control URL")
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/api/v1/nodes/connect"
	base.RawQuery = ""
	base.Fragment = ""
	header := make(http.Header)
	header.Set("Authorization", "Bearer "+c.credential)
	connectCtx, cancel := context.WithTimeout(ctx, nodeRequestTimeout)
	defer cancel()
	conn, response, err := websocket.Dial(connectCtx, base.String(), &websocket.DialOptions{
		HTTPClient:   c.http,
		HTTPHeader:   header,
		Subprotocols: []string{nodeWakeSubprotocol},
	})
	if err != nil {
		if response != nil && response.Body != nil {
			closeErr := response.Body.Close()
			if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
				return nil, errors.Join(domain.ErrUnauthorized, err, closeErr)
			}
			return nil, errors.Join(err, closeErr)
		}
		if response != nil && (response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden) {
			return nil, errors.Join(domain.ErrUnauthorized, err)
		}
		return nil, err
	}
	if conn.Subprotocol() != nodeWakeSubprotocol {
		closeErr := conn.Close(websocket.StatusProtocolError, "required O Agent node protocol was not negotiated")
		return nil, errors.Join(errors.New("node WSS connection negotiated an unexpected subprotocol"), closeErr)
	}
	conn.SetReadLimit(4096)
	return conn, nil
}

func (c *nodeAPIClient) readWakeMessages(ctx context.Context, conn *websocket.Conn, onWake func()) error {
	if conn == nil || onWake == nil {
		return errors.New("node WSS wake reader is not configured")
	}
	for {
		messageType, payload, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		if messageType != websocket.MessageText {
			return errors.New("node WSS server sent a non-text wake event")
		}
		var message nodeWakeMessage
		decoder := json.NewDecoder(bytes.NewReader(payload))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&message); err != nil {
			return fmt.Errorf("decode node WSS wake event: %w", err)
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			return errors.New("node WSS wake event contains trailing data")
		}
		if message.Type != "wake" || message.Reason != "connected" && message.Reason != "task_available" {
			return errors.New("node WSS server sent an unsupported wake event")
		}
		onWake()
	}
}
