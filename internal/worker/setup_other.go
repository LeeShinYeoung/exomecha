//go:build !unix

package worker

import "os/exec"

// Process groups are a Unix feature. Elsewhere only the shell itself is killed.
func ownGroup(*exec.Cmd) {}

func killGroup(*exec.Cmd) error { return nil }
