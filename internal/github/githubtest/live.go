package githubtest

import (
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Live makes the server act on the bare repository at origin the way GitHub acts on its own: an
// open PR's mergeable comes from a trial merge of its head into its base, MergePull really merges,
// a merge into defaultBranch closes the open issues the PR body names with a closing keyword, and a
// commit's time is its committer date. A HeadSHA not set yet reads origin too.
func (s *Server) Live(origin, defaultBranch string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.origin, s.defaultBranch = origin, defaultBranch
	if s.HeadSHA == nil {
		s.HeadSHA = func(branch string) string {
			sha, _ := s.git("rev-parse", "-q", "--verify", "refs/heads/"+branch)
			return sha
		}
	}
}

func (s *Server) git(args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", s.origin}, args...)...).Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		err = fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(exit.Stderr)))
	}
	return strings.TrimSpace(string(out)), err
}

// out starts with the merged tree, and lists the conflicts when clean is false. err is a failure
// of git itself.
func (s *Server) trialMerge(base, head string) (out string, clean bool, err error) {
	out, err = s.git("merge-tree", "--write-tree", base, head)
	var exit *exec.ExitError
	switch {
	case err == nil:
		return out, true, nil
	case errors.As(err, &exit) && exit.ExitCode() == 1:
		return out, false, nil
	}
	return "", false, err
}

// mergeable is what GitHub would answer for p: nil while it does not know, which under Live is
// only when a branch is missing. A value set with SetMergeable wins. Any other git failure is an
// error, or a worker would wait on the nil forever.
func (s *Server) mergeable(p *Pull) (*bool, error) {
	if s.origin == "" || !p.Open || p.Mergeable != nil {
		return p.Mergeable, nil
	}
	base, err := s.git("rev-parse", "-q", "--verify", "refs/heads/"+p.Base)
	if err != nil {
		return nil, nil
	}
	head, err := s.git("rev-parse", "-q", "--verify", "refs/heads/"+p.Head)
	if err != nil {
		return nil, nil
	}
	_, clean, err := s.trialMerge(base, head)
	if err != nil {
		return nil, fmt.Errorf("trial merge of PR #%d: %w", p.Number, err)
	}
	return &clean, nil
}

// merge makes the merge commit GitHub makes and moves p's base to it. GitHub refuses a PR that
// conflicts, and so does this, by failing the test.
func (s *Server) merge(p *Pull) {
	base, err := s.git("rev-parse", "--verify", "refs/heads/"+p.Base)
	if err != nil {
		s.t.Fatalf("githubtest: merge PR #%d: no base %s", p.Number, p.Base)
	}
	head, err := s.git("rev-parse", "--verify", "refs/heads/"+p.Head)
	if err != nil {
		s.t.Fatalf("githubtest: merge PR #%d: no head %s", p.Number, p.Head)
	}
	out, clean, err := s.trialMerge(base, head)
	if err != nil {
		s.t.Fatalf("githubtest: merge PR #%d: %v", p.Number, err)
	}
	if !clean {
		s.t.Fatalf("githubtest: PR #%d conflicts with %s and cannot be merged: %s", p.Number, p.Base, out)
	}
	tree, _, _ := strings.Cut(out, "\n")
	message := fmt.Sprintf("Merge pull request #%d from %s/%s", p.Number, s.Owner, p.Head)
	commit, err := s.git("-c", "user.name=GitHub", "-c", "user.email=noreply@github.com",
		"commit-tree", tree, "-p", base, "-p", head, "-m", message)
	if err != nil {
		s.t.Fatalf("githubtest: merge PR #%d: %v", p.Number, err)
	}
	// Naming the old base makes a push that raced the merge fail here instead of being lost.
	if _, err := s.git("update-ref", "refs/heads/"+p.Base, commit, base); err != nil {
		s.t.Fatalf("githubtest: merge PR #%d: %s moved during the merge", p.Number, p.Base)
	}
}

var closingKeyword = regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?):?\s+#(\d+)\b`)

// linked returns the open issues the merge of p closes under Live. GitHub closes them only for a
// merge into the default branch.
func (s *Server) linked(p *Pull) []int {
	if s.origin == "" || p.Base != s.defaultBranch {
		return nil
	}
	var out []int
	for _, m := range closingKeyword.FindAllStringSubmatch(p.Body, -1) {
		n, _ := strconv.Atoi(m[1])
		if i, ok := s.issues[n]; ok && i.Open && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}

func (s *Server) commitTime(sha string) (time.Time, bool) {
	out, err := s.git("show", "-s", "--format=%cI", sha)
	if err != nil {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339, out)
	return at, err == nil
}

// Labels returns the labels created in the repository, a name twice if it was created twice.
func (s *Server) Labels() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.labels)
}

// OpenPull opens a PR the way a person opens one, with a title and a body.
func (s *Server) OpenPull(head, base, title, body string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.newPull(head, base, title, body).Number
}

// SetNextNumber makes the next issue or PR number n.
func (s *Server) SetNextNumber(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.number = n - 1
}
