package template

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

type fakeFiles map[string]string

func (f fakeFiles) BaseFile(_ context.Context, path string) (string, bool, error) {
	content, ok := f[path]
	return content, ok, nil
}

func TestParseHosts(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    []string
	}{
		{"one host", "api.expo.dev\n", []string{"api.expo.dev"}},
		{"comments and blank lines", "# for mobile lint\napi.expo.dev # needed by lint\n\n   \n*.npmjs.org\n", []string{"api.expo.dev", "*.npmjs.org"}},
		{"no trailing newline", "api.expo.dev", []string{"api.expo.dev"}},
		{"CRLF", "a.example.com\r\nb.example.com\r\n", []string{"a.example.com", "b.example.com"}},
		{"surrounding whitespace", "\t api.expo.dev \t\n", []string{"api.expo.dev"}},
		{"sbx pattern forms", "**.example.com\napi?.example.com\napi[!1].example.com\nexample.com:443\n10.0.0.0/8\n[2001:db8::1]:443\n", []string{"**.example.com", "api?.example.com", "api[!1].example.com", "example.com:443", "10.0.0.0/8", "[2001:db8::1]:443"}},
		{"only comments", "# nothing yet\n", nil},
		{"empty", "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseHosts(tt.content)
			if err != nil || !slices.Equal(got, tt.want) {
				t.Errorf("parseHosts(%q) = %q, %v, want %q", tt.content, got, err, tt.want)
			}
		})
	}
}

func TestParseHostsRejects(t *testing.T) {
	tests := []struct {
		name    string
		content string
		line    string
	}{
		{"space inside", "ok.example.com\napi.expo.dev cdn.expo.dev\n", "line 2 (`api.expo.dev cdn.expo.dev`)"},
		{"tab inside", "a.example.com\tb.example.com\n", "line 1"},
		{"comma", "a.example.com,b.example.com\n", "line 1"},
		{"allow all", "# everything\n**\n", "line 2 (`**`)"},
		{"allow all on a port", "**:443\n", "line 1"},
		{"URL", "https://api.expo.dev\n", "line 1"},
		{"other character", "api.expo.dev/v2;x\n", "line 1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseHosts(tt.content)
			var p *Problem
			if !errors.As(err, &p) {
				t.Fatalf("parseHosts(%q) = %q, %v, want a Problem", tt.content, got, err)
			}
			if !strings.Contains(p.Why(), hostsPath) || !strings.Contains(p.Why(), tt.line) {
				t.Errorf("Why() = %q, want the file and %q", p.Why(), tt.line)
			}
		})
	}
}

func TestAllowedHosts(t *testing.T) {
	e := New(fakeFiles{hostsPath: "api.expo.dev # mobile lint\n"}, "owner", "repo")
	if got, err := e.AllowedHosts(t.Context()); err != nil || !slices.Equal(got, []string{"api.expo.dev"}) {
		t.Errorf("AllowedHosts = %q, %v", got, err)
	}

	e = New(fakeFiles{}, "owner", "repo")
	if got, err := e.AllowedHosts(t.Context()); err != nil || got != nil {
		t.Errorf("AllowedHosts without the file = %q, %v, want nil", got, err)
	}
}
