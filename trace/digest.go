package trace

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Digester builds an L1 digest: a short, human-readable summary of a run, derived from the L0 events alone.
// Add is called once per event, in trace order; Render writes the digest for everything added so far.
// [agent] Several implementations can exist (ActivityDigest is the first). Because a Digester only sees events,
// it works the same live (via NewDigestSubscriber) and offline (via Replay on a finished .jsonl file).
// Events are the redacted ones, so a digest never shows more than the L0 log does.
type Digester interface {
	Add(ev Event)
	Render(w io.Writer) error
}

// digestSubscriber is the Subscriber that feeds each L0 line to a Digester and renders the digest when the recorder closes.
type digestSubscriber struct {
	d Digester
	f *os.File
}

// NewDigestSubscriber creates <dir>/<runID>.digest.md (owner-only, 0600; create-only, like the JSONL file) and returns a
// Subscriber that feeds d. The digest is written when the recorder closes.
// [agent] Downside: a crash before Close leaves no digest; run Replay on the .jsonl file to rebuild it.
func NewDigestSubscriber(dir, runID string, d Digester) (Subscriber, error) {
	if runID == "" || filepath.Base(runID) != runID {
		return nil, fmt.Errorf("trace: invalid run ID %q", runID)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("trace: create directory: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, runID+".digest.md"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("trace: create digest file: %w", err)
	}
	return &digestSubscriber{d: d, f: f}, nil
}

// Write decodes one L0 line and hands the event to the Digester. A line that does not decode is an error (the recorder keeps the first one).
func (s *digestSubscriber) Write(line []byte) error {
	var ev Event
	if err := json.Unmarshal(line, &ev); err != nil {
		return fmt.Errorf("trace: digest: decode event: %w", err)
	}
	s.d.Add(ev)
	return nil
}

// Close renders the digest into the file, then closes it.
func (s *digestSubscriber) Close() error {
	renderErr := s.d.Render(s.f)
	return errors.Join(renderErr, s.f.Close())
}

// Replay feeds every event of a finished L0 .jsonl file to d, so a digest can be (re)built offline, e.g. after a crash or with a newer Digester.
// [agent] It reads with ReadBytes, not bufio.Scanner: Scanner stops at a fixed line size, and one event can be as large as traceMaxEventBytes.
func Replay(r io.Reader, d Digester) error {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			var ev Event
			if jerr := json.Unmarshal(line, &ev); jerr != nil {
				return fmt.Errorf("trace: replay: decode event: %w", jerr)
			}
			d.Add(ev)
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
