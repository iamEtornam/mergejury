// Command site serves the mergejury landing page. Everything is embedded, so
// this is one binary with no working directory and no filesystem to mount:
// copy it to a host, run it, and point a reverse proxy at it.
//
//	site                      # 127.0.0.1:8080
//	site -addr :8080          # all interfaces
//	PORT=9000 site            # PORT wins when -addr is not passed
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path"
	"strings"
	"syscall"
	"time"

	"github.com/iamEtornam/mergejury/site"
)

func main() {
	defaultAddr := "127.0.0.1:8080"
	if p := os.Getenv("PORT"); p != "" {
		defaultAddr = ":" + p
	}
	addr := flag.String("addr", defaultAddr, "listen address")
	quiet := flag.Bool("quiet", false, "do not log requests")
	flag.Parse()

	handler, err := newHandler(*quiet)
	if err != nil {
		log.Fatalf("site: %v", err)
	}

	srv := &http.Server{
		Addr:    *addr,
		Handler: handler,
		// A public-facing server needs a header timeout; the rest of the
		// defaults are fine for static files.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("site: serving on http://%s", *addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("site: %v", err)
	}
	log.Print("site: stopped")
}

func newHandler(quiet bool) (http.Handler, error) {
	files, err := fs.Sub(site.Files, ".")
	if err != nil {
		return nil, err
	}
	fileServer := http.FileServer(http.FS(files))

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		p := path.Clean(r.URL.Path)

		// The installer is piped straight into a shell, so it must arrive as
		// plain text and must never be cached for long. /install.sh is an
		// alias for the same script.
		if p == "/install" || p == "/install.sh" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("Cache-Control", "public, max-age=300")
			serveFile(w, r, files, "install")
			return
		}
		// No directory listings: this is a flat set of assets.
		if p != "/" && strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		// `_headers` is configuration for other static hosts, not content.
		if base := path.Base(p); strings.HasPrefix(base, "_") || strings.HasPrefix(base, ".") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", cacheControl(p))
		fileServer.ServeHTTP(w, r)
	})

	var h http.Handler = securityHeaders(mux)
	if !quiet {
		h = logRequests(h)
	}
	return h, nil
}

func serveFile(w http.ResponseWriter, r *http.Request, files fs.FS, name string) {
	f, err := files.Open(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	rs, ok := f.(interface {
		Read([]byte) (int, error)
		Seek(int64, int) (int64, error)
	})
	if !ok {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// ServeContent handles Range, If-Modified-Since, and HEAD correctly, and
	// respects the Content-Type already set above.
	http.ServeContent(w, r, name, info.ModTime(), rs)
}

// cacheControl keeps HTML fresh (the copy changes) while letting immutable
// assets sit in caches. Nothing here is content-hashed, so "immutable" would
// be a lie; a day is the compromise.
func cacheControl(p string) string {
	switch path.Ext(p) {
	case ".woff2", ".png", ".svg":
		return "public, max-age=86400"
	case ".txt", ".xml":
		return "public, max-age=3600"
	default:
		return "public, max-age=600"
	}
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		// The page is self-contained: no external scripts, styles, or fonts.
		h.Set("Content-Security-Policy",
			"default-src 'none'; img-src 'self' data:; style-src 'unsafe-inline'; font-src 'self'; script-src 'unsafe-inline'; base-uri 'none'; form-action 'none'")
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Printf("%s %s %d %s", r.Method, r.URL.Path, rec.status,
			fmt.Sprintf("%.1fms", float64(time.Since(start).Microseconds())/1000))
	})
}
