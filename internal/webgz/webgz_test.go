package webgz

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func get(t *testing.T, h http.Handler, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func gunzip(t *testing.T, b []byte) string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("not gzip: %v", err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

var big = strings.Repeat(`{"power": 1234, "soc": 56},`, 100)

func TestHandlerZipsText(t *testing.T) {
	h := Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, big[:300])
		io.WriteString(w, big[300:])
	}))
	w := get(t, h, map[string]string{"Accept-Encoding": "gzip, deflate, br"})
	if w.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding = %q", w.Header().Get("Content-Encoding"))
	}
	if got := gunzip(t, w.Body.Bytes()); got != big {
		t.Errorf("body differs (%d bytes, want %d)", len(got), len(big))
	}
	if w.Body.Len() >= len(big) {
		t.Errorf("zipped %d bytes, plain %d", w.Body.Len(), len(big))
	}
}

func TestHandlerLeavesAlone(t *testing.T) {
	cases := map[string]struct {
		ctype, accept, enc string
		body               string
	}{
		"no gzip asked":   {"application/json", "", "", big},
		"gzip refused":    {"application/json", "gzip;q=0", "", big},
		"small":           {"application/json", "gzip", "", "{}"},
		"event stream":    {"text/event-stream", "gzip", "", big},
		"image":           {"image/png", "gzip", "", big},
		"already encoded": {"text/html", "gzip", "br", big},
	}
	for name, c := range cases {
		h := Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", c.ctype)
			if c.enc != "" {
				w.Header().Set("Content-Encoding", c.enc)
			}
			io.WriteString(w, c.body)
		}))
		w := get(t, h, map[string]string{"Accept-Encoding": c.accept})
		if got := w.Header().Get("Content-Encoding"); got != c.enc {
			t.Errorf("%s: Content-Encoding = %q, want %q", name, got, c.enc)
		}
		if w.Body.String() != c.body {
			t.Errorf("%s: body changed", name)
		}
	}
}

func TestHandlerStatusAndFlush(t *testing.T) {
	h := Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusAccepted)
		io.WriteString(w, "data: 1\n\n")
		w.(http.Flusher).Flush()
	}))
	w := get(t, h, map[string]string{"Accept-Encoding": "gzip"})
	if w.Code != http.StatusAccepted || !w.Flushed || w.Body.String() != "data: 1\n\n" {
		t.Errorf("code %d flushed %v body %q", w.Code, w.Flushed, w.Body.String())
	}
	empty := Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	}))
	if w := get(t, empty, map[string]string{"Accept-Encoding": "gzip"}); w.Code != http.StatusNotModified || w.Header().Get("Content-Encoding") != "" {
		t.Errorf("304: code %d, encoding %q", w.Code, w.Header().Get("Content-Encoding"))
	}
}

func TestAsset(t *testing.T) {
	page := "<!doctype html>" + big
	a := NewAsset("text/html; charset=utf-8", []byte(page))

	w := get(t, a, map[string]string{"Accept-Encoding": "gzip"})
	if w.Header().Get("Content-Encoding") != "gzip" || gunzip(t, w.Body.Bytes()) != page {
		t.Fatal("zipped asset differs")
	}
	etag := w.Header().Get("ETag")
	if !strings.HasSuffix(etag, `-gz"`) || w.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("ETag %q, Cache-Control %q", etag, w.Header().Get("Cache-Control"))
	}

	plain := get(t, a, nil)
	if plain.Header().Get("Content-Encoding") != "" || plain.Body.String() != page {
		t.Error("plain asset differs")
	}

	for _, inm := range []string{etag, plain.Header().Get("ETag"), "W/" + etag, `"x", ` + etag} {
		if w := get(t, a, map[string]string{"If-None-Match": inm, "Accept-Encoding": "gzip"}); w.Code != http.StatusNotModified || w.Body.Len() != 0 {
			t.Errorf("If-None-Match %s: code %d, %d bytes", inm, w.Code, w.Body.Len())
		}
	}
	if w := get(t, a, map[string]string{"If-None-Match": `"other"`}); w.Code != http.StatusOK {
		t.Errorf("other ETag: code %d", w.Code)
	}

	// Through Handler it is sent as it is, not zipped twice.
	w = get(t, Handler(a), map[string]string{"Accept-Encoding": "gzip"})
	if gunzip(t, w.Body.Bytes()) != page {
		t.Error("asset through Handler differs")
	}
}
