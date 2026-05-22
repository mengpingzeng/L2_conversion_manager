package manager

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"session_manager/adapter"
	"session_manager/models"
	"session_manager/pool"
	"session_manager/runner"
	"session_manager/store"

	"github.com/google/uuid"
)

type Config struct {
	DataDir             string
	OpenCodeBinary      string
	DefaultModel        string
	MaxConcurrent       int
	DefaultTimeoutSec   int
	MaxMessagesPerEpoch int
	MaxTokensPerEpoch   int
	StaleTimeoutMin     int
}

func DefaultConfig() Config {
	return Config{
		DataDir:             "/tmp/session_manager",
		OpenCodeBinary:      "opencode",
		DefaultModel:        "team-deepseek/deepseek-chat",
		MaxConcurrent:       3,
		DefaultTimeoutSec:   300,
		MaxMessagesPerEpoch: 40,
		MaxTokensPerEpoch:   60000,
		StaleTimeoutMin:     60,
	}
}

type SessionManager struct {
	cfg        Config
	configPath string
	store      *store.Store
	pool       *pool.ProcessPool
	runner     *runner.OpenCodeRunner
	skills     map[string]adapter.SkillDef

	mu          sync.RWMutex
	subscribers map[string][]chan models.SessionEvent

	stopCh chan struct{}
}

func New(cfg Config) (*SessionManager, error) {
	if cfg.OpenCodeBinary == "" {
		cfg.OpenCodeBinary = "opencode"
	}
	if cfg.DefaultModel == "" {
		cfg.DefaultModel = "team-deepseek/deepseek-chat"
	}
	if cfg.MaxConcurrent == 0 {
		cfg.MaxConcurrent = 3
	}
	if cfg.DefaultTimeoutSec == 0 {
		cfg.DefaultTimeoutSec = 300
	}
	if cfg.StaleTimeoutMin == 0 {
		cfg.StaleTimeoutMin = models.DefaultStaleTimeoutMin
	}

	s, err := store.NewStore(cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("init store: %w", err)
	}

	p := pool.New(cfg.MaxConcurrent)
	ocRunner := runner.NewOpenCodeRunner(cfg.OpenCodeBinary)

	skills := make(map[string]adapter.SkillDef)
	for k, v := range adapter.PrebuiltSkills {
		skills[k] = v
	}

	sm := &SessionManager{
		cfg:         cfg,
		store:       s,
		pool:        p,
		runner:      ocRunner,
		skills:      skills,
		subscribers: make(map[string][]chan models.SessionEvent),
		stopCh:      make(chan struct{}),
	}

	if err := sm.initOpenCodeConfig(); err != nil {
		log.Printf("WARN: failed to init opencode config: %v", err)
	}

	go sm.scanStaleTasks()

	return sm, nil
}

func (sm *SessionManager) Stop() {
	close(sm.stopCh)
}

func (sm *SessionManager) initOpenCodeConfig() error {
	configPath := filepath.Join(sm.cfg.DataDir, "opencode_config.json")

	skillsDir := sm.store.SkillsDir()

	existing := make(map[string]interface{})
	if data, err := os.ReadFile(configPath); err == nil {
		json.Unmarshal(data, &existing)
	}

	existing["permission"] = map[string]interface{}{
		"edit":               "allow",
		"bash":               "deny",
		"write":              "allow",
		"read":               "allow",
		"external_directory": "deny",
		"doom_loop":          "allow",
	}
	existing["skills"] = map[string]interface{}{
		"paths": []string{skillsDir},
	}

	data, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal opencode config: %w", err)
	}

	sm.configPath = configPath
	return os.WriteFile(configPath, data, 0644)
}

func (sm *SessionManager) scanStaleTasks() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-sm.stopCh:
			return
		case <-ticker.C:
			sm.cleanStaleTasks()
		}
	}
}

func (sm *SessionManager) cleanStaleTasks() {
	tasks := sm.store.ListTasks()
	now := time.Now()
	timeout := time.Duration(sm.cfg.StaleTimeoutMin) * time.Minute

	for _, t := range tasks {
		if t.ActiveSessionID != "" {
			if now.Sub(t.LastActiveAt) > timeout {
				log.Printf("auto-archiving stale session %s for task %s", t.ActiveSessionID, t.TaskID)
				if err := sm.Close(context.Background(), t.ActiveSessionID); err != nil {
					log.Printf("WARN: failed to auto-archive %s: %v", t.ActiveSessionID, err)
				}
			}
		}
	}
}

