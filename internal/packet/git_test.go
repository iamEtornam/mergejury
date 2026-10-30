package packet

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

func TestWorktreesConcurrentAdapters(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-qm", "base"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	w, err := NewWorktrees(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Cleanup()
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			id := fmt.Sprintf("adapter-%d", i)
			dest, err := w.Add(ctx, id, "HEAD")
			if err != nil {
				t.Errorf("add %s: %v", id, err)
				return
			}
			if _, err := os.Stat(filepath.Join(dest, ".git")); err != nil {
				t.Errorf("missing worktree %s: %v", id, err)
			}
			for j := 0; j < 20; j++ {
				if err := w.AssertClean(ctx, id); err != nil {
					t.Errorf("clean %s: %v", id, err)
					return
				}
			}
		}(i)
	}
	close(start)
	wg.Wait()
}
