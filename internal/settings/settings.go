// Package settings decides what each editable setting is while the server
// runs (#397): the environment's value when it has one, what the web UI
// stored when it does not, and the default otherwise.
//
// **The environment wins, and that is the decision.** An install described
// in code -- a compose file, a Fly app, a Render Blueprint -- keeps authority
// over what it wrote, and nobody edits a variable, restarts, and finds a row
// they forgot about overriding it. A setting the environment fixes is shown
// as fixed and refused here; a row stored before somebody set the variable is
// kept and ignored, so unsetting the variable brings it back.
//
// What every consumer reads is one snapshot, swapped whole on a change, and a
// channel closed when it is -- so a loop can sleep on it rather than poll.
package settings

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/stratus-private-cloud/stratus-backend/internal/config"
	"github.com/stratus-private-cloud/stratus-backend/internal/db"
)

// Source is where a setting's value came from.
type Source string

// The three places a value can come from, in the order they are asked.
const (
	FromEnvironment Source = "environment"
	FromStore       Source = "stored"
	FromDefault     Source = "default"
)

var (
	// ErrFixed is a change to a setting the environment sets.
	ErrFixed = errors.New("settings: set by the environment")
	// ErrUnknown is a key that is not an editable setting.
	ErrUnknown = errors.New("settings: not a setting")
)

// Entry is one setting as a page shows it: what it is, its value as it would
// be typed, and where that came from.
type Entry struct {
	config.Setting
	Value  string
	Source Source
}

type state struct {
	values  config.Values
	entries []Entry
}

// Service holds the settings in force.
type Service struct {
	getenv func(string) string
	store  db.Settings
	now    func() time.Time

	current atomic.Pointer[state]

	// mu serialises changes, so two of them cannot read the same rows and
	// each write a snapshot that leaves the other out.
	mu      sync.Mutex
	changed chan struct{}
}

// New reads what is stored and works out what is in force. A variable the
// environment sets that does not parse is an error, which config.Load has
// already refused by the time this runs; a stored value that no longer parses
// is logged and passed over, since it is not the operator's mistake to fix
// before the server can start.
func New(ctx context.Context, getenv func(string) string, store db.Settings) (*Service, error) {
	s := &Service{getenv: getenv, store: store, now: time.Now, changed: make(chan struct{})}
	if err := s.reload(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// Values are the settings in force.
func (s *Service) Values() config.Values { return s.current.Load().values }

// Entries are every setting, in the order a page shows them.
func (s *Service) Entries() []Entry { return s.current.Load().entries }

// Changed is closed the next time a setting changes. Ask again after it is.
func (s *Service) Changed() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.changed
}

// Set stores a value for a setting the environment leaves unset. Empty is the
// same as Reset.
func (s *Service) Set(ctx context.Context, key, raw string) error {
	setting, err := s.editable(key)
	if err != nil {
		return err
	}
	if raw == "" {
		return s.Reset(ctx, key)
	}
	var v config.Values
	if err = setting.Apply(&v, raw); err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	return s.change(ctx, func() error { return s.store.PutSetting(ctx, key, raw, s.now()) })
}

// Reset forgets what was stored for a setting, which puts it back to its
// default.
func (s *Service) Reset(ctx context.Context, key string) error {
	if _, err := s.editable(key); err != nil {
		return err
	}
	return s.change(ctx, func() error { return s.store.DeleteSetting(ctx, key) })
}

func (s *Service) editable(key string) (config.Setting, error) {
	setting, ok := config.SettingByKey(key)
	if !ok {
		return config.Setting{}, fmt.Errorf("%w: %s", ErrUnknown, key)
	}
	if s.getenv(key) != "" {
		return config.Setting{}, fmt.Errorf("%w: %s", ErrFixed, key)
	}
	return setting, nil
}

func (s *Service) change(ctx context.Context, write func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := write(); err != nil {
		return err
	}
	if err := s.reload(ctx); err != nil {
		return err
	}
	close(s.changed)
	s.changed = make(chan struct{})
	return nil
}

// reload reads the rows and swaps the snapshot.
func (s *Service) reload(ctx context.Context) error {
	stored, err := s.store.Settings(ctx)
	if err != nil {
		return err
	}
	next := &state{}
	for _, setting := range config.Settings {
		entry := Entry{Setting: setting, Value: setting.Default, Source: FromDefault}
		if env := s.getenv(setting.Key); env != "" {
			if err := setting.Apply(&next.values, env); err != nil {
				return fmt.Errorf("%s: %w", setting.Key, err)
			}
			entry.Value, entry.Source = env, FromEnvironment
			next.entries = append(next.entries, entry)
			continue
		}
		if raw, ok := stored[setting.Key]; ok {
			if err := setting.Apply(&next.values, raw); err == nil {
				entry.Value, entry.Source = raw, FromStore
				next.entries = append(next.entries, entry)
				continue
			}
			slog.WarnContext(ctx, "a stored setting no longer parses, so its default is in force",
				"key", setting.Key, "value", raw)
		}
		if err := setting.Apply(&next.values, ""); err != nil {
			return fmt.Errorf("%s: %w", setting.Key, err)
		}
		next.entries = append(next.entries, entry)
	}
	s.current.Store(next)
	return nil
}
