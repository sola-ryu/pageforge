package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// fakeForgejo serves the minimal API surface pageforge needs:
// branch lookup and the /media/ endpoint, which serves raw file bytes
// and resolves Git LFS pointers server-side (like the real thing).
//
// files maps repo paths to the bytes stored in git (which may be an LFS
// pointer document); lfs maps paths to the resolved LFS object bytes.
func fakeForgejo(t *testing.T, files, lfs map[string]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/acme/blog/branches/main", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"name":   "main",
			"commit": map[string]string{"id": "abc123"},
		})
	})
	mux.HandleFunc("/api/v1/repos/acme/blog/git/commits/abc123", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"sha": "abc123",
			"commit": map[string]any{
				"committer": map[string]any{"date": "2026-10-14T12:00:00Z"},
			},
		})
	})
	mux.HandleFunc("/api/v1/repos/acme/blog/media/", func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.EscapedPath(), "/api/v1/repos/acme/blog/media/")
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
			return
		}
		// Like Forgejo's GetRawFileOrLFS: resolve LFS pointers to the object.
		if strings.HasPrefix(body, "version https://git-lfs.github.com/spec/v1\n") {
			resolved, ok := lfs[rel]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			body = resolved
		}
		w.Write([]byte(body))
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
		WebPQuality:        80,
		CacheMaxAge:        600,
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
	srv := fakeForgejo(t, map[string]string{"index.html": "<h1>root</h1>"}, nil)
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
	}, nil)
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
	srv := fakeForgejo(t, map[string]string{"index.html": "<h1>root</h1>"}, nil)
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
	srv := fakeForgejo(t, map[string]string{"index.html": "<h1>root</h1>"}, nil)
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
	srv := fakeForgejo(t, map[string]string{"index.html": "<h1>spa</h1>"}, nil)
	defer srv.Close()
	app := testApp(t, srv.URL)
	publish(t, app, "acme", "blog", "main", "", true)

	rec := get(t, app, "https://pages.example.com/acme/blog/some/client/route")
	if rec.Code != 200 || rec.Body.String() != "<h1>spa</h1>" {
		t.Fatalf("spa fallback: got %d %q", rec.Code, rec.Body.String())
	}
}

func TestServeLFSFile(t *testing.T) {
	pointer := "version https://git-lfs.github.com/spec/v1\noid sha256:4d7a5f8c3e2a1b9d6f4e8c2a5b7d9e1f3a5b7d9e1f3a5b7d9e1f3a5b7d9e1f3a5\nsize 8\n"
	srv := fakeForgejo(t,
		map[string]string{
			"index.html":       "<h1>root</h1>",
			"assets/logo.png":  pointer,
			"assets/plain.txt": "just text",
		},
		map[string]string{
			"assets/logo.png": "PNGDATA!",
		})
	defer srv.Close()
	app := testApp(t, srv.URL)
	publish(t, app, "acme", "blog", "main", "", false)

	// The LFS pointer must be resolved to the real object bytes,
	// not served as the pointer document.
	rec := get(t, app, "https://pages.example.com/acme/blog/assets/logo.png")
	if rec.Code != 200 {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); body != "PNGDATA!" {
		t.Fatalf("LFS file served pointer instead of object: %q", body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("unexpected content type %q", ct)
	}

	// Regular files still work.
	rec = get(t, app, "https://pages.example.com/acme/blog/assets/plain.txt")
	if rec.Code != 200 || rec.Body.String() != "just text" {
		t.Fatalf("plain file: got %d %q", rec.Code, rec.Body.String())
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

func TestCacheHeaders(t *testing.T) {
	srv := fakeForgejo(t, map[string]string{"index.html": "<h1>root</h1>"}, nil)
	defer srv.Close()
	app := testApp(t, srv.URL)
	publish(t, app, "acme", "blog", "main", "", false)

	rec := getWithHeaders(t, app, "https://pages.example.com/acme/blog/", nil)
	if rec.Code != 200 {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=600, must-revalidate" {
		t.Fatalf("Cache-Control = %q", cc)
	}
	if etag := rec.Header().Get("ETag"); etag != `"abc123"` {
		t.Fatalf("ETag = %q", etag)
	}
	// Last-Modified comes from the HEAD commit date served by the fake.
	if lm := rec.Header().Get("Last-Modified"); lm != "Wed, 14 Oct 2026 12:00:00 GMT" {
		t.Fatalf("Last-Modified = %q", lm)
	}
}

func TestCacheHeadersWebPVariant(t *testing.T) {
	original := noisePNG(t, 128, 128)
	srv := fakeForgejo(t, map[string]string{"photo.png": string(original)}, nil)
	defer srv.Close()
	app := testApp(t, srv.URL)
	publish(t, app, "acme", "blog", "main", "", false)

	rec := getWithAccept(t, app, "https://pages.example.com/acme/blog/photo.png", "image/webp")
	if rec.Code != 200 {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	if etag := rec.Header().Get("ETag"); etag != `"abc123-webp"` {
		t.Fatalf("ETag = %q, want webp variant tag", etag)
	}
}

func TestNotModified(t *testing.T) {
	srv := fakeForgejo(t, map[string]string{"index.html": "<h1>root</h1>"}, nil)
	defer srv.Close()
	app := testApp(t, srv.URL)
	publish(t, app, "acme", "blog", "main", "", false)

	rec := getWithHeaders(t, app, "https://pages.example.com/acme/blog/", nil)
	etag, lastMod := rec.Header().Get("ETag"), rec.Header().Get("Last-Modified")
	if etag == "" || lastMod == "" {
		t.Fatal("missing validators on first response")
	}

	rec2 := getWithHeaders(t, app, "https://pages.example.com/acme/blog/",
		map[string]string{"If-None-Match": etag})
	if rec2.Code != http.StatusNotModified {
		t.Fatalf("If-None-Match: got %d, want 304", rec2.Code)
	}

	rec3 := getWithHeaders(t, app, "https://pages.example.com/acme/blog/",
		map[string]string{"If-Modified-Since": lastMod})
	if rec3.Code != http.StatusNotModified {
		t.Fatalf("If-Modified-Since: got %d, want 304", rec3.Code)
	}
}