func (sm *SessionManager) findSession(sessionID string) (*models.Session, string, error) {
	tasks := sm.store.ListTasks()
	for _, t := range tasks {
		sess, err := sm.store.GetSession(t.TaskID, sessionID)
		if err == nil {
			return sess, t.TaskID, nil
		}
	}
	return nil, "", fmt.Errorf("session not found: %s", sessionID)
}

func (sm *SessionManager) checkTaskConcurrency(taskID string, now time.Time) error {
	task, err := sm.store.GetTask(taskID)
	if err != nil {
		return nil
	}

	if task.ActiveSessionID == "" {
		return nil
	}

	timeout := time.Duration(sm.cfg.StaleTimeoutMin) * time.Minute
	if now.Sub(task.LastActiveAt) > timeout {
		log.Printf("stale session %s detected for task %s, auto-archiving before create",
			task.ActiveSessionID, taskID)
		if err := sm.Close(context.Background(), task.ActiveSessionID); err != nil {
			log.Printf("WARN: auto-archive %s failed: %v", task.ActiveSessionID, err)
		}
		return nil
	}

	return &models.TaskBusyError{
		TaskID:    taskID,
		SessionID: task.ActiveSessionID,
		Message:   fmt.Sprintf("task %s already has active session %s", taskID, task.ActiveSessionID),
	}
}

func (sm *SessionManager) Create(ctx context.Context, req models.CreateSessionRequest) (*models.Session, error) {
	if req.TaskID == "" {
		return nil, fmt.Errorf("task_id is required")
	}
	if req.Topic == "" {
		return nil, fmt.Errorf("topic is required")
	}
	if req.SkillID == "" {
		req.SkillID = "general_fallback_v1"
	}

	skill, ok := sm.skills[req.SkillID]
	if !ok {
		return nil, fmt.Errorf("skill not found: %s", req.SkillID)
	}

	now := time.Now()
	if err := sm.checkTaskConcurrency(req.TaskID, now); err != nil {
		return nil, err
	}

	model := req.Model
	if model == "" {
		model = sm.cfg.DefaultModel
	}
	switch model {
	case "hy3/hy3-preview":
		model = "opencode/big-pickle"
	default:
		model = strings.Replace(model, "deepseek/", "team-deepseek/", 1)
	}

	task, isNew, err := sm.store.GetOrCreateTask(req.TaskID, req.Topic, req.UID, req.MemoryModel, req.Platform, req.SkillID, model, req.AccountID)
	if err != nil {
		return nil, fmt.Errorf("init task: %w", err)
	}

	sessionID := uuid.New().String()[:8]

	ok, existingSID, err := sm.store.TrySetActiveSession(req.TaskID, sessionID)
	if err != nil {
		return nil, fmt.Errorf("set active session: %w", err)
	}
	if !ok {
		return nil, &models.TaskBusyError{
			TaskID:    req.TaskID,
			SessionID: existingSID,
			Message:   fmt.Sprintf("task %s already has active session %s", req.TaskID, existingSID),
		}
	}

	skillDirName := "skill_" + sessionID
	if _, err := adapter.WriteSkillFile(sm.store.SkillsDir(), skillDirName, skill); err != nil {
		return nil, fmt.Errorf("write skill: %w", err)
	}

	cwd, err := sm.store.CreateSessionDir(req.TaskID, sessionID)
	if err != nil {
		return nil, fmt.Errorf("create cwd: %w", err)
	}

	sess := &models.Session{
		SessionID:    sessionID,
		TaskID:       req.TaskID,
		Topic:        req.Topic,
		SkillID:      req.SkillID,
		Model:        model,
		Status:       models.StatusCreated,
		CWDPath:      cwd,
		MessageCount: 0,
		TotalTokens:  0,
		DraftVersion: req.DraftVersion,
		CreatedAt:    now,
		LastActiveAt: now,
	}

	if err := sm.store.UpsertSessionInTask(sess); err != nil {
		return nil, fmt.Errorf("save session: %w", err)
	}

	task, _ = sm.store.GetTask(req.TaskID)
	task.LastActiveAt = now
	if isNew {
		task.SessionIDs = []string{sessionID}
		task.SessionCount = 1
	} else {
		task.SessionIDs = append(task.SessionIDs, sessionID)
		task.SessionCount = len(task.SessionIDs)
	}
	if req.MemoryModel != "" && task.MemoryModel == "" {
		task.MemoryModel = req.MemoryModel
	}
	_ = sm.store.UpdateTask(task)

	hasContext := sm.store.HasTaskContext(req.TaskID)
	if !isNew && hasContext {
		sm.injectTaskContext(cwd, req.TaskID)
	}

	msg := req.InitialMsg
	if msg == "" {
		if !isNew && hasContext {
			msg = adapter.BuildWakeMessage(req.Topic, skill, "", true, true)
		} else {
			msg = adapter.BuildInitialMessage(req.Topic, skill)
		}
	}

	go sm.runSessionLoop(context.Background(), sessionID, req.TaskID, cwd, model, msg, "")

	return sess, nil
}

