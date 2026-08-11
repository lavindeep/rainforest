package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

func annotationsRequest(t *testing.T, s *Server, method, body string) *annotationsDocument {
	t.Helper()
	rec := authenticatedRequest(t, s, method, "/api/annotations", []byte(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("%s /api/annotations status = %d, want 200 (body %q)", method, rec.Code, rec.Body.String())
	}
	var doc annotationsDocument
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode annotations: %v", err)
	}
	return &doc
}

func TestAnnotationsMissingGET(t *testing.T) {
	doc := annotationsRequest(t, newTestServerIn(t, t.TempDir()), http.MethodGet, "")
	if doc.Version != 1 || doc.Nodes == nil || doc.Edges == nil || len(doc.Nodes) != 0 || len(doc.Edges) != 0 {
		t.Fatalf("document = %#v, want normalized empty version 1", doc)
	}
}

func TestAnnotationsPUTGETAndRestartPersistence(t *testing.T) {
	workspace := t.TempDir()
	s := newTestServerIn(t, workspace)
	want := `{"version":1,"nodes":{"aws_vpc.main":{"label":"Core","description":"Primary VPC"}},"edges":{"depends:a:b":{"label":"routes to"}}}`
	annotationsRequest(t, s, http.MethodPut, want)

	path := filepath.Join(workspace, annotationsFilename)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(raw), "\n") || !strings.Contains(string(raw), "\n  \"nodes\": {") {
		t.Fatalf("saved JSON is not pretty with final newline: %q", raw)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("saved mode = %v, %v; want 0600", info, err)
	}

	restarted := newTestServerIn(t, workspace)
	got := annotationsRequest(t, restarted, http.MethodGet, "")
	if got.Nodes["aws_vpc.main"].Label != "Core" || got.Edges["depends:a:b"].Label != "routes to" {
		t.Fatalf("persisted document = %#v", got)
	}
}

func TestAnnotationsNormalization(t *testing.T) {
	s := newTestServerIn(t, t.TempDir())
	doc := annotationsRequest(t, s, http.MethodPut, `{"version":1,"nodes":{"empty":{},"also-empty":{"label":"","description":""},"kept":{"label":"name","description":""}},"edges":null}`)
	if len(doc.Nodes) != 1 || doc.Nodes["kept"].Label != "name" || doc.Nodes["kept"].Description != "" {
		t.Fatalf("nodes = %#v, want only kept label", doc.Nodes)
	}
	if doc.Edges == nil || len(doc.Edges) != 0 {
		t.Fatalf("edges = %#v, want non-nil empty map", doc.Edges)
	}
}

