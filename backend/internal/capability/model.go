package capability

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const APIVersion = "o.capability/v1"

type Tier string

const (
	TierFragment Tier = "fragment"
	TierCapsule  Tier = "capsule"
	TierRelease  Tier = "release"
)

type Scope string

const (
	ScopeTurn         Scope = "turn"
	ScopeConversation Scope = "conversation"
	ScopeWorkspace    Scope = "workspace"
	ScopeGlobal       Scope = "global"
)

type Runtime string

const (
	RuntimeJavaScript Runtime = "javascript"
	RuntimePlugin     Runtime = "plugin"
)

type Limits struct {
	TimeoutMillis int `json:"timeoutMillis"`
	MemoryMiB     int `json:"memoryMiB"`
	MaxOutputKiB  int `json:"maxOutputKiB"`
}

func (l Limits) Normalized() Limits {
	if l.TimeoutMillis < 1 {
		l.TimeoutMillis = 2000
	}
	if l.TimeoutMillis > 10000 {
		l.TimeoutMillis = 10000
	}
	if l.MemoryMiB < 16 {
		l.MemoryMiB = 64
	}
	if l.MemoryMiB > 256 {
		l.MemoryMiB = 256
	}
	if l.MaxOutputKiB < 1 {
		l.MaxOutputKiB = 256
	}
	if l.MaxOutputKiB > 1024 {
		l.MaxOutputKiB = 1024
	}
	return l
}

type Permissions struct {
	WorkspaceRead  bool `json:"workspaceRead,omitempty"`
	WorkspaceWrite bool `json:"workspaceWrite,omitempty"`
	Network        bool `json:"network,omitempty"`
	Process        bool `json:"process,omitempty"`
}

func (p Permissions) Empty() bool {
	return !p.WorkspaceRead && !p.WorkspaceWrite && !p.Network && !p.Process
}

type Contract struct {
	APIVersion   string          `json:"apiVersion"`
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	Summary      string          `json:"summary"`
	Intent       string          `json:"intent"`
	Tags         []string        `json:"tags,omitempty"`
	Tier         Tier            `json:"tier"`
	Scope        Scope           `json:"scope"`
	Runtime      Runtime         `json:"runtime"`
	InputSchema  json.RawMessage `json:"inputSchema"`
	OutputSchema json.RawMessage `json:"outputSchema"`
	Permissions  Permissions     `json:"permissions"`
	Limits       Limits          `json:"limits"`
	Fallback     string          `json:"fallback"`
}

var contractID = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)

func (c *Contract) Normalize() {
	c.APIVersion = APIVersion
	c.ID = strings.ToLower(strings.TrimSpace(c.ID))
	c.Name = strings.TrimSpace(c.Name)
	c.Summary = strings.TrimSpace(c.Summary)
	c.Intent = strings.TrimSpace(c.Intent)
	c.Fallback = strings.TrimSpace(c.Fallback)
	c.Limits = c.Limits.Normalized()
	seen := map[string]bool{}
	tags := make([]string, 0, len(c.Tags))
	for _, tag := range c.Tags {
		tag = strings.ToLower(strings.TrimSpace(tag))
		if tag != "" && !seen[tag] {
			seen[tag] = true
			tags = append(tags, tag)
		}
	}
	c.Tags = tags
}

func (c Contract) Validate() error {
	if c.APIVersion != APIVersion {
		return fmt.Errorf("unsupported capability apiVersion %q", c.APIVersion)
	}
	if !contractID.MatchString(c.ID) || len(c.ID) > 128 {
		return errors.New("capability id must be a lowercase dotted identifier")
	}
	if c.Name == "" || len(c.Name) > 120 || c.Summary == "" || len(c.Summary) > 500 {
		return errors.New("capability name and summary are required")
	}
	if c.Intent == "" || len(c.Intent) > 1000 {
		return errors.New("capability intent is required")
	}
	if c.Fallback == "" || len(c.Fallback) > 1000 {
		return errors.New("capability fallback boundary is required")
	}
	if c.Tier != TierFragment && c.Tier != TierCapsule && c.Tier != TierRelease {
		return errors.New("invalid capability tier")
	}
	if c.Scope != ScopeTurn && c.Scope != ScopeConversation && c.Scope != ScopeWorkspace && c.Scope != ScopeGlobal {
		return errors.New("invalid capability scope")
	}
	if c.Runtime != RuntimeJavaScript && c.Runtime != RuntimePlugin {
		return errors.New("invalid capability runtime")
	}
	if c.Tier != TierRelease && !c.Permissions.Empty() {
		return errors.New("script capabilities cannot request host permissions in v1")
	}
	if err := ValidateSchema(c.InputSchema); err != nil {
		return fmt.Errorf("input schema: %w", err)
	}
	if err := ValidateSchema(c.OutputSchema); err != nil {
		return fmt.Errorf("output schema: %w", err)
	}
	return nil
}

func (c Contract) Digest(program string) string {
	raw, _ := json.Marshal(c)
	hash := sha256.New()
	hash.Write(raw)
	hash.Write([]byte{0})
	hash.Write([]byte(program))
	return hex.EncodeToString(hash.Sum(nil))
}

type EvidenceCase struct {
	ID             string          `json:"id"`
	Input          json.RawMessage `json:"input"`
	Output         json.RawMessage `json:"output"`
	OutputDigest   string          `json:"outputDigest"`
	DurationMillis int64           `json:"durationMillis"`
	CapturedAt     time.Time       `json:"capturedAt"`
}

func NewEvidence(id string, input, output json.RawMessage, duration time.Duration) EvidenceCase {
	digest := sha256.Sum256(output)
	return EvidenceCase{
		ID: id, Input: append(json.RawMessage(nil), input...), Output: append(json.RawMessage(nil), output...),
		OutputDigest: hex.EncodeToString(digest[:]), DurationMillis: duration.Milliseconds(), CapturedAt: time.Now().UTC(),
	}
}
