// Command pageforge serves static sites from Forgejo repositories.
//
// Users log in with their Forgejo account via OAuth2, choose which
// repositories to publish (plus branch and optional subdirectory), and
// pageforge serves the latest files from those branches through the
// Forgejo API — no build step, no deploy pipeline.
package main

import (
	"embed"
	"html/template"
	"log"
	"net/http"
	"time"
)

//go:embed templates/*.html
var tmplFS embed.FS

type App struct {
	cfg       *Config
	store     *Store
	templates *template.Template
	pageCache *pageCache
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	store, err := openStore(cfg.DBPath, cfg.tokenKey())
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer store.close()

	tmpl, err := template.ParseFS(tmplFS, "templates/*.html")
	if err != nil {
		log.Fatalf("templates: %v", err)
	}

	app := &App{cfg: cfg, store: store, templates: tmpl, pageCache: newPageCache()}
	go app.janitor()

	mux := http.NewServeMux()
	// Note: no method constraints on patterns — Go's ServeMux panics on
	// certain method+path overlaps between wildcard and literal routes
	// (e.g. /{owner}/{repo}/ vs GET /sites/...), so handlers enforce
	// methods themselves where it matters (form posts check CSRF).
	// Management UI.
	mux.HandleFunc("/", app.handleHome)
	mux.HandleFunc("/login", app.handleLogin)
	mux.HandleFunc("/oauth/callback", app.handleOAuthCallback)
	mux.HandleFunc("/logout", app.handleLogout)
	mux.HandleFunc("/sites", app.handleSites)
	mux.HandleFunc("/sites/publish", app.handlePublish)
	mux.HandleFunc("/sites/unpublish", app.handleUnpublish)
	mux.HandleFunc("/sites/{owner}/{repo}", app.handleSiteSettingsSave)
	// Published sites.
	mux.HandleFunc("/{owner}/{repo}/", app.handlePage)
	mux.HandleFunc("/{owner}/{repo}", func(w http.ResponseWriter, r *http.Request) {
		q := ""
		if r.URL.RawQuery != "" {
			q = "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, r.URL.EscapedPath()+"/"+q, http.StatusMovedPermanently)
	})

	log.Printf("pageforge listening on %s (forgejo: %s, public: %s)", cfg.Addr, cfg.ForgejoURL, cfg.PublicURL)
	if err := http.ListenAndServe(cfg.Addr, mux); err != nil {
		log.Fatal(err)
	}
}

// janitor periodically deletes expired sessions and OAuth states.
func (a *App) janitor() {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for range t.C {
		if err := a.store.cleanupExpired(); err != nil {
			log.Printf("janitor: %v", err)
		}
	}
}
