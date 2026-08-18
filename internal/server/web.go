package server

import (
	"embed"
	"io/fs"
	"net"
	"net/http"
	"strings"
	"time"
)

const controlSessionCookie = "oort_control_session"

// The dashboard is a React app built by `npm run build` in web/; the dist
// output is committed so `go build` stays self-contained.
//
//go:embed all:web/dist
var embeddedWeb embed.FS

func (s *Server) web() http.Handler {
	assets, err := fs.Sub(embeddedWeb, "web/dist")
	if err != nil {
		panic(err)
	}
	files := http.FileServer(http.FS(assets))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.NotFound(w, r)
			return
		}
		// style-src needs 'unsafe-inline': CodeMirror injects its editor styles
		// as runtime <style> elements. Scripts stay restricted to 'self'.
		w.Header().Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; connect-src 'self'; font-src 'self'; form-action 'self'; frame-ancestors 'none'; img-src 'self' data:; object-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		switch {
		case r.URL.Path == "/" || r.URL.Path == "/index.html":
			w.Header().Set("Cache-Control", "no-store")
		case strings.HasPrefix(r.URL.Path, "/assets/"):
			// Vite emits content-hashed asset names.
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		default:
			w.Header().Set("Cache-Control", "public, max-age=3600")
		}
		localSession := s.localSession
		if localSession != "" && loopbackRemote(r.RemoteAddr) {
			if _, err := r.Cookie(controlSessionCookie); err != nil {
				s.setControlCookie(w, localSession, 24*time.Hour)
			}
		}
		files.ServeHTTP(w, r)
	})
}

func loopbackRemote(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = strings.Trim(address, "[]")
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
