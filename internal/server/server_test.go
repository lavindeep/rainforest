package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/lavindeep/rainforest/internal/workspace"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	return newTestServerIn(t, "/tmp/workspace")
}

func newTestServerIn(t *testing.T, workspace string) *Server {
	t.Helper()
	s, err := New(workspace, "test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func get(t *testing.T, s *Server, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: s.Token()})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder, into any) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), into); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
}

type preflightBody struct {
	Terraform struct {
		Found   bool   `json:"found"`
		Path    string `json:"path"`
		Version string `json:"version"`
	} `json:"terraform"`
	Initialized bool   `json:"initialized"`
	AWSProfile  string `json:"awsProfile"`
	AWSRegion   string `json:"awsRegion"`
}

func stubTerraform(t *testing.T, script string) string {
	t.Helper()
	bin := t.TempDir()
	path := filepath.Join(bin, "terraform")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Setenv("PATH", bin)
	return path
}

func TestPreflightTerraformFound(t *testing.T) {
	want := stubTerraform(t, `echo '{"terraform_version":"1.14.8"}'`)
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".terraform"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Setenv("AWS_PROFILE", "sandbox")
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "eu-west-1")

	var body preflightBody
	decode(t, get(t, newTestServerIn(t, dir), "/api/preflight"), &body)

	if !body.Terraform.Found || body.Terraform.Path != want || body.Terraform.Version != "1.14.8" {
		t.Errorf("terraform = %+v, want found at %s v1.14.8", body.Terraform, want)
	}
	if !body.Initialized {
		t.Error("initialized = false, want true")
	}
	if body.AWSProfile != "sandbox" || body.AWSRegion != "eu-west-1" {
		t.Errorf("aws = %q/%q, want sandbox/eu-west-1", body.AWSProfile, body.AWSRegion)
	}
}

func TestPreflightRegionPrefersAWSRegion(t *testing.T) {
	stubTerraform(t, `echo '{"terraform_version":"1.14.8"}'`)
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_DEFAULT_REGION", "eu-west-1")

	var body preflightBody
	decode(t, get(t, newTestServerIn(t, t.TempDir()), "/api/preflight"), &body)

	if body.AWSRegion != "us-east-1" {
		t.Errorf("awsRegion = %q, want us-east-1", body.AWSRegion)
	}
}

func TestPreflightTerraformUnparsableVersion(t *testing.T) {
	stubTerraform(t, `echo 'not json'`)

	var body preflightBody
	decode(t, get(t, newTestServerIn(t, t.TempDir()), "/api/preflight"), &body)

	if !body.Terraform.Found || body.Terraform.Version != "" {
		t.Errorf("terraform = %+v, want found with empty version", body.Terraform)
	}
}

func TestPreflightTerraformMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")

	var body preflightBody
	decode(t, get(t, newTestServerIn(t, t.TempDir()), "/api/preflight"), &body)

	if body.Terraform.Found || body.Terraform.Path != "" || body.Terraform.Version != "" {
		t.Errorf("terraform = %+v, want not found", body.Terraform)
	}
	if body.Initialized {
		t.Error("initialized = true, want false (no .terraform dir)")
	}
	if body.AWSProfile != "" || body.AWSRegion != "" {
		t.Errorf("aws = %q/%q, want empty", body.AWSProfile, body.AWSRegion)
	}
}

func TestWorkspaceScanEndpoint(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "main.tf", "resource \"aws_s3_bucket\" \"b\" {}\n")

	var body workspace.Result
	decode(t, get(t, newTestServerIn(t, dir), "/api/workspace"), &body)

	if len(body.Files) != 1 || body.Files[0] != "main.tf" {
		t.Fatalf("files = %v, want [main.tf]", body.Files)
	}
	if len(body.Blocks) != 1 || body.Blocks[0].Address != "aws_s3_bucket.b" {
		t.Fatalf("blocks = %+v, want aws_s3_bucket.b", body.Blocks)
	}
}

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func TestFileReadsWorkspaceFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "main.tf", "resource \"aws_s3_bucket\" \"b\" {}\n")

	var body struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	decode(t, get(t, newTestServerIn(t, dir), "/api/file?path=main.tf"), &body)

	if body.Path != "main.tf" || body.Content != "resource \"aws_s3_bucket\" \"b\" {}\n" {
		t.Errorf("body = %+v, want main.tf with its content", body)
	}
}

