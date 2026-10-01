package template

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestSlug(t *testing.T) {
	tests := []struct {
		owner, repo string
		max         int
		want        string
	}{
		{"LeeShinYeoung", "jazzing", 32, "leeshinyeoung-jazzing"},
		{"owner", "my.repo_name", 32, "owner-my-repo-name"},
		{"owner", "a..--b", 32, "owner-a-b"},
		{"-owner-", ".repo.", 32, "owner-repo"},
		{"o", strings.Repeat("r", 61), 63, "o-" + strings.Repeat("r", 61)},
	}
	for _, tt := range tests {
		if got := Slug(tt.owner, tt.repo, tt.max); got != tt.want {
			t.Errorf("Slug(%q, %q, %d) = %q, want %q", tt.owner, tt.repo, tt.max, got, tt.want)
		}
	}
}

func TestSlugTooLong(t *testing.T) {
	owner, repo := "o", strings.Repeat("r", 62)
	sum := sha256.Sum256([]byte("o/" + repo))
	hash := hex.EncodeToString(sum[:])[:8]

	got := Slug(owner, repo, 63)
	if want := "o-" + strings.Repeat("r", 52) + "-" + hash; got != want {
		t.Errorf("Slug of 64 chars = %q, want %q", got, want)
	}
	if len(got) != 63 {
		t.Errorf("len = %d, want 63", len(got))
	}
	if other := Slug(owner, repo+"x", 63); other == got {
		t.Errorf("two repos with the same first 54 chars share the slug %q", got)
	}
	if upper := Slug("O", strings.ToUpper(repo), 63); upper != got {
		t.Errorf("Slug in upper case = %q, want the same as lower case %q", upper, got)
	}
}

func TestSlugCutAtADash(t *testing.T) {
	// The cut lands right after "owner-", which must not leave "--" before the hash.
	got := Slug("owner", strings.Repeat("r", 30), 6+9)
	if strings.Contains(got, "--") || !strings.HasPrefix(got, "owner-") || len(got) != 14 {
		t.Errorf("Slug = %q, want owner-<hash>", got)
	}
}

func TestNames(t *testing.T) {
	e := New(fakeFiles{}, "LeeShinYeoung", "jazzing")
	if got := e.helper(); got != "coworker-build-leeshinyeoung-jazzing" {
		t.Errorf("helper = %q", got)
	}
	ref := e.ref("FROM docker/sandbox-templates:claude-code-docker\n")
	repo, tag, _ := strings.Cut(ref, ":")
	if repo != "docker.io/library/coworker-leeshinyeoung-jazzing" || len(tag) != 12 {
		t.Errorf("ref = %q, want docker.io/library/coworker-<slug>:<12 hex>", ref)
	}
	if other := e.ref("FROM docker/sandbox-templates:claude-code-docker\nRUN true\n"); other == ref {
		t.Errorf("two Dockerfiles share the ref %q", ref)
	}

	long := New(fakeFiles{}, "some-organization", strings.Repeat("long-repository-name-", 3))
	if n := len("coworker-task-" + long.slug + "-9999999-resolver"); n > 63 {
		t.Errorf("task sandbox name is %d chars, sbx allows 63", n)
	}
}
