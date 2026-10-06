package githubtest

import (
	"cmp"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Server is an in-memory GitHub API that covers only the endpoints exomecha uses.
// Issue IDs are kept apart from issue numbers, so sending a number where GitHub wants an ID fails.
// Any request it does not know fails the test.
type Server struct {
	URL   string
	Owner string
	Repo  string

	// HeadSHA returns the commit a branch points at. PR heads are read through it on every
	// request, so a push to the branch shows up right away.
	HeadSHA func(branch string) string

	// The token's user is Owner. Its profile name may be empty, as GitHub allows.
	UserID   int64
	UserName string

	t      *testing.T
	mu     sync.Mutex
	number int
	id     int64
	issues map[int]*Issue
	// listed, once HoldList sets it, is what the issue list answers with instead of issues.
	listed map[int]*Issue
	pulls  map[int]*Pull
	labels []string
	runs   map[string][]Run
	times  map[string]time.Time

	routes   map[string]bool
	fails    map[string]int // route to the status it answers with
	refusals map[int]refusal
	closes   map[int]int // PR to the issue its merge closes
}

type refusal struct {
	message   string
	mergeable *bool
}

type Issue struct {
	ID          int64
	Number      int
	Title       string
	Body        string
	Labels      []string
	Open        bool
	StateReason string
	Comments    []Comment
	Association string
	SubIssues   []int
	BlockedBy   []int
}

type Comment struct {
	Body        string
	Association string
}

// exomecha writes with the repository owner's token, so what it creates comes back as OWNER.
const (
	associationOwner = "OWNER"
	associationNone  = "NONE"
)

func (i Issue) HasLabel(name string) bool {
	return slices.Contains(i.Labels, name)
}

type Pull struct {
	Number      int
	Head        string
	Base        string
	Title       string
	Body        string
	Open        bool
	Mergeable   *bool
	Reviews     []Review
	Merged      bool
	MergeMethod string
	MergedSHA   string
}

type Review struct {
	Body        string
	Event       string
	Association string
}

type Run struct {
	Name       string
	Status     string
	Conclusion string
	URL        string
}

func New(t *testing.T) *Server {
	s := &Server{
		Owner:  "owner",
		Repo:   "repo",
		t:      t,
		id:     9000,
		UserID: 4242,
		issues: map[int]*Issue{},
		pulls:  map[int]*Pull{},
		runs:   map[string][]Run{},
		times:  map[string]time.Time{},

		routes:   map[string]bool{},
		fails:    map[string]int{},
		refusals: map[int]refusal{},
		closes:   map[int]int{},
	}
	mux := http.NewServeMux()
	const repo = "/repos/{owner}/{repo}"
	s.route(mux, "GET /user", s.getUser)
	s.route(mux, "GET "+repo+"/issues", s.listIssues)
	s.route(mux, "GET "+repo+"/issues/{n}", s.getIssue)
	s.route(mux, "PATCH "+repo+"/issues/{n}", s.updateIssue)
	s.route(mux, "GET "+repo+"/issues/{n}/comments", s.listComments)
	s.route(mux, "POST "+repo+"/issues/{n}/comments", s.createComment)
	s.route(mux, "GET "+repo+"/labels", s.listLabels)
	s.route(mux, "POST "+repo+"/labels", s.createLabel)
	s.route(mux, "POST "+repo+"/issues/{n}/labels", s.addLabels)
	s.route(mux, "PUT "+repo+"/issues/{n}/labels", s.setLabels)
	s.route(mux, "GET "+repo+"/issues/{n}/sub_issues", s.listSubIssues)
	s.route(mux, "GET "+repo+"/issues/{n}/dependencies/blocked_by", s.listBlockedBy)
	s.route(mux, "GET "+repo+"/pulls", s.listPulls)
	s.route(mux, "POST "+repo+"/pulls", s.createPull)
	s.route(mux, "GET "+repo+"/pulls/{n}", s.getPull)
	s.route(mux, "GET "+repo+"/pulls/{n}/reviews", s.listReviews)
	s.route(mux, "POST "+repo+"/pulls/{n}/reviews", s.createReview)
	s.route(mux, "PUT "+repo+"/pulls/{n}/merge", s.merge)
	s.route(mux, "GET "+repo+"/actions/runs", s.listRuns)
	s.route(mux, "GET "+repo+"/git/commits/{sha}", s.getCommit)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("githubtest: unexpected request %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	s.URL = srv.URL + "/"
	return s
}

type handler func(r *http.Request) (int, any)

func (s *Server) route(mux *http.ServeMux, pattern string, h handler) {
	s.routes[pattern] = true
	mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(pattern, "{owner}") && (r.PathValue("owner") != s.Owner || r.PathValue("repo") != s.Repo) {
			s.t.Errorf("githubtest: request for another repository: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		s.mu.Lock()
		status, body := s.fails[pattern], message("githubtest: failed on purpose")
		if status == 0 {
			status, body = h(r)
		}
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if err := json.NewEncoder(w).Encode(body); err != nil {
			s.t.Errorf("githubtest: encode response: %v", err)
		}
	})
}

func message(text string) any { return map[string]string{"message": text} }

func notFound() (int, any) { return http.StatusNotFound, message("Not Found") }

func (s *Server) decode(r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		s.t.Errorf("githubtest: decode %s %s: %v", r.Method, r.URL.Path, err)
		return false
	}
	return true
}

func (s *Server) pathIssue(r *http.Request) *Issue {
	n, _ := strconv.Atoi(r.PathValue("n"))
	return s.issues[n]
}

func (s *Server) pathPull(r *http.Request) *Pull {
	n, _ := strconv.Atoi(r.PathValue("n"))
	return s.pulls[n]
}

func (s *Server) head(p *Pull) string {
	if s.HeadSHA == nil {
		return ""
	}
	return s.HeadSHA(p.Head)
}

func issueJSON(i *Issue) any {
	labels := []any{}
	for _, l := range i.Labels {
		labels = append(labels, map[string]string{"name": l})
	}
	state := "open"
	if !i.Open {
		state = "closed"
	}
	m := map[string]any{
		"id":                 i.ID,
		"number":             i.Number,
		"title":              i.Title,
		"body":               i.Body,
		"state":              state,
		"labels":             labels,
		"author_association": i.Association,
	}
	if i.StateReason != "" {
		m["state_reason"] = i.StateReason
	}
	return m
}

func (s *Server) issuesJSON(numbers []int) any {
	out := []any{}
	for _, n := range numbers {
		out = append(out, issueJSON(s.issues[n]))
	}
	return out
}

func labelsJSON(names []string) any {
	out := []any{}
	for _, l := range names {
		out = append(out, map[string]string{"name": l})
	}
	return out
}

func (s *Server) pullJSON(p *Pull) any {
	state := "open"
	if !p.Open {
		state = "closed"
	}
	m := map[string]any{
		"number":    p.Number,
		"state":     state,
		"title":     p.Title,
		"body":      p.Body,
		"head":      map[string]string{"ref": p.Head, "sha": s.head(p)},
		"base":      map[string]string{"ref": p.Base},
		"mergeable": p.Mergeable,
		"merged":    p.Merged,
	}
	if p.Merged {
		m["merged_at"] = commentEpoch.Format(time.RFC3339)
	}
	return m
}

func (s *Server) getUser(*http.Request) (int, any) {
	return http.StatusOK, map[string]any{"login": s.Owner, "id": s.UserID, "name": s.UserName}
}

func (s *Server) newIssue(title, body string, labels []string) *Issue {
	s.number++
	s.id++
	i := &Issue{ID: s.id, Number: s.number, Title: title, Body: body, Labels: slices.Clone(labels), Open: true, Association: associationOwner}
	s.issues[i.Number] = i
	return i
}

func (s *Server) listIssues(r *http.Request) (int, any) {
	q := r.URL.Query()
	state := cmp.Or(q.Get("state"), "open")
	var want []string
	if l := q.Get("labels"); l != "" {
		want = strings.Split(l, ",")
	}
	issues := s.issues
	if s.listed != nil {
		issues = s.listed
	}
	var numbers []int
	for n, i := range issues {
		if state == "open" && !i.Open || state == "closed" && i.Open {
			continue
		}
		if !containsAll(i.Labels, want) {
			continue
		}
		numbers = append(numbers, n)
	}
	// GitHub lists newest first.
	slices.SortFunc(numbers, func(a, b int) int { return cmp.Compare(b, a) })
	out := []any{}
	for _, n := range numbers {
		out = append(out, issueJSON(issues[n]))
	}
	return http.StatusOK, out
}

func containsAll(have, want []string) bool {
	for _, w := range want {
		if !slices.Contains(have, w) {
			return false
		}
	}
	return true
}

func (s *Server) getIssue(r *http.Request) (int, any) {
	i := s.pathIssue(r)
	if i == nil {
		return notFound()
	}
	return http.StatusOK, issueJSON(i)
}

func (s *Server) updateIssue(r *http.Request) (int, any) {
	i := s.pathIssue(r)
	if i == nil {
		return notFound()
	}
	var req struct {
		State       string `json:"state"`
		StateReason string `json:"state_reason"`
	}
	if !s.decode(r, &req) {
		return http.StatusBadRequest, message("bad body")
	}
	if req.State != "" {
		i.Open = req.State == "open"
	}
	if req.StateReason != "" {
		i.StateReason = req.StateReason
	}
	return http.StatusOK, issueJSON(i)
}

// Comments get one minute apart, starting at a fixed time, so their order is stable.
var commentEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func commentJSON(k int, c Comment) any {
	return map[string]any{
		"id":                 k + 1,
		"body":               c.Body,
		"created_at":         commentEpoch.Add(time.Duration(k) * time.Minute).Format(time.RFC3339),
		"author_association": c.Association,
	}
}

func (s *Server) listComments(r *http.Request) (int, any) {
	i := s.pathIssue(r)
	if i == nil {
		return notFound()
	}
	out := []any{}
	for k, c := range i.Comments {
		out = append(out, commentJSON(k, c))
	}
	return http.StatusOK, out
}

func (s *Server) createComment(r *http.Request) (int, any) {
	i := s.pathIssue(r)
	if i == nil {
		return notFound()
	}
	var req struct {
		Body string `json:"body"`
	}
	if !s.decode(r, &req) {
		return http.StatusBadRequest, message("bad body")
	}
	i.Comments = append(i.Comments, Comment{Body: req.Body, Association: associationOwner})
	return http.StatusCreated, commentJSON(len(i.Comments)-1, i.Comments[len(i.Comments)-1])
}

func (s *Server) listLabels(r *http.Request) (int, any) {
	return http.StatusOK, labelsJSON(s.labels)
}

func (s *Server) createLabel(r *http.Request) (int, any) {
	var req struct {
		Name string `json:"name"`
	}
	if !s.decode(r, &req) {
		return http.StatusBadRequest, message("bad body")
	}
	s.labels = append(s.labels, req.Name)
	return http.StatusCreated, map[string]string{"name": req.Name}
}

func (s *Server) addLabels(r *http.Request) (int, any) {
	i := s.pathIssue(r)
	if i == nil {
		return notFound()
	}
	var names []string
	if !s.decode(r, &names) {
		return http.StatusBadRequest, message("bad body")
	}
	for _, name := range names {
		if !slices.Contains(i.Labels, name) {
			i.Labels = append(i.Labels, name)
		}
	}
	return http.StatusOK, labelsJSON(i.Labels)
}

func (s *Server) setLabels(r *http.Request) (int, any) {
	i := s.pathIssue(r)
	if i == nil {
		return notFound()
	}
	var names []string
	if !s.decode(r, &names) {
		return http.StatusBadRequest, message("bad body")
	}
	i.Labels = slices.Compact(names)
	return http.StatusOK, labelsJSON(i.Labels)
}

func (s *Server) listSubIssues(r *http.Request) (int, any) {
	i := s.pathIssue(r)
	if i == nil {
		return notFound()
	}
	return http.StatusOK, s.issuesJSON(i.SubIssues)
}

func (s *Server) listBlockedBy(r *http.Request) (int, any) {
	i := s.pathIssue(r)
	if i == nil {
		return notFound()
	}
	return http.StatusOK, s.issuesJSON(i.BlockedBy)
}

func (s *Server) listPulls(r *http.Request) (int, any) {
	q := r.URL.Query()
	state := cmp.Or(q.Get("state"), "open")
	head, headFilter := strings.CutPrefix(q.Get("head"), s.Owner+":")
	var numbers []int
	for n, p := range s.pulls {
		if state == "open" && !p.Open || state == "closed" && p.Open {
			continue
		}
		if headFilter && p.Head != head {
			continue
		}
		numbers = append(numbers, n)
	}
	// GitHub lists newest first.
	slices.SortFunc(numbers, func(a, b int) int { return cmp.Compare(b, a) })
	out := []any{}
	for _, n := range numbers {
		out = append(out, s.pullJSON(s.pulls[n]))
	}
	return http.StatusOK, out
}

func (s *Server) createPull(r *http.Request) (int, any) {
	var req struct {
		Title string `json:"title"`
		Head  string `json:"head"`
		Base  string `json:"base"`
		Body  string `json:"body"`
	}
	if !s.decode(r, &req) {
		return http.StatusBadRequest, message("bad body")
	}
	return http.StatusCreated, s.pullJSON(s.newPull(req.Head, req.Base, req.Title, req.Body))
}

func (s *Server) newPull(head, base, title, body string) *Pull {
	s.number++
	p := &Pull{Number: s.number, Head: head, Base: base, Title: title, Body: body, Open: true}
	s.pulls[p.Number] = p
	return p
}

func (s *Server) getPull(r *http.Request) (int, any) {
	p := s.pathPull(r)
	if p == nil {
		return notFound()
	}
	return http.StatusOK, s.pullJSON(p)
}

func reviewJSON(k int, rv Review) any {
	return map[string]any{"id": k + 1, "body": rv.Body, "state": "COMMENTED", "author_association": rv.Association}
}

func (s *Server) listReviews(r *http.Request) (int, any) {
	p := s.pathPull(r)
	if p == nil {
		return notFound()
	}
	out := []any{}
	for k, rv := range p.Reviews {
		out = append(out, reviewJSON(k, rv))
	}
	return http.StatusOK, out
}

func (s *Server) createReview(r *http.Request) (int, any) {
	p := s.pathPull(r)
	if p == nil {
		return notFound()
	}
	var req struct {
		Body  string `json:"body"`
		Event string `json:"event"`
	}
	if !s.decode(r, &req) {
		return http.StatusBadRequest, message("bad body")
	}
	p.Reviews = append(p.Reviews, Review{Body: req.Body, Event: req.Event, Association: associationOwner})
	return http.StatusOK, reviewJSON(len(p.Reviews)-1, p.Reviews[len(p.Reviews)-1])
}

func (s *Server) merge(r *http.Request) (int, any) {
	p := s.pathPull(r)
	if p == nil {
		return notFound()
	}
	var req struct {
		MergeMethod string `json:"merge_method"`
		SHA         string `json:"sha"`
	}
	if !s.decode(r, &req) {
		return http.StatusBadRequest, message("bad body")
	}
	if !p.Open {
		return http.StatusMethodNotAllowed, message("Pull Request is not mergeable")
	}
	if rf, ok := s.refusals[p.Number]; ok {
		delete(s.refusals, p.Number)
		p.Mergeable = rf.mergeable
		return http.StatusMethodNotAllowed, message(rf.message)
	}
	if req.SHA != "" && req.SHA != s.head(p) {
		return http.StatusConflict, message("Head branch was modified. Review and try the merge again.")
	}
	p.Open = false
	p.Merged = true
	p.MergeMethod = req.MergeMethod
	p.MergedSHA = req.SHA
	if n, ok := s.closes[p.Number]; ok {
		s.issues[n].Open = false
		s.issues[n].StateReason = "completed"
	}
	return http.StatusOK, map[string]any{"merged": true, "sha": "merge-commit", "message": "Pull Request successfully merged"}
}

func (s *Server) listRuns(r *http.Request) (int, any) {
	runs := s.runs[r.URL.Query().Get("head_sha")]
	out := []any{}
	for k, run := range runs {
		m := map[string]any{"id": k + 1, "name": run.Name, "status": run.Status, "html_url": run.URL}
		if run.Conclusion != "" {
			m["conclusion"] = run.Conclusion
		}
		out = append(out, m)
	}
	return http.StatusOK, map[string]any{"total_count": len(out), "workflow_runs": out}
}

func (s *Server) getCommit(r *http.Request) (int, any) {
	sha := r.PathValue("sha")
	at, ok := s.times[sha]
	if !ok {
		at = time.Now().Add(-time.Hour)
	}
	return http.StatusOK, map[string]any{"sha": sha, "committer": map[string]string{"date": at.Format(time.RFC3339)}}
}

// Fail makes every request to route answer status, until Fail(route, 0). route is a pattern as New
// registers it, such as "PUT /repos/{owner}/{repo}/pulls/{n}/merge".
func (s *Server) Fail(route string, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.routes[route] {
		s.t.Fatalf("githubtest: no route %q", route)
	}
	if status == 0 {
		delete(s.fails, route)
		return
	}
	s.fails[route] = status
}

// RefuseMerge makes GitHub refuse the next merge of PR n with 405 and message, the way it refuses
// a PR that conflicts or that a rule of the base branch blocks. From then on, the PR's mergeable
// is mergeable, as GitHub has worked it out again.
func (s *Server) RefuseMerge(n int, message string, mergeable *bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refusals[n] = refusal{message: message, mergeable: mergeable}
}

// CloseOnMerge makes the merge of PR pr close issue n as completed, the way GitHub closes the issue
// a PR body names with "Closes".
func (s *Server) CloseOnMerge(pr, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closes[pr] = n
}

func (s *Server) AddIssue(title, body string, labels ...string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.newIssue(title, body, labels).Number
}

func (s *Server) AddOutsiderIssue(title, body string, labels ...string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.newIssue(title, body, labels)
	i.Association = associationNone
	return i.Number
}

func (s *Server) AddComment(n int, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.issues[n].Comments = append(s.issues[n].Comments, Comment{Body: body, Association: associationOwner})
}

func (s *Server) AddOutsiderComment(n int, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.issues[n].Comments = append(s.issues[n].Comments, Comment{Body: body, Association: associationNone})
}

func (s *Server) AddSubIssue(parent, child int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.issues[parent].SubIssues = append(s.issues[parent].SubIssues, child)
}

func (s *Server) AddBlockedBy(n, blocker int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.issues[n].BlockedBy = append(s.issues[n].BlockedBy, blocker)
}

// Relabel takes label from off issue n and puts to on, as a person does by hand. An empty from or
// to only adds or only removes.
func (s *Server) Relabel(n int, from, to string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.issues[n]
	i.Labels = slices.DeleteFunc(i.Labels, func(l string) bool { return l == from })
	if to != "" && !slices.Contains(i.Labels, to) {
		i.Labels = append(i.Labels, to)
	}
}

// HoldList makes the issue list keep answering with the issues as they are now. GitHub's list
// can lag behind an issue's own page for up to a minute.
func (s *Server) HoldList() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listed = map[int]*Issue{}
	for n, i := range s.issues {
		c := *i
		c.Labels = slices.Clone(i.Labels)
		s.listed[n] = &c
	}
}

