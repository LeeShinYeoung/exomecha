package git

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/LeeShinYeoung/exomecha/internal/git/gittest"
)

func TestCloneGivesUpOnStalledServer(t *testing.T) {
	gittest.Setup(t)
	lowSpeedTime = "2"
	t.Cleanup(func() { lowSpeedTime = "30" })

	// Accepts the connection and reads the request, then never answers.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { conn.Close() })
			go io.Copy(io.Discard, conn)
		}
	}()

	g := NewAt(t.TempDir(), "http://"+ln.Addr().String()+"/repo.git", "main", "token")
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	start := time.Now()
	err = g.Sync(ctx)
	if err == nil {
		t.Fatal("Sync succeeded against a server that never answers")
	}
	if ctx.Err() != nil {
		t.Fatalf("git kept waiting until the test deadline: %v", err)
	}
	t.Logf("gave up after %s: %v", time.Since(start).Round(time.Second), err)
}

func TestFetchFromGivesUpOnAHungSandbox(t *testing.T) {
	gittest.Setup(t)
	sandboxTransferTimeout = time.Second
	t.Cleanup(func() { sandboxTransferTimeout = 5 * time.Minute })
	origin := gittest.NewOrigin(t, map[string]string{"README.md": "hello\n"})
	g := NewAt(t.TempDir(), origin, "main", "token")
	if err := g.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	// An ext command that never speaks, as an sbx exec that hangs would.
	if _, err := g.FetchFrom(t.Context(), "ext::sleep 30", "box"); err == nil {
		t.Fatal("FetchFrom succeeded from a sandbox that never answers")
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("FetchFrom waited %v", took)
	}
	if err := g.Sync(t.Context()); err != nil {
		t.Errorf("Sync after a hung fetch: %v", err)
	}
}
