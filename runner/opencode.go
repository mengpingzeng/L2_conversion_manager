package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"clawstudios/pkg/logging"
	"session_manager/models"
)

type OpenCodeRunner struct {
	binaryPath string
	mu         sync.Mutex
	runningSID map[string]bool
}

type RunResult struct {
	SessionID string
	Events    []models.SessionEvent
	ExitCode  int
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
	select {
	case w.events <- evt:
	default:
	}
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

func (r *OpenCodeRunner) Run(ctx context.Context, opts RunOptions) (<-chan models.SessionEvent, error) {
	events := make(chan models.SessionEvent, 100)
	w := &streamWriter{events: events}

	go func() {
		defer close(events)
		defer w.close()

		logger := logging.FromContext(ctx)
		if logger == nil {
			logger = logging.NewLogger("OpenCodeRunner")
		}

		startTime := time.Now()

		var textBuf strings.Builder
		hasWrite := false

		args := r.buildArgs(opts)

		cmd := exec.CommandContext(ctx, r.binaryPath, args...)
		cmd.Dir = opts.CWD
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		defer func() {
			if cmd.Process != nil {
				syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
		}()

		if opts.ConfigPath != "" {
			cmd.Env = append(os.Environ(), "OPENCODE_CONFIG="+opts.ConfigPath)
		}
		if opts.DeepseekAPIKey != "" {
			cmd.Env = append(cmd.Env, "DEEPSEEK_API_KEY="+opts.DeepseekAPIKey)
		}

		stdout, err := cmd.StdoutPipe()
		if err != nil {
			w.send(models.SessionEvent{
				Type:  "error",
				Error: fmt.Sprintf("failed to get stdout: %v", err),
			})
			return
		}

		stderr, err := cmd.StderrPipe()
		if err != nil {
			w.send(models.SessionEvent{
				Type:  "error",
				Error: fmt.Sprintf("failed to get stderr: %v", err),
			})
			return
		}

		if err := cmd.Start(); err != nil {
			logger.Error(logging.ErrSessionError, "opencode start failed: cwd=%s model=%s err=%v", opts.CWD, opts.Model, err)
			w.send(models.SessionEvent{
				Type:  "error",
				Error: fmt.Sprintf("failed to start opencode: %v", err),
			})
			return
		}

		logger.Info("opencode process started: pid=%d cwd=%s model=%s", cmd.Process.Pid, opts.CWD, opts.Model)

		go func() {
			sc := bufio.NewScanner(stderr)
			for sc.Scan() {
				log.Printf("[opencode stderr] %s", sc.Text())
			}
		}()

		seq := 0
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024)

		capturedSID := opts.SessionID

		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}

			evt, err := parseOpenCodeEvent(line, capturedSID)
			if err != nil {
				log.Printf("[opencode parse] failed to parse line (len=%d, SID=%s): %v", len(line), capturedSID, err)
				continue
			}

			if evt.Type == "token" && evt.Text != "" {
				textBuf.WriteString(evt.Text)
			}
			if evt.Type == "tool_call" && isWriteTool(evt.Tool) {
				hasWrite = true
			}

			if capturedSID == "" && evt.SessionID != "" {
				capturedSID = evt.SessionID
				evt.SessionID = capturedSID
			} else if capturedSID != "" && evt.SessionID == "" {
				evt.SessionID = capturedSID
			}

			evt.Seq = seq
			seq++
			w.send(evt)
		}

		if err := scanner.Err(); err != nil {
			log.Printf("[opencode stdout] scanner error (SID=%s): %v", capturedSID, err)
		}

	if err := cmd.Wait(); err != nil {
		duration := time.Since(startTime)
		if ctx.Err() != nil {
			logger.Warn(logging.WarnSlowResponse, "opencode timeout/cancelled: pid=%d duration=%s", cmd.Process.Pid, duration)
			w.send(models.SessionEvent{
					Type:      "error",
					SessionID: capturedSID,
					Error:     "process timeout or cancelled",
				})
			} else {
				w.send(models.SessionEvent{
					Type:      "error",
					SessionID: capturedSID,
					Error:     fmt.Sprintf("opencode exited: %v", err),
				})
		}
	}

	duration := time.Since(startTime)
	logger.Info("opencode process exited: pid=%d duration=%s lines=%d has_write=%v",
		cmd.Process.Pid, duration, seq, hasWrite)

	if !hasWrite && textBuf.Len() > 0 {
			draftPath := filepath.Join(opts.CWD, "current_draft.md")
			if err := os.WriteFile(draftPath, []byte(textBuf.String()), 0644); err == nil {
				w.send(models.SessionEvent{
					Type:      "draft_updated",
					SessionID: capturedSID,
				})
			}
		}

		w.send(models.SessionEvent{
			Type:      "done",
			SessionID: capturedSID,
		})
	}()

	return events, nil
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

