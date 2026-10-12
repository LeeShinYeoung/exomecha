package worker

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/LeeShinYeoung/exomecha/internal/git"
	"github.com/LeeShinYeoung/exomecha/internal/github"
)

// Repo is a repository PRDs may change.
type Repo struct {
	Name   string // owner/name
	GitHub *github.Client
	Git    *git.Git
}

// folder is the repository's folder in a sandbox that holds several. The names of the
// repositories exomecha may change differ, so the folders do too.
func (r *Repo) folder() string {
	_, name, _ := strings.Cut(r.Name, "/")
	return name
}

const reposHeading = "## Repositories"

const reposFix = " Fix the section in the PRD's body, then relabel this issue as below. You don't need to answer."

// prdRepos returns the repositories the PRD's Repositories section lists, in its order. why says
// what is wrong with the section when it is missing, empty, or names a repository exomecha may not
// change, and only a person can fix that.
func (d Deps) prdRepos(prd github.Issue) (repos []*Repo, why string) {
	names, bad, found := parseRepos(prd.Body)
	switch {
	case !found:
		return nil, "The PRD has no `" + reposHeading + "` section, so exomecha does not know which repositories to change. List each one under it as `- owner/name`." + reposFix
	case bad != "":
		return nil, "An item in the PRD's `" + reposHeading + "` section is not `- owner/name`: `" + bad + "`." + reposFix
	case len(names) == 0:
		return nil, "The PRD's `" + reposHeading + "` section lists no repository." + reposFix
	}
	for _, name := range names {
		i := slices.IndexFunc(d.Repos, func(r *Repo) bool { return strings.EqualFold(r.Name, name) })
		if i < 0 {
			return nil, fmt.Sprintf("The PRD's `%s` section lists `%s`, which is not one of the repositories exomecha may change (%s).%s",
				reposHeading, name, d.repoNames(), reposFix)
		}
		if slices.Contains(repos, d.Repos[i]) {
			return nil, fmt.Sprintf("The PRD's `%s` section lists `%s` twice.%s", reposHeading, name, reposFix)
		}
		repos = append(repos, d.Repos[i])
	}
	return repos, ""
}

// parseRepos reads the list items under the Repositories heading, up to the next heading. Other
// lines are text around the list. bad is the first item that is not owner/name.
func parseRepos(body string) (names []string, bad string, found bool) {
	for line := range strings.Lines(body) {
		t := strings.TrimSpace(line)
		if !found {
			found = t == reposHeading
			continue
		}
		if t == "" {
			continue
		}
		if strings.HasPrefix(t, "#") {
			break
		}
		item, ok := strings.CutPrefix(t, "- ")
		if !ok {
			item, ok = strings.CutPrefix(t, "* ")
		}
		if !ok {
			continue
		}
		name := strings.Trim(strings.TrimSpace(item), "`")
		if strings.Count(name, "/") != 1 || strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") || strings.ContainsAny(name, " \t") {
			return nil, t, true
		}
		names = append(names, name)
	}
	return names, "", found
}

func (d Deps) repoNames() string {
	var names []string
	for _, r := range d.Repos {
		names = append(names, r.Name)
	}
	return strings.Join(names, ", ")
}

func (d Deps) repo(name string) (*Repo, bool) {
	i := slices.IndexFunc(d.Repos, func(r *Repo) bool { return r.Name == name })
	if i < 0 {
		return nil, false
	}
	return d.Repos[i], true
}

// ref is how something in repository repo names PRD n: a bare number in the PRD repository itself.
func (d Deps) ref(repo string, n int) string {
	return d.issueRef(repo, d.PRDRepo, n)
}

// issueRef is how something in repository from names issue or PR n of repository of.
func (d Deps) issueRef(from, of string, n int) string {
	if strings.EqualFold(from, of) {
		return "#" + strconv.Itoa(n)
	}
	return of + "#" + strconv.Itoa(n)
}

// dirOf is where repository r is in a sandbox for a run of count repositories: the run's whole
// directory for one, and a folder of its own in it for several.
func dirOf(count int, r *Repo) string {
	if count == 1 {
		return repoDir
	}
	return repoDir + "/" + r.folder()
}

// repoPR is a PRD's PR in one of its repositories.
type repoPR struct {
	repo *Repo
	// open is the open PR from the PRD's branch. Its Number is 0 when there is none.
	open github.PR
	// closed is the newest closed PR from the branch, read only when there is no open one.
	closed       int // 0 for none
	closedMerged bool
}

// prdPRs looks up the PRD's PRs in each of its repositories.
func prdPRs(ctx context.Context, repos []*Repo, n int) ([]repoPR, error) {
	var out []repoPR
	for _, repo := range repos {
		p := repoPR{repo: repo}
		number, ok, err := repo.GitHub.OpenPR(ctx, branchName(n))
		if err != nil {
			return nil, err
		}
		if ok {
			if p.open, err = repo.GitHub.PR(ctx, number); err != nil {
				return nil, err
			}
		} else if p.closed, p.closedMerged, _, err = repo.GitHub.LastClosedPR(ctx, branchName(n)); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func openPRs(prs []repoPR) []repoPR {
	var out []repoPR
	for _, p := range prs {
		if p.open.Number != 0 {
			out = append(out, p)
		}
	}
	return out
}

// conflicting splits the PRDs in review between the workers: a PRD with an open PR that conflicts
// is resolver's, and any other, even one GitHub has not checked yet, is reviewer's.
func conflicting(prs []repoPR) bool {
	return slices.ContainsFunc(prs, func(p repoPR) bool { return p.open.Number != 0 && p.open.Mergeable != nil && !*p.open.Mergeable })
}

// prRefs names the PRs for a comment on the PRD.
func (d Deps) prRefs(prs []repoPR) string {
	var refs []string
	for _, p := range prs {
		refs = append(refs, "PR "+d.issueRef(d.PRDRepo, p.repo.Name, p.open.Number))
	}
	return strings.Join(refs, ", ")
}
