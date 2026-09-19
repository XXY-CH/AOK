package runtime

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebLadderAndCapabilityChecks(t *testing.T) {
	s := newKernelTestSupervisor(t)
	app, err := s.CreateApplication("tester", "owner", "on_event")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><a href="https://example.com/a">a</a><a href="https://example.com/b">b</a><p>hello world</p></html>`))
	}))
	defer server.Close()

	fetch := WebCapability{Kind: "web.fetch", URLPrefixes: []string{server.URL + "/"}, Methods: []string{"GET"}}
	// The granted layer executes and taints the caller.
	result, extraction, err := s.WebExecute("tester", app.ApplicationID, fetch, WebRequest{Kind: "web.fetch", URL: server.URL + "/page"})
	if err != nil || result.Status != 200 || result.Bytes == 0 || result.ContentSHA256 == "" {
		t.Fatalf("fetch failed: %+v %v", result, err)
	}
	if result.Taint&TaintExternal == 0 {
		t.Fatal("web result not externally tainted")
	}
	if extraction != nil {
		t.Fatal("fetch layer must not run document extraction")
	}
	inspected, _ := s.InspectApplication(app.ApplicationID)
	if inspected.TaintBits&TaintExternal == 0 {
		t.Fatal("web execution did not taint the application ledger")
	}

	// Requesting above the granted layer is refused regardless of URL.
	if _, _, err := s.WebExecute("tester", app.ApplicationID, fetch, WebRequest{Kind: "web.document", URL: server.URL + "/page"}); !errors.Is(err, ErrWebLayerMismatch) {
		t.Fatalf("layer escalation accepted: %v", err)
	}
	// Outside the URL prefix and outside the method list are denied.
	if _, _, err := s.WebExecute("tester", app.ApplicationID, fetch, WebRequest{Kind: "web.fetch", URL: "https://elsewhere.example/"}); !errors.Is(err, ErrWebDenied) {
		t.Fatalf("out-of-scope url accepted: %v", err)
	}
	if _, _, err := s.WebExecute("tester", app.ApplicationID, fetch, WebRequest{Kind: "web.fetch", Method: "POST", URL: server.URL + "/page"}); !errors.Is(err, ErrWebDenied) {
		t.Fatalf("out-of-scope method accepted: %v", err)
	}
	// Expired capability is denied.
	expired := fetch
	expired.ExpiresAt = 1
	if _, _, err := s.WebExecute("tester", app.ApplicationID, expired, WebRequest{Kind: "web.fetch", URL: server.URL + "/page"}); !errors.Is(err, ErrWebDenied) {
		t.Fatalf("expired capability accepted: %v", err)
	}

	// The document layer extracts structure without JS or sessions.
	doc := WebCapability{Kind: "web.document", URLPrefixes: []string{server.URL + "/"}}
	result, extraction, err = s.WebExecute("tester", app.ApplicationID, doc, WebRequest{Kind: "web.document", URL: server.URL + "/page"})
	if err != nil || result.Kind != "web.document" {
		t.Fatalf("document failed: %+v %v", result, err)
	}
	links, _ := extraction["links"].([]string)
	if len(links) != 2 || !strings.Contains(links[0], "example.com") {
		t.Fatalf("links wrong: %+v", links)
	}
	if text, _ := extraction["text"].(string); !strings.Contains(text, "hello world") {
		t.Fatalf("text extraction wrong: %q", text)
	}
	// Every request is audited.
	found := 0
	for _, r := range s.Audit() {
		if strings.HasPrefix(r.Action, "web.") && r.Decision == "allow" {
			found++
		}
	}
	if found != 2 {
		t.Fatalf("expected 2 audited web calls, got %d", found)
	}
}

// Redirects and host-boundary tricks must not escape the granted prefixes;
// MaxResponseBytes must actually bind.
func TestWebRedirectAndBoundaryEnforcement(t *testing.T) {
	s := newKernelTestSupervisor(t)
	app, err := s.CreateApplication("tester", "owner", "on_event")
	if err != nil {
		t.Fatal(err)
	}
	var secret string
	outside := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secret = "LEAKED"
		w.Write([]byte("SECRET-EVIL-CONTENT"))
	}))
	defer outside.Close()
	inside := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, outside.URL+"/x", http.StatusFound)
			return
		}
		w.Write([]byte("ok"))
	}))
	defer inside.Close()
	cap := WebCapability{Kind: "web.fetch", URLPrefixes: []string{inside.URL + "/"}}
	// A redirect out of the prefix fails closed.
	if _, _, err := s.WebExecute("tester", app.ApplicationID, cap, WebRequest{Kind: "web.fetch", URL: inside.URL + "/redirect"}); err == nil {
		t.Fatal("redirect out of the granted prefix was followed")
	}
	if secret == "LEAKED" {
		t.Fatal("out-of-scope content was fetched via redirect")
	}
	// Host-boundary: a sibling host sharing the string prefix is denied.
	_, host, _ := net.SplitHostPort(strings.TrimPrefix(inside.URL, "http://"))
	evil := "http://" + host + ".evil.example/x"
	if _, _, err := s.WebExecute("tester", app.ApplicationID, cap, WebRequest{Kind: "web.fetch", URL: evil}); err == nil {
		t.Fatal("host-boundary prefix bypass accepted")
	}
	// MaxResponseBytes is enforced in both directions.
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(make([]byte, 4096))
	}))
	defer big.Close()
	small := WebCapability{Kind: "web.fetch", URLPrefixes: []string{big.URL + "/"}, MaxResponseBytes: 100}
	if _, _, err := s.WebExecute("tester", app.ApplicationID, small, WebRequest{Kind: "web.fetch", URL: big.URL + "/b"}); err == nil {
		t.Fatal("response above MaxResponseBytes accepted")
	}
	generous := WebCapability{Kind: "web.fetch", URLPrefixes: []string{big.URL + "/"}, MaxResponseBytes: 1 << 20}
	if _, _, err := s.WebExecute("tester", app.ApplicationID, generous, WebRequest{Kind: "web.fetch", URL: big.URL + "/b"}); err != nil {
		t.Fatalf("response under MaxResponseBytes rejected: %v", err)
	}
}
