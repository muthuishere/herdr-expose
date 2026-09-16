package config

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

// Provider is the narrow view other packages consume so that nothing outside
// this package has to import the concrete Config type.
//
// There is no Token() here: per B5 credentials live as hashes in the state
// file, not in config, so nothing can reach a secret through this interface.
//
// Bind and Mode report the RESOLVED exposure (E1), not anything the user typed:
// internal/expose decides them at startup and hands them back with SetBinding.
type Provider interface {
	Port() int
	Bind() string
	Mode() string
}

// Store holds the live config and reloads it on SIGHUP. It is safe for
// concurrent use; every reader gets an immutable snapshot pointer.
type Store struct {
	mu        sync.RWMutex
	cur       *Config
	path      string
	mode      Mode
	bind      string
	listeners []func(*Config)
	logf      func(format string, args ...any)
}

var _ Provider = (*Store)(nil)

// Open loads the config from the default path and returns a Store around it.
func Open() (*Store, error) {
	p, err := DefaultPath()
	if err != nil {
		return nil, err
	}
	return OpenFrom(p)
}

// OpenFrom loads the config from path and returns a Store around it.
func OpenFrom(path string) (*Store, error) {
	cfg, err := LoadFrom(path)
	if err != nil {
		return nil, err
	}
	return &Store{cur: cfg, path: path, mode: ModeLocal, bind: BindLoopback}, nil
}

// NewStore wraps an already-loaded Config (useful in tests).
func NewStore(cfg *Config) *Store {
	return &Store{cur: cfg, path: cfg.path, mode: ModeLocal, bind: BindLoopback}
}

// SetBinding records the resolved exposure mode and bind address. internal/expose
// computes them (it is the package that knows whether cloudflared exists) and
// calls this once at startup and again after every SIGHUP.
func (s *Store) SetBinding(mode Mode, bind string) error {
	if err := ValidateBind(bind); err != nil {
		return err
	}
	s.mu.Lock()
	s.mode, s.bind = mode, bind
	if s.cur != nil {
		s.cur.Server.Bind = bind
	}
	s.mu.Unlock()
	return nil
}

// SetLogger installs a logging function. It is only ever called with messages
// that contain no secrets.
func (s *Store) SetLogger(logf func(format string, args ...any)) {
	s.mu.Lock()
	s.logf = logf
	s.mu.Unlock()
}

func (s *Store) log(format string, args ...any) {
	s.mu.RLock()
	logf := s.logf
	s.mu.RUnlock()
	if logf != nil {
		logf(format, args...)
	}
}

// Current returns the live config snapshot.
func (s *Store) Current() *Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cur
}

// Path is the file backing this store.
func (s *Store) Path() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.path
}

// Port implements Provider.
func (s *Store) Port() int { return s.Current().Server.Port }

// Bind implements Provider: the RESOLVED bind address.
func (s *Store) Bind() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.bind
}

// Mode implements Provider: cloudflare | ngrok | js | lan | local.
func (s *Store) Mode() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return string(s.mode)
}

// OnChange registers a callback invoked after every successful reload. The
// callback runs on the reload goroutine; keep it quick.
func (s *Store) OnChange(fn func(*Config)) {
	s.mu.Lock()
	s.listeners = append(s.listeners, fn)
	s.mu.Unlock()
}

// Reload re-reads the file. On error the previous config stays live: a typo in
// the file must never take the server down.
func (s *Store) Reload() error {
	cfg, err := LoadFrom(s.Path())
	if err != nil {
		s.log("config reload failed, keeping previous config: %v", err)
		return err
	}
	s.mu.Lock()
	cfg.Server.Bind = s.bind // keep the resolved bind; the file cannot set it
	s.cur = cfg
	listeners := append([]func(*Config){}, s.listeners...)
	s.mu.Unlock()
	s.log("config reloaded from %s", cfg.path)
	for _, fn := range listeners {
		func() {
			defer func() {
				if r := recover(); r != nil {
					s.log("config change listener panicked: %v", r)
				}
			}()
			fn(cfg)
		}()
	}
	return nil
}

// WatchSignals reloads the config on every SIGHUP until ctx is cancelled.
func (s *Store) WatchSignals(ctx context.Context) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)
	go func() {
		defer signal.Stop(ch)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ch:
				_ = s.Reload()
			}
		}
	}()
}
