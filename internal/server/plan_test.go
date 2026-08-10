package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const planFixture = `{
  "resource_changes": [
    {
      "address": "aws_s3_bucket.created",
      "mode": "managed",
      "type": "aws_s3_bucket",
      "name": "created",
      "change": {
        "actions": ["create"],
        "after": {"bucket": "CANARY_DO_NOT_LEAK"}
      }
    },
    {
      "address": "aws_instance.replaced",
      "mode": "managed",
      "type": "aws_instance",
      "name": "replaced",
      "change": {
        "actions": ["delete", "create"],
        "after": {"user_data": "CANARY_DO_NOT_LEAK"}
      }
    },
    {
      "address": "aws_vpc.unchanged",
      "mode": "managed",
      "type": "aws_vpc",
      "name": "unchanged",
      "change": {"actions": ["no-op"]}
    },
    {
      "address": "data.aws_caller_identity.current",
      "mode": "data",
      "type": "aws_caller_identity",
      "name": "current",
      "change": {"actions": ["read"]}
    }
  ]
}`

type planStartBody struct {
	RunID string   `json:"runId"`
	Argv  []string `json:"argv"`
	CWD   string   `json:"cwd"`
}

type planCountsBody struct {
	Add     int `json:"add"`
	Change  int `json:"change"`
	Destroy int `json:"destroy"`
}

type planChangeBody struct {
	Address string   `json:"address"`
	Type    string   `json:"type"`
	Name    string   `json:"name"`
	Actions []string `json:"actions"`
}

type planSummaryBody struct {
	RunID    string `json:"runId"`
	State    string `json:"state"`
	ExitCode int    `json:"exitCode"`
	Run      struct {
		Argv             []string  `json:"argv"`
		CWD              string    `json:"cwd"`
		TerraformVersion string    `json:"terraformVersion"`
		StartedAt        time.Time `json:"startedAt"`
		FinishedAt       time.Time `json:"finishedAt"`
	} `json:"run"`
	Counts    planCountsBody   `json:"counts"`
	NoChanges bool             `json:"noChanges"`
	ShowError string           `json:"showError"`
	Changes   []planChangeBody `json:"changes"`
	Err       string           `json:"err"`
}

type planDoneBody struct {
	State  string          `json:"state"`
	Counts *planCountsBody `json:"counts"`
	Err    string          `json:"err"`
}

type sseEvent struct {
	Name string
	Data []byte
}

type planTestListener struct{}

func (planTestListener) Accept() (net.Conn, error) { return nil, errors.New("not listening") }
func (planTestListener) Close() error              { return nil }
func (planTestListener) Addr() net.Addr            { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }

type blockingCompletionRecorder struct {
	*httptest.ResponseRecorder
	writeStarted chan struct{}
	allowWrite   chan struct{}
	postDone     chan struct{}
	startOnce    sync.Once
	doneOnce     sync.Once
}

func (w *blockingCompletionRecorder) Write(p []byte) (int, error) {
	w.startOnce.Do(func() { close(w.writeStarted) })
	<-w.allowWrite
	n, err := w.ResponseRecorder.Write(p)
	w.doneOnce.Do(func() { close(w.postDone) })
	return n, err
}

func newPlanTestServerIn(t *testing.T, workspace string) *Server {
	t.Helper()
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	s := &Server{
		workspace:       workspace,
		version:         "test",
		token:           "test-token",
		listener:        planTestListener{},
		lifecycleCtx:    lifecycleCtx,
		lifecycleCancel: lifecycleCancel,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/plan", s.startPlan)
	mux.HandleFunc("GET /api/plan/events", s.planEvents)
	mux.HandleFunc("DELETE /api/plan", s.cancelPlan)
	mux.HandleFunc("GET /api/plan/summary", s.planSummary)
	s.handler = s.checkOrigin(s.requireToken(mux))
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func planScript(t *testing.T, planBody, showBody string) string {
	t.Helper()
	return stubTerraform(t, `case "$1" in
version)
  echo '{"terraform_version":"1.14.8"}'
  ;;
show)
`+showBody+`
  ;;
plan)
`+planBody+`
  ;;
esac`)
}

func initializedWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".terraform"), 0o755); err != nil {
		t.Fatalf("mkdir .terraform: %v", err)
	}
	return dir
}

func authenticatedRequest(t *testing.T, s *Server, method, target string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	req.AddCookie(&http.Cookie{Name: cookieName, Value: s.Token()})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func startPlan(t *testing.T, s *Server) planStartBody {
	t.Helper()
	rec := authenticatedRequest(t, s, http.MethodPost, "/api/plan", nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST /api/plan status = %d, want 202 (body %q)", rec.Code, rec.Body.String())
	}
	var body planStartBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode plan start: %v", err)
	}
	if len(body.RunID) != 8 {
		t.Fatalf("runId = %q, want 8 hex characters", body.RunID)
	}
	return body
}

