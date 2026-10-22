package trace

import (
	"fmt"
	"os"
	"path/filepath"
)

// jsonlSubscriber is the Subscriber that writes each event as one line of a <dir>/<run_id>.jsonl file.
type jsonlSubscriber struct {
	f *os.File
}

// NewJSONLSubscriber creates dir (owner-only, 0700) if needed and a new trace file <dir>/<runID>.jsonl (owner-only, 0600).
// It fails if the file already exists: a run never appends to or overwrites another run's trace.
func NewJSONLSubscriber(dir, runID string) (Subscriber, error) {
	if runID == "" || filepath.Base(runID) != runID { // the run ID becomes a file name, so it must not carry a path
		return nil, fmt.Errorf("trace: invalid run ID %q", runID)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("trace: create directory: %w", err)
	}
	// O_EXCL: create-only, and it does not follow a symlink planted at the path
	f, err := os.OpenFile(filepath.Join(dir, runID+".jsonl"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("trace: create file: %w", err)
	}
	return &jsonlSubscriber{f: f}, nil
}

// Write appends line and a newline. [agent] It is one Write call and the file is not buffered, so each event reaches
// the operating system at once and a crash of the agent loses at most the event being written.
func (s *jsonlSubscriber) Write(line []byte) error {
	buf := make([]byte, 0, len(line)+1) // a copy, so the caller's slice is never touched
	buf = append(buf, line...)
	buf = append(buf, '\n')
	_, err := s.f.Write(buf)
	return err
}

// Close flushes the file to disk and closes it.
func (s *jsonlSubscriber) Close() error {
	syncErr := s.f.Sync()
	if err := s.f.Close(); err != nil {
		return err
	}
	return syncErr
}
