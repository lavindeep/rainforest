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
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	maxPlanLines   = 10000
	planTempPrefix = "rainforest-plan-"
)

var (
	closeGrace            = 3 * time.Second
	killGrace             = 3 * time.Second
	errProcessNotStarted  = errors.New("process not started")
	planStartHandoff      func()
	planCancelHandoff     func()
	planShowHandoff       func()
	planShowReapedHandoff func()
)

type planPhase uint8

const (
	planPhaseStarting planPhase = iota
	planPhaseRunning
	planPhaseReaped
	planPhaseShowRunning
	planPhaseShowReaped
	planPhaseTerminal
)

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

type planSubscriber struct {
	notify    chan struct{}
	cursor    int
	truncated bool
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
	phase            planPhase
	stdout           io.ReadCloser
	stderr           io.ReadCloser
	closePipesOnce   sync.Once
	closePipesErr    error
	pipesAbandoned   bool
	cancelRequested  bool
	intSent          bool
	subscribers      map[*planSubscriber]struct{}
	done             chan struct{}
}

func sweepPlanTempDirs() {
	tempDir := os.TempDir()
	entries, err := os.ReadDir(tempDir)
	if err != nil {
		log.Printf("sweep plan temp directories: %v", err)
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, ok := planTempDirPID(entry.Name())
		if !ok {
			continue
		}
		err := syscall.Kill(pid, 0)
		if err == nil || errors.Is(err, syscall.EPERM) {
			continue
		}
		if !errors.Is(err, syscall.ESRCH) {
			log.Printf("check plan temp directory %s: %v", entry.Name(), err)
			continue
		}
		if err := os.RemoveAll(filepath.Join(tempDir, entry.Name())); err != nil {
			log.Printf("remove stale plan temp directory %s: %v", entry.Name(), err)
		}
	}
}

func planTempDirPID(name string) (int, bool) {
	remainder, ok := strings.CutPrefix(name, planTempPrefix)
	if !ok {
		return 0, false
	}
	pidText, suffix, ok := strings.Cut(remainder, "-")
	if !ok || pidText == "" || suffix == "" {
		return 0, false
	}
	for i := range len(pidText) {
		if pidText[i] < '0' || pidText[i] > '9' {
			return 0, false
		}
	}
	pid, err := strconv.Atoi(pidText)
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
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
	tempDir, err := os.MkdirTemp("", planTempPrefix+strconv.Itoa(os.Getpid())+"-")
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
		phase:            planPhaseStarting,
		subscribers:      make(map[*planSubscriber]struct{}),
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
	run.stdout = stdout
	run.stderr = stderr
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
	if planStartHandoff != nil {
		planStartHandoff()
	}
	if err := cmd.Start(); err != nil {
		run.phase = planPhaseTerminal
		s.planMu.Unlock()
		_ = stdout.Close()
		_ = stderr.Close()
		s.finishPlanError(run, "terraform plan start: "+err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	run.pid = cmd.Process.Pid
	run.phase = planPhaseRunning
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
	_ = run.closePipes()
	waitErr := run.cmd.Wait()
	s.planMu.Lock()
	run.pid = 0
	run.phase = planPhaseReaped
	canceled := run.cancelRequested
	s.planMu.Unlock()
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

	if planShowHandoff != nil {
		planShowHandoff()
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
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		s.appendPlanLine(run, stream, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		message := stream + " scanner: " + err.Error()
		s.planMu.Lock()
		abandoned := run.pipesAbandoned
		pid := 0
		if planProcessRunningLocked(run) {
			pid = run.pid
		}
		s.planMu.Unlock()
		if abandoned {
			return nil
		}
		_ = signalProcessGroup(pid, syscall.SIGKILL)
		return errors.New(message)
	}
	return nil
}

func (run *planRun) closePipes() error {
	run.closePipesOnce.Do(func() {
		var stdoutErr, stderrErr error
		if run.stdout != nil {
			stdoutErr = run.stdout.Close()
		}
		if run.stderr != nil {
			stderrErr = run.stderr.Close()
		}
		run.closePipesErr = errors.Join(stdoutErr, stderrErr)
	})
	return run.closePipesErr
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
	waitErr := cmd.Wait()
	s.planMu.Lock()
	run.pid = 0
	run.phase = planPhaseShowReaped
	s.planMu.Unlock()
	if planShowReapedHandoff != nil {
		planShowReapedHandoff()
	}
	s.planMu.Lock()
	canceled = run.cancelRequested
	s.planMu.Unlock()
	if waitErr != nil {
		if canceled {
			return nil, true
		}
		message := "terraform show: " + waitErr.Error()
		if text := strings.TrimSpace(stderr.String()); text != "" {
			message += ": " + text
		}
		return &planSummary{Changes: []planChange{}, ShowError: message}, false
	}
	if canceled {
		return nil, true
	}
	summary, err := parsePlanSummary(raw.Bytes())
	if err != nil {
		return &planSummary{Changes: []planChange{}, ShowError: "terraform show JSON: " + err.Error()}, false
	}
	s.planMu.Lock()
	canceled = run.cancelRequested
	s.planMu.Unlock()
	if canceled {
		return nil, true
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
	run.phase = planPhaseShowRunning
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
	if run.cancelRequested {
		state = "canceled"
		message = ""
		summary = nil
	}
	run.state = state
	run.exitCode = exitCode
	run.finishedAt = time.Now()
	run.err = message
	run.summary = summary
	run.pid = 0
	run.phase = planPhaseTerminal
	s.notifyPlanSubscribersLocked(run)
	close(run.done)
}

func (s *Server) notifyPlanSubscribersLocked(run *planRun) {
	for subscriber := range run.subscribers {
		select {
		case subscriber.notify <- struct{}{}:
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
	var subscriber *planSubscriber
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
			subscriber = &planSubscriber{notify: make(chan struct{}, 1)}
			if run.state == "running" {
				run.subscribers[subscriber] = struct{}{}
			}
		}
		oldestSeq := oldestPlanSeqLocked(run)
		gap := run.truncated && oldestSeq > 0 && subscriber.cursor < oldestSeq-1
		sendTruncated := false
		if gap {
			subscriber.cursor = oldestSeq - 1
			if !subscriber.truncated {
				subscriber.truncated = true
				sendTruncated = true
			}
		}
		line, hasLine := nextPlanLineLocked(run, subscriber.cursor)
		terminal := run.state != "running"
		done := planDoneLocked(run)
		s.planMu.Unlock()

		if sendTruncated {
			if err := writePlanEvent(w, flusher, "truncated", map[string]string{"runId": run.id}); err != nil {
				return
			}
			continue
		}
		if hasLine {
			payload := struct {
				RunID string `json:"runId"`
				planLine
			}{RunID: run.id, planLine: line}
			if err := writePlanEvent(w, flusher, "line", payload); err != nil {
				return
			}
			subscriber.cursor = line.Seq
			continue
		}
		if terminal {
			if err := writePlanEvent(w, flusher, "done", done); err != nil {
				return
			}
			return
		}
		select {
		case <-subscriber.notify:
		case <-r.Context().Done():
			return
		}
	}
}

func oldestPlanSeqLocked(run *planRun) int {
	if len(run.lines) == 0 {
		return 0
	}
	if run.truncated {
		return run.lines[run.lineStart].Seq
	}
	return run.lines[0].Seq
}

func nextPlanLineLocked(run *planRun, cursor int) (planLine, bool) {
	oldestSeq := oldestPlanSeqLocked(run)
	if oldestSeq == 0 || cursor >= run.seq {
		return planLine{}, false
	}
	nextSeq := cursor + 1
	if nextSeq < oldestSeq {
		nextSeq = oldestSeq
	}
	index := nextSeq - oldestSeq
	if run.truncated {
		index = (run.lineStart + index) % len(run.lines)
	}
	return run.lines[index], true
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
	if planCancelHandoff != nil {
		planCancelHandoff()
	}
	s.planMu.Lock()
	if s.plan == nil || s.plan.state != "running" {
		s.planMu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]string{"error": "no plan is running"})
		return
	}
	run := s.plan
	if !planProcessRunningLocked(run) {
		s.planMu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]string{"error": "plan is already finishing"})
		return
	}
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
		if errors.Is(signalErr, syscall.ESRCH) || errors.Is(signalErr, errProcessNotStarted) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "plan is already finishing"})
			return
		}
		message := signalName + ": " + signalErr.Error()
		s.appendPlanLine(run, "system", message)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": message, "runId": run.id})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"runId": run.id, "signal": signalName})
}