func (sm *SessionManager) injectTaskContext(cwd, taskID string) {
	shortData, errShort := sm.store.ReadShortTerm(taskID)
	medData, errMed := sm.store.ReadMediumTerm(taskID)

	if errShort == nil && len(shortData) > 0 {
		path := filepath.Join(cwd, "RECENT_DRAFTS.md")
		if err := os.WriteFile(path, shortData, 0644); err != nil {
			log.Printf("WARN: failed to write RECENT_DRAFTS.md: %v", err)
		}
	}

	if errMed == nil && len(medData) > 0 {
		path := filepath.Join(cwd, "HISTORY_SUMMARY.md")
		if err := os.WriteFile(path, medData, 0644); err != nil {
			log.Printf("WARN: failed to write HISTORY_SUMMARY.md: %v", err)
		}
	}
}

func (sm *SessionManager) Send(ctx context.Context, sessionID string, req models.SendMessageRequest) error {
	sess, taskID, err := sm.findSession(sessionID)
	if err != nil {
		return err
	}

	if sess.Status == models.StatusArchived {
		return fmt.Errorf("session is archived: %s", sessionID)
	}

	if sess.Status == models.StatusCold {
		return fmt.Errorf("session is cold, use wake first: %s", sessionID)
	}

	sess.LastActiveAt = time.Now()
	sess.DraftVersion = req.DraftVersion
	_ = sm.store.UpsertSessionInTask(sess)

	task, err := sm.store.GetTask(taskID)
	if err == nil {
		task.LastActiveAt = sess.LastActiveAt
		_ = sm.store.UpdateTask(task)
	}

	go sm.runSessionLoop(context.Background(), sessionID, taskID, sess.CWDPath, sess.Model, req.Text, sess.OpenCodeSID)

	return nil
}

