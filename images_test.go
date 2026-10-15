package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"testing"
)

// noisePNG builds a PNG that lossy WebP reliably beats: random noise is
// incompressible as PNG but compresses well with lossy encoding,
// like a photographic image saved as PNG.
func noisePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	rng := rand.New(rand.NewSource(1))
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{
				uint8(rng.Intn(256)), uint8(rng.Intn(256)), uint8(rng.Intn(256)), 255,
			})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func getWithAccept(t *testing.T, app *App, target, accept string) *httptest.ResponseRecorder {
	t.Helper()
	headers := map[string]string{}
	if accept != "" {
		headers["Accept"] = accept
	}
	return getWithHeaders(t, app, target, headers)
}

func getWithHeaders(t *testing.T, app *App, target string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.SetPathValue("owner", "acme")
	req.SetPathValue("repo", "blog")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	app.handlePage(rec, req)
	return rec
}

func TestWebPTranscoding(t *testing.T) {
	original := noisePNG(t, 128, 128)
	srv := fakeForgejo(t, map[string]string{"photo.png": string(original)}, nil)
	defer srv.Close()
	app := testApp(t, srv.URL)
	publish(t, app, "acme", "blog", "main", "", false)

	// Sanity check on the test input itself.
	if webpBytes, err := transcodeWebP(original, 80); err != nil || len(webpBytes) >= len(original) {
		t.Fatalf("test assumption broken: webp err=%v sizes %d vs %d", err, len(webpBytes), len(original))
	}

	rec := getWithAccept(t, app, "https://pages.example.com/acme/blog/photo.png", "image/webp")
	if rec.Code != 200 {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/webp" {
		t.Fatalf("content type = %q, want image/webp", ct)
	}
	if vary := rec.Header().Get("Vary"); vary != "Accept" {
		t.Fatalf("Vary = %q, want Accept", vary)
	}
	if rec.Body.Len() >= len(original) {
		t.Fatalf("served %d bytes, original was %d", rec.Body.Len(), len(original))
	}
	webpBody := append([]byte(nil), rec.Body.Bytes()...)

	// Without a WebP Accept header: the original PNG, no Vary.
	recNoAccept := getWithAccept(t, app, "https://pages.example.com/acme/blog/photo.png", "text/html")
	if ct := recNoAccept.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("content type = %q, want image/png", ct)
	}
	if recNoAccept.Body.Len() != len(original) {
		t.Fatalf("body = %d bytes, want original %d", recNoAccept.Body.Len(), len(original))
	}
	if vary := recNoAccept.Header().Get("Vary"); vary != "" {
		t.Fatalf("Vary = %q, want empty", vary)
	}

	// A second WebP request is served from the variant cache.
	rec2 := getWithAccept(t, app, "https://pages.example.com/acme/blog/photo.png", "image/webp")
	if !bytes.Equal(webpBody, rec2.Body.Bytes()) {
		t.Fatal("cached variant differs from first transcode")
	}
}

func TestWebPNeverLargerThanOriginal(t *testing.T) {
	// A 2x2 flat PNG: WebP container overhead may exceed the original,
	// in which case the original must be served.
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	original := buf.Bytes()

	srv := fakeForgejo(t, map[string]string{"tiny.png": string(original)}, nil)
	defer srv.Close()
	app := testApp(t, srv.URL)
	publish(t, app, "acme", "blog", "main", "", false)

	rec := getWithAccept(t, app, "https://pages.example.com/acme/blog/tiny.png", "image/webp")
	if rec.Code != 200 {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	if rec.Body.Len() > len(original) {
		t.Fatalf("served %d bytes, larger than original %d", rec.Body.Len(), len(original))
	}
}

func TestNonImageUnaffectedByAccept(t *testing.T) {
	srv := fakeForgejo(t, map[string]string{"index.html": "<h1>x</h1>"}, nil)
	defer srv.Close()
	app := testApp(t, srv.URL)
	publish(t, app, "acme", "blog", "main", "", false)

	rec := getWithAccept(t, app, "https://pages.example.com/acme/blog/", "image/webp")
	if rec.Code != 200 {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Fatalf("content type = %q", ct)
	}
	if vary := rec.Header().Get("Vary"); vary != "" {
		t.Fatalf("Vary = %q, want empty", vary)
	}
}

func TestAccepts(t *testing.T) {
	cases := []struct {
		header string
		mime   string
		want   bool
	}{
		{"image/webp", "image/webp", true},
		{"text/html, image/webp", "image/webp", true},
		{"image/*", "image/webp", true},
		{"*/*", "image/webp", true},
		{"image/avif,image/webp;q=0.9,*/*;q=0.8", "image/webp", true},
		{"image/webp;q=0", "image/webp", false},
		{"image/webp;q=0.0, image/png", "image/webp", false},
		{"text/html", "image/webp", false},
		{"", "image/webp", false},
	}
	for _, c := range cases {
		if got := accepts(c.header, c.mime); got != c.want {
			t.Errorf("accepts(%q, %q) = %v, want %v", c.header, c.mime, got, c.want)
		}
	}
}

func TestTranscodableImage(t *testing.T) {
	for _, ct := range []string{"image/png", "image/jpeg"} {
		if !transcodableImage(ct) {
			t.Errorf("%s should be transcodable", ct)
		}
	}
	for _, ct := range []string{"image/gif", "image/svg+xml", "image/webp", "image/avif", "text/html"} {
		if transcodableImage(ct) {
			t.Errorf("%s should not be transcodable", ct)
		}
	}
}
