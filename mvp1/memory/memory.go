// Package memory holds agent.Memory implementations: where the transcript lives.
package memory

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"mvp1/agent"
)

// [agent] InMemory keeps the transcript for the life of the process.
// The zero value is ready to use. Good for subagents and tests.
type InMemory struct {
	mu   sync.Mutex
	msgs []agent.Message
}

func (m *InMemory) Append(_ context.Context, msgs ...agent.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.msgs = append(m.msgs, msgs...)
	return nil
}

func (m *InMemory) Messages(_ context.Context) ([]agent.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]agent.Message(nil), m.msgs...), nil // [agent] copy: callers can't mutate our state
}

// [agent] File persists the transcript as JSON so sessions survive restarts.
// It is deliberately a stub-grade store (rewrite whole file per append): the
// point is the interface. Swap for SQLite/Postgres/vector DB by implementing
// agent.Memory; nothing else changes.
type File struct {
	Path string
	InMemory
}

func OpenFile(path string) (*File, error) {
	f := &File{Path: path}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return nil, err
	}
	return f, json.Unmarshal(b, &f.msgs)
}

func (f *File) Append(_ context.Context, msgs ...agent.Message) error {
	// [agent] Snapshot and write under one lock so two concurrent appends can't
	// persist an older snapshot after a newer one. Write via temp file + rename so
	// an interrupted write never leaves a truncated transcript that OpenFile rejects.
	f.mu.Lock()
	defer f.mu.Unlock()
	f.msgs = append(f.msgs, msgs...)
	b, err := json.MarshalIndent(f.msgs, "", " ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(f.Path), "."+filepath.Base(f.Path)+".*")
	if err != nil {
		return err
	}
	if _, err = tmp.Write(b); err == nil {
		err = tmp.Chmod(0o600)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), f.Path)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
	}
	return err
}
