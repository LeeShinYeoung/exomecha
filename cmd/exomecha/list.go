package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
)

func list(home string, out io.Writer) error {
	paths, err := filepath.Glob(filepath.Join(home, ".exomecha", "*.yaml"))
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		fmt.Fprintln(out, "No projects. Make one with: exomecha init <project>")
		return nil
	}
	w := tabwriter.NewWriter(out, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "NAME\tPRD REPO\tRUNNING")
	var problems []error
	for _, path := range paths {
		name := strings.TrimSuffix(filepath.Base(path), ".yaml")
		c, err := loadConfig(path)
		if err != nil {
			fmt.Fprintf(w, "%s\t-\t-\n", name)
			problems = append(problems, err)
			continue
		}
		state := "no"
		if up, err := running(projectRoot(home, name)); err != nil {
			state = "-"
			problems = append(problems, err)
		} else if up {
			state = "yes"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", name, c.prd.full(), state)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	return errors.Join(problems...)
}

// Unlike lock, running creates nothing.
func running(root string) (bool, error) {
	f, err := os.Open(filepath.Join(root, "lock"))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	// Shared, so two lists at the same time do not take each other for an exomecha.
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return true, nil
	}
	return false, err
}