func planProcessRunningLocked(run *planRun) bool {
	return run.pid > 0 && (run.phase == planPhaseRunning || run.phase == planPhaseShowRunning)
}

func signalProcessGroup(pid int, signal syscall.Signal) error {
	if pid == 0 {
		return errProcessNotStarted
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
	running := run.state == "running"
	if running {
		run.cancelRequested = true
	}
	var signalErr error
	if running && planProcessRunningLocked(run) {
		if err := signalProcessGroup(run.pid, syscall.SIGINT); err == nil {
			run.intSent = true
		} else if !errors.Is(err, syscall.ESRCH) && !errors.Is(err, errProcessNotStarted) {
			signalErr = err
		}
	}
	s.planMu.Unlock()

	if running {
		timer := time.NewTimer(closeGrace)
		select {
		case <-done:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
			s.planMu.Lock()
			if run.state == "running" && planProcessRunningLocked(run) {
				if err := signalProcessGroup(run.pid, syscall.SIGKILL); err != nil &&
					!errors.Is(err, syscall.ESRCH) && !errors.Is(err, errProcessNotStarted) {
					signalErr = errors.Join(signalErr, err)
				}
			}
			s.planMu.Unlock()
			killTimer := time.NewTimer(killGrace)
			select {
			case <-done:
				if !killTimer.Stop() {
					select {
					case <-killTimer.C:
					default:
					}
				}
			case <-killTimer.C:
				s.planMu.Lock()
				abandon := run.state == "running"
				if abandon {
					run.pipesAbandoned = true
				}
				s.planMu.Unlock()
				if abandon {
					log.Printf("plan %s did not stop after SIGKILL; abandoning plan output and shutdown wait", run.id)
					signalErr = errors.Join(signalErr, run.closePipes())
				}
			}
		}
	}
	removeErr := os.RemoveAll(tempDir)
	return errors.Join(signalErr, removeErr)
}
