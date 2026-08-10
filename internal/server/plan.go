package server

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const maxPlanLines = 10000

type planLine struct {
	Seq    int    `json:"seq"`
	Stream string `json:"stream"`
	Text   string `json:"text"`
}

type planCounts struct {
	Add     int `json:"add"`
	Change  int `json:"change"`
	Destroy int `json:"destroy"`
}

type planChange struct {
	Address string   `json:"address"`
	Type    string   `json:"type"`
	Name    string   `json:"name"`
	Actions []string `json:"actions"`
}

type planSummary struct {
	Counts    planCounts
	Changes   []planChange
	ShowError string
}

type planRun struct {
	id               string
	state            string
	argv             []string
	startedAt        time.Time
	finishedAt       time.Time
	exitCode         int
	lines            []planLine
	lineStart        int
	truncated        bool
	summary          *planSummary
	err              string
	seq              int
	cwd              string
	terraformPath    string
	terraformVersion string
	planFile         string
	tempDir          string
	cmd              *exec.Cmd
	pid              int
	cancelRequested  bool
	intSent          bool
	subscribers      map[chan struct{}]struct{}
	done             chan struct{}
}

func (s *Server) startPlan(w http.ResponseWriter, _ *http.Request) {
	s.planMu.Lock()
	if s.closed {
		s.planMu.Unlock()
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "server is closed"})
		return
	}
	if s.planStarting {
		s.planMu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]string{"error": "a plan is starting"})
		return
	}
	if s.plan != nil && s.plan.state == "running" {
		id := s.plan.id
		s.planMu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]string{"error": "a plan is already running", "runId": id})
		return
	}
	s.planStarting = true
	s.planStarts.Add(1)
	previousTempDir := ""
	if s.plan != nil {
		previousTempDir = s.plan.tempDir
	}
	s.planMu.Unlock()
	reservationActive := true
	defer func() {
		if reservationActive {
			s.planMu.Lock()
			s.planStarting = false
			s.planMu.Unlock()
		}
		s.planStarts.Done()
	}()

	terraformPath, err := exec.LookPath("terraform")
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "terraform binary not found"})
		return
	}
	info, err := os.Stat(filepath.Join(s.workspace, ".terraform"))
	if err != nil || !info.IsDir() {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "workspace not initialized — run terraform init first"})
		return
	}
	terraformVersion := s.terraformVersion(terraformPath)
	if s.lifecycleCtx != nil && s.lifecycleCtx.Err() != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "server is closed"})
		return
	}
	tempDir, err := os.MkdirTemp("", "rainforest-plan-")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if err := os.Chmod(tempDir, 0o700); err != nil {
		_ = os.RemoveAll(tempDir)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	id, err := newPlanID()
	if err != nil {
		_ = os.RemoveAll(tempDir)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	planFile := filepath.Join(tempDir, "plan.tfplan")
	argv := []string{"terraform", "plan", "-input=false", "-no-color", "-out=" + planFile}
	cmd := exec.Command(terraformPath, argv[1:]...)
	cmd.Dir = s.workspace
	cmd.Env = terraformEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	run := &planRun{
		id:               id,
		state:            "running",
		argv:             argv,
		startedAt:        time.Now(),
		exitCode:         -1,
		lines:            make([]planLine, 0, maxPlanLines),
		cwd:              s.workspace,
		terraformPath:    terraformPath,
		terraformVersion: terraformVersion,
		planFile:         planFile,
		tempDir:          tempDir,
		cmd:              cmd,
		subscribers:      make(map[chan struct{}]struct{}),
		done:             make(chan struct{}),
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = os.RemoveAll(tempDir)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdout.Close()
		_ = os.RemoveAll(tempDir)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if previousTempDir != "" {
		if err := os.RemoveAll(previousTempDir); err != nil {
			_ = stdout.Close()
			_ = stderr.Close()
			_ = os.RemoveAll(tempDir)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
	}

	s.planMu.Lock()
	if s.closed {
		s.planMu.Unlock()
		_ = stdout.Close()
		_ = stderr.Close()
		_ = os.RemoveAll(tempDir)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "server is closed"})
		return
	}
	s.plan = run
	s.planStarting = false
	reservationActive = false
	if err := cmd.Start(); err != nil {
		s.planMu.Unlock()
		_ = stdout.Close()
		_ = stderr.Close()
		s.finishPlanError(run, "terraform plan start: "+err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	run.pid = cmd.Process.Pid
	s.planMu.Unlock()
	go s.executePlan(run, stdout, stderr)
	writeJSON(w, http.StatusAccepted, map[string]any{"runId": id, "argv": argv, "cwd": s.workspace})
}

func newPlanID() (string, error) {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func terraformEnv() []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if name == "TF_CLI_ARGS" || strings.HasPrefix(name, "TF_CLI_ARGS_") || name == "TF_IN_AUTOMATION" {
			continue
		}
		env = append(env, entry)
	}
	return append(env, "TF_IN_AUTOMATION=1")
}

func (s *Server) executePlan(run *planRun, stdout, stderr io.ReadCloser) {
	errs := make(chan error, 2)
	go func() { errs <- s.scanPlanStream(run, stdout, "stdout") }()
	go func() { errs <- s.scanPlanStream(run, stderr, "stderr") }()
	firstScanErr := <-errs
	secondScanErr := <-errs
	waitErr := run.cmd.Wait()
	if firstScanErr != nil {
		s.appendPlanLine(run, "system", firstScanErr.Error())
	}
	if secondScanErr != nil {
		s.appendPlanLine(run, "system", secondScanErr.Error())
	}
	scanErr := firstScanErr
	if scanErr == nil {
		scanErr = secondScanErr
	}
	exitCode := planExitCode(waitErr)

	s.planMu.Lock()
	canceled := run.cancelRequested
	s.planMu.Unlock()
	if scanErr != nil {
		s.finishPlan(run, "error", exitCode, scanErr.Error(), nil)
		return
	}
	if canceled {
		s.finishPlan(run, "canceled", exitCode, "", nil)
		return
	}
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			s.finishPlan(run, "failed", exitCode, "", nil)
			return
		}
		s.finishPlanError(run, "terraform plan wait: "+waitErr.Error())
		return
	}

	summary, canceled := s.showPlan(run)
	if canceled {
		s.finishPlan(run, "canceled", 0, "", nil)
		return
	}
	if summary.ShowError != "" {
		s.appendPlanLine(run, "system", summary.ShowError)
		s.finishPlan(run, "error", 0, summary.ShowError, summary)
		return
	}
	s.finishPlan(run, "succeeded", 0, "", summary)
}

