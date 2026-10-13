package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// forgejoRepo is the subset of the Forgejo repository API object we need.
type forgejoRepo struct {
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"default_branch"`
	Owner         struct {
		Login string `json:"login"`
	} `json:"owner"`
}

// forgejoBranch is the subset of the branch API object we need.
type forgejoBranch struct {
	Name   string `json:"name"`
	Commit struct {
		ID string `json:"id"`
	} `json:"commit"`
}

// forgejoGet performs an authenticated GET against the Forgejo API
// (server-side, so it uses the internal docker-network URL).
func forgejoGet(ctx context.Context, base, accessToken, apiPath string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1"+apiPath, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "token "+accessToken)
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("forgejo api %s: %s", apiPath, resp.Status)
	}
	return json.Unmarshal(body, out)
}

// maxFileBytes caps a single served file at 64 MiB.
const maxFileBytes = 64 << 20

// forgejoGetRaw performs an authenticated GET against the Forgejo API
// and returns the raw response body and status code.
func forgejoGetRaw(ctx context.Context, base, accessToken, apiPath string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1"+apiPath, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "token "+accessToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFileBytes+1))
	if err != nil {
		return nil, 0, err
	}
	if int64(len(body)) > maxFileBytes {
		return nil, 0, fmt.Errorf("file exceeds %d MiB limit", maxFileBytes>>20)
	}
	return body, resp.StatusCode, nil
}

// tokenResponse is the OAuth2 token endpoint response.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

// refreshUserToken exchanges the stored refresh token for a new access token.
func (a *App) refreshUserToken(ctx context.Context, user *User) error {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {user.RefreshToken},
		"client_id":     {a.cfg.ClientID},
		"client_secret": {a.cfg.ClientSecret},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		a.cfg.ForgejoInternalURL+"/login/oauth/access_token",
		bytes.NewBufferString(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("token refresh: %s", resp.Status)
	}
	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return err
	}
	if tr.AccessToken == "" {
		return fmt.Errorf("token refresh: empty access token")
	}
	expiry := time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	if tr.ExpiresIn == 0 {
		expiry = time.Now().Add(time.Hour)
	}
	refresh := tr.RefreshToken
	if refresh == "" {
		refresh = user.RefreshToken // some providers only rotate the access token
	}
	if err := a.store.updateUserTokens(user.ID, tr.AccessToken, refresh, expiry); err != nil {
		return err
	}
	user.AccessToken = tr.AccessToken
	user.RefreshToken = refresh
	user.TokenExpiry = expiry
	return nil
}

// apiGet performs an authenticated Forgejo API GET as the given user,
// refreshing the OAuth token first if it is expired or about to expire.
func (a *App) apiGet(ctx context.Context, user *User, apiPath string, out any) error {
	if time.Until(user.TokenExpiry) < time.Minute {
		rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := a.refreshUserToken(rctx, user)
		cancel()
		if err != nil {
			return fmt.Errorf("refreshing token: %w", err)
		}
	}
	return forgejoGet(ctx, a.cfg.ForgejoInternalURL, user.AccessToken, apiPath, out)
}
