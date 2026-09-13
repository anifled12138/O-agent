package capsule

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"axiom.local/agent/internal/capability"
)

const APIVersion = "o.capsule/v1"

type Provenance struct {
	FragmentID     string    `json:"fragmentId"`
	ConversationID string    `json:"conversationId"`
	TurnID         string    `json:"turnId"`
	CapturedAt     time.Time `json:"capturedAt"`
}

type Verifier struct {
	Kind string `json:"kind"`
}

type Manifest struct {
	APIVersion string                    `json:"apiVersion"`
	ID         string                    `json:"id"`
	Digest     string                    `json:"digest"`
	Contract   capability.Contract       `json:"contract"`
	Program    string                    `json:"program"`
	Evidence   []capability.EvidenceCase `json:"evidence"`
	Verifier   Verifier                  `json:"verifier"`
	Provenance Provenance                `json:"provenance"`
	CreatedAt  time.Time                 `json:"createdAt"`
}

type Summary struct {
	ID            string             `json:"id"`
	Digest        string             `json:"digest"`
	Name          string             `json:"name"`
	Summary       string             `json:"summary"`
	Intent        string             `json:"intent"`
	Tags          []string           `json:"tags,omitempty"`
	EvidenceCount int                `json:"evidenceCount"`
	Fallback      string             `json:"fallback"`
	Runtime       capability.Runtime `json:"runtime"`
	Scope         capability.Scope   `json:"scope"`
	CreatedAt     time.Time          `json:"createdAt"`
}

type VerificationReport struct {
	CapsuleID      string    `json:"capsuleId"`
	Digest         string    `json:"digest"`
	Passed         bool      `json:"passed"`
	Cases          int       `json:"cases"`
	PassedCases    int       `json:"passedCases"`
	DurationMillis int64     `json:"durationMillis"`
	Failure        string    `json:"failure,omitempty"`
	VerifiedAt     time.Time `json:"verifiedAt"`
}

var capsuleID = regexp.MustCompile(`^capsule\.[a-z0-9]+(?:[._-][a-z0-9]+)*$`)

func FromFragment(fragment capability.Fragment, intent, fallback string) (Manifest, error) {
	if len(fragment.Evidence) == 0 {
		return Manifest{}, errors.New("a fragment needs at least one successful execution before it can become a capsule")
	}
	contract := fragment.Contract
	contract.Tier = capability.TierCapsule
	contract.Scope = capability.ScopeWorkspace
	contract.ID = capsuleIdentifier(contract.Name)
	if strings.TrimSpace(intent) != "" {
		contract.Intent = intent
	}
	if strings.TrimSpace(fallback) != "" {
		contract.Fallback = fallback
	}
	contract.Normalize()
	if err := contract.Validate(); err != nil {
		return Manifest{}, err
	}
	manifest := Manifest{
		APIVersion: APIVersion, ID: contract.ID, Contract: contract, Program: fragment.Program,
		Evidence: append([]capability.EvidenceCase(nil), fragment.Evidence...), Verifier: Verifier{Kind: "exact-json/v1"},
		Provenance: Provenance{FragmentID: fragment.ID, ConversationID: fragment.ConversationID, TurnID: fragment.TurnID, CapturedAt: time.Now().UTC()},
		CreatedAt:  time.Now().UTC(),
	}
	manifest.Digest = manifestDigest(manifest)
	return manifest, nil
}

func (m Manifest) Validate() error {
	if m.APIVersion != APIVersion || !capsuleID.MatchString(m.ID) || m.Contract.ID != m.ID {
		return errors.New("invalid capsule identity")
	}
	if m.Contract.Tier != capability.TierCapsule || m.Contract.Scope != capability.ScopeWorkspace || m.Contract.Runtime != capability.RuntimeJavaScript {
		return errors.New("capsule contract must be a workspace JavaScript capability")
	}
	if err := m.Contract.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(m.Program) == "" || len(m.Program) > 32<<10 {
		return errors.New("invalid capsule program")
	}
	if len(m.Evidence) == 0 || len(m.Evidence) > 32 {
		return errors.New("capsule requires between 1 and 32 evidence cases")
	}
	if m.Verifier.Kind != "exact-json/v1" {
		return fmt.Errorf("unsupported capsule verifier %q", m.Verifier.Kind)
	}
	if expected := manifestDigest(m); m.Digest != expected {
		return errors.New("capsule digest does not match its contents")
	}
	return nil
}

func (m Manifest) Summary() Summary {
	return Summary{ID: m.ID, Digest: m.Digest, Name: m.Contract.Name, Summary: m.Contract.Summary, Intent: m.Contract.Intent, Tags: append([]string(nil), m.Contract.Tags...), EvidenceCount: len(m.Evidence), Fallback: m.Contract.Fallback, Runtime: m.Contract.Runtime, Scope: m.Contract.Scope, CreatedAt: m.CreatedAt}
}

func manifestDigest(manifest Manifest) string {
	manifest.Digest = ""
	raw, _ := json.Marshal(manifest)
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func capsuleIdentifier(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	var result strings.Builder
	lastSeparator := false
	for _, char := range name {
		allowed := char >= 'a' && char <= 'z' || char >= '0' && char <= '9'
		if allowed {
			result.WriteRune(char)
			lastSeparator = false
		} else if !lastSeparator && result.Len() > 0 {
			result.WriteByte('-')
			lastSeparator = true
		}
	}
	value := strings.Trim(result.String(), "-")
	if value == "" {
		digest := sha256.Sum256([]byte(name))
		value = "capability-" + hex.EncodeToString(digest[:4])
	}
	if len(value) > 80 {
		value = strings.Trim(value[:80], "-")
	}
	return "capsule." + value
}