func (s *Server) scanPlanStream(run *planRun, reader io.ReadCloser, stream string) error {
	defer reader.Close()
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		s.appendPlanLine(run, stream, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		message := stream + " scanner: " + err.Error()
		s.planMu.Lock()
		pid := run.pid
		s.planMu.Unlock()
		_ = signalProcessGroup(pid, syscall.SIGKILL)
		return errors.New(message)
	}
	return nil
}

func (s *Server) showPlan(run *planRun) (*planSummary, bool) {
	cmd := exec.Command(run.terraformPath, "show", "-json", run.planFile)
	cmd.Dir = run.cwd
	cmd.Env = terraformEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var raw bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &raw
	cmd.Stderr = &stderr

	canceled, err := s.startShowProcess(run, cmd, cmd.Start)
	if canceled {
		return nil, true
	}
	if err != nil {
		s.planMu.Lock()
		canceled = run.cancelRequested
		s.planMu.Unlock()
		if canceled {
			return nil, true
		}
		return &planSummary{Changes: []planChange{}, ShowError: "terraform show start: " + err.Error()}, false
	}
	if err := cmd.Wait(); err != nil {
		s.planMu.Lock()
		canceled := run.cancelRequested
		s.planMu.Unlock()
		if canceled {
			return nil, true
		}
		message := "terraform show: " + err.Error()
		if text := strings.TrimSpace(stderr.String()); text != "" {
			message += ": " + text
		}
		return &planSummary{Changes: []planChange{}, ShowError: message}, false
	}
	s.planMu.Lock()
	canceled = run.cancelRequested
	s.planMu.Unlock()
	if canceled {
		return nil, true
	}
	summary, err := parsePlanSummary(raw.Bytes())
	if err != nil {
		return &planSummary{Changes: []planChange{}, ShowError: "terraform show JSON: " + err.Error()}, false
	}
	return summary, false
}

func (s *Server) startShowProcess(run *planRun, cmd *exec.Cmd, start func() error) (bool, error) {
	s.planMu.Lock()
	defer s.planMu.Unlock()
	if run.cancelRequested {
		return true, nil
	}
	run.pid = 0
	if err := start(); err != nil {
		return false, err
	}
	run.pid = cmd.Process.Pid
	return false, nil
}

