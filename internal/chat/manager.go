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

	mu     sync.Mutex
	live   map[string]*Runner
	gaveUp map[string]*GiveUp
	// refused is the adapters Start looked at and would not spawn, by id, with
	// the reasons in a person's words.
	//
	// It is recorded because skipping an adapter used to leave NO trace in the
	// manager: Plan found nothing live for an adapter that was enabled and
	// whose command was on disk, and fell through to StateRestarting. The UI
	// and the CLI then said "restarting" about an adapter that had been
	// refused for an unset variable and was never going to start.
	refused map[string][]string
	started bool
}

func NewManager(hub *core.Hub, store *core.Store, dir string, logf Logf) *Manager {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Manager{
		hub: hub, store: store, dir: dir, logf: logf,
		live: map[string]*Runner{}, gaveUp: map[string]*GiveUp{},
		refused: map[string][]string{},
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
			m.refuse(a.ID, err.Error())
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
			// Recorded as well as logged. A refusal that only reaches the log
			// is a refusal the UI reports as "restarting", and the log is the
			// one place the person looking at the UI is not.
			m.refuse(a.ID, msgs...)
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
		// A refusal from an earlier Start is stale the moment this id spawns,
		// and a stale refusal outranks "running" in Live.
		delete(m.refused, a.ID)
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

// refuse records why an adapter was not spawned, for Live and therefore for
// Plan. Reasons name variables, never values.
func (m *Manager) refuse(id string, why ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refused[id] = append(m.refused[id], why...)
}

// Live reports the runners' states for Plan, so the UI shows what is actually
// happening rather than what the config implies.
func (m *Manager) Live() map[string]Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]Status, len(m.live)+len(m.refused))
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
	// Reported even though nothing is running for them: an adapter the daemon
	// decided against is a FACT the daemon holds, and leaving it out of Live is
	// what let Plan guess "restarting" about something that will never start.
	for id, why := range m.refused {
		out[id] = Status{
			ID: id, State: StateRefused, MaxRestarts: MaxRestarts,
			Problems: append([]string(nil), why...),
		}
	}
	return out
}
