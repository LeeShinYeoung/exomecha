package github

import (
	"bytes"
	"container/list"
	"io"
	"net/http"
	"strings"
	"sync"
)

// etagLimit bounds the answers etagTransport keeps, since CheckCI asks a new URL for every commit.
// The URLs asked every minute are about ten for each PRD a worker holds, so 1000 keeps all of them
// for a hundred PRDs at once. What goes first is mostly the URLs of commits that are no longer a
// PR's head.
const etagLimit = 1000

// etagTransport asks again with If-None-Match for every GET it has an answer to. GitHub does not
// count a 304 against the rate limit, and most of exomecha's checks get the same answer every
// minute. It never reuses an answer without asking: a standard HTTP cache would honor GitHub's
// max-age=60 and could hide a label exomecha changed a moment ago.
type etagTransport struct {
	base  http.RoundTripper
	limit int

	mu sync.Mutex
	// The answer used last is at the front of order, and the one at the back goes first.
	order *list.List
	seen  map[string]*list.Element
}

type answer struct {
	key    string
	etag   string
	header http.Header
	body   []byte
}

func newETagTransport(base http.RoundTripper) *etagTransport {
	return &etagTransport{base: base, limit: etagLimit, order: list.New(), seen: map[string]*list.Element{}}
}

func (t *etagTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet {
		return t.base.RoundTrip(req)
	}
	key := req.URL.String()
	prev, ok := t.load(key)
	if ok {
		req = req.Clone(req.Context())
		req.Header.Set("If-None-Match", prev.etag)
	}

	res, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if ok && res.StatusCode == http.StatusNotModified {
		res.Body.Close()
		h := prev.header.Clone()
		// The client reads the remaining rate limit from these, so they must be current.
		for k, v := range res.Header {
			if strings.HasPrefix(k, "X-Ratelimit-") {
				h[k] = v
			}
		}
		return &http.Response{
			Status:        "200 OK",
			StatusCode:    http.StatusOK,
			Proto:         res.Proto,
			ProtoMajor:    res.ProtoMajor,
			ProtoMinor:    res.ProtoMinor,
			Header:        h,
			Body:          io.NopCloser(bytes.NewReader(prev.body)),
			ContentLength: int64(len(prev.body)),
			Request:       req,
		}, nil
	}

	etag := res.Header.Get("ETag")
	if res.StatusCode != http.StatusOK || etag == "" {
		return res, nil
	}
	body, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil {
		return nil, err
	}
	t.store(answer{key: key, etag: etag, header: res.Header.Clone(), body: body})
	res.Body = io.NopCloser(bytes.NewReader(body))
	return res, nil
}

func (t *etagTransport) load(key string) (answer, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.seen[key]
	if !ok {
		return answer{}, false
	}
	t.order.MoveToFront(e)
	return e.Value.(answer), true
}

func (t *etagTransport) store(a answer) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if e, ok := t.seen[a.key]; ok {
		e.Value = a
		t.order.MoveToFront(e)
		return
	}
	t.seen[a.key] = t.order.PushFront(a)
	if t.order.Len() > t.limit {
		oldest := t.order.Remove(t.order.Back()).(answer)
		delete(t.seen, oldest.key)
	}
}
