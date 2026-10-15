package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config is everything pageforge needs, via environment variables.
type Config struct {
	// ForgejoURL is the public Forgejo URL, e.g. https://git.example.com.
	// Used for browser-facing OAuth redirects.
	ForgejoURL string
	// ForgejoInternalURL is the Forgejo URL reachable from this container,
	// e.g. http://forgejo:3000 on the docker network. Used for server-side
	// API calls and the OAuth token exchange. Defaults to ForgejoURL.
	ForgejoInternalURL string
	// PublicURL is this app's own public URL, e.g. https://pages.example.com.
	// Used for the OAuth callback and for displaying published site links.
	PublicURL    string
	ClientID     string
	ClientSecret string
	// SessionSecret signs session cookies and encrypts stored OAuth tokens.
	SessionSecret string
	// WebPQuality is the quality (1-100) used when transcoding images.
	WebPQuality int
	Addr        string
	DBPath      string
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func loadConfig() (*Config, error) {
	cfg := &Config{
		ForgejoURL:         strings.TrimSuffix(env("PAGEFORGE_FORGEJO_URL", ""), "/"),
		ForgejoInternalURL: strings.TrimSuffix(env("PAGEFORGE_FORGEJO_INTERNAL_URL", ""), "/"),
		PublicURL:          strings.TrimSuffix(env("PAGEFORGE_PUBLIC_URL", ""), "/"),
		ClientID:           env("PAGEFORGE_CLIENT_ID", ""),
		ClientSecret:       env("PAGEFORGE_CLIENT_SECRET", ""),
		SessionSecret:      env("PAGEFORGE_SESSION_SECRET", ""),
		Addr:               env("PAGEFORGE_ADDR", ":8080"),
		DBPath:             env("PAGEFORGE_DB", "./pageforge.db"),
		WebPQuality:        webpQuality(),
	}
	if cfg.ForgejoInternalURL == "" {
		cfg.ForgejoInternalURL = cfg.ForgejoURL
	}
	var missing []string
	for _, kv := range [][2]string{
		{"PAGEFORGE_FORGEJO_URL", cfg.ForgejoURL},
		{"PAGEFORGE_PUBLIC_URL", cfg.PublicURL},
		{"PAGEFORGE_CLIENT_ID", cfg.ClientID},
		{"PAGEFORGE_CLIENT_SECRET", cfg.ClientSecret},
		{"PAGEFORGE_SESSION_SECRET", cfg.SessionSecret},
	} {
		if kv[1] == "" {
			missing = append(missing, kv[0])
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required env vars: %s", strings.Join(missing, ", "))
	}
	return cfg, nil
}

// webpQuality parses PAGEFORGE_WEBP_QUALITY (default 80, clamped to 1-100).
func webpQuality() int {
	q := 80
	if v := env("PAGEFORGE_WEBP_QUALITY", ""); v != "" {
		if f, err := strconv.Atoi(v); err == nil {
			q = f
		}
	}
	if q < 1 {
		q = 1
	}
	if q > 100 {
		q = 100
	}
	return q
}

// tokenKey derives the AES-256 key used to encrypt OAuth tokens at rest.
func (c *Config) tokenKey() []byte {
	sum := sha256.Sum256([]byte("pageforge:v1:tokens:" + c.SessionSecret))
	return sum[:]
}
