package manager

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"session_manager/adapter"
	"session_manager/chaterr"
	"session_manager/models"
	"session_manager/pool"
	"session_manager/runner"
	"session_manager/store"

	"clawstudios/pkg/logging"

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
	DeepseekAPIKey      string
	SkillRegistryURL    string
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

type taskRunGate struct {
	mu      sync.Mutex
	running bool
}

type SessionManager struct {
	cfg        Config
	configPath string
	store      *store.Store
	pool       *pool.ProcessPool
	runner     *runner.OpenCodeRunner
	skills     map[string]adapter.SkillDef

	skillRegistryURL string
	fetchedSkills    map[string]adapter.SkillDef
	fetchedMu        sync.RWMutex

	taskRunGates sync.Map // taskID -> *taskRunGate

	runningSessions   map[string]bool
	runningSessionsMu sync.Mutex

	stopCh chan struct{}
}

func (sm *SessionManager) tryAcquireTaskRun(taskID string) bool {
	v, _ := sm.taskRunGates.LoadOrStore(taskID, &taskRunGate{})
	g := v.(*taskRunGate)
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.running {
		return false
	}
	g.running = true
	return true
}

func (sm *SessionManager) releaseTaskRun(taskID string) {
	v, ok := sm.taskRunGates.Load(taskID)
	if !ok {
		return
	}
	g := v.(*taskRunGate)
	g.mu.Lock()
	g.running = false
	g.mu.Unlock()
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
		cfg:              cfg,
		store:            s,
		pool:             p,
		runner:           ocRunner,
		skills:           skills,
		skillRegistryURL: cfg.SkillRegistryURL,
		fetchedSkills:    make(map[string]adapter.SkillDef),
		runningSessions:  make(map[string]bool),
		stopCh:           make(chan struct{}),
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

func (sm *SessionManager) IsSessionRunning(sessionID string) bool {
	sm.runningSessionsMu.Lock()
	defer sm.runningSessionsMu.Unlock()
	return sm.runningSessions[sessionID]
}

func (sm *SessionManager) ListTaskSkillIDs() []string {
	return sm.store.ListTaskSkillIDs()
}

func (sm *SessionManager) resolveSkill(ctx context.Context, skillID string) (adapter.SkillDef, error) {
	if skill, ok := sm.skills[skillID]; ok {
		return skill, nil
	}

	sm.fetchedMu.RLock()
	if skill, ok := sm.fetchedSkills[skillID]; ok {
		sm.fetchedMu.RUnlock()
		return skill, nil
	}
	sm.fetchedMu.RUnlock()

	if sm.skillRegistryURL == "" {
		return adapter.SkillDef{}, fmt.Errorf("skill not found: %s (no registry configured)", skillID)
	}

	fetched, err := adapter.FetchSkillFromL1(ctx, sm.skillRegistryURL, skillID)
	if err != nil {
		return adapter.SkillDef{}, fmt.Errorf("fetch skill %s: %w", skillID, err)
	}

	sm.fetchedMu.Lock()
	sm.fetchedSkills[skillID] = fetched
	sm.fetchedMu.Unlock()

	return fetched, nil
}

func (sm *SessionManager) initOpenCodeConfig() error {
	configPath := filepath.Join(sm.cfg.DataDir, "opencode_config.json")

	skillsDir := sm.store.SkillsDir()
	tasksDir := filepath.Join(sm.cfg.DataDir, "tasks")

	existing := make(map[string]interface{})
	if data, err := os.ReadFile(configPath); err == nil {
		json.Unmarshal(data, &existing)
	}

	existing["permission"] = map[string]interface{}{
		"edit":  "allow",
		"bash":  "deny",
		"write": "allow",
		"read":  "allow",
		"external_directory": map[string]interface{}{
			skillsDir + "/*":  "allow",
			tasksDir + "/*":   "allow",
			"/tmp/opencode/*": "allow",
		},
		"doom_loop": "allow",
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

func (sm *SessionManager) normalizeModel(model string) string {
	if model == "" {
		model = sm.cfg.DefaultModel
	}
	model = strings.TrimPrefix(model, "team-")
	switch model {
	case "hy3/hy3-preview":
		model = "opencode/big-pickle"
	default:
		if !strings.HasPrefix(model, "team-deepseek/") {
			model = strings.Replace(model, "deepseek/", "team-deepseek/", 1)
		}
	}
	return model
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

	skill, err := sm.resolveSkill(ctx, req.SkillID)
	if err != nil {
		return nil, fmt.Errorf("resolve skill %s: %w", req.SkillID, err)
	}

	now := time.Now()
	if err := sm.checkTaskConcurrency(req.TaskID, now); err != nil {
		return nil, err
	}

	model := sm.normalizeModel(req.Model)

	task, isNew, err := sm.store.GetOrCreateTask(req.TaskID, req.Topic, req.UID, req.MemoryModel, req.Platform, req.SkillID, model, req.AccountID, req.NovelName)
	if err != nil {
		return nil, fmt.Errorf("init task: %w", err)
	}

	sessionID := uuid.New().String()[:8]

	activeOK, existingSID, err := sm.store.TrySetActiveSession(req.TaskID, sessionID)
	if err != nil {
		return nil, fmt.Errorf("set active session: %w", err)
	}
	if !activeOK {
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
		SessionID:     sessionID,
		TaskID:        req.TaskID,
		Topic:         req.Topic,
		SkillID:       req.SkillID,
		Model:         model,
		Status:        models.StatusCreated,
		CWDPath:       cwd,
		MessageCount:  0,
		TotalTokens:   0,
		DraftVersion:  req.DraftVersion,
		NovelName:     req.NovelName,
		VolumeName:    task.VolumeName,
		ChapterNumber: task.ChapterNumber,
		CreatedAt:     now,
		LastActiveAt:  now,
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

	if !isNew {
		sm.injectTaskContext(cwd, req.TaskID)
	}

	if l := logging.FromContext(ctx); l != nil {
		l.Info("session created: task=%s session=%s skill=%s model=%s chapter=%d",
			req.TaskID, sessionID, req.SkillID, model, task.ChapterNumber)
	}

	return sess, nil
}

func (sm *SessionManager) injectTaskContext(cwd, taskID string) {
	shortData, errShort := sm.store.ReadShortTerm(taskID)
	medData, errMed := sm.store.ReadMediumTerm(taskID)

	if errShort != nil || len(shortData) == 0 {
		sessions, err := sm.store.LoadTaskSessions(taskID)
		if err == nil && len(sessions) > 0 {
			var validSessions []*models.Session
			for _, s := range sessions {
				if s.ChapterNumber <= 0 {
					continue
				}
				if s.Status == models.StatusCreated || s.Status == models.StatusGenerating {
					continue
				}
				sessCwd := sm.store.GetSessionCWDDir(taskID, s.SessionID)
				draftPath := filepath.Join(sessCwd, "current_draft.md")
				if info, statErr := os.Stat(draftPath); statErr != nil || info.Size() == 0 {
					continue
				}
				validSessions = append(validSessions, s)
			}
			if len(validSessions) > 0 {
				var content string
				start := 0
				if len(validSessions) > models.ShortTermWindowSize {
					start = len(validSessions) - models.ShortTermWindowSize
				}
				for i := start; i < len(validSessions); i++ {
					sess := validSessions[i]
					sessCwd := sm.store.GetSessionCWDDir(taskID, sess.SessionID)
					draftPath := filepath.Join(sessCwd, "current_draft.md")
					if data, err := os.ReadFile(draftPath); err == nil && len(data) > 0 {
						chapterNo := sm.inferChapterNumberForSession(taskID, sess)
						content += fmt.Sprintf("## 章节 %d\n\n", chapterNo)
						content += string(data)
						content += "\n\n"
					}
				}
				if len(content) > 0 {
					shortData = []byte(content)
					errShort = nil
				}
			}
		}
	}

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

func (sm *SessionManager) chatOpenCodeSessionPath(cwd string) string {
	return filepath.Join(cwd, ".opencode_session")
}

func (sm *SessionManager) loadChatOpenCodeSession(cwd string) string {
	data, err := os.ReadFile(sm.chatOpenCodeSessionPath(cwd))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func (sm *SessionManager) saveChatOpenCodeSession(cwd, sid string) {
	if sid == "" {
		return
	}
	_ = os.WriteFile(sm.chatOpenCodeSessionPath(cwd), []byte(sid), 0644)
}

func (sm *SessionManager) resolveChapterNoticeMeta(taskID, sessionID string, sess *models.Session, draftPath string) (chapterNo int, volumeName, chapterTitle string, effectiveDraftPath string) {
	// 对话模式 runSessionLoop 的 sessionID 为空时，回退到任务当前/最新章节 session
	if sessionID == "" && taskID != "" {
		if task, err := sm.store.GetTask(taskID); err == nil {
			if task.ActiveSessionID != "" {
				sessionID = task.ActiveSessionID
			} else if len(task.SessionIDs) > 0 {
				sessionID = task.SessionIDs[len(task.SessionIDs)-1]
			}
		}
	}
	if sess == nil && sessionID != "" && taskID != "" {
		if s, err := sm.store.GetSession(taskID, sessionID); err == nil {
			sess = s
		}
	}
	if draftPath == "" && sessionID != "" && taskID != "" {
		draftPath = filepath.Join(sm.store.GetSessionCWDDir(taskID, sessionID), "current_draft.md")
	}
	chapterNo = 0
	if sess != nil && sess.ChapterNumber > 0 {
		chapterNo = sess.ChapterNumber
	} else if sess != nil && taskID != "" {
		chapterNo = sm.inferChapterNumberForSession(taskID, sess)
	}
	chapterTitle = parseDraftChapterTitle(draftPath)
	volumeName = ""
	if sess != nil {
		volumeName = strings.TrimSpace(sess.VolumeName)
	}
	if volumeName == "" && taskID != "" {
		if task, err := sm.store.GetTask(taskID); err == nil {
			volumeName = strings.TrimSpace(task.VolumeName)
		}
	}
	return chapterNo, volumeName, chapterTitle, draftPath
}

func (sm *SessionManager) inferChapterNumberForSession(taskID string, sess *models.Session) int {
	sessions, err := sm.store.LoadTaskSessions(taskID)
	if err != nil {
		return 1
	}
	chapNo := 1
	for _, s := range sessions {
		if s.SessionID == sess.SessionID {
			return chapNo
		}
		if s.ChapterNumber <= 0 {
			continue
		}
		if s.Status == models.StatusCreated || s.Status == models.StatusGenerating {
			continue
		}
		if !sm.hasDraftFile(taskID, s.SessionID) {
			continue
		}
		chapNo++
	}
	return chapNo
}

func (sm *SessionManager) Send(ctx context.Context, sessionID string, req models.SendMessageRequest) error {
	sess, taskID, err := sm.findSession(sessionID)
	if err != nil {
		return err
	}

	if sess.Status == models.StatusCold {
		return fmt.Errorf("session is cold, use wake first: %s", sessionID)
	}

	sess.LastActiveAt = time.Now()
	sess.DraftVersion = req.DraftVersion
	sess.OpenCodeSID = ""
	if sess.Status == models.StatusArchived {
		sess.Status = models.StatusWarm
		sess.ArchivedAt = nil
	}
	_ = sm.store.UpsertSessionInTask(sess)
	if !sm.tryAcquireTaskRun(taskID) {
		return fmt.Errorf("上一条消息还在处理中，请稍后再试")
	}
	sm.appendTaskMessage(taskID, sessionID, "user", req.Text, req.DraftVersion)

	task, err := sm.store.GetTask(taskID)
	if err == nil {
		task.LastActiveAt = sess.LastActiveAt
		_ = sm.store.UpdateTask(task)
	}

	go func() {
		defer sm.releaseTaskRun(taskID)
		sm.runSessionLoop(context.Background(), sessionID, taskID, sess.CWDPath, sess.Model, req.Text, sess.OpenCodeSID)
	}()

	return nil
}

func (sm *SessionManager) SendTaskMessage(ctx context.Context, taskID string, req models.TaskMessageRequest) error {
	if req.Text == "" {
		return fmt.Errorf("text is required")
	}

	mode := req.Mode
	if mode == "" {
		mode = "edit"
	}
	task, err := sm.store.GetTask(taskID)
	if err != nil {
		return err
	}

	if mode == "chat" {
		if !sm.tryAcquireTaskRun(taskID) {
			return fmt.Errorf("上一条消息还在处理中，请稍后再试")
		}
		cwd := filepath.Join(sm.store.TaskDir(taskID), "chat")
		if err := os.MkdirAll(cwd, 0755); err != nil {
			sm.releaseTaskRun(taskID)
			return err
		}
		sm.appendTaskMessage(taskID, "", "user", req.Text, req.DraftVersion)
		task.LastActiveAt = time.Now()
		_ = sm.store.UpdateTask(task)
		ocSID := sm.loadChatOpenCodeSession(cwd)
		go func() {
			defer sm.releaseTaskRun(taskID)
			sm.runSessionLoop(context.Background(), "", taskID, cwd, task.Model, req.Text, ocSID)
		}()
		return nil
	}

	targetSessionID := req.TargetSessionID
	if targetSessionID == "" {
		targetSessionID = task.ActiveSessionID
		if targetSessionID == "" && len(task.SessionIDs) > 0 {
			targetSessionID = task.SessionIDs[len(task.SessionIDs)-1]
		}
	}
	if targetSessionID == "" {
		sm.appendTaskMessage(taskID, "", "user", req.Text, req.DraftVersion)
		sm.appendTaskMessage(taskID, "", "system", "当前任务没有可修改的章节草稿", req.DraftVersion)
		return nil
	}

	for idx, sid := range task.SessionIDs {
		if sid == targetSessionID && idx+1 <= task.PublishedChapterCount {
			return fmt.Errorf("当前章节已发布，不能再通过 AI 修改内容")
		}
	}

	sess, err := sm.store.GetSession(taskID, targetSessionID)
	if err != nil {
		return err
	}
	if sess.Status == models.StatusCold {
		return fmt.Errorf("session is cold, use wake first: %s", targetSessionID)
	}

	sess.LastActiveAt = time.Now()
	sess.DraftVersion = req.DraftVersion
	if sess.Status == models.StatusArchived {
		sess.Status = models.StatusWarm
		sess.ArchivedAt = nil
		sess.OpenCodeSID = ""
	}
	_ = sm.store.UpsertSessionInTask(sess)
	if !sm.tryAcquireTaskRun(taskID) {
		return fmt.Errorf("上一条消息还在处理中，请稍后再试")
	}
	sm.appendTaskMessage(taskID, targetSessionID, "user", req.Text, req.DraftVersion)

	task.LastActiveAt = sess.LastActiveAt
	_ = sm.store.UpdateTask(task)

	go func() {
		defer sm.releaseTaskRun(taskID)
		sm.runSessionLoop(context.Background(), targetSessionID, taskID, sess.CWDPath, sess.Model, req.Text, "")
	}()
	return nil
}

func (sm *SessionManager) runSessionLoop(ctx context.Context, sessionID, taskID, cwd, model, message, ocSID string) {
	logger := logging.NewLogger("SessionWorker",
		logging.WithTaskID(taskID),
		logging.WithSessionID(sessionID),
	)

	if sessionID != "" {
		sm.runningSessionsMu.Lock()
		sm.runningSessions[sessionID] = true
		sm.runningSessionsMu.Unlock()
		defer func() {
			sm.runningSessionsMu.Lock()
			delete(sm.runningSessions, sessionID)
			sm.runningSessionsMu.Unlock()
		}()
	}

	if err := sm.pool.Acquire(ctx); err != nil {
		logger.Error(logging.ErrTimeout, "pool acquire failed: session=%s err=%v", sessionID, err)
		sm.appendTaskMessage(taskID, sessionID, "system", chaterr.UserFacing("server busy, please retry later"), 0)
		return
	}
	defer sm.pool.Release()

	activeKey, keySource := sm.readActiveAPIKey(model)
	costStart := sm.logSessionCostStart(logger, sessionID, taskID, model, activeKey, keySource)

	keyShow := activeKey
	if len(keyShow) > 16 {
		keyShow = keyShow[:16]
	}
	logger.Info("pool acquired, session loop start: session=%s task=%s model=%s key=%s... source=%s", sessionID, taskID, model, keyShow, keySource)

	var sess *models.Session
	var err error
	if sessionID != "" {
		sess, err = sm.store.GetSession(taskID, sessionID)
	}
	if err == nil && sess != nil && sess.Status == models.StatusCreated {
		sess.Status = models.StatusGenerating
		sess.LastActiveAt = time.Now()
		_ = sm.store.UpsertSessionInTask(sess)
		logger.Info("status changed: CREATED -> GENERATING: session=%s", sessionID)
	}

	timeout := time.Duration(sm.cfg.DefaultTimeoutSec) * time.Second

	apiKey := sm.cfg.DeepseekAPIKey
	if apiKey == "" {
		apiKey = activeKey
	}

	opts := runner.RunOptions{
		CWD:            cwd,
		Model:          model,
		SessionID:      ocSID,
		Message:        message,
		Timeout:        timeout,
		ConfigPath:     sm.configPath,
		DeepseekAPIKey: apiKey,
	}

	msgCount := 0
	totalTokens := 0
	apiEverResponded := false
	capturedSID := ocSID
	var lastStepReason string
	var draftSizeFromDone int64
	var assistantText strings.Builder
	assistantPersisted := false
	var evtCountStepStart, evtCountToken, evtCountToolCall, evtCountStepFinish, evtCountReasoning, evtCountDraftUpdated, evtCountError, evtCountOther int
	noContentDetected := false
	sessionDraftPath := ""
	var draftBaselineMod time.Time
	var draftBaselineSize int64
	if sessionID != "" {
		sessionDraftPath = filepath.Join(sm.store.GetSessionCWDDir(taskID, sessionID), "current_draft.md")
		if st, err := os.Stat(sessionDraftPath); err == nil {
			draftBaselineMod = st.ModTime()
			draftBaselineSize = st.Size()
		}
	}

	emitDraftUpdated := func() {
		if sessionID == "" || sessionDraftPath == "" {
			return
		}
		st, err := os.Stat(sessionDraftPath)
		if err != nil || st.Size() == 0 {
			return
		}
		if !st.ModTime().After(draftBaselineMod) && st.Size() == draftBaselineSize {
			return
		}
		version := 0
		if fresh, getErr := sm.store.GetSession(taskID, sessionID); getErr == nil {
			version = fresh.DraftVersion
			if version == 0 {
				version = fresh.ChapterNumber
			}
			if fresh.Status == models.StatusGenerating {
				fresh.Status = models.StatusDraftReady
				_ = sm.store.UpsertSessionInTask(fresh)
			}
		}
	}

	persistAssistantMessage := func() {
		if assistantPersisted {
			return
		}
		chapterNo, volumeName, chapterTitle, noticeDraftPath := sm.resolveChapterNoticeMeta(taskID, sessionID, sess, sessionDraftPath)
		draftWrittenThisTurn := DraftFileChangedSince(noticeDraftPath, draftBaselineMod, draftBaselineSize)
		if sessionID == "" {
			noticeDraftPath = ""
			chapterNo = 0
			volumeName = ""
			chapterTitle = ""
			draftWrittenThisTurn = false
		}
		display := chatDisplayOrDraftNotice(
			assistantText.String(),
			noticeDraftPath,
			chapterNo,
			volumeName,
			chapterTitle,
			draftWrittenThisTurn,
		)
		if strings.TrimSpace(display) == "" {
			return
		}
		version := 0
		if sess != nil {
			version = sess.DraftVersion
		}
		sm.appendTaskMessage(taskID, sessionID, "assistant", display, version)
		assistantPersisted = true
		if isDraftWrittenNotice(display) {
			emitDraftUpdated()
		}
	}

	maxRetries := 5
	for attempt := 0; attempt < maxRetries; attempt++ {
		assistantText.Reset()
		assistantPersisted = false
		noContentDetected = false
		draftSizeFromDone = 0
		lastStepReason = ""
		evtCountStepStart = 0
		evtCountToken = 0
		evtCountToolCall = 0
		evtCountStepFinish = 0
		evtCountReasoning = 0
		evtCountDraftUpdated = 0
		evtCountError = 0
		evtCountOther = 0

		runCtx, cancel := context.WithTimeout(ctx, timeout)
		runCtx = logging.NewContext(runCtx, logger)
		logger.Info("launching opencode: session=%s model=%s cwd=%s attempt=%d/%d", sessionID, model, cwd, attempt+1, maxRetries)
		events, err := sm.runner.Run(runCtx, opts)
		if err != nil {
			cancel()
			if attempt < maxRetries-1 {
				logger.Warn(logging.WarnProcessStuck, "opencode launch failed, retrying: attempt=%d/%d session=%s err=%v", attempt+1, maxRetries, sessionID, err)
				time.Sleep(3 * time.Second)
				continue
			}
			logger.Error(logging.ErrSessionError, "opencode launch failed: session=%s err=%v", sessionID, err)
			errText := chaterr.UserFacing(fmt.Sprintf("failed to start opencode: %v", err))
			sm.appendTaskMessage(taskID, sessionID, "system", errText, 0)
			return
		}

		for evt := range events {
			if capturedSID == "" && evt.SessionID != "" && evt.SessionID != sessionID {
				capturedSID = evt.SessionID
			}

			evt.SessionID = sessionID
			evt.TaskID = taskID

			switch evt.Type {
			case "step_start":
				evtCountStepStart++
			case "token":
				evtCountToken++
			case "tool_call":
				evtCountToolCall++
			case "step_finish":
				evtCountStepFinish++
			case "reasoning":
				evtCountReasoning++
			case "draft_updated":
				evtCountDraftUpdated++
			case "error":
				evtCountError++
			default:
				evtCountOther++
			}

		if evt.Type == "reasoning" || (evt.Type == "step_finish" && evt.Reason != "") {
			apiEverResponded = true
		}
		if evt.Type == "step_finish" && evt.Reason != "" {
			lastStepReason = evt.Reason
		}

			if evt.Type == "token" || (evt.Type == "step_finish" && strings.TrimSpace(evt.Text) != "") {
				if evt.Text != "" {
					assistantText.WriteString(evt.Text)
				}
			}

			if sessionID != "" && evt.Type == "step_start" && capturedSID != "" {
				sess, err := sm.store.GetSession(taskID, sessionID)
				if err == nil && sess.OpenCodeSID == "" {
					sess.OpenCodeSID = capturedSID
					_ = sm.store.UpsertSessionInTask(sess)
				}
			}

			if evt.Type == "step_finish" && evt.Tokens != nil {
				totalTokens += evt.Tokens.Total
			}

		if evt.Type == "done" {
			emitDraftUpdated()
			if evt.DraftSize > 0 {
				draftSizeFromDone = evt.DraftSize
			}
			if strings.TrimSpace(assistantText.String()) != "" {
				persistAssistantMessage()
			} else if sessionID != "" {
				_, _, _, noticeDraftPath := sm.resolveChapterNoticeMeta(taskID, sessionID, sess, sessionDraftPath)
				if DraftFileChangedSince(noticeDraftPath, draftBaselineMod, draftBaselineSize) {
					persistAssistantMessage()
				}
			}
		}

		if (evt.Type == "done" || evt.Type == "error") && draftSizeFromDone == 0 {
			noContentDetected = true
			logger.Warn(logging.WarnProcessStuck, "opencode returned no content (draft_size=0): session=%s task=%s model=%s last_step_reason=%s total_tokens=%d msg_count=%d events(step_start=%d tool_call=%d step_finish=%d reasoning=%d draft_updated=%d error=%d other=%d)",
				sessionID, taskID, model, lastStepReason, totalTokens, msgCount,
				evtCountStepStart, evtCountToolCall, evtCountStepFinish, evtCountReasoning, evtCountDraftUpdated, evtCountError, evtCountOther)
		}

			if evt.Type == "token" || evt.Type == "tool_call" || evt.Type == "step_finish" ||
				evt.Type == "done" || evt.Type == "error" || evt.Type == "draft_updated" ||
				evt.Type == "reasoning" {
				if evt.Type == "error" && evt.Error != "" {
					sm.appendTaskMessage(taskID, sessionID, "system", chaterr.UserFacing(evt.Error), 0)
				}
				out := evt
				if out.Type == "token" && out.Text != "" {
					out.Text = chatDisplayTextDelta(assistantText.String(), sessionDraftPath, out.Text)
				}
				if out.Type == "step_finish" && out.Text != "" {
					out.Text = ChatDisplayText(out.Text, sessionDraftPath)
				}
			}
		}
		cancel()

		msgCount++
		if !noContentDetected {
			break
		}
		if attempt < maxRetries-1 {
			logger.Warn(logging.WarnProcessStuck, "retrying session: attempt=%d/%d session=%s", attempt+1, maxRetries, sessionID)
			time.Sleep(3 * time.Second)
		}
	}

	if noContentDetected {
		msg := "AI 未返回内容，请重试"
		if !apiEverResponded && apiKey != "" {
			logger.Info("key validation: no API response in any attempt, checking key...")
			vCtx, vCancel := context.WithTimeout(context.Background(), 15*time.Second)
			if vErr := validateDeepseekKey(vCtx, apiKey); vErr != nil {
				msg = fmt.Sprintf("API key 无效 (%v)，请检查 key 配置是否过期或额度不足", vErr)
				logger.Error(logging.ErrSessionError, "API key validation failed: session=%s err=%v", sessionID, vErr)
			} else {
				msg = "AI 未返回内容（key 有效，可能是步配额耗尽），请重试"
				logger.Info("key validation passed: session=%s (key valid)", sessionID)
			}
			vCancel()
		}
		sm.appendTaskMessage(taskID, sessionID, "system", msg, 0)
	}

	if sessionID != "" {
		sessionCwd := sm.store.GetSessionCWDDir(taskID, sessionID)
		chkPath := filepath.Join(sessionCwd, "current_draft.md")
		if data, err := os.ReadFile(chkPath); err == nil && len(data) > 0 {
			firstLine := strings.TrimSpace(strings.SplitN(string(data), "\n", 2)[0])
			if !strings.HasPrefix(firstLine, "# 第") || !strings.Contains(firstLine, "章") {
				chapterNo := sm.inferChapterNumberForSession(taskID, sess)
				newContent := fmt.Sprintf("# 第%d章\n\n%s", chapterNo, string(data))
				if err := os.WriteFile(chkPath, []byte(newContent), 0644); err == nil {
					logger.Info("chapter title auto-fixed: session=%s chapter=%d", sessionID, chapterNo)
				}
			}
		}
	}

	if sessionID != "" {
		sess, err = sm.store.GetSession(taskID, sessionID)
	}
	if sessionID != "" && err == nil {
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
			if noContentDetected {
				sess.Status = models.StatusNoContent
				logger.Info("status changed: -> NO_CONTENT: session=%s msg_count=%d total_tokens=%d draft_version=%d",
					sessionID, sess.MessageCount, sess.TotalTokens, sess.DraftVersion)
			} else {
				sess.Status = models.StatusWarm
				if sess.DraftVersion == 0 {
					sess.DraftVersion = sess.ChapterNumber
				}
				logger.Info("status changed: -> WARM: session=%s msg_count=%d total_tokens=%d draft_version=%d",
					sessionID, sess.MessageCount, sess.TotalTokens, sess.DraftVersion)
			}
		}
		if capturedSID != "" && sess.OpenCodeSID == "" {
			sess.OpenCodeSID = capturedSID
		}
		_ = sm.store.UpsertSessionInTask(sess)

		if sess.DraftVersion > 0 {
			sm.saveDraftVersion(taskID, sessionID, sess.DraftVersion, logger)
		}

		if sess.MessageCount >= sm.cfg.MaxMessagesPerEpoch || sess.TotalTokens >= sm.cfg.MaxTokensPerEpoch {
			logger.Info("archive threshold reached: session=%s msgs=%d tokens=%d, auto-archiving",
				sessionID, sess.MessageCount, sess.TotalTokens)
			sm.Close(context.Background(), sessionID)
		}
	}

	task, err := sm.store.GetTask(taskID)
	if err == nil {
		task.LastActiveAt = time.Now()
		_ = sm.store.UpdateTask(task)
	}
	if sessionID != "" {
		_ = sm.store.ClearActiveSession(taskID, sessionID)
	}

	if strings.TrimSpace(assistantText.String()) != "" {
		persistAssistantMessage()
	}

	if sessionID == "" && capturedSID != "" {
		sm.saveChatOpenCodeSession(cwd, capturedSID)
	}

	if sessionID != "" {
		sessionCwd := sm.store.GetSessionCWDDir(taskID, sessionID)
		draftPath := filepath.Join(sessionCwd, "current_draft.md")
		if info, err := os.Stat(draftPath); err == nil {
			logger.Info("draft file exists: session=%s path=%s size=%d bytes", sessionID, draftPath, info.Size())
		} else {
			logger.Warn(logging.WarnProcessStuck, "draft file not found: session=%s path=%s assistant_persisted=%v", sessionID, draftPath, assistantPersisted)
		}
	}

	logger.Info("session loop done: session=%s opencode_sid=%s", sessionID, capturedSID)
	sm.logSessionCostEnd(logger, sessionID, taskID, model, totalTokens, costStart)
	logger.Close()
}

func (sm *SessionManager) readActiveAPIKey(model string) (key, source string) {
	configPath := sm.configPath
	if configPath == "" {
		return "", "no_config_path"
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		return "", "config_not_found"
	}
	var cfg struct {
		Provider map[string]struct {
			APIKey string `json:"api_key"`
		} `json:"provider"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return "", "invalid_config"
	}
	if p, ok := cfg.Provider["team-deepseek"]; ok && p.APIKey != "" {
		return p.APIKey, "L1_AI_Provider_config"
	}
	return "", "no_team-deepseek_provider"
}

func (sm *SessionManager) logSessionCostStart(logger *logging.Logger, sessionID, taskID, model, keyFull, keySource string) time.Time {
	now := time.Now()
	keyPrefix := keyFull
	if len(keyPrefix) > 16 {
		keyPrefix = keyPrefix[:16]
	}
	logger.Info("💰 SESSION_COST_START: session=%s task=%s model=%s key=%s... source=%s time=%s",
		sessionID, taskID, model, keyPrefix, keySource, now.Format("15:04:05"))
	return now
}

func (sm *SessionManager) logSessionCostEnd(logger *logging.Logger, sessionID, taskID, model string, totalTokens int, costStart time.Time) {
	duration := time.Since(costStart)
	costUSD := estimateCost(model, totalTokens)
	costCNY := costUSD * 7.2
	logger.Info("💰 SESSION_COST_END: session=%s task=%s model=%s tokens=%d cost=$%.6f cost≈¥%.6f duration=%s",
		sessionID, taskID, model, totalTokens, costUSD, costCNY, duration.Round(time.Second).String())
}

func estimateCost(model string, tokens int) float64 {
	var rate float64 = 0.00000050
	switch {
	case strings.Contains(model, "v4-pro"):
		rate = 0.00000100
	case strings.Contains(model, "v4-flash"):
		rate = 0.00000025
	default:
		rate = 0.00000050
	}
	return float64(tokens) * rate
}

func (sm *SessionManager) saveDraftVersion(taskID, sessionID string, version int, logger *logging.Logger) {
	cwd := sm.store.GetSessionCWDDir(taskID, sessionID)
	currentDraft := filepath.Join(cwd, "current_draft.md")
	data, err := os.ReadFile(currentDraft)
	if err != nil {
		logger.Warn(logging.WarnProcessStuck, "failed to read current_draft.md for versioning: session=%s version=%d err=%v", sessionID, version, err)
		return
	}
	if err := sm.store.SaveDraftVersion(taskID, version, string(data)); err != nil {
		logger.Warn(logging.WarnProcessStuck, "failed to save draft_v%d.md: session=%s err=%v", version, sessionID, err)
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

	cwd := sm.store.GetSessionCWDDir(taskID, sessionID)
	currentDraft := filepath.Join(cwd, "current_draft.md")
	draftStat, draftErr := os.Stat(currentDraft)
	if draftErr != nil || draftStat.Size() == 0 {
		if sess.Status != models.StatusNoContent {
			sess.Status = models.StatusNoContent
			_ = sm.store.UpsertSessionInTask(sess)
			log.Printf("Close: session %s has no draft file, marking NO_CONTENT", sessionID)
		}
		return nil
	}

	episodeID := uuid.New().String()[:8]
	epochNo := len(sess.Episodes) + 1

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

	if len(draftData) > 0 {
		go sm.generateMediumSummary(taskID, sessionID, task, sess.SkillID, draftData)
	}

	if l := logging.FromContext(ctx); l != nil {
		l.Info("session archived: session=%s task=%s epoch=%d msgs=%d tokens=%d",
			sessionID, taskID, epochNo, sess.MessageCount, sess.TotalTokens)
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

	summaryKey := sm.cfg.DeepseekAPIKey
	if summaryKey == "" {
		summaryKey, _ = sm.readActiveAPIKey(summaryModel)
	}

	opts := runner.RunOptions{
		CWD:            tmpDir,
		Model:          summaryModel,
		SessionID:      "",
		Message:        prompt,
		Timeout:        120 * time.Second,
		ConfigPath:     sm.configPath,
		DeepseekAPIKey: summaryKey,
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

	skillID := req.SkillID
	if skillID == "" {
		skillID = "general_fallback_v1"
		if len(task.SessionIDs) > 0 {
			sessions, _ := sm.store.LoadTaskSessions(taskID)
			if sessions != nil && len(sessions) > 0 {
				lastSession := sessions[len(sessions)-1]
				if lastSession.SkillID != "" {
					skillID = lastSession.SkillID
				}
			}
		}
	}

	skill, err := sm.resolveSkill(ctx, skillID)
	if err != nil {
		return nil, fmt.Errorf("resolve skill %s: %w", skillID, err)
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

	model := req.Model
	if model == "" {
		model = task.Model
	}
	model = sm.normalizeModel(model)

	novelName := req.NovelName
	if novelName == "" {
		novelName = task.NovelName
	}
	volumeName := req.VolumeName
	if volumeName == "" {
		volumeName = task.VolumeName
	}
	if volumeName == "" {
		volumeName = "第一卷"
	}
	chapterNumber := req.ChapterNumber
	if chapterNumber <= 0 {
		chapterNumber = task.ChapterNumber
	}

	sess := &models.Session{
		SessionID:     sessionID,
		TaskID:        taskID,
		Topic:         task.Topic,
		SkillID:       skillID,
		Model:         model,
		Status:        models.StatusCreated,
		CWDPath:       cwd,
		MessageCount:  0,
		TotalTokens:   0,
		DraftVersion:  req.DraftVersion,
		NovelName:     novelName,
		VolumeName:    volumeName,
		ChapterNumber: chapterNumber,
		CreatedAt:     now,
		LastActiveAt:  now,
	}

	if err := sm.store.UpsertSessionInTask(sess); err != nil {
		return nil, fmt.Errorf("save session: %w", err)
	}

	log.Printf("[wake] task=%s session=%s vol=%s ch=%d novel=%s", taskID, sessionID, volumeName, chapterNumber, novelName)

	task, _ = sm.store.GetTask(taskID)
	task.LastActiveAt = now
	task.SessionIDs = append(task.SessionIDs, sessionID)
	task.SessionCount = len(task.SessionIDs)
	_ = sm.store.UpdateTask(task)

	hasShort, hasMed := sm.store.HasTaskContext(taskID)
	if !hasShort {
		if _, err := os.Stat(filepath.Join(cwd, "RECENT_DRAFTS.md")); err == nil {
			hasShort = true
		}
	}
	if !hasMed {
		if _, err := os.Stat(filepath.Join(cwd, "HISTORY_SUMMARY.md")); err == nil {
			hasMed = true
		}
	}

	chapterNum := req.ChapterNumber
	if chapterNum <= 0 {
		chapterNum = task.SessionCount
	}

	var msg string
	if req.IsFinale {
		msg = adapter.BuildFinaleMessage(task.Topic, skill, req.Text, hasShort, hasMed, chapterNum)
	} else {
		msg = adapter.BuildStartMessage(novelName, skill, req.Text, chapterNum)
	}

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

func (sm *SessionManager) FillSessionsDraftSize(sessions []*models.Session) {
	for _, s := range sessions {
		if s.CWDPath == "" {
			continue
		}
		draftPath := filepath.Join(s.CWDPath, "current_draft.md")
		if info, err := os.Stat(draftPath); err == nil {
			s.DraftSize = info.Size()
			if info.Size() > 0 {
				if data, readErr := os.ReadFile(draftPath); readErr == nil {
					s.ChapterTitle = store.ExtractChapterTitle(string(data))
				}
			}
		}
	}
}

func (sm *SessionManager) hasDraftFile(taskID, sessionID string) bool {
	cwd := sm.store.GetSessionCWDDir(taskID, sessionID)
	draftPath := filepath.Join(cwd, "current_draft.md")
	info, err := os.Stat(draftPath)
	return err == nil && info.Size() > 0
}

func (sm *SessionManager) ListTasks() []models.TaskInfo {
	tasks := sm.store.ListTasks()
	return sm.tasksToInfo(tasks)
}

func (sm *SessionManager) ListTasksByUID(uid string) []models.TaskInfo {
	tasks := sm.store.ListTasksByUID(uid)
	return sm.tasksToInfo(tasks)
}

func (sm *SessionManager) ListTasksPage(uid, search string, page, size int) ([]models.TaskInfo, int) {
	tasks, total := sm.store.ListTasksPage(uid, search, page, size)
	return sm.tasksToInfo(tasks), total
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
			VolumeName:            t.VolumeName,
			Title:                 t.Title,
			ChapterNumber:         t.ChapterNumber,
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
		VolumeName:            task.VolumeName,
		Title:                 task.Title,
		ChapterNumber:         task.ChapterNumber,
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

func (sm *SessionManager) ListTaskMessages(taskID string) ([]models.ChatMessage, error) {
	return sm.store.LoadTaskMessages(taskID)
}

func (sm *SessionManager) ClearTaskMessages(taskID string) error {
	return sm.store.ClearTaskMessages(taskID)
}

func (sm *SessionManager) appendTaskMessage(taskID, sessionID, role, text string, draftVersion int) {
	msg := models.ChatMessage{
		ID:           uuid.New().String(),
		TaskID:       taskID,
		SessionID:    sessionID,
		Role:         role,
		Text:         text,
		Timestamp:    time.Now(),
		DraftVersion: draftVersion,
	}
	if err := sm.store.AppendTaskMessage(taskID, msg); err != nil {
		log.Printf("WARN: failed to append task message task=%s session=%s role=%s: %v", taskID, sessionID, role, err)
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

func (sm *SessionManager) UpdateTaskFields(taskID, novelName, accountID, volumeName, title string, chapterNumber int, chapterCountDelta int) error {
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
	if volumeName != "" {
		task.VolumeName = volumeName
	}
	if title != "" {
		task.Title = title
	}
	if chapterNumber > 0 {
		task.ChapterNumber = chapterNumber
	}
	if chapterCountDelta > 0 {
		task.PublishedChapterCount += chapterCountDelta
	}
	return sm.store.UpdateTask(task)
}

func (sm *SessionManager) DeleteTask(taskID string) error {
	return sm.store.DeleteTask(taskID)
}

func (sm *SessionManager) CreateTaskDirect(req models.CreateTaskRequest) error {
	_, _, err := sm.store.GetOrCreateTask(req.TaskID, req.Topic, req.UID, "", req.Platform, req.SkillID, req.Model, req.AccountID, req.NovelName)
	return err
}

func (sm *SessionManager) SetSessionPostID(sessionID, postID string) error {
	sess, _, err := sm.findSession(sessionID)
	if err != nil {
		return err
	}
	sess.PostID = postID
	return sm.store.UpsertSessionInTask(sess)
}

func (sm *SessionManager) inferChapterNumber(taskID string) int {
	task, err := sm.store.GetTask(taskID)
	if err != nil {
		return 1
	}
	if task.SessionCount > 0 {
		return task.SessionCount
	}
	sessions, err := sm.store.LoadTaskSessions(taskID)
	if err == nil && len(sessions) > 0 {
		return len(sessions)
	}
	return 1
}

func validateDeepseekKey(ctx context.Context, apiKey string) error {
	body, _ := json.Marshal(map[string]interface{}{
		"model": "deepseek-chat",
		"messages": []map[string]string{
			{"role": "user", "content": "hi"},
		},
		"max_tokens": 1,
	})
	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.deepseek.com/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create validation request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("API unreachable: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return fmt.Errorf("HTTP %d (auth failed)", resp.StatusCode)
	}
	if resp.StatusCode >= 500 {
		return fmt.Errorf("HTTP %d (server error)", resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	return nil
}
