package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// The web capability ladder (docs/abi/web-application.md): fetch < document
// < session < browser. A grant names the lowest layer that satisfies the
// request; execution may never exceed the granted layer, and every response
// carries the external-content taint so the export gate sees it.
type WebCapability struct {
	Kind             string   `json:"kind"` // web.fetch | web.document | web.session | web.browser
	URLPrefixes      []string `json:"url_prefixes"`
	Methods          []string `json:"methods,omitempty"`
	MaxResponseBytes int64    `json:"max_response_bytes,omitempty"`
	ExpiresAt        int64    `json:"expires_at,omitempty"`
}

var webKinds = map[string]int{"web.fetch": 1, "web.document": 2, "web.session": 3, "web.browser": 4}

var (
	ErrWebDenied        = errors.New("web capability denied")
	ErrWebLayerMismatch = errors.New("requested layer exceeds the granted capability")
)

// WebRequest is the capability-checked input; WebResult the evidence trail.
type WebRequest struct {
	Kind     string `json:"kind"`
	Method   string `json:"method,omitempty"`
	URL      string `json:"url"`
	Body     string `json:"body,omitempty"`
	Selector string `json:"selector,omitempty"` // document layer
}

type WebResult struct {
	Kind          string `json:"kind"`
	URL           string `json:"url"`
	Status        int    `json:"status"`
	ContentSHA256 string `json:"content_sha256"`
	Bytes         int64  `json:"bytes"`
	ContentType   string `json:"content_type,omitempty"`
	Taint         uint64 `json:"taint"` // always includes TaintExternal
	FetchedAt     int64  `json:"fetched_at"`
	Artifact      string `json:"artifact,omitempty"`
}

func kindRank(kind string) int { return webKinds[kind] }

// webFetch executes the fetch layer. The transport is supervisor-owned;
// callers never touch sockets, cookies or credentials directly.
func (s *Supervisor) webFetch(client *http.Client, req WebRequest) (WebResult, []byte, error) {
	method := strings.ToUpper(req.Method)
	if method == "" {
		method = http.MethodGet
	}
	httpReq, err := http.NewRequest(method, req.URL, strings.NewReader(req.Body))
	if err != nil {
		return WebResult{}, nil, err
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return WebResult{}, nil, err
	}
	defer resp.Body.Close()
	limit := int64(1 << 20)
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return WebResult{}, nil, err
	}
	if int64(len(body)) > limit {
		return WebResult{}, nil, errors.New("web response exceeds the fetch limit")
	}
	sum := sha256.Sum256(body)
	result := WebResult{Kind: "web.fetch", URL: req.URL, Status: resp.StatusCode,
		ContentSHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(body)),
		ContentType: resp.Header.Get("Content-Type"), Taint: TaintExternal,
		FetchedAt: time.Now().UnixNano()}
	return result, body, nil
}

// webDocument runs the document layer: fetch plus a structural extraction
// (links and text) over the fetched body. No JS, no cookies, no sessions.
func webDocument(body []byte, contentType string) map[string]any {
	links := []string{}
	text := string(body)
	if strings.Contains(contentType, "html") {
		links = extractLinks(body)
		text = stripTags(body)
	}
	return map[string]any{"links": links, "text": text}
}

func extractLinks(body []byte) []string {
	seen := map[string]bool{}
	out := []string{}
	rest := string(body)
	for {
		i := strings.Index(rest, "href=\"")
		if i < 0 {
			break
		}
		rest = rest[i+len("href=\""):]
		j := strings.Index(rest, "\"")
		if j < 0 {
			break
		}
		href := rest[:j]
		rest = rest[j:]
		if href != "" && !seen[href] && (strings.HasPrefix(href, "http://") || strings.HasPrefix(href, "https://")) {
			seen[href] = true
			out = append(out, href)
		}
	}
	sort.Strings(out)
	return out
}

func stripTags(body []byte) string {
	text := string(body)
	var b strings.Builder
	inTag := false
	for _, r := range text {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
		case !inTag:
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// WebExecute checks the capability, executes at the granted layer, folds
// the external taint into the application ledger and audits the request.
func (s *Supervisor) WebExecute(principal, applicationID string, cap WebCapability, req WebRequest) (WebResult, map[string]any, error) {
	if rank := kindRank(req.Kind); rank == 0 {
		return WebResult{}, nil, errors.New("unknown web layer")
	}
	if kindRank(req.Kind) > kindRank(cap.Kind) {
		return WebResult{}, nil, ErrWebLayerMismatch
	}
	parsed, err := url.Parse(req.URL)
	if err != nil || parsed.Scheme != "https" && parsed.Scheme != "http" {
		return WebResult{}, nil, errors.New("invalid web url")
	}
	if !prefixAllowed(cap.URLPrefixes, req.URL) {
		return WebResult{}, nil, ErrWebDenied
	}
	method := strings.ToUpper(req.Method)
	if method == "" {
		method = http.MethodGet
	}
	if len(cap.Methods) > 0 && !containsString(cap.Methods, method) {
		return WebResult{}, nil, ErrWebDenied
	}
	if cap.ExpiresAt != 0 && time.Now().Unix() > cap.ExpiresAt {
		return WebResult{}, nil, ErrWebDenied
	}
	result, body, err := s.webFetch(&http.Client{Timeout: 20 * time.Second}, req)
	if err != nil {
		return WebResult{}, nil, err
	}
	var extraction map[string]any
	if req.Kind != "web.fetch" {
		extraction = webDocument(body, result.ContentType)
	}
	s.mu.Lock()
	s.accumulateTaint(applicationID, TaintExternal)
	_ = s.auditLocked(principal, applicationID, "web."+req.Kind,
		fmt.Sprintf("%s -> %d", req.URL, result.Status), "allow", "")
	s.mu.Unlock()
	if err := s.persistStateForWeb(); err != nil {
		return WebResult{}, nil, err
	}
	if req.Kind != "web.fetch" {
		result.Kind = req.Kind
	}
	return result, extraction, nil
}

func (s *Supervisor) persistStateForWeb() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.persistLocked(nil)
}

func prefixAllowed(prefixes []string, target string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(target, prefix) {
			return true
		}
	}
	return false
}
