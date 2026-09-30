package github

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"sync"
)

// etagTransport asks again with If-None-Match for every GET it has an answer to. GitHub does not
// count a 304 against the rate limit, and most of coworker's checks get the same answer every
// minute. It never reuses an answer without asking: a standard HTTP cache would honor GitHub's
// max-age=60 and could hide a label coworker changed a moment ago.
type etagTransport struct {
	base http.RoundTripper

	mu   sync.Mutex
	seen map[string]answer
}

type answer struct {
	etag   string
	header http.Header
	body   []byte
}

func newETagTransport(base http.RoundTripper) *etagTransport {
	return &etagTransport{base: base, seen: map[string]answer{}}
}

func (t *etagTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet {
		return t.base.RoundTrip(req)
	}
	key := req.URL.String()
	t.mu.Lock()
	prev, ok := t.seen[key]
	t.mu.Unlock()
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
		header := prev.header.Clone()
		// The client reads the remaining rate limit from these, so they must be current.
		for k, v := range res.Header {
			if strings.HasPrefix(k, "X-Ratelimit-") {
				header[k] = v
			}
		}
		return &http.Response{
			Status:        "200 OK",
			StatusCode:    http.StatusOK,
			Proto:         res.Proto,
			ProtoMajor:    res.ProtoMajor,
			ProtoMinor:    res.ProtoMinor,
			Header:        header,
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
	t.mu.Lock()
	t.seen[key] = answer{etag: etag, header: res.Header.Clone(), body: body}
	t.mu.Unlock()
	res.Body = io.NopCloser(bytes.NewReader(body))
	return res, nil
}
