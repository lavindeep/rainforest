package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"unicode/utf8"
)

const (
	annotationsFilename = "rainforest.annotations.json"
	maxAnnotationsSize  = 1 << 20
	maxAnnotationKey    = 1024
	maxLabelLength      = 80
	maxDescription      = 4000
)

var (
	errAnnotationsTooLarge   = errors.New("annotations data is too large")
	errAnnotationsUnsafeFile = errors.New("annotations file must be a regular file")
)

type annotation struct {
	Label       string `json:"label,omitempty"`
	Description string `json:"description,omitempty"`
}

type annotationsDocument struct {
	Version int                   `json:"version"`
	Nodes   map[string]annotation `json:"nodes"`
	Edges   map[string]annotation `json:"edges"`
}

func emptyAnnotations() annotationsDocument {
	return annotationsDocument{
		Version: 1,
		Nodes:   make(map[string]annotation),
		Edges:   make(map[string]annotation),
	}
}

func (s *Server) annotations(w http.ResponseWriter, r *http.Request) {
	s.annotationsMu.Lock()
	defer s.annotationsMu.Unlock()

	switch r.Method {
	case http.MethodGet:
		s.getAnnotations(w)
	case http.MethodPut:
		s.putAnnotations(w, r)
	default:
		w.Header().Set("Allow", "GET, PUT")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

func (s *Server) getAnnotations(w http.ResponseWriter) {
	path := filepath.Join(s.workspace, annotationsFilename)
	before, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		writeJSON(w, http.StatusOK, emptyAnnotations())
		return
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "cannot read annotations file"})
		return
	}
	if !before.Mode().IsRegular() {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "annotations file must be a regular file"})
		return
	}
	if before.Size() > maxAnnotationsSize {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "annotations file is too large"})
		return
	}
	raw, err := readAnnotationsFile(path, before)
	if err != nil {
		if errors.Is(err, errAnnotationsTooLarge) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "annotations file is too large"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "cannot read annotations file"})
		return
	}
	doc, err := decodeAnnotations(raw)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid annotations file"})
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

func (s *Server) putAnnotations(w http.ResponseWriter, r *http.Request) {
	if r.ContentLength > maxAnnotationsSize {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body is too large"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxAnnotationsSize)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body is too large"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "cannot read request body"})
		return
	}
	doc, err := decodeAnnotations(raw)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	encoded, err := encodeAnnotations(doc)
	if errors.Is(err, errAnnotationsTooLarge) {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "annotations data is too large"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "cannot save annotations"})
		return
	}
	if err := s.writeAnnotations(encoded); err != nil {
		if errors.Is(err, errAnnotationsUnsafeFile) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": errAnnotationsUnsafeFile.Error()})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "cannot save annotations"})
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

func readAnnotationsFile(path string, before os.FileInfo) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return nil, errors.New("annotations file changed while opening")
	}
	if opened.Size() > maxAnnotationsSize {
		return nil, errAnnotationsTooLarge
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxAnnotationsSize+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxAnnotationsSize {
		return nil, errAnnotationsTooLarge
	}
	return raw, nil
}

func decodeAnnotations(raw []byte) (annotationsDocument, error) {
	var doc annotationsDocument
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&doc); err != nil {
		return annotationsDocument{}, fmt.Errorf("invalid annotations JSON")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return annotationsDocument{}, fmt.Errorf("annotations JSON must contain one document")
	}
	if doc.Version != 1 {
		return annotationsDocument{}, fmt.Errorf("annotations version must be 1")
	}
	if doc.Nodes == nil {
		doc.Nodes = make(map[string]annotation)
	}
	if doc.Edges == nil {
		doc.Edges = make(map[string]annotation)
	}
	if err := normalizeAnnotations(doc.Nodes, "node"); err != nil {
		return annotationsDocument{}, err
	}
	if err := normalizeAnnotations(doc.Edges, "edge"); err != nil {
		return annotationsDocument{}, err
	}
	return doc, nil
}

func normalizeAnnotations(entries map[string]annotation, kind string) error {
	for key, value := range entries {
		keyLength := utf8.RuneCountInString(key)
		if keyLength == 0 || keyLength > maxAnnotationKey {
			return fmt.Errorf("%s key must be 1 to %d Unicode code points", kind, maxAnnotationKey)
		}
		if utf8.RuneCountInString(value.Label) > maxLabelLength {
			return fmt.Errorf("%s label must be at most %d Unicode code points", kind, maxLabelLength)
		}
		if utf8.RuneCountInString(value.Description) > maxDescription {
			return fmt.Errorf("%s description must be at most %d Unicode code points", kind, maxDescription)
		}
		if value.Label == "" && value.Description == "" {
			delete(entries, key)
		}
	}
	return nil
}

func encodeAnnotations(doc annotationsDocument) ([]byte, error) {
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	raw = append(raw, '\n')
	if len(raw) > maxAnnotationsSize {
		return nil, errAnnotationsTooLarge
	}
	return raw, nil
}

func (s *Server) writeAnnotations(raw []byte) error {
	path := filepath.Join(s.workspace, annotationsFilename)
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return errAnnotationsUnsafeFile
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("cannot inspect annotations file")
	}

	temp, err := os.CreateTemp(s.workspace, ".rainforest.annotations-*")
	if err != nil {
		return fmt.Errorf("cannot create annotations file")
	}
	tempPath := temp.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tempPath)
		}
	}()

	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return fmt.Errorf("cannot secure annotations file")
	}
	if _, err := temp.Write(raw); err != nil {
		_ = temp.Close()
		return fmt.Errorf("cannot write annotations file")
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("cannot close annotations file")
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("cannot replace annotations file")
	}
	removeTemp = false
	return nil
}
