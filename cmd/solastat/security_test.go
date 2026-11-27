package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadProfileRejectsPaths(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(filepath.Dir(dir), "secret.json")
	if err := os.WriteFile(secret, []byte(`{"name":"x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(secret) })
	for _, id := range []string{"../secret", "a/b", "..", "x.json/..", `..\secret`} {
		if _, err := loadProfile(id, dir); err == nil {
			t.Errorf("loadProfile(%q) accepted a path", id)
		}
	}
}

func TestProtectCrossOrigin(t *testing.T) {
	h := protectCrossOrigin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	cases := []struct {
		name, method, site, origin string
		want                       int
	}{
		{"own page", http.MethodPost, "same-origin", "", http.StatusOK},
		{"other site", http.MethodPost, "cross-site", "", http.StatusForbidden},
		{"other site, reading", http.MethodGet, "cross-site", "", http.StatusOK},
		{"curl", http.MethodPost, "", "", http.StatusOK},
		{"old browser, other site", http.MethodPost, "", "http://evil.example", http.StatusForbidden},
		{"old browser, own page", http.MethodPost, "", "http://device:55000", http.StatusOK},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, "http://device:55000/api/setup/reset", nil)
		if c.site != "" {
			req.Header.Set("Sec-Fetch-Site", c.site)
		}
		if c.origin != "" {
			req.Header.Set("Origin", c.origin)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s: status %d, want %d", c.name, rec.Code, c.want)
		}
	}
}

func TestRequireSetup(t *testing.T) {
	done := false
	h := requireSetup(func() bool { return done }, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	const page = "text/html,application/xhtml+xml,*/*;q=0.8"
	cases := []struct {
		name, method, path, accept string
		want                       int
	}{
		{"settings page", http.MethodGet, "/settings", page, http.StatusFound},
		{"plugin page", http.MethodGet, "/smartspot", page, http.StatusFound},
		{"wizard page", http.MethodGet, "/wizard", page, http.StatusOK},
		{"api call", http.MethodGet, "/api/ports", "*/*", http.StatusOK},
		{"api with html accept", http.MethodGet, "/api/config", page, http.StatusOK},
		{"script", http.MethodGet, "/nav.js", "*/*", http.StatusOK},
		{"post", http.MethodPost, "/settings", page, http.StatusOK},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, nil)
		req.Header.Set("Accept", c.accept)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s: got %d, want %d", c.name, rec.Code, c.want)
		}
		if c.want == http.StatusFound && rec.Header().Get("Location") != "/wizard" {
			t.Errorf("%s: redirected to %q", c.name, rec.Header().Get("Location"))
		}
	}
	done = true
	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	req.Header.Set("Accept", page)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("after setup: got %d, want 200", rec.Code)
	}
}
