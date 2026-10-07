package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTempFile creates a file with the given contents in a per-test temp dir (deleted by Go
// after the test) and returns its path.
func writeTempFile(t *testing.T, name string, contents []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, contents, 0o644); err != nil {
		t.Fatalf("writing temp file: %v", err)
	}
	return path
}

// readFileArgs builds the JSON arguments the model would send for read_file.
func readFileArgs(path string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"path": path})
	return b
}

// - Verify that a small text file is returned in full, unchanged
func Test_ReadFile_SmallFile_ReturnsContents(t *testing.T) {
	path := writeTempFile(t, "minagent-read-file-test-small.txt", []byte("hello"))

	got, err := ReadFileTool{}.CallTool(context.Background(), readFileArgs(path))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "hello" {
		t.Errorf("expected %q, got %q", "hello", got)
	}
}

// - Verify that a file of exactly ReadFileMaxBytes is returned in full with no truncation note
// (the +1 byte read is what tells "exactly at the cap" apart from "over the cap")
func Test_ReadFile_ExactlyMaxBytes_NotTruncated(t *testing.T) {
	path := writeTempFile(t, "minagent-read-file-test-exact.txt", bytes.Repeat([]byte("a"), ReadFileMaxBytes))

	got, err := ReadFileTool{}.CallTool(context.Background(), readFileArgs(path))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != ReadFileMaxBytes {
		t.Errorf("expected %d bytes, got %d", ReadFileMaxBytes, len(got))
	}
	if strings.Contains(got, "[truncated") {
		t.Errorf("expected no truncation note for a file exactly at the cap")
	}
}

// - Verify that a file over ReadFileMaxBytes returns the first ReadFileMaxBytes plus a truncation
// note, so the model knows the file doesn't really end there
func Test_ReadFile_OverMaxBytes_TruncatedWithNote(t *testing.T) {
	path := writeTempFile(t, "minagent-read-file-test-big.txt", bytes.Repeat([]byte("a"), ReadFileMaxBytes+10))
	const note = "\n[truncated: file is larger than 64 KB]"

	got, err := ReadFileTool{}.CallTool(context.Background(), readFileArgs(path))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasSuffix(got, note) {
		t.Fatalf("expected output to end with the truncation note, got tail %q", got[len(got)-50:])
	}
	if len(got) != ReadFileMaxBytes+len(note) {
		t.Errorf("expected %d content bytes before the note, got %d", ReadFileMaxBytes, len(got)-len(note))
	}
}

// - Verify that every failure is returned as an error (for the model to read), never a panic
// and never an empty "success"
func Test_ReadFile_InvalidInputs_ReturnErrors(t *testing.T) {
	dir := t.TempDir()
	binary := writeTempFile(t, "minagent-read-file-test-bin.dat", []byte{'a', 0, 'b'})

	cases := map[string]struct {
		args    json.RawMessage
		wantErr string // substring the error message must contain
	}{
		"bad json":     {json.RawMessage(`{"path":`), "invalid arguments"},
		"empty path":   {json.RawMessage(`{}`), "missing required argument: path"},
		"missing file": {readFileArgs(filepath.Join(dir, "nope.txt")), "no such file"},
		"directory":    {readFileArgs(dir), "is a directory"},
		"binary file":  {readFileArgs(binary), "binary file"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := ReadFileTool{}.CallTool(context.Background(), tc.args)
			if err == nil {
				t.Fatalf("expected an error, got nil (output %q)", got)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("expected error containing %q, got %q", tc.wantErr, err.Error())
			}
			if got != "" {
				t.Errorf("expected empty output on error, got %q", got)
			}
		})
	}
}