func waitForSummary(t *testing.T, s *Server) ([]byte, planSummaryBody) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rec := authenticatedRequest(t, s, http.MethodGet, "/api/plan/summary", nil)
		if rec.Code == http.StatusOK {
			var body planSummaryBody
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode plan summary: %v", err)
			}
			return rec.Body.Bytes(), body
		}
		if rec.Code != http.StatusConflict {
			t.Fatalf("GET /api/plan/summary status = %d, want 200 or 409 (body %q)", rec.Code, rec.Body.String())
		}
		if time.Now().After(deadline) {
			t.Fatalf("plan did not finish (body %q)", rec.Body.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func parseSSE(t *testing.T, body []byte) []sseEvent {
	t.Helper()
	var events []sseEvent
	var event sseEvent
	scanner := bufio.NewScanner(bytes.NewReader(body))
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event.Name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			event.Data = append(event.Data, strings.TrimPrefix(line, "data: ")...)
		case line == "":
			if event.Name != "" {
				events = append(events, event)
				event = sseEvent{}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan SSE: %v", err)
	}
	if event.Name != "" {
		events = append(events, event)
	}
	return events
}

func planDoneEvent(t *testing.T, s *Server) planDoneBody {
	t.Helper()
	rec := authenticatedRequest(t, s, http.MethodGet, "/api/plan/events", nil)
	events := parseSSE(t, rec.Body.Bytes())
	if len(events) == 0 || events[len(events)-1].Name != "done" {
		t.Fatalf("events = %+v, want terminal done event", events)
	}
	var done planDoneBody
	if err := json.Unmarshal(events[len(events)-1].Data, &done); err != nil {
		t.Fatalf("decode done event: %v", err)
	}
	return done
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("file %s was not created", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPlanHappyPathSanitizesSummaryAndReplaysEvents(t *testing.T) {
	planScript(t, `for arg in "$@"; do
  case "$arg" in -out=*) planfile=${arg#-out=}; : > "$planfile" ;; esac
done
echo stdout-one
echo stdout-two
echo stdout-three
echo stderr-one >&2
exit 0`, `echo '`+planFixture+`'`)
	workspace := initializedWorkspace(t)
	s := newPlanTestServerIn(t, workspace)

	started := startPlan(t, s)
	if started.CWD != workspace {
		t.Errorf("cwd = %q, want %q", started.CWD, workspace)
	}
	if len(started.Argv) != 5 || started.Argv[0] != "terraform" || started.Argv[1] != "plan" ||
		started.Argv[2] != "-input=false" || started.Argv[3] != "-no-color" || !strings.HasPrefix(started.Argv[4], "-out=") {
		t.Fatalf("argv = %v, want terraform plan flags and planfile", started.Argv)
	}
	if !strings.HasPrefix(started.Argv[len(started.Argv)-1], "-out=") {
		t.Fatalf("argv = %v, want final -out flag", started.Argv)
	}

	rawSummary, summary := waitForSummary(t, s)
	if summary.RunID != started.RunID || summary.State != "succeeded" || summary.ExitCode != 0 {
		t.Errorf("summary identity/state = %+v, want %s/succeeded/0", summary, started.RunID)
	}
	if summary.Run.CWD != workspace || summary.Run.TerraformVersion != "1.14.8" || len(summary.Run.Argv) != 5 {
		t.Errorf("run metadata = %+v, want workspace, terraform 1.14.8, and argv", summary.Run)
	}
	if summary.Run.StartedAt.IsZero() || summary.Run.FinishedAt.IsZero() || summary.Run.FinishedAt.Before(summary.Run.StartedAt) {
		t.Errorf("timestamps = %s to %s, want ordered non-zero values", summary.Run.StartedAt, summary.Run.FinishedAt)
	}
	if summary.Counts != (planCountsBody{Add: 2, Destroy: 1}) {
		t.Errorf("counts = %+v, want add=2 change=0 destroy=1", summary.Counts)
	}
	if summary.NoChanges || summary.ShowError != "" || summary.Err != "" {
		t.Errorf("noChanges/showError/err = %v/%q/%q, want false/empty/empty", summary.NoChanges, summary.ShowError, summary.Err)
	}
	if len(summary.Changes) != 2 {
		t.Fatalf("changes = %+v, want two managed changes", summary.Changes)
	}
	if summary.Changes[0].Address != "aws_s3_bucket.created" || summary.Changes[0].Type != "aws_s3_bucket" ||
		summary.Changes[0].Name != "created" || strings.Join(summary.Changes[0].Actions, ",") != "create" {
		t.Errorf("first change = %+v, want managed create", summary.Changes[0])
	}
	if summary.Changes[1].Address != "aws_instance.replaced" || strings.Join(summary.Changes[1].Actions, ",") != "delete,create" {
		t.Errorf("second change = %+v, want delete/create actions verbatim", summary.Changes[1])
	}
	if bytes.Contains(rawSummary, []byte("CANARY_DO_NOT_LEAK")) || bytes.Contains(rawSummary, []byte("aws_vpc.unchanged")) ||
		bytes.Contains(rawSummary, []byte("data.aws_caller_identity.current")) {
		t.Fatalf("summary leaked filtered or attribute data: %s", rawSummary)
	}

	eventsRec := authenticatedRequest(t, s, http.MethodGet, "/api/plan/events", nil)
	if eventsRec.Code != http.StatusOK {
		t.Fatalf("events status = %d, want 200 (body %q)", eventsRec.Code, eventsRec.Body.String())
	}
	if got := eventsRec.Header().Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
	if eventsRec.Header().Get("Cache-Control") != "no-cache" || eventsRec.Header().Get("Connection") != "keep-alive" {
		t.Errorf("SSE headers = %+v, want no-cache and keep-alive", eventsRec.Header())
	}
	if bytes.Contains(eventsRec.Body.Bytes(), []byte("CANARY_DO_NOT_LEAK")) {
		t.Fatalf("SSE leaked plan attribute value: %s", eventsRec.Body.Bytes())
	}
	events := parseSSE(t, eventsRec.Body.Bytes())
	if len(events) != 5 {
		t.Fatalf("events = %+v, want four lines and done", events)
	}
	stdout := []string{}
	stderr := []string{}
	lastSeq := 0
	for _, event := range events[:len(events)-1] {
		if event.Name != "line" {
			t.Fatalf("event = %+v, want line", event)
		}
		var line struct {
			RunID  string `json:"runId"`
			Seq    int    `json:"seq"`
			Stream string `json:"stream"`
			Text   string `json:"text"`
		}
		if err := json.Unmarshal(event.Data, &line); err != nil {
			t.Fatalf("decode line event: %v", err)
		}
		if line.RunID != started.RunID || line.Seq <= lastSeq {
			t.Errorf("line identity/seq = %+v after %d", line, lastSeq)
		}
		lastSeq = line.Seq
		switch line.Stream {
		case "stdout":
			stdout = append(stdout, line.Text)
		case "stderr":
			stderr = append(stderr, line.Text)
		default:
			t.Errorf("unexpected stream %q", line.Stream)
		}
	}
	if strings.Join(stdout, ",") != "stdout-one,stdout-two,stdout-three" || strings.Join(stderr, ",") != "stderr-one" {
		t.Errorf("stdout/stderr = %v/%v, want three stdout and one stderr line", stdout, stderr)
	}
	if events[len(events)-1].Name != "done" {
		t.Fatalf("last event = %+v, want done", events[len(events)-1])
	}
	var done struct {
		RunID    string          `json:"runId"`
		State    string          `json:"state"`
		ExitCode int             `json:"exitCode"`
		Counts   *planCountsBody `json:"counts"`
		Err      string          `json:"err"`
	}
	if err := json.Unmarshal(events[len(events)-1].Data, &done); err != nil {
		t.Fatalf("decode done event: %v", err)
	}
	if done.RunID != started.RunID || done.State != "succeeded" || done.ExitCode != 0 || done.Counts == nil ||
		*done.Counts != (planCountsBody{Add: 2, Destroy: 1}) || done.Err != "" {
		t.Errorf("done = %+v, want succeeded counts", done)
	}
}

func TestPlanSingleFlight(t *testing.T) {
	planScript(t, `echo ready
/bin/sleep 2`, `echo '{"resource_changes":[]}'`)
	s := newPlanTestServerIn(t, initializedWorkspace(t))
	started := startPlan(t, s)

	rec := authenticatedRequest(t, s, http.MethodPost, "/api/plan", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("second POST status = %d, want 409 (body %q)", rec.Code, rec.Body.String())
	}
	var body struct {
		Error string `json:"error"`
		RunID string `json:"runId"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode conflict: %v", err)
	}
	if body.Error != "a plan is already running" || body.RunID != started.RunID {
		t.Errorf("conflict = %+v, want first runId", body)
	}
}

func TestPlanConcurrentStartsProbeVersionOnce(t *testing.T) {
	workspace := initializedWorkspace(t)
	stubTerraform(t, `case "$1" in
version)
  : > "$PWD/version-$$"
  /bin/sleep 0.2
  echo '{"terraform_version":"1.14.8"}'
  ;;
plan)
  while :; do /bin/sleep 1; done
  ;;
show)
  echo '{"resource_changes":[]}'
  ;;
esac`)
	s := newPlanTestServerIn(t, workspace)
	start := make(chan struct{})
	responses := make(chan *httptest.ResponseRecorder, 2)
	for range 2 {
		go func() {
			<-start
			responses <- authenticatedRequest(t, s, http.MethodPost, "/api/plan", nil)
		}()
	}
	close(start)
	accepted, conflicts := 0, 0
	for range 2 {
		rec := <-responses
		switch rec.Code {
		case http.StatusAccepted:
			accepted++
		case http.StatusConflict:
			conflicts++
			var conflict map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &conflict); err != nil {
				t.Fatalf("decode starting conflict: %v", err)
			}
			if conflict["error"] != "a plan is starting" || conflict["runId"] != nil {
				t.Errorf("starting conflict = %v, want error without runId", conflict)
			}
		}
	}
	if accepted != 1 || conflicts != 1 {
		t.Fatalf("POST statuses = %d accepted/%d conflicts, want 1/1", accepted, conflicts)
	}
	probes, err := filepath.Glob(filepath.Join(workspace, "version-*"))
	if err != nil {
		t.Fatalf("glob version probes: %v", err)
	}
	if len(probes) != 1 {
		t.Fatalf("terraform version probes = %d, want 1", len(probes))
	}
}

func TestCloseCancelsHangingVersionProbeBeforePlanStarts(t *testing.T) {
	workspace := initializedWorkspace(t)
	stubTerraform(t, `case "$1" in
version)
  echo "$$" > "$PWD/version-pid"
  : > "$PWD/version-ready"
  exec /bin/sleep 30
  ;;
plan)
  : > "$PWD/plan-started"
  ;;
esac`)
	before, err := filepath.Glob(filepath.Join(os.TempDir(), "rainforest-plan-*"))
	if err != nil {
		t.Fatalf("glob existing plan temp dirs: %v", err)
	}
	existing := make(map[string]struct{}, len(before))
	for _, path := range before {
		existing[path] = struct{}{}
	}
	s := newPlanTestServerIn(t, workspace)
	req := httptest.NewRequest(http.MethodPost, "/api/plan", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: s.Token()})
	rec := &blockingCompletionRecorder{
		ResponseRecorder: httptest.NewRecorder(),
		writeStarted:     make(chan struct{}),
		allowWrite:       make(chan struct{}),
		postDone:         make(chan struct{}),
	}
	go s.Handler().ServeHTTP(rec, req)
	waitForFile(t, filepath.Join(workspace, "version-ready"))
	rawPID, err := os.ReadFile(filepath.Join(workspace, "version-pid"))
	if err != nil {
		t.Fatalf("read version PID: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(rawPID)))
	if err != nil {
		t.Fatalf("parse version PID %q: %v", rawPID, err)
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- s.Close() }()
	select {
	case <-rec.writeStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("POST did not reach its shutdown response")
	}
	var closeErr error
	closeReturnedBeforePOST := false
	select {
	case closeErr = <-closeDone:
		closeReturnedBeforePOST = true
	case <-time.After(200 * time.Millisecond):
	}
	close(rec.allowWrite)
	if !closeReturnedBeforePOST {
		select {
		case closeErr = <-closeDone:
		case <-time.After(5 * time.Second):
			t.Fatal("Close did not return after POST cleanup completed")
		}
	}
	if closeReturnedBeforePOST {
		t.Error("Close returned before the reserved POST completed cleanup")
	}
	if closeErr != nil {
		t.Errorf("Close: %v", closeErr)
	}
	select {
	case <-rec.postDone:
	default:
		t.Error("POST response was not complete when Close returned")
	}
	if rec.Code == http.StatusAccepted || !strings.Contains(rec.Body.String(), "server is closed") {
		t.Errorf("POST = %d %q, want visible shutdown error and no 202", rec.Code, rec.Body.String())
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("version process %d still exists: %v", pid, err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "plan-started")); !os.IsNotExist(err) {
		t.Errorf("plan started during shutdown; stat error = %v", err)
	}
	after, err := filepath.Glob(filepath.Join(os.TempDir(), "rainforest-plan-*"))
	if err != nil {
		t.Fatalf("glob final plan temp dirs: %v", err)
	}
	for _, path := range after {
		if _, ok := existing[path]; !ok {
			t.Errorf("plan temp dir was not cleaned up: %s", path)
		}
	}
}

func TestPlanCancelTreatsAnyExitAfterRequestAsCanceled(t *testing.T) {
	workspace := initializedWorkspace(t)
	planScript(t, `trap 'echo interrupted >&2; exit 130' INT
echo ready
: > "$PWD/ready"
while :; do /bin/sleep 1; done`, `echo '{"resource_changes":[]}'`)
	s := newPlanTestServerIn(t, workspace)
	started := startPlan(t, s)
	waitForFile(t, filepath.Join(workspace, "ready"))

	rec := authenticatedRequest(t, s, http.MethodDelete, "/api/plan", nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("DELETE status = %d, want 202 (body %q)", rec.Code, rec.Body.String())
	}
	var canceled struct {
		RunID  string `json:"runId"`
		Signal string `json:"signal"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &canceled); err != nil {
		t.Fatalf("decode cancel: %v", err)
	}
	if canceled.RunID != started.RunID || canceled.Signal != "SIGINT" {
		t.Errorf("cancel = %+v, want first run and SIGINT", canceled)
	}

	_, summary := waitForSummary(t, s)
	if summary.State != "canceled" {
		t.Errorf("state = %q, want canceled", summary.State)
	}
}

