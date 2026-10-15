package main

import (
	"bytes"
	"errors"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"net/http"
	"strconv"
	"strings"

	webpstd "github.com/SeriousBug/webp-go-pure/std"
)

// maxImagePixels bounds the decoded size of images eligible for
// transcoding (16 MP ≈ 64 MiB as RGBA), so a pathological image can't
// OOM the transcoder.
const maxImagePixels = 16_000_000

// transcodableImage reports whether we can produce a better variant of
// this content type. PNG and JPEG only: GIFs may be animated (transcoding
// would silently drop frames) and SVG is already vector.
func transcodableImage(contentType string) bool {
	switch contentType {
	case "image/png", "image/jpeg":
		return true
	}
	return false
}

// negotiatedFormat picks the best image format the client accepts that we
// can encode, or "" to serve the original. WebP only for now: AVIF
// encoding needs cgo (libaom), which doesn't fit the static-binary
// container.
func negotiatedFormat(r *http.Request, contentType string) string {
	if !transcodableImage(contentType) {
		return ""
	}
	if accepts(r.Header.Get("Accept"), "image/webp") {
		return "webp"
	}
	return ""
}

// accepts reports whether an Accept header allows the given MIME type,
// honoring q-values (q=0 means "not acceptable").
func accepts(header, mime string) bool {
	if header == "" {
		return false
	}
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		mediaType := part
		q := 1.0
		if i := strings.Index(part, ";"); i >= 0 {
			mediaType = strings.TrimSpace(part[:i])
			for _, param := range strings.Split(part[i+1:], ";") {
				param = strings.TrimSpace(param)
				if v, ok := strings.CutPrefix(param, "q="); ok {
					if f, err := strconv.ParseFloat(v, 64); err == nil {
						q = f
					}
				}
			}
		}
		if q <= 0 {
			continue
		}
		if mediaType == mime || mediaType == "image/*" || mediaType == "*/*" {
			return true
		}
	}
	return false
}

// transcodeWebP decodes data (PNG or JPEG) and re-encodes it as lossy WebP
// at the given quality (1-100), using a pure-Go encoder (no cgo).
func transcodeWebP(data []byte, quality int) ([]byte, error) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	b := img.Bounds()
	if w, h := b.Dx(), b.Dy(); int64(w)*int64(h) > maxImagePixels {
		return nil, errors.New("image too large to transcode")
	}
	var buf bytes.Buffer
	if err := webpstd.Encode(&buf, img, &webpstd.Options{Quality: quality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
