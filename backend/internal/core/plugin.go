package core

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
)

type Manifest struct {
	ID           string   `json:"id"`
	Version      string   `json:"version"`
	Description  string   `json:"description"`
	Requires     []string `json:"requires"`
	Capabilities []string `json:"capabilities"`
}

type Plugin interface {
	Manifest() Manifest
	Init(context.Context, *Host) error
	Start(context.Context) error
	Stop(context.Context) error
}

type Host struct {
	mu       sync.RWMutex
	services map[string]any
}

func NewHost() *Host { return &Host{services: make(map[string]any)} }

func (h *Host) Provide(name string, service any) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if name == "" || service == nil {
		return errors.New("service name and value are required")
	}
	if _, exists := h.services[name]; exists {
		return fmt.Errorf("service %q is already registered", name)
	}
	h.services[name] = service
	return nil
}

func Service[T any](h *Host, name string) (T, error) {
	h.mu.RLock()
	value, ok := h.services[name]
	h.mu.RUnlock()
	var zero T
	if !ok {
		return zero, fmt.Errorf("service %q is unavailable", name)
	}
	typed, ok := value.(T)
	if !ok {
		return zero, fmt.Errorf("service %q has unexpected type %T", name, value)
	}
	return typed, nil
}

type Component struct {
	Info    Manifest
	InitFn  func(context.Context, *Host) error
	StartFn func(context.Context) error
	StopFn  func(context.Context) error
}

func (c *Component) Manifest() Manifest { return c.Info }
func (c *Component) Init(ctx context.Context, h *Host) error {
	if c.InitFn == nil {
		return nil
	}
	return c.InitFn(ctx, h)
}
func (c *Component) Start(ctx context.Context) error {
	if c.StartFn == nil {
		return nil
	}
	return c.StartFn(ctx)
}
func (c *Component) Stop(ctx context.Context) error {
	if c.StopFn == nil {
		return nil
	}
	return c.StopFn(ctx)
}

type Snapshot struct {
	Manifest
	State string `json:"state"`
}

type Manager struct {
	host    *Host
	plugins map[string]Plugin
	order   []string
	states  map[string]string
	mu      sync.RWMutex
}

func NewManager(host *Host) *Manager {
	return &Manager{host: host, plugins: make(map[string]Plugin), states: make(map[string]string)}
}

func (m *Manager) Register(p Plugin) error {
	info := p.Manifest()
	if info.ID == "" {
		return errors.New("plugin id is required")
	}
	if _, exists := m.plugins[info.ID]; exists {
		return fmt.Errorf("plugin %q already registered", info.ID)
	}
	m.plugins[info.ID] = p
	m.states[info.ID] = "registered"
	return nil
}

func (m *Manager) StartAll(ctx context.Context) error {
	order, err := m.resolve()
	if err != nil {
		return err
	}
	for _, id := range order {
		if err := m.plugins[id].Init(ctx, m.host); err != nil {
			return fmt.Errorf("init plugin %s: %w", id, err)
		}
		m.states[id] = "initialized"
	}
	for _, id := range order {
		if err := m.plugins[id].Start(ctx); err != nil {
			return fmt.Errorf("start plugin %s: %w", id, err)
		}
		m.states[id] = "running"
	}
	m.order = order
	return nil
}

func (m *Manager) StopAll(ctx context.Context) error {
	var joined error
	for i := len(m.order) - 1; i >= 0; i-- {
		id := m.order[i]
		if err := m.plugins[id].Stop(ctx); err != nil {
			joined = errors.Join(joined, fmt.Errorf("stop plugin %s: %w", id, err))
		}
		m.states[id] = "stopped"
	}
	return joined
}

func (m *Manager) Snapshots() []Snapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]Snapshot, 0, len(m.plugins))
	for id, p := range m.plugins {
		result = append(result, Snapshot{Manifest: p.Manifest(), State: m.states[id]})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func (m *Manager) resolve() ([]string, error) {
	visiting, visited := map[string]bool{}, map[string]bool{}
	order := make([]string, 0, len(m.plugins))
	var visit func(string) error
	visit = func(id string) error {
		if visiting[id] {
			return fmt.Errorf("plugin dependency cycle at %q", id)
		}
		if visited[id] {
			return nil
		}
		p, ok := m.plugins[id]
		if !ok {
			return fmt.Errorf("required plugin %q is not registered", id)
		}
		visiting[id] = true
		for _, dep := range p.Manifest().Requires {
			if err := visit(dep); err != nil {
				return err
			}
		}
		visiting[id], visited[id] = false, true
		order = append(order, id)
		return nil
	}
	ids := make([]string, 0, len(m.plugins))
	for id := range m.plugins {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := visit(id); err != nil {
			return nil, err
		}
	}
	return order, nil
}