func TestPlanCancelDoesNotConsumeSIGINTWhenProcessIsNotStarted(t *testing.T) {
	run := &planRun{
		id:          "12345678",
		state:       "running",
		subscribers: make(map[chan struct{}]struct{}),
		done:        make(chan struct{}),
	}
	s := newPlanTestServerIn(t, t.TempDir())
	s.planMu.Lock()
	s.plan = run
	s.planMu.Unlock()
	t.Cleanup(func() {
		s.planMu.Lock()
		if run.state == "running" {
			run.state = "canceled"
			close(run.done)
		}
		s.planMu.Unlock()
	})

	rec := authenticatedRequest(t, s, http.MethodDelete, "/api/plan", nil)
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "process not started") {
		t.Fatalf("DELETE = %d %q, want 500 process-not-started error", rec.Code, rec.Body.String())
	}
	s.planMu.Lock()
	defer s.planMu.Unlock()
	if run.intSent || run.cancelRequested {
		t.Errorf("intSent/cancelRequested = %v/%v, want false/false after failed signal", run.intSent, run.cancelRequested)
	}
}

func TestShowPlanStartBlocksCancellationUntilPIDPublication(t *testing.T) {
	s := newPlanTestServerIn(t, t.TempDir())
	run := &planRun{state: "running"}
	startErr := errors.New("start failed")

	canceled, err := s.startShowProcess(run, &exec.Cmd{}, func() error {
		if s.planMu.TryLock() {
			s.planMu.Unlock()
			t.Error("plan mutex was unlocked during show start; DELETE could observe pid zero")
		}
		return startErr
	})
	if canceled || !errors.Is(err, startErr) {
		t.Fatalf("startShowProcess = canceled %v, err %v; want false, start failure", canceled, err)
	}
}

