package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestUpdate(t *testing.T) {
	asset := fmt.Sprintf("exomecha_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	archive := tarGz(t, map[string]string{"LICENSE": "MIT", "exomecha": "exomecha 0.5.0"})
	sum := sha256.Sum256(archive)
	goodSums := hex.EncodeToString(sum[:]) + "  " + asset + "\n"
	badSums := strings.Repeat("0", 64) + "  " + asset + "\n"
	pluginList := func(v string) string {
		return `[{"id":"exomecha@exomecha","version":"` + v + `","scope":"user","enabled":true}]`
	}
	for _, c := range []struct {
		name            string
		version, sums   string
		plugins         string // what claude plugin list prints
		wantErr         bool
		wantExe         string
		wantCalls, want []string
	}{
		{
			name: "both behind", version: "0.4.0", sums: goodSums, plugins: pluginList("0.4.0"),
			wantExe:   "exomecha 0.5.0",
			wantCalls: []string{"plugin list --json", "plugin marketplace update exomecha", "plugin update exomecha@exomecha --scope user"},
			want: []string{"", "Update", "✓ The latest release is 0.5.0", "✓ exomecha updated from 0.4.0 to 0.5.0", "✓ exomecha plugin updated from 0.4.0 to 0.5.0",
				"", "Next", "Restart each exomecha run that is running, so it runs 0.5.0.", "Restart Claude Code, so it loads the new plugin."},
		},
		{
			name: "checksum mismatch", version: "0.4.0", sums: badSums, plugins: pluginList("0.4.0"),
			wantErr: true, wantExe: "exomecha 0.4.0",
			want: []string{"", "Update", "✓ The latest release is 0.5.0", "✗ exomecha not updated", "  " + asset + " does not match the release's checksum", ""},
		},
		{
			name: "both latest", version: "0.5.0", sums: goodSums, plugins: pluginList("0.5.0"),
			wantExe:   "exomecha 0.4.0",
			wantCalls: []string{"plugin list --json"},
			want:      []string{"", "Update", "✓ The latest release is 0.5.0", "✓ exomecha 0.5.0 is the latest", "✓ exomecha plugin 0.5.0 is the latest"},
		},
		{
			name: "no plugin", version: "0.5.0", sums: goodSums, plugins: "[]",
			wantExe:   "exomecha 0.4.0",
			wantCalls: []string{"plugin list --json", "plugin marketplace list --json", "plugin marketplace update exomecha", "plugin install exomecha@exomecha --scope user"},
			want: []string{"", "Update", "✓ The latest release is 0.5.0", "✓ exomecha 0.5.0 is the latest", "✓ exomecha plugin 0.5.0 installed",
				"", "Next", "Restart Claude Code, so it loads the new plugin."},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/latest", func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, `{"tag_name":"v0.5.0"}`)
			})
			mux.HandleFunc("/download/v0.5.0/"+asset, func(w http.ResponseWriter, r *http.Request) {
				w.Write(archive)
			})
			mux.HandleFunc("/download/v0.5.0/exomecha_0.5.0_checksums.txt", func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, c.sums)
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()
			calls := fakePluginClaude(t, c.plugins, `[{"name":"exomecha"}]`)
			exe := filepath.Join(t.TempDir(), "exomecha")
			if err := os.WriteFile(exe, []byte("exomecha 0.4.0"), 0o755); err != nil {
				t.Fatal(err)
			}

			var out strings.Builder
			u := updater{version: c.version, exe: exe, latestURL: srv.URL + "/latest", downloadURL: srv.URL + "/download"}
			if err := u.run(t.Context(), &out, false); (err != nil) != c.wantErr {
				t.Errorf("run = %v, want an error: %v", err, c.wantErr)
			}
			if got := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n"); !slices.Equal(got, c.want) {
				t.Errorf("run printed\n%s\nwant\n%s", out.String(), strings.Join(c.want, "\n"))
			}
			if got := calls(); !slices.Equal(got, c.wantCalls) {
				t.Errorf("claude ran %q, want %q", got, c.wantCalls)
			}
			data, err := os.ReadFile(exe)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != c.wantExe {
				t.Errorf("exomecha holds %q, want %q", data, c.wantExe)
			}
			if fi, err := os.Stat(exe); err != nil || fi.Mode().Perm() != 0o755 {
				t.Errorf("exomecha's mode = %v, %v, want 0755", fi.Mode(), err)
			}
			if entries, _ := os.ReadDir(filepath.Dir(exe)); len(entries) != 1 {
				t.Errorf("exomecha's folder holds %d files, want exomecha alone", len(entries))
			}
		})
	}
}

// fakePluginClaude puts a claude on PATH that answers the plugin and marketplace lists with plugins
// and markets. calls returns the arguments of each claude run, in order.
func fakePluginClaude(t *testing.T, plugins, markets string) (calls func() []string) {
	t.Helper()
	dir := t.TempDir()
	for name, content := range map[string]string{"plugins": plugins, "markets": markets} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	script := `#!/bin/sh
dir=$(dirname "$0")
echo "$*" >> "$dir/calls"
case "$*" in
"plugin list --json") cat "$dir/plugins" ;;
"plugin marketplace list --json") cat "$dir/markets" ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() []string {
		data, _ := os.ReadFile(filepath.Join(dir, "calls"))
		return strings.FieldsFunc(string(data), func(r rune) bool { return r == '\n' })
	}
}

// tarGz is a release archive holding files, in name order.
func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	w := tar.NewWriter(gz)
	for _, name := range slices.Sorted(maps.Keys(files)) {
		if err := w.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(files[name]))}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(files[name])); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
