package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path"
	"strings"
	"time"
)

// pageData is the template data for every management-UI page.
type pageData struct {
	User       *User
	CSRF       string
	Title      string
	PublicURL  string
	ForgejoURL string
	Repos      []repoRow
	Site       *Site
	Branches   []string
	SiteURL    string
	Error      string
}

type repoRow struct {
	Owner         string
	Name          string
	FullName      string
	Private       bool
	DefaultBranch string
	Site          *Site
}

func (a *App) render(w http.ResponseWriter, name string, data *pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.templates.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("render %s: %v", name, err)
	}
}

func (a *App) baseData(user *User, sess *Session) *pageData {
	d := &pageData{PublicURL: a.cfg.PublicURL, ForgejoURL: a.cfg.ForgejoURL}
	if user != nil {
		d.User = user
	}
	if sess != nil {
		d.CSRF = sess.CSRFToken
	}
	return d
}

func (a *App) handleHome(w http.ResponseWriter, r *http.Request) {
	if user, _ := a.currentUser(r); user != nil {
		http.Redirect(w, r, "/sites", http.StatusFound)
		return
	}
	d := a.baseData(nil, nil)
	d.Title = "pageforge"
	a.render(w, "home", d)
}

// handleSites lists the user's repositories with their publish state.
func (a *App) handleSites(w http.ResponseWriter, r *http.Request) {
	user, sess, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	var repos []forgejoRepo
	for page := 1; page <= 3; page++ {
		var batch []forgejoRepo
		err := a.apiGet(ctx, user,
			fmt.Sprintf("/user/repos?limit=50&page=%d&sort=updated&order=desc", page), &batch)
		if err != nil {
			d := a.baseData(user, sess)
			d.Title = "Sites"
			d.Error = "Could not list repositories: " + err.Error()
			a.render(w, "sites", d)
			return
		}
		repos = append(repos, batch...)
		if len(batch) < 50 {
			break
		}
	}

	sites, err := a.store.listSitesByUser(user.ID)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	byRepo := make(map[string]*Site, len(sites))
	for i := range sites {
		byRepo[sites[i].Owner+"/"+sites[i].Repo] = &sites[i]
	}

	rows := make([]repoRow, 0, len(repos))
	for _, repo := range repos {
		rows = append(rows, repoRow{
			Owner:         repo.Owner.Login,
			Name:          repo.Name,
			FullName:      repo.FullName,
			Private:       repo.Private,
			DefaultBranch: repo.DefaultBranch,
			Site:          byRepo[repo.Owner.Login+"/"+repo.Name],
		})
	}

	d := a.baseData(user, sess)
	d.Title = "Sites"
	d.Repos = rows
	a.render(w, "sites", d)
}