func TestPlanSecondCancelSendsSIGKILL(t *testing.T) {
	workspace := initializedWorkspace(t)
	planScript(t, `trap '' INT
: > "$PWD/ready"
while :; do /bin/sleep 1; done`, `echo '{"resource_changes":[]}'`)
	s := newPlanTestServerIn(t, workspace)
	startPlan(t, s)
	waitForFile(t, filepath.Join(workspace, "ready"))

	first := authenticatedRequest(t, s, http.MethodDelete, "/api/plan", nil)
	second := authenticatedRequest(t, s, http.MethodDelete, "/api/plan", nil)
	if first.Code != http.StatusAccepted || !strings.Contains(first.Body.String(), "SIGINT") {
		t.Fatalf("first DELETE = %d %q, want 202 SIGINT", first.Code, first.Body.String())
	}
	if second.Code != http.StatusAccepted || !strings.Contains(second.Body.String(), "SIGKILL") {
		t.Fatalf("second DELETE = %d %q, want 202 SIGKILL", second.Code, second.Body.String())
	}
	_, summary := waitForSummary(t, s)
	if summary.State != "canceled" {
		t.Errorf("state = %q, want canceled", summary.State)
	}
}

func TestPlanFailurePreservesStderr(t *testing.T) {
	planScript(t, `echo plan-failed >&2
exit 1`, `echo '{"resource_changes":[]}'`)
	s := newPlanTestServerIn(t, initializedWorkspace(t))
	startPlan(t, s)

	_, summary := waitForSummary(t, s)
	if summary.State != "failed" || summary.ExitCode != 1 || summary.NoChanges {
		t.Errorf("summary = %+v, want failed exit 1", summary)
	}
	rec := authenticatedRequest(t, s, http.MethodGet, "/api/plan/events", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"stream":"stderr"`) ||
		!strings.Contains(rec.Body.String(), `"text":"plan-failed"`) {
		t.Errorf("events = %d %q, want stderr replay", rec.Code, rec.Body.String())
	}
}

func TestPlanShowFailureFinishesWithErrorAndPreservesStderr(t *testing.T) {
	planScript(t, `for arg in "$@"; do
  case "$arg" in -out=*) planfile=${arg#-out=}; : > "$planfile" ;; esac
done
exit 0`, `echo show-failed >&2
exit 1`)
	s := newPlanTestServerIn(t, initializedWorkspace(t))
	startPlan(t, s)

	_, summary := waitForSummary(t, s)
	if summary.State != "error" || summary.ExitCode != 0 || summary.NoChanges ||
		!strings.Contains(summary.ShowError, "show-failed") || summary.Err != summary.ShowError {
		t.Errorf("summary = %+v, want error with noChanges=false and show stderr", summary)
	}
	rec := authenticatedRequest(t, s, http.MethodGet, "/api/plan/events", nil)
	events := parseSSE(t, rec.Body.Bytes())
	if len(events) != 2 || events[0].Name != "line" || events[1].Name != "done" {
		t.Fatalf("events = %+v, want system line and done", events)
	}
	var done planDoneBody
	if err := json.Unmarshal(events[1].Data, &done); err != nil {
		t.Fatalf("decode done event: %v", err)
	}
	if done.State != "error" || done.Counts != nil || !strings.Contains(done.Err, "show-failed") {
		t.Errorf("done = %+v, want error, null counts, and show stderr", done)
	}
	if !strings.Contains(rec.Body.String(), `"stream":"system"`) || !strings.Contains(rec.Body.String(), "show-failed") {
		t.Errorf("events = %q, want system show failure", rec.Body.String())
	}
}

func TestPlanShowOutcomeControlsTerminalState(t *testing.T) {
	planBody := `for arg in "$@"; do
  case "$arg" in -out=*) planfile=${arg#-out=}; : > "$planfile" ;; esac
done`
	tests := []struct {
		name              string
		planBody          string
		showBody          string
		wantState         string
		wantNoChanges     bool
		wantShowError     string
		wantCountsPresent bool
	}{
		{
			name: "start failure",
			planBody: planBody + `
/bin/chmod 000 "$0"`,
			showBody:      `echo '{"resource_changes":[]}'`,
			wantState:     "error",
			wantShowError: "terraform show start",
		},
		{
			name:          "malformed JSON",
			planBody:      planBody,
			showBody:      `echo '{not-json'`,
			wantState:     "error",
			wantShowError: "terraform show JSON",
		},
		{
			name:              "empty successful show",
			planBody:          planBody,
			showBody:          `echo '{"resource_changes":[]}'`,
			wantState:         "succeeded",
			wantNoChanges:     true,
			wantCountsPresent: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			planScript(t, tt.planBody, tt.showBody)
			s := newPlanTestServerIn(t, initializedWorkspace(t))
			startPlan(t, s)
			_, summary := waitForSummary(t, s)
			if summary.State != tt.wantState || summary.NoChanges != tt.wantNoChanges ||
				!strings.Contains(summary.ShowError, tt.wantShowError) || summary.Err != summary.ShowError {
				t.Errorf("summary = %+v, want state=%q noChanges=%v showError containing %q", summary, tt.wantState, tt.wantNoChanges, tt.wantShowError)
			}
			done := planDoneEvent(t, s)
			if done.State != tt.wantState || (done.Counts != nil) != tt.wantCountsPresent || done.Err != summary.Err {
				t.Errorf("done = %+v, want state=%q countsPresent=%v err=%q", done, tt.wantState, tt.wantCountsPresent, summary.Err)
			}
		})
	}
}

func TestPlanCancelWhileShowIsRunningSendsSIGINT(t *testing.T) {
	workspace := initializedWorkspace(t)
	planScript(t, `for arg in "$@"; do
  case "$arg" in -out=*) planfile=${arg#-out=}; : > "$planfile" ;; esac
done`, `trap ': > "$PWD/show-interrupted"; exit 130' INT
: > "$PWD/show-ready"
while :; do /bin/sleep 1; done`)
	s := newPlanTestServerIn(t, workspace)
	startPlan(t, s)
	waitForFile(t, filepath.Join(workspace, "show-ready"))

	rec := authenticatedRequest(t, s, http.MethodDelete, "/api/plan", nil)
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"signal":"SIGINT"`) {
		t.Fatalf("DELETE = %d %q, want 202 SIGINT", rec.Code, rec.Body.String())
	}
	waitForFile(t, filepath.Join(workspace, "show-interrupted"))
	_, summary := waitForSummary(t, s)
	if summary.State != "canceled" || summary.NoChanges {
		t.Errorf("summary = %+v, want canceled with noChanges=false", summary)
	}
}

