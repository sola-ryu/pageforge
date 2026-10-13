package main

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

const sessionCookieName = "pageforge_session"

// oauth2Config builds the OAuth2 config. The authorize URL is public
// (the browser goes there); the token URL is server-side, so it uses the
// internal docker-network address when one is configured.
func (a *App) oauth2Config() *oauth2.Config {
	return &oauth2.Config{
		ClientID:     a.cfg.ClientID,
		ClientSecret: a.cfg.ClientSecret,
		RedirectURL:  a.cfg.PublicURL + "/oauth/callback",
		Scopes:       []string{"read:user", "read:repository"},
		Endpoint: oauth2.Endpoint{
			AuthURL:  a.cfg.ForgejoURL + "/login/oauth/authorize",
			TokenURL: a.cfg.ForgejoInternalURL + "/login/oauth/access_token",
		},
	}
}

func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	state, err := randHex(24)
	if err != nil {
		http.Error(w, "could not start login", http.StatusInternalServerError)
		return
	}
	if err := a.store.saveOAuthState(state, 10*time.Minute); err != nil {
		http.Error(w, "could not start login", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, a.oauth2Config().AuthCodeURL(state), http.StatusFound)
}

func (a *App) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("error") != "" {
		http.Error(w, "authorization failed: "+q.Get("error_description"), http.StatusBadRequest)
		return
	}
	state, code := q.Get("state"), q.Get("code")
	if state == "" || code == "" || !a.store.consumeOAuthState(state) {
		http.Error(w, "invalid or expired login request", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	tok, err := a.oauth2Config().Exchange(ctx, code)
	if err != nil {
		log.Printf("oauth exchange: %v", err)
		http.Error(w, "could not complete login", http.StatusBadGateway)
		return
	}

	// Who just logged in?
	var fu struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
	}
	if err := forgejoGet(ctx, a.cfg.ForgejoInternalURL, tok.AccessToken, "/user", &fu); err != nil {
		log.Printf("oauth user lookup: %v", err)
		http.Error(w, "could not complete login", http.StatusBadGateway)
		return
	}
	if fu.ID == 0 || fu.Login == "" {
		http.Error(w, "could not complete login", http.StatusBadGateway)
		return
	}

	expiry := tok.Expiry
	if expiry.IsZero() {
		expiry = time.Now().Add(time.Hour)
	}
	user, err := a.store.upsertUser(fu.ID, fu.Login, tok.AccessToken, tok.RefreshToken, expiry)
	if err != nil {
		log.Printf("user upsert: %v", err)
		http.Error(w, "could not complete login", http.StatusInternalServerError)
		return
	}

	sess, err := a.store.createSession(user.ID)
	if err != nil {
		log.Printf("session create: %v", err)
		http.Error(w, "could not complete login", http.StatusInternalServerError)
		return
	}
	a.setSessionCookie(w, sess.ID, sess.ExpiresAt)
	http.Redirect(w, r, "/sites", http.StatusFound)
}

func (a *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookieName); err == nil {
		a.store.deleteSession(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
	})
	http.Redirect(w, r, "/", http.StatusFound)
}

func (a *App) setSessionCookie(w http.ResponseWriter, sessionID string, expires time.Time) {
	// Secure cookies only when the app itself is served over HTTPS
	// (normally TLS terminates at the reverse proxy).
	secure := strings.HasPrefix(a.cfg.PublicURL, "https://")
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    sessionID,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// currentUser resolves the logged-in user from the session cookie, or nil.
func (a *App) currentUser(r *http.Request) (*User, *Session) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return nil, nil
	}
	sess, err := a.store.getSession(c.Value)
	if err != nil {
		return nil, nil
	}
	user, err := a.store.getUserByID(sess.UserID)
	if err != nil {
		return nil, nil
	}
	return user, sess
}

// requireUser redirects anonymous visitors to the login page.
func (a *App) requireUser(w http.ResponseWriter, r *http.Request) (*User, *Session, bool) {
	user, sess := a.currentUser(r)
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusFound)
		return nil, nil, false
	}
	return user, sess, true
}

// checkCSRF validates the CSRF token submitted with a POST form.
func checkCSRF(sess *Session, r *http.Request) bool {
	if err := r.ParseForm(); err != nil {
		return false
	}
	tok := r.Form.Get("csrf_token")
	return tok != "" && tok == sess.CSRFToken
}
