package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"clawstudios/pkg/logging"
	"session_manager/chaterr"
	"session_manager/models"
)

type OpenCodeRunner struct {
	binaryPath string
	mu         sync.Mutex
	runningSID map[string]bool
}

type streamWriter struct {
	events chan<- models.SessionEvent
	mu     sync.Mutex
	closed bool
}

func (w *streamWriter) send(evt models.SessionEvent) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	w.events <- evt
}

func (w *streamWriter) close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
}

func NewOpenCodeRunner(binaryPath string) *OpenCodeRunner {
	return &OpenCodeRunner{
		binaryPath: binaryPath,
		runningSID: make(map[string]bool),
	}
}

type RunOptions struct {
	CWD            string
	Model          string
	SessionID      string
	Message        string
	Timeout        time.Duration
	ConfigPath     string
	DeepseekAPIKey string
}

func (r *OpenCodeRunner) Run(ctx context.Context, opts RunOptions) (<-chan models.SessionEvent, error) {
	events := make(chan models.SessionEvent, 50)
	w := &streamWriter{events: events}

	go func() {
		defer close(events)
		defer w.close()

		logger := logging.FromContext(ctx)
		if logger == nil {
			logger = logging.NewLogger("OpenCodeRunner")
		}

		startTime := time.Now()
		capturedSID := opts.SessionID

		args := []string{
			"run",
			"--format", "json",
			"--thinking",
			"--model", opts.Model,
		}
		if opts.SessionID != "" {
			args = append(args, "--session", opts.SessionID)
		}
		args = append(args, opts.Message)

		cmd := exec.CommandContext(ctx, r.binaryPath, args...)
		cmd.Dir = opts.CWD
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		defer func() {
			if cmd.Process != nil {
				syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
		}()

		cmd.Env = cleanEnv(os.Environ())
		if opts.ConfigPath != "" {
			cmd.Env = append(cmd.Env, "OPENCODE_CONFIG="+opts.ConfigPath)
		}
		if opts.DeepseekAPIKey != "" {
			cmd.Env = append(cmd.Env, "TEAM_DEEPSEEK_API_KEY="+opts.DeepseekAPIKey)
		}

		logger.Info("opencode launch: cwd=%s model=%s session_id=%s msg_len=%d config_path=%s deepseek_key_set=%v binary=%s",
			opts.CWD, opts.Model, opts.SessionID, len(opts.Message), opts.ConfigPath, opts.DeepseekAPIKey != "", r.binaryPath)
		msgPreview := opts.Message
		if len(msgPreview) > 300 {
			msgPreview = msgPreview[:300] + "..."
		}
		logger.Info("opencode message: %s", msgPreview)
		if entries, err := os.ReadDir(opts.CWD); err == nil {
			names := make([]string, 0, len(entries))
			for _, e := range entries {
				names = append(names, e.Name())
			}
			logger.Info("opencode cwd listing (%d entries): %v", len(names), names)
		} else {
			logger.Warn(logging.WarnProcessStuck, "opencode cwd read error: %v", err)
		}

		stdout, err := cmd.StdoutPipe()
		if err != nil {
			w.send(models.SessionEvent{Type: "error", Error: chaterr.UserFacing(fmt.Sprintf("stdout pipe: %v", err))})
			return
		}
		stderrPipe, err := cmd.StderrPipe()
		if err != nil {
			w.send(models.SessionEvent{Type: "error", Error: chaterr.UserFacing(fmt.Sprintf("stderr pipe: %v", err))})
			return
		}

		if err := cmd.Start(); err != nil {
			logger.Error(logging.ErrSessionError, "opencode start failed: cwd=%s model=%s err=%v", opts.CWD, opts.Model, err)
			w.send(models.SessionEvent{Type: "error", Error: chaterr.UserFacing(fmt.Sprintf("start failed: %v", err))})
			return
		}

		logger.Info("opencode started: pid=%d cwd=%s model=%s", cmd.Process.Pid, opts.CWD, opts.Model)
		w.send(models.SessionEvent{Type: "step_start", SessionID: capturedSID})

		draftPath := filepath.Join(opts.CWD, "current_draft.md")
		var lastDraftMod time.Time
		var lastDraftSize int64
		if st, err := os.Stat(draftPath); err == nil {
			lastDraftMod = st.ModTime()
			lastDraftSize = st.Size()
		}

		draftStop := make(chan struct{})
		go func() {
			ticker := time.NewTicker(3 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-draftStop:
					return
				case <-ticker.C:
					st, err := os.Stat(draftPath)
					if err != nil {
						continue
					}
					if st.ModTime().After(lastDraftMod) || st.Size() != lastDraftSize {
						lastDraftMod = st.ModTime()
						lastDraftSize = st.Size()
						w.send(models.SessionEvent{
							Type:      "draft_updated",
							SessionID: capturedSID,
						})
					}
				}
			}
		}()

		stdoutDone := make(chan struct{})
		stderrDone := make(chan struct{})
		stdoutLineCount := 0
		var stderrData []byte

		go func() {
			defer close(stdoutDone)
			scanner := bufio.NewScanner(stdout)
			scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024)
			for scanner.Scan() {
				stdoutLineCount++
				line := scanner.Text()
				if stdoutLineCount <= 20 {
					truncated := line
					if len(truncated) > 500 {
						truncated = truncated[:500] + "...(truncated)"
					}
					logger.Info("opencode stdout[%d]: %s", stdoutLineCount, truncated)
				}

				var raw openCodeRawEvent
				if err := json.Unmarshal([]byte(line), &raw); err != nil {
					continue
				}

				switch raw.Type {
				case "step_start":
					w.send(models.SessionEvent{
						Type:      "step_start",
						SessionID: capturedSID,
					})

				case "reasoning":
					w.send(models.SessionEvent{
						Type:      "reasoning",
						SessionID: capturedSID,
						Text:      raw.Part.Text,
					})

			case "tool_use":
				w.send(models.SessionEvent{
					Type:       "tool_call",
					SessionID:  capturedSID,
					Tool:       raw.Part.Tool,
					ToolResult: raw.Part.State.Output,
				})

				case "text":
					w.send(models.SessionEvent{
						Type:      "step_finish",
						SessionID: capturedSID,
						Text:      raw.Part.Text,
					})

				case "step_finish":
					evt := models.SessionEvent{
						Type:      "step_finish",
						SessionID: capturedSID,
						Reason:    raw.Part.Reason,
					}
					if raw.Part.Tokens.Total > 0 {
						evt.Tokens = &models.TokenInfo{
							Total:     raw.Part.Tokens.Total,
							Input:     raw.Part.Tokens.Input,
							Output:    raw.Part.Tokens.Output,
							Reasoning: raw.Part.Tokens.Reasoning,
						}
					}
					w.send(evt)
				}
			}
			if err := scanner.Err(); err != nil {
				logger.Warn(logging.WarnProcessStuck, "opencode stdout scanner error: %v", err)
			}
			logger.Info("opencode stdout done: total_lines=%d", stdoutLineCount)
		}()

		go func() {
			defer close(stderrDone)
			data, _ := io.ReadAll(stderrPipe)
			stderrData = data
			if len(data) > 0 {
				stderrStr := string(data)
				if len(stderrStr) > 2000 {
					stderrStr = stderrStr[:2000] + "...(truncated)"
				}
				logger.Info("opencode stderr (%d bytes): %s", len(data), strings.TrimSpace(stderrStr))
			} else {
				logger.Info("opencode stderr: (empty)")
			}
		}()

		hadError := false
		exitCode := 0
		if err := cmd.Wait(); err != nil {
			hadError = true
			if exitErr, ok := err.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			} else {
				exitCode = -1
			}
			if ctx.Err() != nil {
				logger.Warn(logging.WarnSlowResponse, "opencode timeout/cancelled: pid=%d duration=%s", cmd.Process.Pid, time.Since(startTime))
				w.send(models.SessionEvent{Type: "error", SessionID: capturedSID, Error: chaterr.UserFacing("process timeout or cancelled")})
			} else {
				logger.Error(logging.ErrSessionError, "opencode exited with error: pid=%d exit_code=%d err=%v", cmd.Process.Pid, exitCode, err)
				w.send(models.SessionEvent{Type: "error", SessionID: capturedSID, Error: chaterr.UserFacing(fmt.Sprintf("opencode exited: %v", err))})
			}
		}
		close(draftStop)

		<-stdoutDone
		<-stderrDone

		duration := time.Since(startTime)
		draftSize := int64(0)
		if st, err := os.Stat(draftPath); err == nil {
			draftSize = st.Size()
		}

		if draftSize == 0 {
			parentDraftPath := filepath.Join(filepath.Dir(opts.CWD), "current_draft.md")
			if st, err := os.Stat(parentDraftPath); err == nil && st.Size() > 0 {
				logger.Warn(logging.WarnProcessStuck,
					"misplaced draft detected, moving: from=%s to=%s size=%d",
					parentDraftPath, draftPath, st.Size())
				if data, err := os.ReadFile(parentDraftPath); err == nil {
					if err := os.WriteFile(draftPath, data, 0644); err == nil {
						draftSize = st.Size()
						_ = os.Remove(parentDraftPath)
					} else {
						logger.Warn(logging.WarnProcessStuck,
							"failed to copy misplaced draft: from=%s err=%v", parentDraftPath, err)
					}
				}
			}
		}

		logger.Info("opencode done: pid=%d duration=%s draft_size=%d had_error=%v exit_code=%d stdout_lines=%d stderr_bytes=%d",
			cmd.Process.Pid, duration, draftSize, hadError, exitCode, stdoutLineCount, len(stderrData))

		if !hadError && draftSize > 0 {
			w.send(models.SessionEvent{
				Type:      "draft_updated",
				SessionID: capturedSID,
			})
		}

		if !hadError {
			w.send(models.SessionEvent{
				Type:      "done",
				SessionID: capturedSID,
				DraftSize: draftSize,
			})
		}
	}()

	return events, nil
}

