package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSecondExomechaOnARepositoryDoesNotStart(t *testing.T) {
	root := t.TempDir()
	first, err := lock(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lock(root); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Errorf("second lock = %v, want already running", err)
	}
	first.Close()
	again, err := lock(root)
	if err != nil {
		t.Fatalf("lock after the first let go = %v", err)
	}
	again.Close()
}

func TestLockWaitsForListToLetGo(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filepath.Join(root, "lock"))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(200*time.Millisecond, func() { f.Close() })
	held, err := lock(root)
	if err != nil {
		t.Fatalf("lock while list held it for a moment = %v", err)
	}
	held.Close()
}
