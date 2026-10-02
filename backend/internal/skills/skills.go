package skills

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Skill represents a hot-pluggable Markdown Skill
type Skill struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Description   string   `json:"description"`
	Prompt        string   `json:"prompt"`
	SourceContent string   `json:"-"`
	FilePath      string   `json:"filePath"`
	Enabled       bool     `json:"enabled"`
	Tags          []string `json:"tags,omitempty"`
	Triggers      []string `json:"triggers,omitempty"`
	ContentHash   string   `json:"contentHash"`
}

type skillStateConfig struct {
	Skills map[string]bool `json:"skills"`
}

// Registry manages in-memory skills and disk synchronization
type Registry struct {
	mu           sync.RWMutex
	workspaceDir string
	configFile   string
	skills       map[string]*Skill
}

// NewRegistry creates a skill registry bound to a workspace directory
func NewRegistry(workspaceDir string) (*Registry, error) {
	cfgFile := filepath.Join(workspaceDir, ".axiom", "skills.json")
	r := &Registry{
		workspaceDir: workspaceDir,
		configFile:   cfgFile,
		skills:       make(map[string]*Skill),
	}
	if err := r.Reload(); err != nil {
		return nil, err
	}
	return r, nil
}

// Reload scans the workspace .skills directory and parses all SKILL.md files
func (r *Registry) Reload() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	persistedStates, err := r.loadPersistedStatesLocked()
	if err != nil {
		return err
	}

	skillsDir := filepath.Join(r.workspaceDir, ".skills")
	if _, err := os.Stat(skillsDir); os.IsNotExist(err) {
		dataSkillsDir := filepath.Join(r.workspaceDir, "data", ".skills")
		if _, err := os.Stat(dataSkillsDir); err == nil {
			skillsDir = dataSkillsDir
		} else if os.IsNotExist(err) {
			r.skills = make(map[string]*Skill)
			return nil
		} else {
			return fmt.Errorf("failed to inspect fallback skills directory: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("failed to inspect skills directory: %w", err)
	}

	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		return fmt.Errorf("failed to read skills directory: %w", err)
	}

	found := make(map[string]*Skill)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		skillPath := filepath.Join(skillsDir, entry.Name(), "SKILL.md")
		if _, err := os.Stat(skillPath); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("failed to inspect skill %q: %w", entry.Name(), err)
		}

		skill, err := parseSkillFile(entry.Name(), skillPath)
		if err != nil {
			return fmt.Errorf("failed to parse skill %q: %w", entry.Name(), err)
		}

		if existing, ok := r.skills[skill.ID]; ok {
			skill.Enabled = existing.Enabled
		} else if persistedEnabled, ok := persistedStates[skill.ID]; ok {
			skill.Enabled = persistedEnabled
		} else {
			skill.Enabled = true
		}

		found[skill.ID] = skill
	}

	r.skills = found
	return nil
}

// List returns all skills in the registry
func (r *Registry) List() []*Skill {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]*Skill, 0, len(r.skills))
	for _, s := range r.skills {
		copied := *s
		copied.Tags = append([]string(nil), s.Tags...)
		copied.Triggers = append([]string(nil), s.Triggers...)
		result = append(result, &copied)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

// Get finds a skill by ID
func (r *Registry) Get(id string) (*Skill, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	s, ok := r.skills[id]
	if !ok {
		return nil, false
	}
	copied := *s
	copied.Tags = append([]string(nil), s.Tags...)
	copied.Triggers = append([]string(nil), s.Triggers...)
	return &copied, true
}

// SetEnabled toggles skill activation status and persists the state
func (r *Registry) SetEnabled(id string, enabled bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	s, ok := r.skills[id]
	if !ok {
		return fmt.Errorf("skill %q not found", id)
	}
	previous := s.Enabled
	s.Enabled = enabled
	if err := r.savePersistedStatesLocked(); err != nil {
		s.Enabled = previous
		return fmt.Errorf("persist skill %q state: %w", id, err)
	}
	return nil
}

func (r *Registry) loadPersistedStatesLocked() (map[string]bool, error) {
	states := make(map[string]bool)
	if r.configFile == "" {
		return states, nil
	}
	data, err := os.ReadFile(r.configFile)
	if os.IsNotExist(err) {
		return states, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read persisted skill state: %w", err)
	}
	var cfg skillStateConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("decode persisted skill state: %w", err)
	}
	if cfg.Skills != nil {
		return cfg.Skills, nil
	}
	return states, nil
}

