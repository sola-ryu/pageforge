package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Serving static files from Forgejo repositories.
//
// A published site maps:
//
//	/{owner}/{repo}/<path...>  ->  {branch}:{subdir}/<path...>
//
// Files are read through the Forgejo contents API using the publishing
// user's OAuth token, so private repositories can be published too —
// with the caveat that the published site is then public to anyone with
// the URL (documented in the README).
//
// Freshness: file blobs are cached keyed by the branch HEAD commit SHA,
// which is re-checked every headTTL. A push to the branch changes the
// HEAD, invalidating the cache without any webhook.
// ---------------------------------------------------------------------------

const headTTL = 15 * time.Second
const maxCachedFiles = 2048

type headEntry struct {
	sha string
	at  time.Time
}

type fileEntry struct {
	data        []byte
	contentType string
	at          time.Time
}

type pageCache struct {
	mu    sync.Mutex
	heads map[int64]headEntry
	files map[string]fileEntry
}

func newPageCache() *pageCache {
	return &pageCache{
		heads: make(map[int64]headEntry),
		files: make(map[string]fileEntry),
	}
}

var contentTypes = map[string]string{
	".html":  "text/html; charset=utf-8",
	".htm":   "text/html; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".mjs":   "text/javascript; charset=utf-8",
	".json":  "application/json",
	".map":   "application/json",
	".svg":   "image/svg+xml",
	".png":   "image/png",
	".jpg":   "image/jpeg",
	".jpeg":  "image/jpeg",
	".gif":   "image/gif",
	".webp":  "image/webp",
	".avif":  "image/avif",
	".ico":   "image/x-icon",
	".txt":   "text/plain; charset=utf-8",
	".md":    "text/markdown; charset=utf-8",
	".xml":   "application/xml",
	".pdf":   "application/pdf",
	".woff":  "font/woff",
	".woff2": "font/woff2",
	".ttf":   "font/ttf",
	".otf":   "font/otf",
	".eot":   "application/vnd.ms-fontobject",
	".mp4":   "video/mp4",
	".webm":  "video/webm",
	".mp3":   "audio/mpeg",
	".ogg":   "audio/ogg",
	".wasm":  "application/wasm",
}

func contentTypeFor(name string, data []byte) string {
	if ct, ok := contentTypes[strings.ToLower(path.Ext(name))]; ok {
		return ct
	}
	return http.DetectContentType(data)
}

// branchHead returns the HEAD commit SHA of the site's branch, cached briefly.
func (a *App) branchHead(ctx context.Context, user *User, site *Site) (string, error) {
	a.pageCache.mu.Lock()
	if e, ok := a.pageCache.heads[site.ID]; ok && time.Since(e.at) < headTTL {
		a.pageCache.mu.Unlock()
		return e.sha, nil
	}
	a.pageCache.mu.Unlock()

	var br forgejoBranch
	apiPath := "/repos/" + url.PathEscape(site.Owner) + "/" + url.PathEscape(site.Repo) +
		"/branches/" + url.PathEscape(site.Branch)
	if err := a.apiGet(ctx, user, apiPath, &br); err != nil {
		return "", err
	}
	if br.Commit.ID == "" {
		return "", fmt.Errorf("no commit for branch %s", site.Branch)
	}
	a.pageCache.mu.Lock()
	a.pageCache.heads[site.ID] = headEntry{sha: br.Commit.ID, at: time.Now()}
	a.pageCache.mu.Unlock()
	return br.Commit.ID, nil
}