func (sm *SessionManager) runSessionLoop(ctx context.Context, sessionID, taskID, cwd, model, message, ocSID string) {
	if err := sm.pool.Acquire(ctx); err != nil {
		log.Printf("pool acquire failed for %s: %v", sessionID, err)
		sm.broadcast(sessionID, models.SessionEvent{
			Type:      "error",
			SessionID: sessionID,
			TaskID:    taskID,
			Error:     "server busy, please retry later",
		})
		return
	}
	defer sm.pool.Release()

	timeout := time.Duration(sm.cfg.DefaultTimeoutSec) * time.Second
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	opts := runner.RunOptions{
		CWD:        cwd,
		Model:      model,
		SessionID:  ocSID,
		Message:    message,
		Timeout:    timeout,
		ConfigPath: sm.configPath,
	}

	events, err := sm.runner.Run(runCtx, opts)
	if err != nil {
		sm.broadcast(sessionID, models.SessionEvent{
			Type:      "error",
			SessionID: sessionID,
			TaskID:    taskID,
			Error:     fmt.Sprintf("failed to start opencode: %v", err),
		})
		return
	}

	msgCount := 0
	totalTokens := 0
	capturedSID := ocSID
	var textBuf strings.Builder

	for evt := range events {
		evt.SessionID = sessionID
		evt.TaskID = taskID

		if evt.Type == "token" {
			textBuf.WriteString(evt.Text)
		}

		if evt.Type == "step_start" && capturedSID == "" {
			sess, err := sm.store.GetSession(taskID, sessionID)
			if err == nil && sess.OpenCodeSID == "" {
				_ = sm.store.UpsertSessionInTask(sess)
			}
		}

		if evt.Type == "step_finish" && evt.Tokens != nil {
			totalTokens += evt.Tokens.Total
		}

		if evt.Type == "tool_call" && evt.DraftPath != "" && evt.ToolResult != "" {
			sm.broadcast(sessionID, models.SessionEvent{
				Type:      "draft_updated",
				SessionID: sessionID,
				TaskID:    taskID,
				DraftPath: evt.DraftPath,
			})
		}

		if (evt.Type == "step_finish" || evt.Type == "done") && textBuf.Len() > 0 {
			cwd := sm.store.GetSessionCWDDir(taskID, sessionID)
			draftPath := filepath.Join(cwd, "current_draft.md")
			newContent := []byte(textBuf.String())
			// 不覆盖更长的已有内容（AI 可能在后续回复中输出短确认消息，覆盖掉已写好的章节正文）
			write := true
			if existing, err := os.ReadFile(draftPath); err == nil && len(existing) > len(newContent) {
				write = false
			}
			if write {
				if err := os.WriteFile(draftPath, newContent, 0644); err == nil {
					sm.broadcast(sessionID, models.SessionEvent{
						Type:      "draft_updated",
						SessionID: sessionID,
						TaskID:    taskID,
						DraftPath: draftPath,
					})
				}
			}
		}

		if evt.Type == "token" || evt.Type == "tool_call" || evt.Type == "step_finish" ||
			evt.Type == "done" || evt.Type == "error" || evt.Type == "draft_updated" {
			sm.broadcast(sessionID, evt)
		}
	}

	msgCount++
	sess, err := sm.store.GetSession(taskID, sessionID)
	if err == nil {
		fresh, freshErr := sm.store.GetSession(taskID, sessionID)
		if freshErr == nil {
			sess = fresh
		}

		if sess.Status == models.StatusArchived {
			sess.MessageCount += msgCount
			sess.TotalTokens += totalTokens
			sess.LastActiveAt = time.Now()
		} else {
			sess.MessageCount += msgCount
			sess.TotalTokens += totalTokens
			sess.LastActiveAt = time.Now()
			sess.Status = models.StatusWarm
		}
		if capturedSID != "" && sess.OpenCodeSID == "" {
			sess.OpenCodeSID = capturedSID
		}
		_ = sm.store.UpsertSessionInTask(sess)

		if sess.DraftVersion > 0 {
			sm.saveDraftVersion(taskID, sessionID, sess.DraftVersion)
		}

		if sess.MessageCount >= sm.cfg.MaxMessagesPerEpoch || sess.TotalTokens >= sm.cfg.MaxTokensPerEpoch {
			log.Printf("session %s reached archive threshold: msgs=%d tokens=%d, auto-archiving",
				sessionID, sess.MessageCount, sess.TotalTokens)
			sm.Close(context.Background(), sessionID)
		}
	}

	task, err := sm.store.GetTask(taskID)
	if err == nil {
		task.LastActiveAt = time.Now()
		_ = sm.store.UpdateTask(task)
	}
}

func (sm *SessionManager) saveDraftVersion(taskID, sessionID string, version int) {
	cwd := sm.store.GetSessionCWDDir(taskID, sessionID)
	currentDraft := filepath.Join(cwd, "current_draft.md")
	data, err := os.ReadFile(currentDraft)
	if err != nil {
		log.Printf("WARN: failed to read current_draft.md for versioning: %v", err)
		return
	}
	if err := sm.store.SaveDraftVersion(taskID, version, string(data)); err != nil {
		log.Printf("WARN: failed to save draft_v%d.md: %v", version, err)
	}
}