func (r *Registry) savePersistedStatesLocked() error {
	if r.configFile == "" {
		return nil
	}
	states := make(map[string]bool, len(r.skills))
	for id, s := range r.skills {
		states[id] = s.Enabled
	}
	cfg := skillStateConfig{Skills: states}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(r.configFile)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	tmpFile := r.configFile + ".tmp"
	if err := os.WriteFile(tmpFile, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmpFile, r.configFile)
}

// GeneratePrompt is retained for callers that need the enabled-skill catalog.
func (r *Registry) GeneratePrompt() string { return r.ActivePrompts() }

// ActivePrompts returns a compact catalog. Skill bodies are included only
// by SelectForTask or loaded explicitly through the capability tool.
func (r *Registry) ActivePrompts() string {
	var sb strings.Builder
	for _, s := range r.List() {
		if !s.Enabled {
			continue
		}
		sb.WriteString("\n- id: local-skill:" + s.ID + "\n  name: " + s.Name + "\n  purpose: " + s.Description + "\n")
		if len(s.Triggers) > 0 {
			sb.WriteString("  triggers: " + strings.Join(s.Triggers, ", ") + "\n")
		}
	}
	return sb.String()
}

// SelectForTask uses explicit skill triggers to proactively load obvious
// matches. Skills without a matching trigger remain discoverable by metadata
// and capability search but do not add their body to every request.
func (r *Registry) SelectForTask(task string) []*Skill {
	if strings.TrimSpace(task) == "" {
		return nil
	}
	selected := []*Skill{}
	for _, skill := range r.List() {
		if !skill.Enabled {
			continue
		}
		for _, trigger := range skill.Triggers {
			if MatchesTaskTrigger(task, trigger) {
				selected = append(selected, skill)
				break
			}
		}
	}
	return selected
}

// MatchesTaskTrigger matches an explicit trigger as a case-insensitive
// substring. Ambiguous tasks remain available to capability search.
func MatchesTaskTrigger(task, trigger string) bool {
	trigger = strings.TrimSpace(trigger)
	if trigger == "" {
		return false
	}
	return strings.Contains(strings.ToLower(task), strings.ToLower(trigger))
}

// parseSkillFile extracts YAML frontmatter and markdown body
func parseSkillFile(dirName, filePath string) (*Skill, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(data)
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	inFrontmatter := false
	frontmatterChecked := false
	var frontmatterLines []string
	var bodyLines []string

	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		if !frontmatterChecked {
			if trimmed == "---" {
				inFrontmatter = true
				frontmatterChecked = true
				continue
			}
			frontmatterChecked = true
		}

		if inFrontmatter {
			if trimmed == "---" {
				inFrontmatter = false
				continue
			}
			frontmatterLines = append(frontmatterLines, line)
		} else {
			bodyLines = append(bodyLines, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read skill markdown: %w", err)
	}

	skill := &Skill{
		ID:            dirName,
		Name:          dirName,
		FilePath:      filePath,
		Prompt:        strings.TrimSpace(strings.Join(bodyLines, "\n")),
		SourceContent: string(data),
		ContentHash:   hex.EncodeToString(digest[:]),
	}

	for _, fLine := range frontmatterLines {
		parts := strings.SplitN(fLine, ":", 2)
		if len(parts) != 2 {
			continue
		}
		k := strings.TrimSpace(parts[0])
		v := strings.TrimSpace(parts[1])
		v = strings.Trim(v, "\"'")

		switch strings.ToLower(k) {
		case "name":
			if v != "" {
				skill.Name = v
			}
		case "description":
			skill.Description = v
		case "tags":
			for _, t := range parseSkillList(v) {
				if t != "" {
					skill.Tags = append(skill.Tags, t)
				}
			}
		case "triggers":
			for _, trigger := range parseSkillList(v) {
				if trigger != "" {
					skill.Triggers = append(skill.Triggers, trigger)
				}
			}
		}
	}

	if skill.Description == "" {
		skill.Description = fmt.Sprintf("Workspace skill from %s", dirName)
	}

	return skill, nil
}

func parseSkillList(value string) []string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "[")
	value = strings.TrimSuffix(value, "]")
	items := strings.Split(value, ",")
	result := make([]string, 0, len(items))
	for _, item := range items {
		item = strings.TrimSpace(strings.Trim(item, "\"'"))
		if item != "" {
			result = append(result, item)
		}
	}
	return result
}