// fetchFile reads one file from the repo at the given branch HEAD.
// apiPath is the repo-relative path (no leading slash).
//
// Files are fetched via the /media/ API endpoint, which serves raw bytes
// and resolves Git LFS pointers server-side (the /contents/ endpoint would
// only return the pointer file text). Directories 404, which the caller
// uses to fall through to index.html probing. Returns found=false when the
// path does not exist.
func (a *App) fetchFile(ctx context.Context, user *User, site *Site, headSHA, apiPath string) (data []byte, contentType string, found bool, err error) {
	key := fmt.Sprintf("%d|%s|%s", site.ID, headSHA, apiPath)
	a.pageCache.mu.Lock()
	if e, ok := a.pageCache.files[key]; ok {
		a.pageCache.mu.Unlock()
		return e.data, e.contentType, true, nil
	}
	a.pageCache.mu.Unlock()

	// Escape per path segment so slashes stay separators.
	segs := strings.Split(apiPath, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	body, status, err := forgejoGetRaw(ctx, a.cfg.ForgejoInternalURL, user.AccessToken,
		"/repos/"+url.PathEscape(site.Owner)+"/"+url.PathEscape(site.Repo)+
			"/media/"+strings.Join(segs, "/")+"?ref="+url.QueryEscape(site.Branch))
	if err != nil {
		return nil, "", false, err
	}
	if status == http.StatusNotFound {
		return nil, "", false, nil
	}
	if status < 200 || status >= 300 {
		return nil, "", false, fmt.Errorf("forgejo media %s: status %d", apiPath, status)
	}
	contentType = contentTypeFor(apiPath, body)

	a.pageCache.mu.Lock()
	if len(a.pageCache.files) >= maxCachedFiles {
		a.pageCache.files = make(map[string]fileEntry, maxCachedFiles)
	}
	a.pageCache.files[key] = fileEntry{data: body, contentType: contentType, at: time.Now()}
	a.pageCache.mu.Unlock()
	return body, contentType, true, nil
}

func (a *App) handlePage(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	repoName := r.PathValue("repo")

	site, err := a.store.getSite(owner, repoName)
	if err != nil || site == nil {
		http.NotFound(w, r)
		return
	}
	user, err := a.store.getUserByID(site.UserID)
	if err != nil || user == nil {
		http.NotFound(w, r)
		return
	}

	// Everything after /{owner}/{repo}/, segment by segment.
	ep := r.URL.EscapedPath()
	prefix := "/" + url.PathEscape(owner) + "/" + url.PathEscape(repoName)
	rest := strings.TrimPrefix(ep, prefix)
	rest = strings.TrimPrefix(rest, "/")
	var segs []string
	if rest != "" {
		for _, s := range strings.Split(rest, "/") {
			if s == "" || s == "." || s == ".." {
				http.NotFound(w, r)
				return
			}
			u, err := url.PathUnescape(s)
			if err != nil || u == "" || u == "." || u == ".." || strings.Contains(u, "/") {
				http.NotFound(w, r)
				return
			}
			segs = append(segs, u)
		}
	}

	full := path.Join(append([]string{"/", site.Subdir}, segs...)...)
	apiPath := strings.TrimPrefix(full, "/")

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	head, err := a.branchHead(ctx, user, site)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	var candidates []string
	if rest == "" || strings.HasSuffix(rest, "/") {
		candidates = []string{path.Join(apiPath, "index.html")}
	} else {
		candidates = []string{apiPath, apiPath + "/index.html"}
	}
	for _, c := range candidates {
		data, ct, found, err := a.fetchFile(ctx, user, site, head, c)
		if err != nil {
			http.Error(w, "upstream error", http.StatusBadGateway)
			return
		}
		if found {
			serveFile(w, r, data, ct, path.Base(c))
			return
		}
	}

	// SPA fallback: serve the site root's index.html for unknown paths.
	if site.SPAFallback {
		idx := path.Join(strings.TrimPrefix(path.Join("/", site.Subdir), "/"), "index.html")
		if data, ct, found, err := a.fetchFile(ctx, user, site, head, idx); err == nil && found {
			serveFile(w, r, data, ct, "index.html")
			return
		}
	}

	http.NotFound(w, r)
}

func serveFile(w http.ResponseWriter, r *http.Request, data []byte, contentType, name string) {
	w.Header().Set("Content-Type", contentType)
	http.ServeContent(w, r, name, time.Now(), bytes.NewReader(data))
}