func cleanEnv(env []string) []string {
	var filtered []string
	for _, e := range env {
		if strings.HasPrefix(e, "OPENCODE=") ||
			strings.HasPrefix(e, "OPENCODE_PROCESS_ROLE=") ||
			strings.HasPrefix(e, "OPENCODE_PID=") ||
			strings.HasPrefix(e, "OPENCODE_RUN_ID=") ||
			strings.HasPrefix(e, "TEAM_DEEPSEEK_API_KEY=") ||
			strings.HasPrefix(e, "DEEPSEEK_API_KEY=") ||
			strings.HasPrefix(e, "HY3_API_KEY=") ||
			strings.HasPrefix(e, "TEAM_HY3_API_KEY=") {
			continue
		}
		filtered = append(filtered, e)
	}
	return filtered
}

type openCodeRawEvent struct {
	Type      string          `json:"type"`
	Timestamp int64           `json:"timestamp"`
	SessionID string          `json:"sessionID"`
	Part      openCodeRawPart `json:"part"`
}

type openCodeRawPart struct {
	ID        string `json:"id"`
	MessageID string `json:"messageID"`
	SessionID string `json:"sessionID"`
	Type      string `json:"type"`
	Text      string `json:"text"`
	Reason    string `json:"reason"`
	Tool      string `json:"tool"`
	CallID    string `json:"callID"`
	State     struct {
		Status string          `json:"status"`
		Input  json.RawMessage `json:"input"`
		Output string          `json:"output"`
	} `json:"state"`
	Tokens struct {
		Total     int `json:"total"`
		Input     int `json:"input"`
		Output    int `json:"output"`
		Reasoning int `json:"reasoning"`
	} `json:"tokens"`
}