func (s *Server) CloseIssue(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.issues[n].Open = false
	s.issues[n].StateReason = "completed"
}

func (s *Server) Issue(n int) Issue {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := *s.issues[n]
	i.Labels = slices.Clone(i.Labels)
	i.Comments = slices.Clone(i.Comments)
	i.SubIssues = slices.Clone(i.SubIssues)
	i.BlockedBy = slices.Clone(i.BlockedBy)
	return i
}

func (s *Server) AddPull(head, base string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.newPull(head, base, "", "").Number
}

func (s *Server) ClosePull(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pulls[n].Open = false
}

func (s *Server) SetMergeable(n int, mergeable *bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pulls[n].Mergeable = mergeable
}

func (s *Server) AddReview(n int, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pulls[n].Reviews = append(s.pulls[n].Reviews, Review{Body: body, Event: "COMMENT", Association: associationOwner})
}

func (s *Server) AddOutsiderReview(n int, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pulls[n].Reviews = append(s.pulls[n].Reviews, Review{Body: body, Event: "COMMENT", Association: associationNone})
}

func (s *Server) Pull(n int) Pull {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := *s.pulls[n]
	p.Reviews = slices.Clone(p.Reviews)
	return p
}

func (s *Server) Pulls() []Pull {
	s.mu.Lock()
	numbers := slices.Sorted(maps.Keys(s.pulls))
	s.mu.Unlock()
	var out []Pull
	for _, n := range numbers {
		out = append(out, s.Pull(n))
	}
	return out
}

func (s *Server) SetRuns(sha string, runs ...Run) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs[sha] = runs
}

// SetCommitTime sets when a commit was made. Commits without a set time are an hour old.
func (s *Server) SetCommitTime(sha string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.times[sha] = at
}
