//go:build linux

package browsercontrol

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/coder/websocket"
)

const (
	maxBrowserRequestBytes    = 1 << 20
	maxBrowserResponseBytes   = 8 << 20
	maxBrowserPageTextBytes   = 24 << 10
	maxBrowserScreenshotBytes = 6 << 20
)

type Request struct {
	Action   string  `json:"action"`
	URL      string  `json:"url,omitempty"`
	Selector string  `json:"selector,omitempty"`
	Text     string  `json:"text,omitempty"`
	Key      string  `json:"key,omitempty"`
	DeltaX   float64 `json:"deltaX,omitempty"`
	DeltaY   float64 `json:"deltaY,omitempty"`
}

type Response struct {
	OK         bool   `json:"ok"`
	Error      string `json:"error,omitempty"`
	URL        string `json:"url,omitempty"`
	Title      string `json:"title,omitempty"`
	Text       string `json:"text,omitempty"`
	Screenshot string `json:"screenshot,omitempty"`
}

type chromeTarget struct {
	Type      string `json:"type"`
	URL       string `json:"url"`
	WebSocket string `json:"webSocketDebuggerUrl"`
}

type cdpResponse struct {
	ID     int             `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// RunWorker is invoked only by the trusted O host, inside a Bubblewrap task
// namespace. The worker owns Chrome and its CDP socket; no debugging port is
// exposed outside that namespace. It processes one bounded JSON request per
// line and keeps the live tab for the duration of the task turn.
func RunWorker(chromePath string, stdin io.Reader, stdout io.Writer) error {
	if strings.TrimSpace(chromePath) == "" || stdin == nil || stdout == nil {
		return errors.New("browser worker requires Chrome and streams")
	}
	profile := filepath.Join(os.TempDir(), "o-agent-browser-profile")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		return fmt.Errorf("create isolated browser profile: %w", err)
	}
	if err := os.Chmod(profile, 0o700); err != nil {
		return fmt.Errorf("restrict isolated browser profile: %w", err)
	}
	stderr := &boundedBuffer{max: 32 << 10}
	command := exec.Command(chromePath,
		"--headless=new", "--remote-debugging-port=0", "--remote-debugging-address=127.0.0.1",
		"--user-data-dir="+profile, "--no-first-run", "--no-default-browser-check",
		"--disable-background-networking", "--disable-default-apps", "--disable-dev-shm-usage",
		"--window-size=1280,800", "--force-device-scale-factor=1",
	)
	command.Dir = profile
	command.Stdin = nil
	command.Stdout = io.Discard
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		return fmt.Errorf("start sandboxed Chrome: %w", err)
	}
	processDone := make(chan struct{})
	var processErr error
	go func() {
		processErr = command.Wait()
		close(processDone)
	}()
	defer func() {
		if command.Process != nil {
			_ = command.Process.Kill()
		}
		<-processDone
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	port, err := waitForDevTools(ctx, filepath.Join(profile, "DevToolsActivePort"), processDone, &processErr, stderr)
	if err != nil {
		return err
	}
	target, err := waitForPageTarget(ctx, port)
	if err != nil {
		return err
	}
	cdpURL, err := localCDPURL(target.WebSocket)
	if err != nil {
		return err
	}
	connection, _, err := websocket.Dial(ctx, cdpURL, &websocket.DialOptions{HTTPClient: &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 5 * time.Second}})
	if err != nil {
		return fmt.Errorf("connect to sandboxed Chrome DevTools: %w", err)
	}
	defer connection.Close(websocket.StatusNormalClosure, "browser worker stopped")
	connection.SetReadLimit(maxBrowserResponseBytes)
	client := &cdpClient{connection: connection}
	if _, err := client.call(ctx, "Page.enable", nil); err != nil {
		return err
	}
	if _, err := client.call(ctx, "Runtime.enable", nil); err != nil {
		return err
	}

	scanner := bufio.NewScanner(stdin)
	scanner.Buffer(make([]byte, 4096), maxBrowserRequestBytes)
	encoder := json.NewEncoder(stdout)
	for scanner.Scan() {
		var request Request
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			if encodeErr := encoder.Encode(Response{Error: "invalid browser request JSON"}); encodeErr != nil {
				return encodeErr
			}
			continue
		}
		response := handleRequest(client, request)
		encoded, err := json.Marshal(response)
		if err != nil {
			return fmt.Errorf("encode browser response: %w", err)
		}
		if len(encoded) > maxBrowserResponseBytes {
			response = Response{Error: "browser response exceeded the 8 MiB frame limit"}
		}
		if err := encoder.Encode(response); err != nil {
			return fmt.Errorf("write browser response: %w", err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read browser request: %w", err)
	}
	return nil
}

type cdpClient struct {
	connection *websocket.Conn
	nextID     int
}

func (c *cdpClient) call(parent context.Context, method string, params any) (json.RawMessage, error) {
	if c == nil || c.connection == nil || strings.TrimSpace(method) == "" {
		return nil, errors.New("Chrome DevTools client is unavailable")
	}
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	c.nextID++
	request := map[string]any{"id": c.nextID, "method": method}
	if params != nil {
		request["params"] = params
	}
	if err := c.connection.Write(ctx, websocket.MessageText, mustJSON(request)); err != nil {
		return nil, fmt.Errorf("send Chrome DevTools %s: %w", method, err)
	}
	for {
		_, payload, err := c.connection.Read(ctx)
		if err != nil {
			return nil, fmt.Errorf("read Chrome DevTools %s: %w", method, err)
		}
		var response cdpResponse
		if err := json.Unmarshal(payload, &response); err != nil {
			return nil, fmt.Errorf("decode Chrome DevTools response: %w", err)
		}
		if response.ID != c.nextID {
			continue // Ignore page lifecycle events; each worker sends one command at a time.
		}
		if response.Error != nil {
			return nil, fmt.Errorf("Chrome DevTools %s: %s", method, response.Error.Message)
		}
		return response.Result, nil
	}
}

func handleRequest(client *cdpClient, request Request) Response {
	ctx := context.Background()
	switch request.Action {
	case "open":
		parsed, err := url.Parse(strings.TrimSpace(request.URL))
		if err != nil || parsed == nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Hostname() == "" || parsed.User != nil {
			return Response{Error: "browser navigation requires an HTTP(S) URL without embedded credentials"}
		}
		if _, err := client.call(ctx, "Page.navigate", map[string]any{"url": parsed.String()}); err != nil {
			return Response{Error: err.Error()}
		}
		time.Sleep(500 * time.Millisecond)
	case "inspect", "screenshot":
	case "click":
		if strings.TrimSpace(request.Selector) == "" {
			return Response{Error: "click requires a CSS selector"}
		}
		expression, _ := json.Marshal(request.Selector)
		js := "(()=>{const e=document.querySelector(" + string(expression) + ");if(!e)throw new Error('selector not found');e.click();return true})()"
		if _, err := evaluate(client, js); err != nil {
			return Response{Error: err.Error()}
		}
		time.Sleep(250 * time.Millisecond)
	case "type":
		if strings.TrimSpace(request.Selector) == "" || len(request.Text) > 16<<10 {
			return Response{Error: "type requires a CSS selector and text no larger than 16 KiB"}
		}
		expression, _ := json.Marshal(request.Selector)
		js := "(()=>{const e=document.querySelector(" + string(expression) + ");if(!e)throw new Error('selector not found');e.focus();return true})()"
		if _, err := evaluate(client, js); err != nil {
			return Response{Error: err.Error()}
		}
		if _, err := client.call(ctx, "Input.insertText", map[string]any{"text": request.Text}); err != nil {
			return Response{Error: err.Error()}
		}
	case "press":
		if !allowedBrowserKey(request.Key) {
			return Response{Error: "press supports Enter, Tab, Escape, Backspace, Delete, Space, and arrow keys"}
		}
		if _, err := client.call(ctx, "Input.dispatchKeyEvent", map[string]any{"type": "keyDown", "key": request.Key}); err != nil {
			return Response{Error: err.Error()}
		}
		if _, err := client.call(ctx, "Input.dispatchKeyEvent", map[string]any{"type": "keyUp", "key": request.Key}); err != nil {
			return Response{Error: err.Error()}
		}
	case "scroll":
		if request.DeltaX < -5000 || request.DeltaX > 5000 || request.DeltaY < -5000 || request.DeltaY > 5000 {
			return Response{Error: "scroll delta must be between -5000 and 5000 pixels"}
		}
		js := fmt.Sprintf("window.scrollBy(%f,%f)", request.DeltaX, request.DeltaY)
		if _, err := evaluate(client, js); err != nil {
			return Response{Error: err.Error()}
		}
	default:
		return Response{Error: "unsupported browser action"}
	}
	return inspectPage(client, request.Action == "screenshot" || request.Action != "inspect")
}

func inspectPage(client *cdpClient, includeScreenshot bool) Response {
	value, err := client.call(context.Background(), "Runtime.evaluate", map[string]any{
		"expression":    `({url:location.href,title:document.title,text:(document.body?.innerText||"").slice(0,24576)})`,
		"returnByValue": true,
	})
	if err != nil {
		return Response{Error: err.Error()}
	}
	var result struct {
		Result struct {
			Value struct {
				URL   string `json:"url"`
				Title string `json:"title"`
				Text  string `json:"text"`
			} `json:"value"`
		} `json:"result"`
		ExceptionDetails json.RawMessage `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(value, &result); err != nil {
		return Response{Error: fmt.Sprintf("decode browser page snapshot: %v", err)}
	}
	response := Response{OK: true, URL: result.Result.Value.URL, Title: result.Result.Value.Title, Text: result.Result.Value.Text}
	if !includeScreenshot {
		return response
	}
	image, err := client.call(context.Background(), "Page.captureScreenshot", map[string]any{"format": "jpeg", "quality": 55, "fromSurface": true, "captureBeyondViewport": false})
	if err != nil {
		return Response{Error: err.Error()}
	}
	var screenshot struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(image, &screenshot); err != nil {
		return Response{Error: fmt.Sprintf("decode browser screenshot: %v", err)}
	}
	if len(screenshot.Data) > base64.StdEncoding.EncodedLen(maxBrowserScreenshotBytes) {
		return Response{Error: "browser screenshot exceeds the 6 MiB image limit"}
	}
	decoded, err := base64.StdEncoding.DecodeString(screenshot.Data)
	if err != nil || len(decoded) > maxBrowserScreenshotBytes {
		return Response{Error: "browser screenshot failed its image size or encoding check"}
	}
	response.Screenshot = "![browser screenshot](data:image/jpeg;base64," + screenshot.Data + ")"
	return response
}

