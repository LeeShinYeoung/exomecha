package worker

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/LeeShinYeoung/exomecha/internal/logging"
)

var (
	// The end of an output is where a failure shows.
	keptFileLimit = 1 << 20
	keptLimit     = int64(100 << 20)
)

// The run's files worth keeping: what the setup script and claude printed.
var keptFiles = []string{"setup.log", "stream.jsonl", "stderr.log"}

// keepOutputs copies what the run of ro for issue n printed, and outputs, file names to what else
// it printed, into a new directory of the issue's in OutputDir, for the person the issue goes to.
// It returns the directory, or "" when there was nothing to keep or keeping failed. Neither stops
// the issue going to stuck.
func (r *runs) keepOutputs(ctx context.Context, ro role, n int, outputs map[string]string) string {
	files := map[string][]byte{}
	for _, file := range keptFiles {
		// A file the run never wrote, or one the AI swapped for something else, is left out.
		if b, err := tailCtl(r.ctl(r.name(ro, n)), file, int64(keptFileLimit)); err == nil {
			files[file] = b
		}
	}
	for file, out := range outputs {
		files[file] = []byte(out[max(0, len(out)-keptFileLimit):])
	}
	if len(files) == 0 {
		return ""
	}
	// Named by the run's start, so keeping again after the stuck comment failed overwrites the same
	// directory. A start that stopped before claude has no start.json and keeps once.
	at := r.now()
	if info, err := os.Stat(filepath.Join(r.dir(r.name(ro, n)), "start.json")); err == nil {
		at = info.ModTime()
	}
	dir := filepath.Join(r.OutputDir, strconv.Itoa(n), at.UTC().Format("20060102T150405Z")+"-"+ro.name)
	if err := writeKept(dir, files); err != nil {
		slog.WarnContext(ctx, "side task failed", logging.Task, "keep the run's output", logging.Err, err)
		return ""
	}
	if err := trimKept(r.OutputDir, dir, keptLimit); err != nil {
		slog.WarnContext(ctx, "side task failed", logging.Task, "remove old kept outputs", logging.Err, err)
	}
	return dir
}

func writeKept(dir string, files map[string][]byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for file, b := range files {
		if err := os.WriteFile(filepath.Join(dir, file), b, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// trimKept removes the oldest runs' outputs in root, all but newest, until the rest fit in limit.
// A run's directory name starts with when the run started, or when it was kept for a start that
// stopped before claude, so the names sort oldest first across issues.
func trimKept(root, newest string, limit int64) error {
	type kept struct {
		dir  string
		size int64
	}
	var all []kept
	var total int64
	issues, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, issue := range issues {
		runDirs, err := os.ReadDir(filepath.Join(root, issue.Name()))
		if err != nil {
			return err
		}
		for _, runDir := range runDirs {
			dir := filepath.Join(root, issue.Name(), runDir.Name())
			files, err := os.ReadDir(dir)
			if err != nil {
				return err
			}
			var size int64
			for _, f := range files {
				info, err := f.Info()
				if err != nil {
					return err
				}
				size += info.Size()
			}
			all = append(all, kept{dir, size})
			total += size
		}
	}
	slices.SortFunc(all, func(a, b kept) int { return strings.Compare(filepath.Base(a.dir), filepath.Base(b.dir)) })
	for _, k := range all {
		if total <= limit {
			break
		}
		if k.dir == newest {
			continue
		}
		if err := os.RemoveAll(k.dir); err != nil {
			return err
		}
		total -= k.size
		// Fails while the issue has other runs' outputs, and removes it once it has none.
		os.Remove(filepath.Dir(k.dir))
	}
	return nil
}
