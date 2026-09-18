package skills

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Skill represents a hot-pluggable Markdown Skill
type Skill struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Prompt      string   `json:"prompt"`
	FilePath    string   `json:"filePath"`
	Enabled     bool     `json:"enabled"`
	Tags        []string `json:"tags,omitempty"`
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
func NewRegistry(workspaceDir string) *Registry {
	cfgFile := filepath.Join(workspaceDir, ".axiom", "skills.json")
	r := &Registry{
		workspaceDir: workspaceDir,
		configFile:   cfgFile,
		skills:       make(map[string]*Skill),
	}
	_ = r.Reload()
	return r
}

// Reload scans the workspace .skills directory and parses all SKILL.md files
func (r *Registry) Reload() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	persistedStates := r.loadPersistedStatesLocked()

	skillsDir := filepath.Join(r.workspaceDir, ".skills")
	if _, err := os.Stat(skillsDir); os.IsNotExist(err) {
		dataSkillsDir := filepath.Join(r.workspaceDir, "data", ".skills")
		if _, err := os.Stat(dataSkillsDir); err == nil {
			skillsDir = dataSkillsDir
		} else {
			return nil
		}
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
			continue
		}

		skill, err := parseSkillFile(entry.Name(), skillPath)
		if err != nil {
			continue
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
		result = append(result, &copied)
	}
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
	s.Enabled = enabled
	_ = r.savePersistedStatesLocked()
	return nil
}

func (r *Registry) loadPersistedStatesLocked() map[string]bool {
	states := make(map[string]bool)
	if r.configFile == "" {
		return states
	}
	data, err := os.ReadFile(r.configFile)
	if err != nil {
		return states
	}
	var cfg skillStateConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return states
	}
	if cfg.Skills != nil {
		return cfg.Skills
	}
	return states
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

// ActivePrompts returns a compiled markdown prompt of all enabled skills
func (r *Registry) GeneratePrompt() string {
	return r.ActivePrompts()
}

func (r *Registry) ActivePrompts() string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var sb strings.Builder
	for _, s := range r.skills {
		if !s.Enabled {
			continue
		}
		sb.WriteString("\n### Skill: " + s.Name + "\n" + s.Description + "\n\n" + s.Prompt + "\n")
	}
	return sb.String()
}

// parseSkillFile extracts YAML frontmatter and markdown body
func parseSkillFile(dirName, filePath string) (*Skill, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
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

	skill := &Skill{
		ID:       dirName,
		Name:     dirName,
		FilePath: filePath,
		Prompt:   strings.TrimSpace(strings.Join(bodyLines, "\n")),
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
			tags := strings.Split(v, ",")
			for _, t := range tags {
				t = strings.TrimSpace(t)
				if t != "" {
					skill.Tags = append(skill.Tags, t)
				}
			}
		}
	}

	if skill.Description == "" {
		skill.Description = fmt.Sprintf("Workspace skill from %s", dirName)
	}

	return skill, nil
}