func (r *OpenCodeRunner) buildArgs(opts RunOptions) []string {
	args := []string{
		"run",
		"--format", "json",
		"--model", opts.Model,
		"--dir", opts.CWD,
	}

	if opts.SessionID != "" {
		args = append(args, "--session", opts.SessionID)
	}

	args = append(args, opts.Message)
	return args
}

type rawEvent struct {
	Type      string `json:"type"`
	Timestamp int64  `json:"timestamp"`
	SessionID string `json:"sessionID"`
	Part      struct {
		ID        string `json:"id"`
		MessageID string `json:"messageID"`
		SessionID string `json:"sessionID"`
		Type      string `json:"type"`
		Text      string `json:"text"`
		Tool      string `json:"tool"`
		CallID    string `json:"callID"`
		Reason    string `json:"reason"`
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
	} `json:"part"`
}

func isWriteTool(tool string) bool {
	lower := strings.ToLower(tool)
	return lower == "write" || lower == "write_file" || lower == "write_to_file" ||
		lower == "filewrite" || lower == "edit"
}

func extractDraftPath(input json.RawMessage) string {
	var m map[string]interface{}
	if err := json.Unmarshal(input, &m); err == nil {
		if fp := pickPath(m); fp != "" {
			return fp
		}
	} else {
		var s string
		if err := json.Unmarshal(input, &s); err == nil {
			var m2 map[string]interface{}
			if err := json.Unmarshal([]byte(s), &m2); err == nil {
				if fp := pickPath(m2); fp != "" {
					return fp
				}
			}
		}
	}
	return ""
}

func pickPath(m map[string]interface{}) string {
	for _, key := range []string{"filePath", "file_path", "path", "outputPath", "output_path"} {
		if v, ok := m[key]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

func parseOpenCodeEvent(line string, fallbackSID string) (models.SessionEvent, error) {
	var raw rawEvent
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return models.SessionEvent{}, err
	}

	sid := raw.SessionID
	if sid == "" {
		sid = raw.Part.SessionID
	}
	if sid == "" {
		sid = fallbackSID
	}

	switch raw.Type {
	case "step_start":
		return models.SessionEvent{
			Type:      "step_start",
			SessionID: sid,
		}, nil

	case "text":
		return models.SessionEvent{
			Type:      "token",
			SessionID: sid,
			Text:      raw.Part.Text,
		}, nil

	case "tool_use":
		evt := models.SessionEvent{
			Type:      "tool_call",
			SessionID: sid,
			Tool:      raw.Part.Tool,
			ToolArgs:  raw.Part.State.Input,
		}
		if raw.Part.State.Output != "" {
			evt.ToolResult = raw.Part.State.Output
		}
		if isWriteTool(raw.Part.Tool) && len(raw.Part.State.Input) > 0 {
			evt.DraftPath = extractDraftPath(raw.Part.State.Input)
		}
		return evt, nil

	case "step_finish":
		evt := models.SessionEvent{
			Type:      "step_finish",
			SessionID: sid,
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
		return evt, nil

	default:
		return models.SessionEvent{
			Type:      raw.Type,
			SessionID: sid,
		}, nil
	}
}