// handlePublish publishes a repository on Pages.
func (a *App) handlePublish(w http.ResponseWriter, r *http.Request) {
	user, sess, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	if !checkCSRF(sess, r) {
		http.Error(w, "bad csrf token", http.StatusBadRequest)
		return
	}
	owner, repoName := r.Form.Get("owner"), r.Form.Get("repo")
	if owner == "" || repoName == "" {
		http.Error(w, "missing repository", http.StatusBadRequest)
		return
	}
	if existing, _ := a.store.getSite(owner, repoName); existing != nil {
		http.Redirect(w, r, "/sites/"+owner+"/"+repoName, http.StatusFound)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	var repo forgejoRepo
	if err := a.apiGet(ctx, user, "/repos/"+owner+"/"+repoName, &repo); err != nil {
		http.Error(w, "cannot access repository: "+err.Error(), http.StatusBadRequest)
		return
	}
	branch := repo.DefaultBranch
	if branch == "" {
		branch = "main"
	}
	if _, err := a.store.createSite(user.ID, repo.Owner.Login, repo.Name, branch); err != nil {
		http.Error(w, "could not publish: "+err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/sites/"+repo.Owner.Login+"/"+repo.Name, http.StatusFound)
}

// handleUnpublish removes a site.
func (a *App) handleUnpublish(w http.ResponseWriter, r *http.Request) {
	user, sess, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	if !checkCSRF(sess, r) {
		http.Error(w, "bad csrf token", http.StatusBadRequest)
		return
	}
	var id int64
	if _, err := fmt.Sscanf(r.Form.Get("site_id"), "%d", &id); err != nil {
		http.Error(w, "bad site id", http.StatusBadRequest)
		return
	}
	site, err := a.store.getSiteByID(id)
	if err != nil || site == nil || site.UserID != user.ID {
		http.NotFound(w, r)
		return
	}
	if err := a.store.deleteSite(id); err != nil {
		http.Error(w, "could not unpublish", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/sites", http.StatusFound)
}

// handleSiteSettingsSave dispatches the per-repo settings page:
// GET renders the form, POST saves it.
func (a *App) handleSiteSettingsSave(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.handleSiteSettings(w, r)
	case http.MethodPost:
		a.handleSiteSave(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleSiteSettings shows the per-repo Pages settings form.
func (a *App) handleSiteSettings(w http.ResponseWriter, r *http.Request) {
	user, sess, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	site, err := a.store.getSite(r.PathValue("owner"), r.PathValue("repo"))
	if err != nil || site == nil || site.UserID != user.ID {
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "database error", http.StatusInternalServerError)
			return
		}
		http.NotFound(w, r)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	var branches []forgejoBranch
	if err := a.apiGet(ctx, user, "/repos/"+site.Owner+"/"+site.Repo+"/branches?limit=100", &branches); err != nil {
		d := a.baseData(user, sess)
		d.Title = site.Owner + "/" + site.Repo
		d.Site = site
		d.Error = "Could not list branches: " + err.Error()
		a.render(w, "site", d)
		return
	}
	names := make([]string, 0, len(branches))
	for _, b := range branches {
		names = append(names, b.Name)
	}

	d := a.baseData(user, sess)
	d.Title = site.Owner + "/" + site.Repo
	d.Site = site
	d.Branches = names
	d.SiteURL = a.cfg.PublicURL + "/" + site.Owner + "/" + site.Repo + "/"
	a.render(w, "site", d)
}

// handleSiteSave stores the per-repo Pages settings.
func (a *App) handleSiteSave(w http.ResponseWriter, r *http.Request) {
	user, sess, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	if !checkCSRF(sess, r) {
		http.Error(w, "bad csrf token", http.StatusBadRequest)
		return
	}
	site, err := a.store.getSite(r.PathValue("owner"), r.PathValue("repo"))
	if err != nil || site == nil || site.UserID != user.ID {
		http.NotFound(w, r)
		return
	}

	branch := r.Form.Get("branch")
	rawSubdir := r.Form.Get("subdir")
	subdir := cleanSubdir(rawSubdir)
	spa := r.Form.Get("spa_fallback") == "on"
	if branch == "" {
		http.Error(w, "branch is required", http.StatusBadRequest)
		return
	}
	if subdir == "" && strings.Trim(rawSubdir, "/") != "" {
		// cleanSubdir rejected the input
		http.Error(w, "invalid subdirectory", http.StatusBadRequest)
		return
	}

	// Confirm the branch exists (and the token can still read the repo).
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	var br forgejoBranch
	if err := a.apiGet(ctx, user, "/repos/"+site.Owner+"/"+site.Repo+"/branches/"+branch, &br); err != nil {
		http.Error(w, "branch not found: "+branch, http.StatusBadRequest)
		return
	}

	if err := a.store.updateSite(site.ID, branch, subdir, spa); err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	// Drop cached HEAD/files for this site so the new settings apply now.
	a.pageCache.mu.Lock()
	delete(a.pageCache.heads, site.ID)
	a.pageCache.mu.Unlock()
	http.Redirect(w, r, "/sites/"+site.Owner+"/"+site.Repo, http.StatusFound)
}

// cleanSubdir normalizes the subdirectory setting. Returns "" both for empty
// input and for invalid input; callers distinguish via the raw input value.
func cleanSubdir(in string) string {
	in = strings.Trim(in, "/")
	if in == "" {
		return ""
	}
	cleaned := path.Clean(in)
	if cleaned == "." || cleaned == "/" {
		return ""
	}
	for _, seg := range strings.Split(cleaned, "/") {
		if seg == ".." || seg == "" {
			return ""
		}
	}
	return cleaned
}