func TestPlanSummaryUsesVersionCapturedOnceAtRunStart(t *testing.T) {
	workspace := initializedWorkspace(t)
	versionCount := filepath.Join(workspace, "version-count")
	terraformPath := stubTerraform(t, `case "$1" in
version)
  count=0
  if [ -f "$PWD/version-count" ]; then read count < "$PWD/version-count"; fi
  count=$((count + 1))
  echo "$count" > "$PWD/version-count"
  echo '{"terraform_version":"1.14.8"}'
  ;;
show)
  echo '{"resource_changes":[]}'
  ;;
plan)
  for arg in "$@"; do
    case "$arg" in -out=*) planfile=${arg#-out=}; : > "$planfile" ;; esac
  done
  ;;
esac
`)
	s := newPlanTestServerIn(t, workspace)
	startPlan(t, s)
	_, first := waitForSummary(t, s)
	for i := 0; i < 2; i++ {
		rec := authenticatedRequest(t, s, http.MethodGet, "/api/plan/summary", nil)
		var summary planSummaryBody
		if err := json.Unmarshal(rec.Body.Bytes(), &summary); err != nil {
			t.Fatalf("decode repeated summary %d: %v", i+1, err)
		}
		if rec.Code != http.StatusOK || summary.Run.TerraformVersion != "1.14.8" {
			t.Errorf("repeated summary %d = %d %+v, want stored terraform 1.14.8", i+1, rec.Code, summary.Run)
		}
	}
	count, err := os.ReadFile(versionCount)
	if err != nil {
		t.Fatalf("read version invocation count: %v", err)
	}
	if strings.TrimSpace(string(count)) != "1" {
		t.Fatalf("terraform version invocation count = %q, want 1", strings.TrimSpace(string(count)))
	}
	if err := os.Remove(terraformPath); err != nil {
		t.Fatalf("remove terraform stub: %v", err)
	}
	rec := authenticatedRequest(t, s, http.MethodGet, "/api/plan/summary", nil)
	var historical planSummaryBody
	if err := json.Unmarshal(rec.Body.Bytes(), &historical); err != nil {
		t.Fatalf("decode historical summary: %v", err)
	}
	if first.Run.TerraformVersion != "1.14.8" || historical.Run.TerraformVersion != first.Run.TerraformVersion {
		t.Errorf("versions before/after removal = %q/%q, want stable 1.14.8", first.Run.TerraformVersion, historical.Run.TerraformVersion)
	}
}

