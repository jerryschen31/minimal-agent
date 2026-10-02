package memory

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"mvp1/agent"
)

func TestFileAppendPersistsAndReloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.json")
	f, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ { // [agent] concurrent appends must all land on disk
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = f.Append(context.Background(), agent.Message{Role: agent.RoleUser, Content: "x"})
		}()
	}
	wg.Wait()
	g, err := OpenFile(path)
	if err != nil || len(g.msgs) != 20 {
		t.Fatalf("reloaded %d msgs, %v", len(g.msgs), err)
	}
	if ents, _ := os.ReadDir(filepath.Dir(path)); len(ents) != 1 {
		t.Fatalf("temp files left behind: %v", ents)
	}
}