func parsePlanSummary(raw []byte) (*planSummary, error) {
	var parsed struct {
		ResourceChanges []struct {
			Address string `json:"address"`
			Mode    string `json:"mode"`
			Type    string `json:"type"`
			Name    string `json:"name"`
			Change  struct {
				Actions []string `json:"actions"`
			} `json:"change"`
		} `json:"resource_changes"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}
	summary := &planSummary{Changes: make([]planChange, 0)}
	for _, resource := range parsed.ResourceChanges {
		if resource.Mode != "managed" || actionsEqual(resource.Change.Actions, "no-op") || actionsEqual(resource.Change.Actions, "read") {
			continue
		}
		actions := append([]string(nil), resource.Change.Actions...)
		summary.Changes = append(summary.Changes, planChange{
			Address: resource.Address,
			Type:    resource.Type,
			Name:    resource.Name,
			Actions: actions,
		})
		for _, action := range actions {
			switch action {
			case "create":
				summary.Counts.Add++
			case "update":
				summary.Counts.Change++
			case "delete":
				summary.Counts.Destroy++
			}
		}
	}
	return summary, nil
}

func actionsEqual(actions []string, action string) bool {
	return len(actions) == 1 && actions[0] == action
}

func planExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func (s *Server) appendPlanLine(run *planRun, stream, text string) {
	s.planMu.Lock()
	defer s.planMu.Unlock()
	run.seq++
	line := planLine{Seq: run.seq, Stream: stream, Text: text}
	if len(run.lines) == maxPlanLines {
		run.lines[run.lineStart] = line
		run.lineStart = (run.lineStart + 1) % maxPlanLines
		run.truncated = true
	} else {
		run.lines = append(run.lines, line)
	}
	s.notifyPlanSubscribersLocked(run)
}

func (s *Server) finishPlanError(run *planRun, message string) {
	s.appendPlanLine(run, "system", message)
	s.finishPlan(run, "error", -1, message, nil)
}

func (s *Server) finishPlan(run *planRun, state string, exitCode int, message string, summary *planSummary) {
	s.planMu.Lock()
	defer s.planMu.Unlock()
	if run.state != "running" {
		return
	}
	run.state = state
	run.exitCode = exitCode
	run.finishedAt = time.Now()
	run.err = message
	run.summary = summary
	run.pid = 0
	s.notifyPlanSubscribersLocked(run)
	close(run.done)
}

func (s *Server) notifyPlanSubscribersLocked(run *planRun) {
	for subscriber := range run.subscribers {
		select {
		case subscriber <- struct{}{}:
		default:
		}
	}
}

func (s *Server) planEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming unsupported"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	var run *planRun
	var subscriber chan struct{}
	lastSeq := 0
	first := true
	defer func() {
		if run == nil || subscriber == nil {
			return
		}
		s.planMu.Lock()
		delete(run.subscribers, subscriber)
		s.planMu.Unlock()
	}()
	for {
		s.planMu.Lock()
		if run == nil {
			run = s.plan
			if run == nil {
				s.planMu.Unlock()
				if err := writePlanEvent(w, flusher, "none", map[string]any{}); err != nil {
					return
				}
				return
			}
			if run.state == "running" {
				subscriber = make(chan struct{}, 1)
				run.subscribers[subscriber] = struct{}{}
			}
		}
		ordered := orderedPlanLinesLocked(run)
		lines := make([]planLine, 0, len(ordered))
		for _, line := range ordered {
			if line.Seq > lastSeq {
				lines = append(lines, line)
			}
		}
		gap := run.truncated && (first || len(ordered) > 0 && lastSeq < ordered[0].Seq-1)
		terminal := run.state != "running"
		done := planDoneLocked(run)
		s.planMu.Unlock()

		if gap {
			if err := writePlanEvent(w, flusher, "truncated", map[string]string{"runId": run.id}); err != nil {
				return
			}
		}
		first = false
		for _, line := range lines {
			payload := struct {
				RunID string `json:"runId"`
				planLine
			}{RunID: run.id, planLine: line}
			if err := writePlanEvent(w, flusher, "line", payload); err != nil {
				return
			}
			lastSeq = line.Seq
		}
		if terminal {
			if err := writePlanEvent(w, flusher, "done", done); err != nil {
				return
			}
			return
		}
		select {
		case <-subscriber:
		case <-r.Context().Done():
			return
		}
	}
}

func orderedPlanLinesLocked(run *planRun) []planLine {
	lines := make([]planLine, len(run.lines))
	if !run.truncated {
		copy(lines, run.lines)
		return lines
	}
	for i := range run.lines {
		lines[i] = run.lines[(run.lineStart+i)%len(run.lines)]
	}
	return lines
}

func writePlanEvent(w http.ResponseWriter, flusher http.Flusher, name string, data any) error {
	payload, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, payload); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

func planDoneLocked(run *planRun) any {
	var counts *planCounts
	if run.state == "succeeded" && run.summary != nil {
		value := run.summary.Counts
		counts = &value
	}
	return struct {
		RunID    string      `json:"runId"`
		State    string      `json:"state"`
		ExitCode int         `json:"exitCode"`
		Counts   *planCounts `json:"counts"`
		Err      string      `json:"err"`
	}{
		RunID:    run.id,
		State:    run.state,
		ExitCode: run.exitCode,
		Counts:   counts,
		Err:      run.err,
	}
}

func (s *Server) cancelPlan(w http.ResponseWriter, _ *http.Request) {
	s.planMu.Lock()
	if s.plan == nil || s.plan.state != "running" {
		s.planMu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]string{"error": "no plan is running"})
		return
	}
	run := s.plan
	signal := syscall.SIGINT
	signalName := "SIGINT"
	if run.intSent {
		signal = syscall.SIGKILL
		signalName = "SIGKILL"
	}
	signalErr := signalProcessGroup(run.pid, signal)
	if signalErr == nil {
		run.cancelRequested = true
		if signal == syscall.SIGINT {
			run.intSent = true
		}
	}
	s.planMu.Unlock()

	if signalErr != nil {
		message := signalName + ": " + signalErr.Error()
		s.appendPlanLine(run, "system", message)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": message, "runId": run.id})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"runId": run.id, "signal": signalName})
}

func signalProcessGroup(pid int, signal syscall.Signal) error {
	if pid == 0 {
		return errors.New("process not started")
	}
	return syscall.Kill(-pid, signal)
}

func (s *Server) planSummary(w http.ResponseWriter, _ *http.Request) {
	s.planMu.Lock()
	if s.plan == nil {
		s.planMu.Unlock()
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no plan has been run"})
		return
	}
	run := s.plan
	if run.state == "running" {
		id := run.id
		s.planMu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]string{"error": "plan still running", "runId": id})
		return
	}
	argv := append([]string(nil), run.argv...)
	changes := make([]planChange, 0)
	counts := planCounts{}
	showError := ""
	if run.summary != nil {
		counts = run.summary.Counts
		showError = run.summary.ShowError
		for _, change := range run.summary.Changes {
			change.Actions = append([]string(nil), change.Actions...)
			changes = append(changes, change)
		}
	}
	response := struct {
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
		Counts    planCounts   `json:"counts"`
		NoChanges bool         `json:"noChanges"`
		ShowError string       `json:"showError"`
		Changes   []planChange `json:"changes"`
		Err       string       `json:"err"`
	}{
		RunID:     run.id,
		State:     run.state,
		ExitCode:  run.exitCode,
		Counts:    counts,
		NoChanges: run.state == "succeeded" && run.exitCode == 0 && showError == "" && len(changes) == 0,
		ShowError: showError,
		Changes:   changes,
		Err:       run.err,
	}
	response.Run.Argv = argv
	response.Run.CWD = run.cwd
	response.Run.StartedAt = run.startedAt
	response.Run.FinishedAt = run.finishedAt
	response.Run.TerraformVersion = run.terraformVersion
	s.planMu.Unlock()

	writeJSON(w, http.StatusOK, response)
}

func (s *Server) closePlan() error {
	s.planMu.Lock()
	s.closed = true
	run := s.plan
	if run == nil {
		s.planMu.Unlock()
		return nil
	}
	tempDir := run.tempDir
	done := run.done
	pid := run.pid
	running := run.state == "running"
	if running {
		run.cancelRequested = true
	}
	var signalErr error
	if running {
		if err := signalProcessGroup(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) && err.Error() != "process not started" {
			signalErr = err
		}
	}
	s.planMu.Unlock()

	if running {
		<-done
	}
	removeErr := os.RemoveAll(tempDir)
	return errors.Join(signalErr, removeErr)
}