func (sm *SessionManager) Close(ctx context.Context, sessionID string) error {
	sess, taskID, err := sm.findSession(sessionID)
	if err != nil {
		return err
	}

	if sess.Status == models.StatusArchived {
		return nil
	}

	episodeID := uuid.New().String()[:8]
	epochNo := len(sess.Episodes) + 1

	cwd := sm.store.GetSessionCWDDir(taskID, sessionID)
	currentDraft := filepath.Join(cwd, "current_draft.md")
	var summaryContent string
	var draftData []byte
	if data, err := os.ReadFile(currentDraft); err == nil {
		draftData = data
		summaryContent = fmt.Sprintf("# Session Episode %d\n\n## Latest Draft\n\n%s\n\n## Metadata\n- Session: %s\n- Task: %s\n- Skill: %s\n- Model: %s\n- Messages: %d\n- Total Tokens: %d\n",
			epochNo, string(data), sessionID, taskID, sess.SkillID, sess.Model, sess.MessageCount, sess.TotalTokens)
	} else {
		summaryContent = fmt.Sprintf("# Session Episode %d\n\n(No draft available)\n\n## Metadata\n- Session: %s\n- Task: %s\n- Skill: %s\n- Model: %s\n- Messages: %d\n- Total Tokens: %d\n",
			epochNo, sessionID, taskID, sess.SkillID, sess.Model, sess.MessageCount, sess.TotalTokens)
	}

	summaryPath, err := sm.store.SaveSummary(taskID, sessionID, epochNo, summaryContent)
	if err != nil {
		log.Printf("WARN: failed to save summary: %v", err)
		summaryPath = ""
	}

	episode := models.Episode{
		EpisodeID:    episodeID,
		SessionID:    sessionID,
		EpochNo:      epochNo,
		UserIntent:   sess.Topic,
		Decisions:    summaryContent,
		SummaryPath:  summaryPath,
		MessageCount: sess.MessageCount,
		CreatedAt:    time.Now(),
	}

	sess.Episodes = append(sess.Episodes, episode)
	now := time.Now()
	sess.ArchivedAt = &now
	sess.Status = models.StatusArchived

	if err := sm.store.UpsertSessionInTask(sess); err != nil {
		return err
	}

	skillDirName := "skill_" + sessionID
	skillDir := sm.store.SkillDir(skillDirName)
	os.RemoveAll(skillDir)

	task, err := sm.store.GetTask(taskID)
	if err == nil {
		if task.ActiveSessionID == sessionID {
			task.ActiveSessionID = ""
			_ = sm.store.UpdateTask(task)
		}
	}

	sm.broadcast(sessionID, models.SessionEvent{
		Type:      "session_closed",
		SessionID: sessionID,
		TaskID:    taskID,
	})

	if len(draftData) > 0 {
		go sm.generateMediumSummary(taskID, sessionID, task, sess.SkillID, draftData)
	}

	return nil
}

func (sm *SessionManager) generateMediumSummary(taskID, sessionID string, task *models.Task, skillID string, draftData []byte) {
	summaryModel := sm.cfg.DefaultModel
	if task != nil && task.MemoryModel != "" {
		summaryModel = task.MemoryModel
	}

	prompt := adapter.BuildSummaryPrompt(string(draftData))

	tmpDir, err := os.MkdirTemp("", "sm_summary_*")
	if err != nil {
		log.Printf("WARN: failed to create temp dir for summary: %v", err)
		return
	}
	defer os.RemoveAll(tmpDir)

	summaryCtx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	opts := runner.RunOptions{
		CWD:        tmpDir,
		Model:      summaryModel,
		SessionID:  "",
		Message:    prompt,
		Timeout:    120 * time.Second,
		ConfigPath: sm.configPath,
	}

	events, err := sm.runner.Run(summaryCtx, opts)
	if err != nil {
		log.Printf("WARN: failed to run summary opencode: %v", err)
		return
	}

	var fullText strings.Builder
	for evt := range events {
		if evt.Type == "token" {
			fullText.WriteString(evt.Text)
		}
	}

	text := strings.TrimSpace(fullText.String())
	if text == "" {
		log.Printf("WARN: empty summary response for session %s", sessionID)
		return
	}

	text = stripMarkdownCodeFences(text)

	var memSummary models.MemorySummary
	if err := json.Unmarshal([]byte(text), &memSummary); err != nil {
		log.Printf("WARN: failed to parse summary JSON: %v, raw: %s", err, text[:min(len(text), 200)])
		return
	}

	formatted := adapter.FormatMemorySummary(
		memSummary.Topic,
		memSummary.Intent,
		memSummary.Summary,
		memSummary.KeyDecisions,
		memSummary.DraftPreview,
	)

	if err := sm.store.AppendMediumTerm(taskID, formatted); err != nil {
		log.Printf("WARN: failed to append medium_term: %v", err)
	}

	sessions, _ := sm.store.LoadTaskSessions(taskID)
	if sessions != nil && len(sessions) > 0 {
		if err := sm.store.UpdateShortTerm(taskID, sessions); err != nil {
			log.Printf("WARN: failed to update short_term: %v", err)
		}
	}
}