func TestAnnotationsRejectInvalidDocuments(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"malformed", `{"version":`},
		{"unknown document field", `{"version":1,"nodes":{},"edges":{},"extra":true}`},
		{"unknown annotation field", `{"version":1,"nodes":{"x":{"label":"ok","extra":true}},"edges":{}}`},
		{"unsupported version", `{"version":2,"nodes":{},"edges":{}}`},
		{"trailing JSON", `{"version":1,"nodes":{},"edges":{}} {}`},
		{"empty key", `{"version":1,"nodes":{"":{"label":"x"}},"edges":{}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := authenticatedRequest(t, newTestServerIn(t, t.TempDir()), http.MethodPut, "/api/annotations", []byte(tt.body))
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Header().Get("Content-Type"), "application/json") || !strings.Contains(rec.Body.String(), "error") {
				t.Fatalf("status/body = %d %q, want clear JSON 400", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestAnnotationsUnicodeBounds(t *testing.T) {
	s := newTestServerIn(t, t.TempDir())
	valid := annotationsDocument{Version: 1, Nodes: map[string]annotation{
		strings.Repeat("界", 1024): {Label: strings.Repeat("🙂", 80), Description: strings.Repeat("界", 4000)},
	}, Edges: map[string]annotation{}}
	raw, _ := json.Marshal(valid)
	rec := authenticatedRequest(t, s, http.MethodPut, "/api/annotations", raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid Unicode bounds status = %d (body %q)", rec.Code, rec.Body.String())
	}

	invalid := []annotationsDocument{
		{Version: 1, Nodes: map[string]annotation{strings.Repeat("界", 1025): {Label: "x"}}, Edges: map[string]annotation{}},
		{Version: 1, Nodes: map[string]annotation{"x": {Label: strings.Repeat("🙂", 81)}}, Edges: map[string]annotation{}},
		{Version: 1, Nodes: map[string]annotation{"x": {Description: strings.Repeat("界", 4001)}}, Edges: map[string]annotation{}},
	}
	for i, doc := range invalid {
		raw, _ := json.Marshal(doc)
		rec := authenticatedRequest(t, s, http.MethodPut, "/api/annotations", raw)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("invalid Unicode case %d status = %d, want 400", i, rec.Code)
		}
	}
	if utf8.RuneCountInString(strings.Repeat("🙂", 80)) != 80 {
		t.Fatal("test setup did not use multibyte Unicode")
	}
}

func TestAnnotationsBodyLimit(t *testing.T) {
	s := newTestServerIn(t, t.TempDir())
	body := `{"version":1,"nodes":{},"edges":{},"` + strings.Repeat("x", maxAnnotationsSize) + `":0}`
	rec := authenticatedRequest(t, s, http.MethodPut, "/api/annotations", []byte(body))
	if rec.Code != http.StatusRequestEntityTooLarge && rec.Code != http.StatusBadRequest {
		t.Fatalf("oversize body status = %d, want 413 or 400 (body %q)", rec.Code, rec.Body.String())
	}
}

func TestAnnotationsRejectPrettyRepresentationOverLimit(t *testing.T) {
	doc := annotationsDocument{Version: 1, Nodes: make(map[string]annotation), Edges: map[string]annotation{}}
	for i := 0; i < 15_000; i++ {
		doc.Nodes[fmt.Sprintf("node-%05d", i)] = annotation{Label: strings.Repeat("x", 40)}
	}
	compact, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(compact) > maxAnnotationsSize {
		t.Fatalf("test compact representation = %d bytes, want <= %d", len(compact), maxAnnotationsSize)
	}
	if pretty, err := encodeAnnotations(doc); !errors.Is(err, errAnnotationsTooLarge) || pretty != nil {
		t.Fatalf("test pretty representation unexpectedly fits: len=%d err=%v", len(pretty), err)
	}

	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing=%t", existing), func(t *testing.T) {
			workspace := t.TempDir()
			s := newTestServerIn(t, workspace)
			path := filepath.Join(workspace, annotationsFilename)
			var before []byte
			if existing {
				annotationsRequest(t, s, http.MethodPut, `{"version":1,"nodes":{"x":{"label":"prior"}},"edges":{}}`)
				before, err = os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
			}

			rec := authenticatedRequest(t, s, http.MethodPut, "/api/annotations", compact)
			if rec.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("status = %d, want 413 (body %q)", rec.Code, rec.Body.String())
			}
			after, err := os.ReadFile(path)
			if existing {
				if err != nil || string(after) != string(before) {
					t.Fatalf("oversize save replaced prior file: err=%v", err)
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("oversize save created file: err=%v", err)
			}
		})
	}
}

func TestAnnotationsRejectSymlinkAndNonRegularFile(t *testing.T) {
	for _, kind := range []string{"symlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			workspace := t.TempDir()
			path := filepath.Join(workspace, annotationsFilename)
			if kind == "symlink" {
				target := filepath.Join(t.TempDir(), "outside.json")
				if err := os.WriteFile(target, []byte(`{"version":1,"nodes":{},"edges":{}}`), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
			s := newTestServerIn(t, workspace)
			for _, method := range []string{http.MethodGet, http.MethodPut} {
				rec := authenticatedRequest(t, s, method, "/api/annotations", []byte(`{"version":1,"nodes":{},"edges":{}}`))
				if rec.Code != http.StatusBadRequest {
					t.Errorf("%s %s status = %d, want 400", kind, method, rec.Code)
				}
			}
		})
	}
}

func TestAnnotationsRejectMalformedAndOversizeFilesWithoutLeakingContents(t *testing.T) {
	for _, tt := range []struct {
		name string
		body []byte
	}{
		{"malformed", []byte(`{"secret":"do-not-return"`)},
		{"oversize", []byte(strings.Repeat("s", maxAnnotationsSize+1))},
	} {
		t.Run(tt.name, func(t *testing.T) {
			workspace := t.TempDir()
			if err := os.WriteFile(filepath.Join(workspace, annotationsFilename), tt.body, 0o600); err != nil {
				t.Fatal(err)
			}
			rec := authenticatedRequest(t, newTestServerIn(t, workspace), http.MethodGet, "/api/annotations", nil)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "error") {
				t.Fatalf("status/body = %d %q, want JSON 400", rec.Code, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "do-not-return") {
				t.Fatalf("response leaked file contents: %q", rec.Body.String())
			}
		})
	}
}

func TestReadAnnotationsFileRejectsDescriptorMismatch(t *testing.T) {
	dir := t.TempDir()
	beforePath := filepath.Join(dir, "before")
	openedPath := filepath.Join(dir, "opened")
	if err := os.WriteFile(beforePath, []byte(`{"version":1,"nodes":{},"edges":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(openedPath, []byte("external-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(beforePath)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := readAnnotationsFile(openedPath, before)
	if err == nil || raw != nil {
		t.Fatalf("mismatched descriptor returned raw=%q err=%v", raw, err)
	}
}

func TestAnnotationsConcurrentPUTsRemainValid(t *testing.T) {
	workspace := t.TempDir()
	s := newTestServerIn(t, workspace)
	const writers = 24
	var wg sync.WaitGroup
	errs := make(chan string, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := `{"version":1,"nodes":{"x":{"label":"` + strings.Repeat("x", 40) + `"}},"edges":{}}`
			rec := authenticatedRequest(t, s, http.MethodPut, "/api/annotations", []byte(body))
			if rec.Code != http.StatusOK {
				errs <- rec.Body.String()
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent PUT failed: %s", err)
	}
	raw, err := os.ReadFile(filepath.Join(workspace, annotationsFilename))
	if err != nil {
		t.Fatal(err)
	}
	var doc annotationsDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("final file is invalid JSON: %v", err)
	}
}

func TestAnnotationsValidationFailurePreservesPriorFile(t *testing.T) {
	workspace := t.TempDir()
	s := newTestServerIn(t, workspace)
	annotationsRequest(t, s, http.MethodPut, `{"version":1,"nodes":{"x":{"label":"prior"}},"edges":{}}`)
	path := filepath.Join(workspace, annotationsFilename)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rec := authenticatedRequest(t, s, http.MethodPut, "/api/annotations", []byte(`{"version":2}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid PUT status = %d, want 400", rec.Code)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("failed PUT changed prior file\nbefore: %q\nafter: %q", before, after)
	}
}

func TestAnnotationsWriteFailurePreservesPriorFileAndCleansTemp(t *testing.T) {
	workspace := t.TempDir()
	s := newTestServerIn(t, workspace)
	annotationsRequest(t, s, http.MethodPut, `{"version":1,"nodes":{"x":{"label":"prior"}},"edges":{}}`)
	path := filepath.Join(workspace, annotationsFilename)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(workspace, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(workspace, 0o700) })

	rec := authenticatedRequest(t, s, http.MethodPut, "/api/annotations", []byte(`{"version":1,"nodes":{"x":{"label":"replacement"}},"edges":{}}`))
	if err := os.Chmod(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), workspace) {
		t.Fatalf("write failure status/body = %d %q, want generic 500", rec.Code, rec.Body.String())
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatalf("write failure changed prior file: err=%v", err)
	}
	temps, err := filepath.Glob(filepath.Join(workspace, ".rainforest.annotations-*"))
	if err != nil || len(temps) != 0 {
		t.Fatalf("temporary debris = %v, err=%v", temps, err)
	}
}
