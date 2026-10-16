package agent

import (
	"bytes"
	"io"
	"sync"
)

// lineWriter prefixes every line written through it (e.g. "[t3 w2] ") and writes whole lines only, under
// a mutex shared by all the lineWriters of one output. So output from tasks running at the same time
// interleaves line by line, never mid-line.
// [agent] It buffers a partial line until its newline arrives; Flush writes what is left at the end. The
// lock covers the buffer too, because a session's background compaction can print while its turn does.
type lineWriter struct {
	w      io.Writer
	mu     *sync.Mutex
	prefix string
	buf    []byte
}

func (lw *lineWriter) Write(p []byte) (int, error) {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	lw.buf = append(lw.buf, p...)
	for {
		i := bytes.IndexByte(lw.buf, '\n')
		if i < 0 {
			return len(p), nil
		}
		if _, err := lw.w.Write(append([]byte(lw.prefix), lw.buf[:i+1]...)); err != nil {
			return len(p), err
		}
		lw.buf = lw.buf[i+1:]
	}
}

// Flush writes a buffered partial line, with a newline added.
func (lw *lineWriter) Flush() error {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	if len(lw.buf) == 0 {
		return nil
	}
	_, err := lw.w.Write(append(append([]byte(lw.prefix), lw.buf...), '\n'))
	lw.buf = nil
	return err
}
