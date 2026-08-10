package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/lavindeep/rainforest/internal/workspace"
	"github.com/lavindeep/rainforest/web"
)

const (
	cookieName  = "rainforest_token"
	maxFileSize = 1 << 20
)

type Server struct {
	workspace       string
	version         string
	token           string
	listener        net.Listener
	handler         http.Handler
	planMu          sync.Mutex
	plan            *planRun
	planStarting    bool
	planStarts      sync.WaitGroup
	closed          bool
	lifecycleCtx    context.Context
	lifecycleCancel context.CancelFunc
	closeOnce       sync.Once
	closeErr        error
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
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	s := &Server{
		workspace:       workspace,
		version:         version,
		token:           hex.EncodeToString(buf),
		listener:        listener,
		lifecycleCtx:    lifecycleCtx,
		lifecycleCancel: lifecycleCancel,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", s.health)
	mux.HandleFunc("GET /api/preflight", s.preflight)
	mux.HandleFunc("GET /api/workspace", s.scan)
	mux.HandleFunc("GET /api/file", s.file)
	mux.HandleFunc("POST /api/plan", s.startPlan)
	mux.HandleFunc("GET /api/plan/events", s.planEvents)
	mux.HandleFunc("DELETE /api/plan", s.cancelPlan)
	mux.HandleFunc("GET /api/plan/summary", s.planSummary)
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

func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		s.planMu.Lock()
		s.closed = true
		s.planMu.Unlock()
		if s.lifecycleCancel != nil {
			s.lifecycleCancel()
		}
		s.planStarts.Wait()
		planErr := s.closePlan()
		listenerErr := s.listener.Close()
		s.closeErr = errors.Join(planErr, listenerErr)
	})
	return s.closeErr
}

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

func (s *Server) preflight(w http.ResponseWriter, _ *http.Request) {
	terraform := map[string]any{"found": false, "path": "", "version": ""}
	if path, err := exec.LookPath("terraform"); err == nil {
		terraform["found"] = true
		terraform["path"] = path
		terraform["version"] = s.terraformVersion(path)
	}
	info, err := os.Stat(filepath.Join(s.workspace, ".terraform"))
	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = os.Getenv("AWS_DEFAULT_REGION")
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"terraform":   terraform,
		"initialized": err == nil && info.IsDir(),
		"awsProfile":  os.Getenv("AWS_PROFILE"),
		"awsRegion":   region,
	})
}

func (s *Server) terraformVersion(path string) string {
	ctx := s.lifecycleCtx
	if ctx == nil {
		ctx = context.Background()
	}
	cmd := exec.CommandContext(ctx, path, "version", "-json")
	cmd.Dir = s.workspace
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	var parsed struct {
		Version string `json:"terraform_version"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return ""
	}
	return parsed.Version
}

func (s *Server) scan(w http.ResponseWriter, _ *http.Request) {
	result, err := workspace.Scan(s.workspace)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) file(w http.ResponseWriter, r *http.Request) {
	rel := r.URL.Query().Get("path")
	full, err := s.resolve(rel)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	info, err := os.Stat(full)
	if err != nil || !info.Mode().IsRegular() {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "file not found"})
		return
	}
	if info.Size() > maxFileSize {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "file too large"})
		return
	}
	content, err := os.ReadFile(full)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "file not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"path": rel, "content": string(content)})
}

func (s *Server) resolve(rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) {
		return "", fmt.Errorf("invalid path")
	}
	for _, segment := range strings.Split(rel, "/") {
		if segment == ".." {
			return "", fmt.Errorf("invalid path")
		}
	}
	clean := filepath.Clean(rel)
	if ext := filepath.Ext(clean); ext != ".tf" && ext != ".tfvars" {
		return "", fmt.Errorf("only .tf and .tfvars files can be read")
	}
	if first, _, _ := strings.Cut(clean, string(filepath.Separator)); first == ".terraform" {
		return "", fmt.Errorf("invalid path")
	}
	root, err := filepath.EvalSymlinks(s.workspace)
	if err != nil {
		return "", fmt.Errorf("invalid path")
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(root, clean))
	if err != nil {
		return filepath.Join(root, clean), nil
	}
	if !strings.HasPrefix(resolved, root+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes the workspace")
	}
	return resolved, nil
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
