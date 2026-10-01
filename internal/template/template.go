package template

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

type Files interface {
	BaseFile(ctx context.Context, path string) (content string, ok bool, err error)
}

// Problem is a failure only a person can fix on the base branch. Its Reason goes into the
// Task's stuck comment.
type Problem struct {
	Reason string
	Output string // Log only: build output can hold secrets.
}

func (p *Problem) Error() string { return p.Reason }

func (p *Problem) Why() string { return p.Reason }

type Env struct {
	files Files
	slug  string
}

// The same max the task sandbox names use, so every name of one project carries one slug:
// 63 chars minus "coworker-task-" and "-9999999-resolver".
const slugMax = 32

func New(files Files, owner, repo string) *Env {
	return &Env{files: files, slug: Slug(owner, repo, slugMax)}
}

func Slug(owner, repo string, max int) string {
	var b strings.Builder
	for _, r := range strings.ToLower(owner + "-" + repo) {
		switch {
		case 'a' <= r && r <= 'z' || '0' <= r && r <= '9':
			b.WriteRune(r)
		case !strings.HasSuffix(b.String(), "-"):
			b.WriteByte('-')
		}
	}
	slug := strings.Trim(b.String(), "-")
	if len(slug) <= max {
		return slug
	}
	// Lowercase like the slug, since GitHub matches owner and repo names in any case.
	sum := sha256.Sum256([]byte(strings.ToLower(owner + "/" + repo)))
	hash := hex.EncodeToString(sum[:])[:8]
	keep := max - len(hash) - 1
	switch {
	case max <= 0:
		return ""
	case keep <= 0:
		return hash[:min(len(hash), max)]
	}
	return strings.TrimRight(slug[:keep], "-") + "-" + hash
}

func (e *Env) repository() string { return "docker.io/library/coworker-" + e.slug }

func (e *Env) helper() string { return "coworker-build-" + e.slug }

func (e *Env) ref(dockerfile string) string {
	sum := sha256.Sum256([]byte(dockerfile))
	return e.repository() + ":" + hex.EncodeToString(sum[:])[:12]
}