func (sm *SessionManager) WakeTask(ctx context.Context, taskID string, req models.WakeTaskRequest) (*models.Session, error) {
	task, err := sm.store.GetTask(taskID)
	if err != nil {
		return nil, fmt.Errorf("task not found: %s", taskID)
	}

	now := time.Now()
	if err := sm.checkTaskConcurrency(taskID, now); err != nil {
		return nil, err
	}

	skillID := "general_fallback_v1"
	if len(task.SessionIDs) > 0 {
		sessions, _ := sm.store.LoadTaskSessions(taskID)
		if sessions != nil && len(sessions) > 0 {
			lastSession := sessions[len(sessions)-1]
			if lastSession.SkillID != "" {
				skillID = lastSession.SkillID
			}
		}
	}

	skill, ok := sm.skills[skillID]
	if !ok {
		skill = sm.skills["general_fallback_v1"]
	}

	sessionID := uuid.New().String()[:8]

	ok, existingSID, err := sm.store.TrySetActiveSession(taskID, sessionID)
	if err != nil {
		return nil, fmt.Errorf("set active session: %w", err)
	}
	if !ok {
		return nil, &models.TaskBusyError{
			TaskID:    taskID,
			SessionID: existingSID,
			Message:   fmt.Sprintf("task %s already has active session %s", taskID, existingSID),
		}
	}

	skillDirName := "skill_" + sessionID
	if _, err := adapter.WriteSkillFile(sm.store.SkillsDir(), skillDirName, skill); err != nil {
		return nil, fmt.Errorf("write skill: %w", err)
	}

	cwd, err := sm.store.CreateSessionDir(taskID, sessionID)
	if err != nil {
		return nil, fmt.Errorf("create cwd: %w", err)
	}

	sm.injectTaskContext(cwd, taskID)

	model := sm.cfg.DefaultModel

	sess := &models.Session{
		SessionID:    sessionID,
		TaskID:       taskID,
		Topic:        task.Topic,
		SkillID:      skillID,
		Model:        model,
		Status:       models.StatusCreated,
		CWDPath:      cwd,
		MessageCount: 0,
		TotalTokens:  0,
		DraftVersion: req.DraftVersion,
		CreatedAt:    now,
		LastActiveAt: now,
	}

	if err := sm.store.UpsertSessionInTask(sess); err != nil {
		return nil, fmt.Errorf("save session: %w", err)
	}

	task, _ = sm.store.GetTask(taskID)
	task.LastActiveAt = now
	task.SessionIDs = append(task.SessionIDs, sessionID)
	task.SessionCount = len(task.SessionIDs)
	_ = sm.store.UpdateTask(task)

	hasShort := sm.store.HasTaskContext(taskID)
	hasMed := hasShort

	msg := adapter.BuildWakeMessage(task.Topic, skill, req.Text, hasShort, hasMed)

	go sm.runSessionLoop(context.Background(), sessionID, taskID, cwd, model, msg, "")

	return sess, nil
}

func (sm *SessionManager) GetSession(sessionID string) (*models.Session, string, error) {
	return sm.findSession(sessionID)
}

func (sm *SessionManager) ListSessions(taskID string) ([]*models.Session, error) {
	return sm.store.LoadTaskSessions(taskID)
}

func (sm *SessionManager) ListAllSessions() []*models.Session {
	return sm.store.ListAllSessions()
}

func (sm *SessionManager) ListTasks() []models.TaskInfo {
	tasks := sm.store.ListTasks()
	return sm.tasksToInfo(tasks)
}

func (sm *SessionManager) ListTasksByUID(uid string) []models.TaskInfo {
	tasks := sm.store.ListTasksByUID(uid)
	return sm.tasksToInfo(tasks)
}

