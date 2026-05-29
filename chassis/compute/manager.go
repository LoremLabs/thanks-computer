package compute

import (
	"context"
	"errors"
	"sync"
)

// Manager is the default Runner: it resolves a ref to its artifact and runs
// it on the artifact's engine. Engines are opened once per name and reused
// (an engine owns any per-digest compile cache); a fresh Instance is loaded
// per invocation. Implements Runner.
type Manager struct {
	res Resolver
	cfg EngineConfig
	lim Limits

	mu      sync.Mutex
	engines map[string]Engine
}

// NewManager builds a Manager over a Resolver with per-invocation limits. The
// memory cap is also handed to engines at construction (some bound memory at
// runtime-creation rather than per call); the wall-clock cap is enforced per
// invocation.
func NewManager(res Resolver, lim Limits) *Manager {
	return &Manager{
		res:     res,
		lim:     lim,
		cfg:     EngineConfig{MaxMemoryMB: lim.MaxMemoryMB},
		engines: map[string]Engine{},
	}
}

func (m *Manager) engine(name string) (Engine, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.engines[name]; ok {
		return e, nil
	}
	e, err := OpenEngine(name, m.cfg)
	if err != nil {
		return nil, err
	}
	m.engines[name] = e
	return e, nil
}

// Run satisfies Runner: resolve → open engine → load → invoke → close.
func (m *Manager) Run(ctx context.Context, ref Ref, input []byte) ([]byte, error) {
	if m.res == nil {
		return nil, errors.New("compute: no resolver configured")
	}
	art, err := m.res.Resolve(ctx, ref)
	if err != nil {
		return nil, err
	}
	eng, err := m.engine(art.Engine)
	if err != nil {
		return nil, err
	}
	inst, err := eng.Load(ctx, art)
	if err != nil {
		return nil, err
	}
	defer func() { _ = inst.Close(ctx) }()
	lim := m.lim
	if t, ok := nowFrom(ctx); ok {
		lim.Now = t
	}
	if w, ok := logWriterFrom(ctx); ok {
		lim.Stderr = w
	}
	if s, ok := metricsSinkFrom(ctx); ok {
		lim.MetricsSink = s
	}
	return inst.Invoke(ctx, input, lim)
}