func TestPlanSummaryCachesFailedVersionProbe(t *testing.T) {
	workspace := initializedWorkspace(t)
	versionCount := filepath.Join(workspace, "version-count")
	terraformPath := stubTerraform(t, `case "$1" in
version)
  count=0
  if [ -f "$PWD/version-count" ]; then read count < "$PWD/version-count"; fi
  count=$((count + 1))
  echo "$count" > "$PWD/version-count"
  exit 1
  ;;
show)
  echo '{"resource_changes":[]}'
  ;;
plan)
  for arg in "$@"; do
    case "$arg" in -out=*) planfile=${arg#-out=}; : > "$planfile" ;; esac
  done
  ;;
esac`)
	s := newPlanTestServerIn(t, workspace)
	startPlan(t, s)
	_, first := waitForSummary(t, s)
	if first.Run.TerraformVersion != "" {
		t.Fatalf("terraformVersion = %q, want empty after failed probe", first.Run.TerraformVersion)
	}
	for i := 0; i < 2; i++ {
		rec := authenticatedRequest(t, s, http.MethodGet, "/api/plan/summary", nil)
		var summary planSummaryBody
		if err := json.Unmarshal(rec.Body.Bytes(), &summary); err != nil {
			t.Fatalf("decode repeated summary %d: %v", i+1, err)
		}
		if rec.Code != http.StatusOK || summary.Run.TerraformVersion != "" {
			t.Errorf("repeated summary %d = %d version %q, want 200 with empty version", i+1, rec.Code, summary.Run.TerraformVersion)
		}
	}
	count, err := os.ReadFile(versionCount)
	if err != nil {
		t.Fatalf("read version invocation count: %v", err)
	}
	if strings.TrimSpace(string(count)) != "1" {
		t.Fatalf("terraform version invocation count = %q, want 1", strings.TrimSpace(string(count)))
	}
	if err := os.Remove(terraformPath); err != nil {
		t.Fatalf("remove terraform stub: %v", err)
	}
	rec := authenticatedRequest(t, s, http.MethodGet, "/api/plan/summary", nil)
	var historical planSummaryBody
	if err := json.Unmarshal(rec.Body.Bytes(), &historical); err != nil {
		t.Fatalf("decode historical summary: %v", err)
	}
	if historical.Run.TerraformVersion != "" {
		t.Errorf("historical terraformVersion = %q, want cached empty value", historical.Run.TerraformVersion)
	}
}

