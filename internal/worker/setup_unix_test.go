//go:build unix

package worker

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const spawnChild = "sleep 60 &\necho $! > \"$SETUP_PID\"\n"

func pidFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pid")
	t.Setenv("SETUP_PID", path)
	return path
}

func wantProcessGone(t *testing.T, pidFile string) {
	t.Helper()
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	// The kill is already sent. It only has to land.
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if processGone(pid) {
			return
		}
		if time.Now().After(deadline) {
			syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("process %d, started by the setup script, is still running", pid)
		}
	}
}

func processGone(pid int) bool {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return true
	}
	// A killed process still answers signal 0 until something reaps it, and in a container
	// whose first process does not reap orphans, nothing ever does.
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	return err == nil && strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}

func TestSetupKillsWhatTheScriptStartedWhenInterrupted(t *testing.T) {
	pid := pidFile(t)
	started := filepath.Join(t.TempDir(), "started")
	t.Setenv("SETUP_STARTED", started)
	e := newEnv(t, setupRepo(spawnChild+"touch \"$SETUP_STARTED\"\nwait\n"))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cancelWhenExists(ctx, cancel, started)

	if _, problem, err := e.runSetup(ctx); problem != "" || !errors.Is(err, context.Canceled) {
		t.Errorf("setup = problem %q, err %v, want only context.Canceled", problem, err)
	}

	wantProcessGone(t, pid)
}

func TestSetupKillsWhatTheScriptStartedWhenItTimesOut(t *testing.T) {
	setupTimeout = 300 * time.Millisecond
	t.Cleanup(func() { setupTimeout = 10 * time.Minute })
	pid := pidFile(t)
	e := newEnv(t, setupRepo(spawnChild+"wait\n"))

	e.wantSetupProblem("did not end cleanly")

	wantProcessGone(t, pid)
}

func TestSetupKillsWhatTheScriptStartedWhenItFails(t *testing.T) {
	setupWaitDelay = 100 * time.Millisecond
	t.Cleanup(func() { setupWaitDelay = 5 * time.Second })
	pid := pidFile(t)
	e := newEnv(t, setupRepo(spawnChild+"exit 1\n"))

	e.wantSetupProblem("did not end cleanly (exit status 1)")

	wantProcessGone(t, pid)
}

// The script exits cleanly, with a child that holds nothing open, and is rejected for another reason.
func TestSetupKillsWhatARejectedScriptStarted(t *testing.T) {
	pid := pidFile(t)
	e := newEnv(t, setupRepo("sleep 60 >/dev/null 2>&1 &\necho $! > \"$SETUP_PID\"\necho stray > stray.txt\n"))

	e.wantSetupProblem("changed the working copy")

	wantProcessGone(t, pid)
}

func TestSetupKillsTheProcessTheScriptLeftRunning(t *testing.T) {
	setupWaitDelay = 100 * time.Millisecond
	t.Cleanup(func() { setupWaitDelay = 5 * time.Second })
	pid := pidFile(t)
	e := newEnv(t, setupRepo(spawnChild))

	e.wantSetupProblem("did not end cleanly")

	wantProcessGone(t, pid)
}
