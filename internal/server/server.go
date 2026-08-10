package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"strings"

	"github.com/lavindeep/rainforest/web"
)

const cookieName = "rainforest_token"

type Server struct {
	workspace string
	version   string
	token     string
	listener  net.Listener
	handler   http.Handler
}

func New(workspace, version string) (*Server, error) {
	dist, err := fs.Sub(web.Dist, "dist")
	if err != nil {
		return nil, err
	}
	return newWithFS(workspace, version, dist)
}

func newWithFS(workspace, version string, dist fs.FS) (*Server, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &Server{
		workspace: workspace,
		version:   version,
		token:     hex.EncodeToString(buf),
		listener:  listener,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", s.health)
	mux.Handle("/", staticHandler(dist))
	s.handler = s.checkOrigin(s.requireToken(mux))
	return s, nil
}

func (s *Server) Token() string { return s.token }

func (s *Server) URL() string {
	return fmt.Sprintf("http://%s/?token=%s", s.listener.Addr().String(), s.token)
}

func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) Serve() error { return http.Serve(s.listener, s.handler) }

func (s *Server) Close() error { return s.listener.Close() }

func (s *Server) checkOrigin(next http.Handler) http.Handler {
	_, port, _ := net.SplitHostPort(s.listener.Addr().String())
	allowed := []string{"http://127.0.0.1:" + port, "http://localhost:" + port}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" &&
			origin != allowed[0] && origin != allowed[1] {
			http.Error(w, "forbidden origin", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) requireToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cookie, err := r.Cookie(cookieName); err == nil && s.validToken(cookie.Value) {
			next.ServeHTTP(w, r)
			return
		}
		if s.validToken(r.URL.Query().Get("token")) {
			http.SetCookie(w, &http.Cookie{
				Name:     cookieName,
				Value:    s.token,
				Path:     "/",
				HttpOnly: true,
				SameSite: http.SameSiteStrictMode,
			})
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

func (s *Server) validToken(got string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) == 1
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"version":   s.version,
		"workspace": s.workspace,
	})
}

func staticHandler(dist fs.FS) http.Handler {
	if _, err := fs.Stat(dist, "index.html"); err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			fmt.Fprintln(w, "frontend not built — run: make build")
		})
	}
	return http.FileServerFS(dist)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
