package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/term"

	"github.com/LeeShinYeoung/exomecha/internal/agent"
)

// updater is what update needs from outside: main gives this exomecha's, and the tests their own.
type updater struct {
	version     string
	exe         string // the file update replaces
	latestURL   string // GitHub's API URL of the latest release
	downloadURL string // the URL release files are under, by tag
}

func update(ctx context.Context, out *os.File) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	u := updater{
		version:     version,
		exe:         exe,
		latestURL:   "https://api.github.com/repos/LeeShinYeoung/exomecha/releases/latest",
		downloadURL: "https://github.com/LeeShinYeoung/exomecha/releases/download",
	}
	return u.run(ctx, out, term.IsTerminal(int(out.Fd())))
}

// run brings exomecha and the user's exomecha plugin to the latest release, each only when it is
// behind. The plugin waits for exomecha, so a failed exomecha leaves the plugin as it is.
func (u updater) run(ctx context.Context, out io.Writer, spin bool) error {
	claude := agent.Claude{}
	var latest string
	var updated, pluginUpdated bool
	ok, err := report(ctx, out, spin, []item{
		{group: "Update", doing: "Finding the latest release", gate: true, run: func(ctx context.Context) (string, []error) {
			v, err := latestRelease(ctx, u.latestURL)
			if err != nil {
				return "Could not find the latest release", []error{err}
			}
			latest = v
			return "The latest release is " + v, nil
		}},
		{group: "Update", doing: "Updating exomecha", gate: true, run: func(ctx context.Context) (string, []error) {
			if u.version == latest {
				return "exomecha " + latest + " is the latest", nil
			}
			if err := u.replace(ctx, latest); err != nil {
				return "exomecha not updated", []error{err}
			}
			updated = true
			return fmt.Sprintf("exomecha updated from %s to %s", u.version, latest), nil
		}},
		{group: "Update", doing: "Updating the exomecha plugin", run: func(ctx context.Context) (string, []error) {
			v, err := claude.PluginVersion(ctx)
			if err != nil {
				return "exomecha plugin not updated", []error{err}
			}
			if v == latest {
				return "exomecha plugin " + latest + " is the latest", nil
			}
			line := fmt.Sprintf("exomecha plugin updated from %s to %s", v, latest)
			if v == "" {
				err = claude.InstallPlugin(ctx)
				line = "exomecha plugin " + latest + " installed"
			} else {
				err = claude.UpdatePlugin(ctx)
			}
			if err != nil {
				return "exomecha plugin not updated", []error{err}
			}
			pluginUpdated = true
			return line, nil
		}},
	})
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintln(out)
		return errors.New("fix the problems above, then run exomecha update again")
	}
	if updated || pluginUpdated {
		printHeading(out, "Next")
	}
	if updated {
		fmt.Fprintf(out, "Restart each exomecha run that is running, so it runs %s.\n", latest)
	}
	if pluginUpdated {
		fmt.Fprintln(out, "Restart Claude Code, so it loads the new plugin.")
	}
	return nil
}

func latestRelease(ctx context.Context, url string) (string, error) {
	data, err := get(ctx, url)
	if err != nil {
		return "", err
	}
	var release struct {
		TagName string `json:"tag_name"`
	}
	if err := json.Unmarshal(data, &release); err != nil {
		return "", fmt.Errorf("%s: %w", url, err)
	}
	return strings.TrimPrefix(release.TagName, "v"), nil
}

// replace puts release v's exomecha for this machine in place of u.exe, once the archive matches
// the release's checksum.
func (u updater) replace(ctx context.Context, v string) error {
	asset := fmt.Sprintf("exomecha_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	dir := u.downloadURL + "/v" + v + "/"
	archive, err := get(ctx, dir+asset)
	if err != nil {
		return err
	}
	sums, err := get(ctx, dir+"exomecha_"+v+"_checksums.txt")
	if err != nil {
		return err
	}
	if !checksumMatches(sums, asset, archive) {
		return fmt.Errorf("%s does not match the release's checksum", asset)
	}
	bin, err := untar(archive, "exomecha")
	if err != nil {
		return err
	}
	// Written beside exe and renamed over it: Linux refuses to write into the file of a running
	// program, and a rename works only within one file system.
	f, err := os.CreateTemp(filepath.Dir(u.exe), ".exomecha-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(bin)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(f.Name(), 0o755)
	}
	if err == nil {
		err = os.Rename(f.Name(), u.exe)
	}
	return err
}

func get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// sums holds a "<sha256>  <file>" line for each file of the release.
func checksumMatches(sums []byte, name string, data []byte) bool {
	sum := sha256.Sum256(data)
	for line := range strings.Lines(string(sums)) {
		if f := strings.Fields(line); len(f) == 2 && f[1] == name {
			return f[0] == hex.EncodeToString(sum[:])
		}
	}
	return false
}

func untar(archive []byte, name string) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	r := tar.NewReader(gz)
	for {
		h, err := r.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("the archive has no %s", name)
		}
		if err != nil {
			return nil, err
		}
		if h.Name == name {
			return io.ReadAll(r)
		}
	}
}
