package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// fakeForgejo serves the minimal API surface pageforge needs:
// branch lookup and the contents API.
func fakeForgejo(t *testing.T, files map[string]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/acme/blog/branches/main", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"name":   "main",
			"commit": map[string]string{"id": "abc123"},
		})
	})
	mux.HandleFunc("/api/v1/repos/acme/blog/contents/", func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.EscapedPath(), "/api/v1/repos/acme/blog/contents/")
		var segs []string
		for _, s := range strings.Split(p, "/") {
			u, err := url.PathUnescape(s)
			if err != nil {
				t.Fatalf("bad escape in test: %v", err)
			}
			segs = append(segs, u)
		}
		rel := strings.Join(segs, "/")
		body, ok := files[rel]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"message": "not found"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"type":     "file",
			"encoding": "base64",
			"content":  base64.StdEncoding.EncodeToString([]byte(body)),
			"sha":      "sha-of-" + rel,
		})
	})
	return httptest.NewServer(mux)
}

func testApp(t *testing.T, forgejoURL string) *App {
	t.Helper()
	cfg := &Config{
		ForgejoURL:         forgejoURL,
		ForgejoInternalURL: forgejoURL,
		PublicURL:          "https://pages.example.com",
		SessionSecret:      "test-secret-that-is-long-enough",
	}
	store, err := openStore(t.TempDir()+"/test.db", cfg.tokenKey())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.close() })
	return &App{cfg: cfg, store: store, pageCache: newPageCache()}
}

func publish(t *testing.T, app *App, owner, repo, branch, subdir string, spa bool) *Site {
	t.Helper()
	user, err := app.store.upsertUser(42, owner, "access", "refresh", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	site, err := app.store.createSite(user.ID, owner, repo, branch)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.store.updateSite(site.ID, branch, subdir, spa); err != nil {
		t.Fatal(err)
	}
	return site
}

func get(t *testing.T, app *App, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	// handlePage reads these via r.PathValue, which is only populated
	// when routing through a ServeMux.
	req.SetPathValue("owner", "acme")
	req.SetPathValue("repo", "blog")
	rec := httptest.NewRecorder()
	app.handlePage(rec, req)
	return rec
}

func TestServeRootIndex(t *testing.T) {
	srv := fakeForgejo(t, map[string]string{"index.html": "<h1>root</h1>"})
	defer srv.Close()
	app := testApp(t, srv.URL)
	publish(t, app, "acme", "blog", "main", "", false)

	rec := get(t, app, "https://pages.example.com/acme/blog/")
	if rec.Code != 200 {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); body != "<h1>root</h1>" {
		t.Fatalf("unexpected body %q", body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Fatalf("unexpected content type %q", ct)
	}
}

func TestServeSubdir(t *testing.T) {
	srv := fakeForgejo(t, map[string]string{
		"index.html":      "<h1>root</h1>",
		"docs/index.html": "<h1>docs</h1>",
		"docs/a.css":      "x{}",
	})
	defer srv.Close()
	app := testApp(t, srv.URL)
	publish(t, app, "acme", "blog", "main", "docs", false)

	rec := get(t, app, "https://pages.example.com/acme/blog/")
	if rec.Code != 200 || rec.Body.String() != "<h1>docs</h1>" {
		t.Fatalf("subdir index: got %d %q", rec.Code, rec.Body.String())
	}
	rec = get(t, app, "https://pages.example.com/acme/blog/a.css")
	if rec.Code != 200 || rec.Body.String() != "x{}" {
		t.Fatalf("subdir file: got %d %q", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/css; charset=utf-8" {
		t.Fatalf("unexpected content type %q", ct)
	}
}

func TestNotFound(t *testing.T) {
	srv := fakeForgejo(t, map[string]string{"index.html": "<h1>root</h1>"})
	defer srv.Close()
	app := testApp(t, srv.URL)
	publish(t, app, "acme", "blog", "main", "", false)

	if rec := get(t, app, "https://pages.example.com/acme/blog/nope"); rec.Code != 404 {
		t.Fatalf("got %d, want 404", rec.Code)
	}
	if rec := get(t, app, "https://pages.example.com/acme/other/"); rec.Code != 404 {
		t.Fatalf("unpublished repo: got %d, want 404", rec.Code)
	}
}

func TestPathTraversalRejected(t *testing.T) {
	srv := fakeForgejo(t, map[string]string{"index.html": "<h1>root</h1>"})
	defer srv.Close()
	app := testApp(t, srv.URL)
	publish(t, app, "acme", "blog", "main", "docs", false)

	// %2e%2e decodes to ".." — must not escape the subdir.
	req := httptest.NewRequest(http.MethodGet, "https://pages.example.com/acme/blog/%2e%2e/index.html", nil)
	req.SetPathValue("owner", "acme")
	req.SetPathValue("repo", "blog")
	rec := httptest.NewRecorder()
	app.handlePage(rec, req)
	if rec.Code != 404 {
		t.Fatalf("got %d, want 404", rec.Code)
	}
}

func TestSPAFallback(t *testing.T) {
	srv := fakeForgejo(t, map[string]string{"index.html": "<h1>spa</h1>"})
	defer srv.Close()
	app := testApp(t, srv.URL)
	publish(t, app, "acme", "blog", "main", "", true)

	rec := get(t, app, "https://pages.example.com/acme/blog/some/client/route")
	if rec.Code != 200 || rec.Body.String() != "<h1>spa</h1>" {
		t.Fatalf("spa fallback: got %d %q", rec.Code, rec.Body.String())
	}
}

func TestCleanSubdir(t *testing.T) {
	cases := map[string]string{
		"":          "",
		"/":         "",
		"docs":      "docs",
		"/docs/":    "docs",
		"a/b":       "a/b",
		"../x":      "",
		"a/../../x": "",
		"a//b":      "a/b",
	}
	for in, want := range cases {
		if got := cleanSubdir(in); got != want {
			t.Errorf("cleanSubdir(%q) = %q, want %q", in, got, want)
		}
	}
}