func (sm *SessionManager) tasksToInfo(tasks []*models.Task) []models.TaskInfo {
	var result []models.TaskInfo
	now := time.Now()
	timeout := time.Duration(sm.cfg.StaleTimeoutMin) * time.Minute

	for _, t := range tasks {
		stale := false
		if t.ActiveSessionID != "" && now.Sub(t.LastActiveAt) > timeout {
			stale = true
		}
		result = append(result, models.TaskInfo{
			TaskID:                t.TaskID,
			UID:                   t.UID,
			Topic:                 t.Topic,
			Platform:              t.Platform,
			SkillID:               t.SkillID,
			Model:                 t.Model,
			MemoryModel:           t.MemoryModel,
			ActiveSessionID:       t.ActiveSessionID,
			LastActiveAt:          t.LastActiveAt,
			CreatedAt:             t.CreatedAt,
			SessionCount:          t.SessionCount,
			MemoryVersion:         t.MemoryVersion,
			Stale:                 stale,
			NovelName:             t.NovelName,
			AccountID:             t.AccountID,
			PublishedChapterCount: t.PublishedChapterCount,
		})
	}
	return result
}

func (sm *SessionManager) GetTask(taskID string) (*models.TaskInfo, error) {
	task, err := sm.store.GetTask(taskID)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	timeout := time.Duration(sm.cfg.StaleTimeoutMin) * time.Minute
	stale := task.ActiveSessionID != "" && now.Sub(task.LastActiveAt) > timeout

	return &models.TaskInfo{
		TaskID:                task.TaskID,
		UID:                   task.UID,
		Topic:                 task.Topic,
		Platform:              task.Platform,
		SkillID:               task.SkillID,
		Model:                 task.Model,
		MemoryModel:           task.MemoryModel,
		ActiveSessionID:       task.ActiveSessionID,
		LastActiveAt:          task.LastActiveAt,
		CreatedAt:             task.CreatedAt,
		SessionCount:          task.SessionCount,
		MemoryVersion:         task.MemoryVersion,
		Stale:                 stale,
		NovelName:             task.NovelName,
		AccountID:             task.AccountID,
		PublishedChapterCount: task.PublishedChapterCount,
	}, nil
}

func (sm *SessionManager) ListSkills() []models.SkillInfo {
	var result []models.SkillInfo
	for _, s := range sm.skills {
		result = append(result, models.SkillInfo{
			ID:               s.ID,
			Name:             s.Name,
			Description:      s.Description,
			Category:         s.Category,
			RecommendedFor:   s.Description,
			ModelRecommended: s.ModelRecommended,
			TargetPlatforms:  s.TargetPlatforms,
		})
	}
	return result
}

func (sm *SessionManager) PoolStatus() string {
	return sm.pool.Status()
}

func (sm *SessionManager) Subscribe(sessionID string) chan models.SessionEvent {
	ch := make(chan models.SessionEvent, 100)
	sm.mu.Lock()
	sm.subscribers[sessionID] = append(sm.subscribers[sessionID], ch)
	sm.mu.Unlock()
	return ch
}

func (sm *SessionManager) Unsubscribe(sessionID string, ch chan models.SessionEvent) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	subs := sm.subscribers[sessionID]
	for i, sub := range subs {
		if sub == ch {
			sm.subscribers[sessionID] = append(subs[:i], subs[i+1:]...)
			close(ch)
			return
		}
	}
}

func (sm *SessionManager) broadcast(sessionID string, evt models.SessionEvent) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	for _, ch := range sm.subscribers[sessionID] {
		select {
		case ch <- evt:
		default:
		}
	}
}

func (sm *SessionManager) ScanDraftFile(sessionID string) (string, error) {
	_, taskID, err := sm.findSession(sessionID)
	if err != nil {
		return "", err
	}
	cwd := sm.store.GetSessionCWDDir(taskID, sessionID)
	currentDraft := filepath.Join(cwd, "current_draft.md")
	data, err := os.ReadFile(currentDraft)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func stripMarkdownCodeFences(text string) string {
	text = strings.TrimSpace(text)
	text = strings.TrimPrefix(text, "```json")
	text = strings.TrimPrefix(text, "```")
	text = strings.TrimSuffix(text, "```")
	return strings.TrimSpace(text)
}

func (sm *SessionManager) UpdateTaskFields(taskID, novelName, accountID string, chapterCountDelta int) error {
	task, err := sm.store.GetTask(taskID)
	if err != nil {
		return err
	}
	if novelName != "" {
		task.NovelName = novelName
	}
	if accountID != "" {
		task.AccountID = accountID
	}
	if chapterCountDelta > 0 {
		task.PublishedChapterCount += chapterCountDelta
	}
	return sm.store.UpdateTask(task)
}

func (sm *SessionManager) DeleteTask(taskID string) error {
	return sm.store.DeleteTask(taskID)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