func evaluate(client *cdpClient, expression string) (json.RawMessage, error) {
	result, err := client.call(context.Background(), "Runtime.evaluate", map[string]any{"expression": expression, "awaitPromise": true, "returnByValue": true})
	if err != nil {
		return nil, err
	}
	var response struct {
		ExceptionDetails *struct {
			Text string `json:"text"`
		} `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(result, &response); err != nil {
		return nil, fmt.Errorf("decode browser script result: %w", err)
	}
	if response.ExceptionDetails != nil {
		return nil, fmt.Errorf("browser page action failed: %s", response.ExceptionDetails.Text)
	}
	return result, nil
}

func waitForDevTools(ctx context.Context, activePortPath string, processDone <-chan struct{}, processErr *error, stderr *boundedBuffer) (int, error) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if data, err := os.ReadFile(activePortPath); err == nil {
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			if len(lines) > 0 {
				var port int
				if _, err := fmt.Sscan(lines[0], &port); err == nil && port > 0 && port < 65536 {
					return port, nil
				}
			}
		}
		select {
		case <-processDone:
			return 0, fmt.Errorf("Chrome exited before DevTools was ready: %v (%s)", *processErr, stderr.String())
		case <-ctx.Done():
			return 0, fmt.Errorf("wait for Chrome DevTools port: %w (%s)", ctx.Err(), stderr.String())
		case <-ticker.C:
		}
	}
}

func waitForPageTarget(ctx context.Context, port int) (chromeTarget, error) {
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 2 * time.Second}
	endpoint := fmt.Sprintf("http://127.0.0.1:%d/json/list", port)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return chromeTarget{}, err
		}
		response, err := client.Do(request)
		if err == nil {
			var targets []chromeTarget
			decodeErr := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&targets)
			closeErr := response.Body.Close()
			if decodeErr == nil && closeErr == nil {
				for _, target := range targets {
					if target.Type == "page" && target.WebSocket != "" {
						return target, nil
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return chromeTarget{}, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return chromeTarget{}, errors.New("Chrome did not expose a page target")
}

func localCDPURL(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed == nil || parsed.Scheme != "ws" || parsed.Port() == "" || !strings.HasPrefix(parsed.Path, "/devtools/page/") {
		return "", errors.New("Chrome returned a non-local DevTools endpoint")
	}
	host := parsed.Hostname()
	if host != "localhost" {
		address := net.ParseIP(host)
		if address == nil || !address.IsLoopback() {
			return "", errors.New("Chrome returned a non-local DevTools endpoint")
		}
	}
	return parsed.String(), nil
}

func allowedBrowserKey(key string) bool {
	switch key {
	case "Enter", "Tab", "Escape", "Backspace", "Delete", "Space", "ArrowDown", "ArrowUp", "ArrowLeft", "ArrowRight":
		return true
	default:
		return false
	}
}

func mustJSON(value any) []byte {
	data, _ := json.Marshal(value)
	return data
}

type boundedBuffer struct {
	data []byte
	max  int
}

func (b *boundedBuffer) Write(value []byte) (int, error) {
	if b.max > len(b.data) {
		b.data = append(b.data, value[:min(len(value), b.max-len(b.data))]...)
	}
	return len(value), nil
}

func (b *boundedBuffer) String() string { return string(b.data) }
