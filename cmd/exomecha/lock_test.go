package main

import (
	"strings"
	"testing"
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
