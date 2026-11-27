// Package webgz makes the app's pages light on the wire: Handler gzips every
// text response for a browser that takes gzip (pages, scripts, JSON, CSV),
// and Asset serves an embedded file zipped once at start, with an ETag so a
// page opened again costs a 304.
//
// The Orbit OS launcher in front rewrites the paths in HTML and CSS: it
// unzips a gzipped body, rewrites it and zips it again for the browser.
package webgz

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
)

// acceptsGzip reports whether the request takes a gzipped body.
func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		name, q, _ := strings.Cut(strings.TrimSpace(part), ";")
		if strings.EqualFold(strings.TrimSpace(name), "gzip") {
			q = strings.ReplaceAll(strings.TrimSpace(q), " ", "")
			return q != "q=0" && q != "q=0.0" && q != "q=0.00" && q != "q=0.000"
		}
	}
	return false
}

// compressible reports whether a body of content type ct is worth zipping:
// text, scripts, JSON and SVG; never the event stream, which is flushed a
// message at a time.
func compressible(ct string) bool {
	ct = strings.ToLower(ct)
	if strings.HasPrefix(ct, "text/event-stream") {
		return false
	}
	return strings.HasPrefix(ct, "text/") ||
		strings.Contains(ct, "javascript") ||
		strings.Contains(ct, "json") ||
		strings.Contains(ct, "svg")
}

// minSize is the smallest body Handler zips: below it gzip's header and the
// work cost more than they save.
const minSize = 512

var writers = sync.Pool{New: func() any {
	w, _ := gzip.NewWriterLevel(nil, gzip.BestSpeed)
	return w
}}

// Handler gzips next's text responses for a browser that takes gzip. A
// response that already has a Content-Encoding (an Asset's) passes as it is.
func Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead || !acceptsGzip(r) {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Add("Vary", "Accept-Encoding")
		gw := &gzipWriter{ResponseWriter: w}
		defer gw.close()
		next.ServeHTTP(gw, r)
	})
}

// gzipWriter holds back the start of the body until it knows whether to zip
// it: the content type, and minSize bytes or the end of the response.
type gzipWriter struct {
	http.ResponseWriter
	status  int
	buf     []byte // the body so far, while undecided
	decided bool
	zw      *gzip.Writer // nil: the body goes through as it is
}

func (g *gzipWriter) WriteHeader(status int) {
	if g.status == 0 {
		g.status = status
	}
}

func (g *gzipWriter) Write(p []byte) (int, error) {
	if g.status == 0 {
		g.status = http.StatusOK
	}
	if g.decided {
		if g.zw != nil {
			return g.zw.Write(p)
		}
		return g.ResponseWriter.Write(p)
	}
	g.buf = append(g.buf, p...)
	if len(g.buf) >= minSize {
		if err := g.decide(true); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// decide picks zipped or plain and sends the header and what is buffered;
// big says the body reached minSize.
func (g *gzipWriter) decide(big bool) error {
	g.decided = true
	h := g.Header()
	if h.Get("Content-Type") == "" && len(g.buf) > 0 {
		h.Set("Content-Type", http.DetectContentType(g.buf))
	}
	zip := big && h.Get("Content-Encoding") == "" && compressible(h.Get("Content-Type")) &&
		g.status != http.StatusNoContent && g.status != http.StatusNotModified &&
		g.status >= http.StatusOK
	if zip {
		h.Del("Content-Length")
		h.Set("Content-Encoding", "gzip")
		g.zw = writers.Get().(*gzip.Writer)
		g.zw.Reset(g.ResponseWriter)
	}
	g.ResponseWriter.WriteHeader(g.status)
	buf := g.buf
	g.buf = nil
	if len(buf) == 0 {
		return nil
	}
	var err error
	if g.zw != nil {
		_, err = g.zw.Write(buf)
	} else {
		_, err = g.ResponseWriter.Write(buf)
	}
	return err
}

// Flush sends what is written so far (the event stream needs it).
func (g *gzipWriter) Flush() {
	if g.status == 0 {
		g.status = http.StatusOK
	}
	if !g.decided {
		_ = g.decide(len(g.buf) >= minSize)
	}
	if g.zw != nil {
		_ = g.zw.Flush()
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack hands over the connection when nothing was sent yet.
func (g *gzipWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := g.ResponseWriter.(http.Hijacker); ok && !g.decided {
		g.decided = true
		return h.Hijack()
	}
	return nil, nil, errors.New("webgz: hijack not supported")
}

// Unwrap is for http.ResponseController.
func (g *gzipWriter) Unwrap() http.ResponseWriter { return g.ResponseWriter }

func (g *gzipWriter) close() {
	if !g.decided {
		if g.status == 0 && len(g.buf) == 0 {
			return // the handler wrote nothing: net/http sends its 200
		}
		if g.status == 0 {
			g.status = http.StatusOK
		}
		_ = g.decide(len(g.buf) >= minSize)
	}
	if g.zw != nil {
		_ = g.zw.Close()
		g.zw.Reset(nil)
		writers.Put(g.zw)
		g.zw = nil
	}
}

// Asset is an embedded file served with an ETag, zipped once at start.
type Asset struct {
	ctype    string
	body, gz []byte
	etag     string // of the body; the zipped one's ends in "-gz"
}

// NewAsset is body, of content type ctype, ready to be served.
func NewAsset(ctype string, body []byte) *Asset {
	sum := sha256.Sum256(body)
	var b bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&b, gzip.BestCompression)
	zw.Write(body)
	zw.Close()
	return &Asset{ctype: ctype, body: body, gz: b.Bytes(), etag: hex.EncodeToString(sum[:8])}
}

// ServeHTTP sends the asset with Cache-Control no-cache: the browser asks
// each time (pages and scripts must match after an update) and a copy it has
// costs a 304.
func (a *Asset) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", a.ctype)
	h.Set("Cache-Control", "no-cache")
	h.Add("Vary", "Accept-Encoding")
	zip := acceptsGzip(r)
	etag := `"` + a.etag + `"`
	if zip {
		etag = `"` + a.etag + `-gz"`
	}
	h.Set("ETag", etag)
	if matches(r.Header.Get("If-None-Match"), a.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	body := a.body
	if zip {
		h.Set("Content-Encoding", "gzip")
		body = a.gz
	}
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(body)
}

// matches reports whether an If-None-Match names this asset, in either
// encoding (a proxy may hand the zipped one's ETag back for the plain body).
func matches(inm, etag string) bool {
	for _, t := range strings.Split(inm, ",") {
		t = strings.TrimPrefix(strings.TrimSpace(t), "W/")
		t = strings.Trim(t, `"`)
		if t == "*" || t == etag || t == etag+"-gz" {
			return true
		}
	}
	return false
}
