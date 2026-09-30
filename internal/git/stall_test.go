package git

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/LeeShinYeoung/coworker/internal/git/gittest"
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
	_, err = g.OpenBase(ctx)
	if err == nil {
		t.Fatal("OpenBase succeeded against a server that never answers")
	}
	if ctx.Err() != nil {
		t.Fatalf("git kept waiting until the test deadline: %v", err)
	}
	t.Logf("gave up after %s: %v", time.Since(start).Round(time.Second), err)
}