func TestPlanPreconditions(t *testing.T) {
	t.Run("terraform missing", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		s := newPlanTestServerIn(t, initializedWorkspace(t))
		rec := authenticatedRequest(t, s, http.MethodPost, "/api/plan", nil)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "terraform binary not found") {
			t.Errorf("response = %d %q, want 409 terraform missing", rec.Code, rec.Body.String())
		}
	})

	t.Run("workspace not initialized", func(t *testing.T) {
		planScript(t, `exit 0`, `echo '{"resource_changes":[]}'`)
		s := newPlanTestServerIn(t, t.TempDir())
		rec := authenticatedRequest(t, s, http.MethodPost, "/api/plan", nil)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "workspace not initialized — run terraform init first") {
			t.Errorf("response = %d %q, want 409 init required", rec.Code, rec.Body.String())
		}
	})
}

func TestPlanEventsNoRunAndMiddlewareCoverage(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	s := newPlanTestServerIn(t, t.TempDir())

	events := authenticatedRequest(t, s, http.MethodGet, "/api/plan/events", nil)
	if events.Code != http.StatusOK {
		t.Fatalf("events status = %d, want 200 (body %q)", events.Code, events.Body.String())
	}
	parsed := parseSSE(t, events.Body.Bytes())
	if len(parsed) != 1 || parsed[0].Name != "none" || string(parsed[0].Data) != "{}" {
		t.Errorf("events = %+v, want none with empty data", parsed)
	}

	unauthenticated := httptest.NewRecorder()
	s.Handler().ServeHTTP(unauthenticated, httptest.NewRequest(http.MethodPost, "/api/plan", nil))
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated status = %d, want 401", unauthenticated.Code)
	}

	foreignReq := httptest.NewRequest(http.MethodPost, "/api/plan", nil)
	foreignReq.AddCookie(&http.Cookie{Name: cookieName, Value: s.Token()})
	foreignReq.Header.Set("Origin", "https://evil.example")
	foreign := httptest.NewRecorder()
	s.Handler().ServeHTTP(foreign, foreignReq)
	if foreign.Code != http.StatusForbidden {
		t.Errorf("foreign Origin status = %d, want 403", foreign.Code)
	}
}

