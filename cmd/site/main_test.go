package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testHandler(t *testing.T) http.Handler {
	t.Helper()
	h, err := newHandler(true)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestRoutes(t *testing.T) {
	h := testHandler(t)
	tests := []struct {
		path        string
		wantStatus  int
		wantType    string
		wantContent string
	}{
		{"/", 200, "text/html", "mergejury"},
		// Piped into a shell, so it must be plain text under both names.
		{"/install", 200, "text/plain", "#!/bin/sh"},
		{"/install.sh", 200, "text/plain", "#!/bin/sh"},
		{"/favicon.svg", 200, "image/svg+xml", "svg"},
		{"/robots.txt", 200, "text/plain", "Sitemap"},
		{"/sitemap.xml", 200, "xml", "urlset"},
		{"/gambarino.woff2", 200, "font/woff2", ""},
		{"/og.png", 200, "image/png", ""},
		// Host config, not content.
		{"/_headers", 404, "", ""},
		// No directory listings.
		{"/nope", 404, "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if tc.wantType != "" && !strings.Contains(rec.Header().Get("Content-Type"), tc.wantType) {
				t.Errorf("content-type = %q, want it to contain %q", rec.Header().Get("Content-Type"), tc.wantType)
			}
			if tc.wantContent != "" && !strings.Contains(rec.Body.String(), tc.wantContent) {
				t.Errorf("body does not contain %q", tc.wantContent)
			}
		})
	}
}

func TestInstallScriptIsTheRealOne(t *testing.T) {
	h := testHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/install", nil))
	body := rec.Body.String()
	// Guards against serving a stale or truncated copy: these are the lines
	// that make the installer safe to pipe into a shell.
	for _, want := range []string{"set -eu", "checksum mismatch", "iamEtornam/mergejury"} {
		if !strings.Contains(body, want) {
			t.Errorf("served installer is missing %q", want)
		}
	}
}

func TestOnlyReadMethods(t *testing.T) {
	h := testHandler(t)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, "/", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s / = %d, want 405", method, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("HEAD / = %d, want 200", rec.Code)
	}
}

func TestSecurityHeaders(t *testing.T) {
	h := testHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q", got)
	}
	if rec.Header().Get("Content-Security-Policy") == "" {
		t.Error("missing Content-Security-Policy")
	}
}