func TestFileAllowsDoubleDotInFilename(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a..b.tf", "resource \"aws_s3_bucket\" \"b\" {}\n")

	var body struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	decode(t, get(t, newTestServerIn(t, dir), "/api/file?path=a..b.tf"), &body)

	if body.Path != "a..b.tf" || body.Content != "resource \"aws_s3_bucket\" \"b\" {}\n" {
		t.Errorf("body = %+v, want a..b.tf with its content", body)
	}
}

func TestFileStillRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "main.tf", "")
	s := newTestServerIn(t, dir)

	for _, target := range []string{
		"/api/file?path=" + url.QueryEscape("../x.tf"),
		"/api/file?path=" + url.QueryEscape("a/../../x.tf"),
	} {
		rec := get(t, s, target)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", target, rec.Code)
		}
	}
}

func TestFileUnauthorized(t *testing.T) {
	s := newTestServerIn(t, t.TempDir())
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/file?path=main.tf", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestFileRejectsUnsafePaths(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "main.tf", "")
	writeFile(t, dir, "terraform.tfstate", `{"secret":"aws-key"}`)
	if err := os.MkdirAll(filepath.Join(dir, ".terraform", "modules"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(dir, ".terraform", "modules"), "hidden.tf", "")

	outside := t.TempDir()
	writeFile(t, outside, "secret.tf", "outside the workspace")
	if err := os.Symlink(filepath.Join(outside, "secret.tf"), filepath.Join(dir, "link.tf")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	big := strings.Repeat("x", 1<<20+1)
	writeFile(t, dir, "big.tf", big)

	s := newTestServerIn(t, dir)
	for _, target := range []string{
		"/api/file?path=" + url.QueryEscape("../"+filepath.Base(dir)+"/main.tf"),
		"/api/file?path=" + url.QueryEscape(filepath.Join(dir, "main.tf")),
		"/api/file?path=terraform.tfstate",
		"/api/file?path=" + url.QueryEscape(".terraform/modules/hidden.tf"),
		"/api/file?path=link.tf",
		"/api/file?path=big.tf",
		"/api/file?path=missing.tf",
		"/api/file?path=",
	} {
		rec := get(t, s, target)
		if rec.Code != http.StatusBadRequest && rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 400 or 404 (body %q)", target, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "aws-key") || strings.Contains(rec.Body.String(), "outside the workspace") {
			t.Errorf("%s: leaked file content: %q", target, rec.Body.String())
		}
	}
}

func TestHealthWithoutTokenIsUnauthorized(t *testing.T) {
	s := newTestServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q, want JSON", ct)
	}
}

func TestHealthWithQueryTokenSetsCookie(t *testing.T) {
	s := newTestServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/health?token="+s.Token(), nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var body struct {
		OK        bool   `json:"ok"`
		Version   string `json:"version"`
		Workspace string `json:"workspace"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.OK || body.Version != "test" || body.Workspace != "/tmp/workspace" {
		t.Errorf("body = %+v, want ok/test//tmp/workspace", body)
	}

	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == cookieName {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no rainforest_token cookie set")
	}
	if cookie.Value != s.Token() || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Errorf("cookie = %+v, want token value, HttpOnly, SameSite=Strict", cookie)
	}
}

func TestHealthWithCookie(t *testing.T) {
	s := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: s.Token()})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
}

func TestForeignOriginIsForbidden(t *testing.T) {
	s := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/health?token="+s.Token(), nil)
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestIndexUnauthorizedAndFrontendNotBuilt(t *testing.T) {
	s, err := newWithFS("/tmp/workspace", "test", fstest.MapFS{})
	if err != nil {
		t.Fatalf("newWithFS: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}

	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/?token="+s.Token(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); !strings.Contains(got, "frontend not built — run: make build") {
		t.Errorf("body = %q, want frontend-not-built notice", got)
	}
}