func TestPlanStripsTerraformCLIArgsAndSetsAutomation(t *testing.T) {
	t.Setenv("TF_CLI_ARGS", "-destroy")
	t.Setenv("TF_CLI_ARGS_plan", "-refresh=false")
	t.Setenv("TF_CLI_ARGS_apply", "-auto-approve")
	planScript(t, `echo "TF_CLI_ARGS=${TF_CLI_ARGS-unset}"
echo "TF_CLI_ARGS_plan=${TF_CLI_ARGS_plan-unset}"
echo "TF_CLI_ARGS_apply=${TF_CLI_ARGS_apply-unset}"
echo "TF_IN_AUTOMATION=$TF_IN_AUTOMATION"
for arg in "$@"; do
  case "$arg" in -out=*) planfile=${arg#-out=}; : > "$planfile" ;; esac
done`, `echo '{"resource_changes":[]}'`)
	s := newPlanTestServerIn(t, initializedWorkspace(t))
	startPlan(t, s)
	waitForSummary(t, s)

	rec := authenticatedRequest(t, s, http.MethodGet, "/api/plan/events", nil)
	got := rec.Body.String()
	for _, want := range []string{
		`"text":"TF_CLI_ARGS=unset"`,
		`"text":"TF_CLI_ARGS_plan=unset"`,
		`"text":"TF_CLI_ARGS_apply=unset"`,
		`"text":"TF_IN_AUTOMATION=1"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("events missing %s: %q", want, got)
		}
	}
}

func TestPlanEventsStreamsToMultipleSubscribers(t *testing.T) {
	workspace := initializedWorkspace(t)
	planScript(t, `echo first
: > "$PWD/ready"
/bin/sleep 0.2
echo second
for arg in "$@"; do
  case "$arg" in -out=*) planfile=${arg#-out=}; : > "$planfile" ;; esac
done`, `echo '{"resource_changes":[]}'`)
	s := newPlanTestServerIn(t, workspace)
	startPlan(t, s)
	waitForFile(t, filepath.Join(workspace, "ready"))

	var wg sync.WaitGroup
	bodies := make([]string, 2)
	for i := range bodies {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := authenticatedRequest(t, s, http.MethodGet, "/api/plan/events", nil)
			bodies[i] = rec.Body.String()
		}(i)
	}
	wg.Wait()
	for i, body := range bodies {
		if !strings.Contains(body, `"text":"first"`) || !strings.Contains(body, `"text":"second"`) ||
			!strings.Contains(body, "event: done") {
			t.Errorf("subscriber %d body = %q, want both lines and done", i, body)
		}
	}
}

func TestPlanEventsClientDisconnectDoesNotCancelRun(t *testing.T) {
	workspace := initializedWorkspace(t)
	planScript(t, `: > "$PWD/ready"
/bin/sleep 0.2
for arg in "$@"; do
  case "$arg" in -out=*) planfile=${arg#-out=}; : > "$planfile" ;; esac
done`, `echo '{"resource_changes":[]}'`)
	s := newPlanTestServerIn(t, workspace)
	startPlan(t, s)
	waitForFile(t, filepath.Join(workspace, "ready"))

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/api/plan/events", nil).WithContext(ctx)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: s.Token()})
	canceled := make(chan struct{})
	go func() {
		s.Handler().ServeHTTP(httptest.NewRecorder(), req)
		close(canceled)
	}()
	cancel()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("SSE handler did not return after client disconnect")
	}

	_, summary := waitForSummary(t, s)
	if summary.State != "succeeded" {
		t.Errorf("state = %q, want succeeded after SSE disconnect", summary.State)
	}
}

type nonFlushingResponseWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (w *nonFlushingResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *nonFlushingResponseWriter) WriteHeader(status int) { w.status = status }

func (w *nonFlushingResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(p)
}

func TestPlanEventsRequiresFlusherBeforeSSEHeaders(t *testing.T) {
	s := newPlanTestServerIn(t, t.TempDir())
	req := httptest.NewRequest(http.MethodGet, "/api/plan/events", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: s.Token()})
	w := &nonFlushingResponseWriter{}
	s.Handler().ServeHTTP(w, req)

	if w.status != http.StatusInternalServerError || !strings.Contains(w.Header().Get("Content-Type"), "application/json") ||
		strings.Contains(w.Header().Get("Content-Type"), "text/event-stream") {
		t.Errorf("response = %d %q %q, want 500 JSON before SSE headers", w.status, w.Header().Get("Content-Type"), w.body.String())
	}
}

func TestPlanLineBufferTruncatesOldest(t *testing.T) {
	planScript(t, `i=1
while [ "$i" -le 10005 ]; do
  echo "line-$i"
  i=$((i + 1))
done
for arg in "$@"; do
  case "$arg" in -out=*) planfile=${arg#-out=}; : > "$planfile" ;; esac
done`, `echo '{"resource_changes":[]}'`)
	s := newPlanTestServerIn(t, initializedWorkspace(t))
	startPlan(t, s)
	waitForSummary(t, s)

	rec := authenticatedRequest(t, s, http.MethodGet, "/api/plan/events", nil)
	events := parseSSE(t, rec.Body.Bytes())
	if len(events) != 10002 || events[0].Name != "truncated" || events[len(events)-1].Name != "done" {
		t.Fatalf("events count/edges = %d/%q/%q, want 10002/truncated/done", len(events), events[0].Name, events[len(events)-1].Name)
	}
	var first, last struct {
		Seq  int    `json:"seq"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(events[1].Data, &first); err != nil {
		t.Fatalf("decode first retained line: %v", err)
	}
	if err := json.Unmarshal(events[len(events)-2].Data, &last); err != nil {
		t.Fatalf("decode last retained line: %v", err)
	}
	if first.Seq != 6 || first.Text != "line-6" || last.Seq != 10005 || last.Text != "line-10005" {
		t.Errorf("retained range = %+v to %+v, want seq/text 6 to 10005", first, last)
	}
}

func TestPlanTempDirectoryLifecycle(t *testing.T) {
	planScript(t, `for arg in "$@"; do
  case "$arg" in -out=*) planfile=${arg#-out=}; : > "$planfile" ;; esac
done`, `echo '{"resource_changes":[]}'`)
	s := newPlanTestServerIn(t, initializedWorkspace(t))
	first := startPlan(t, s)
	waitForSummary(t, s)
	firstDir := filepath.Dir(strings.TrimPrefix(first.Argv[len(first.Argv)-1], "-out="))
	if info, err := os.Stat(firstDir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("first temp dir = %v/%v, want existing mode 0700", info, err)
	}

	second := startPlan(t, s)
	waitForSummary(t, s)
	secondDir := filepath.Dir(strings.TrimPrefix(second.Argv[len(second.Argv)-1], "-out="))
	if _, err := os.Stat(firstDir); !os.IsNotExist(err) {
		t.Errorf("old temp dir stat error = %v, want not exist", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(secondDir); !os.IsNotExist(err) {
		t.Errorf("current temp dir stat error = %v, want not exist after Close", err)
	}
}
