package main

import "testing"

func TestCacheMaxAge(t *testing.T) {
	t.Setenv("PAGEFORGE_CACHE_MAX_AGE", "")
	if got := cacheMaxAge(); got != 600 {
		t.Fatalf("default = %d, want 600", got)
	}
	t.Setenv("PAGEFORGE_CACHE_MAX_AGE", "604800")
	if got := cacheMaxAge(); got != 604800 {
		t.Fatalf("custom = %d, want 604800", got)
	}
	t.Setenv("PAGEFORGE_CACHE_MAX_AGE", "0")
	if got := cacheMaxAge(); got != 0 {
		t.Fatalf("zero = %d, want 0", got)
	}
	t.Setenv("PAGEFORGE_CACHE_MAX_AGE", "-5")
	if got := cacheMaxAge(); got != 600 {
		t.Fatalf("negative = %d, want default 600", got)
	}
	t.Setenv("PAGEFORGE_CACHE_MAX_AGE", "bogus")
	if got := cacheMaxAge(); got != 600 {
		t.Fatalf("bogus = %d, want default 600", got)
	}
}

func TestWebPQuality(t *testing.T) {
	t.Setenv("PAGEFORGE_WEBP_QUALITY", "")
	if got := webpQuality(); got != 80 {
		t.Fatalf("default = %d, want 80", got)
	}
	t.Setenv("PAGEFORGE_WEBP_QUALITY", "50")
	if got := webpQuality(); got != 50 {
		t.Fatalf("custom = %d, want 50", got)
	}
	t.Setenv("PAGEFORGE_WEBP_QUALITY", "500")
	if got := webpQuality(); got != 100 {
		t.Fatalf("clamped = %d, want 100", got)
	}
}
