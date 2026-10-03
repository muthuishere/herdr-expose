package chat

import (
	"context"
	"os"
	"sync"

	"github.com/muthuishere/herdr-expose/internal/config"
	"github.com/muthuishere/herdr-expose/internal/core"
)

// Manager starts a Runner per ACTIVE adapter and reports what they are doing.
//
// "Active" is config.ActiveChatAdapters and nothing else, so the daemon cannot
// develop a second opinion about what "enabled" means. With both switches off
// -- which is how it ships -- Start launches nothing and the daemon behaves
// exactly as it did before chat existed.
type Manager struct {
	hub   *core.Hub
	store *core.Store
	dir   string
	logf  Logf

	mu      sync.Mutex
	live    map[string]*Runner
	gaveUp  map[string]*GiveUp
	started bool
}

func NewManager(hub *core.Hub, store *core.Store, dir string, logf Logf) *Manager {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Manager{
		hub: hub, store: store, dir: dir, logf: logf,
		live: map[string]*Runner{}, gaveUp: map[string]*GiveUp{},
	}
}

// Start launches one runner per active adapter. It returns immediately; each
// runner supervises itself until ctx ends or it gives up.
func (m *Manager) Start(ctx context.Context, c config.Chat) {
	active := c.ActiveChatAdapters()
	m.mu.Lock()
	m.started = true
	m.mu.Unlock()
	if len(active) == 0 {
		return
	}

	for _, a := range active {
		argv, err := config.SplitCommand(a.Command)
		if err != nil {
			m.logf("chat %s: %v", a.ID, err)
			continue
		}
		res := a.ResolveEnv()
		if msgs := res.MissingReport(); len(msgs) > 0 {
			// Refuse to start rather than spawn something that will fail five
			// times and be retired. The reason names the variable, never a
			// value.
			for _, msg := range msgs {
				m.logf("chat %s: not starting — %s", a.ID, msg)
			}
			continue
		}

		// The environment, never argv: argv is world-readable through `ps`.
		env := os.Environ()
		for k, v := range res.Values {
			env = append(env, k+"="+v)
		}

		r := NewRunner(m.hub, m.store, a.ID, m.logf)
		m.mu.Lock()
		m.live[a.ID] = r
		m.mu.Unlock()

		spec := Spec{ID: a.ID, Argv: argv, Dir: m.dir, Env: env}
		m.logf("chat %s: starting %q", a.ID, a.Command)
		go func(id string, r *Runner, spec Spec) {
			err := r.Run(ctx, spec)
			var g *GiveUp
			if asGiveUpErr(err, &g) {
				m.mu.Lock()
				m.gaveUp[id] = g
				m.mu.Unlock()
			}
		}(a.ID, r, spec)
	}
}

// Live reports the runners' states for Plan, so the UI shows what is actually
// happening rather than what the config implies.
func (m *Manager) Live() map[string]Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]Status, len(m.live))
	for id := range m.live {
		st := Status{ID: id, State: StateRunning, MaxRestarts: MaxRestarts}
		if g, ok := m.gaveUp[id]; ok {
			st.State = StateDown
			st.Restarts = g.Restarts
			if g.LastErr != nil {
				st.LastExit = g.LastErr.Error()
			}
		}
		out[id] = st
	}
	return out
}
