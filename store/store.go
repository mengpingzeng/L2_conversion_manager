package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"session_manager/models"
)

type Store struct {
	mu        sync.RWMutex
	dataDir   string
	skillsDir string

	tasks     map[string]*models.Task
	tasksPath string
}

func NewStore(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	skillsDir := filepath.Join(dataDir, "skills")
	if err := os.MkdirAll(skillsDir, 0755); err != nil {
		return nil, fmt.Errorf("create skills dir: %w", err)
	}

	tasksDir := filepath.Join(dataDir, "tasks")
	if err := os.MkdirAll(tasksDir, 0755); err != nil {
		return nil, fmt.Errorf("create tasks dir: %w", err)
	}

	tasksPath := filepath.Join(dataDir, "tasks.json")
	s := &Store{
		dataDir:   dataDir,
		skillsDir: skillsDir,
		tasks:     make(map[string]*models.Task),
		tasksPath: tasksPath,
	}

	data, err := os.ReadFile(tasksPath)
	if err == nil {
		var taskList []*models.Task
		if err := json.Unmarshal(data, &taskList); err == nil {
			for _, t := range taskList {
				s.tasks[t.TaskID] = t
			}
		}
	}

	return s, nil
}

func (s *Store) saveTasks() error {
	var taskList []*models.Task
	for _, t := range s.tasks {
		taskList = append(taskList, t)
	}
	sort.Slice(taskList, func(i, j int) bool {
		return taskList[i].CreatedAt.Before(taskList[j].CreatedAt)
	})
	data, err := json.MarshalIndent(taskList, "", "  ")
	if err != nil {
		return err
	}
	tmpPath := s.tasksPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmpPath, s.tasksPath)
}

func (s *Store) DataDir() string                           { return s.dataDir }
func (s *Store) SkillsDir() string                          { return s.skillsDir }
func (s *Store) SkillDir(skillName string) string           { return filepath.Join(s.skillsDir, skillName) }
func (s *Store) TasksDir() string                           { return filepath.Join(s.dataDir, "tasks") }
func (s *Store) TaskDir(taskID string) string               { return filepath.Join(s.dataDir, "tasks", taskID) }
func (s *Store) TaskSessionsDir(taskID string) string       { return filepath.Join(s.dataDir, "tasks", taskID, "sessions") }
func (s *Store) TaskMemoryDir(taskID string) string         { return filepath.Join(s.dataDir, "tasks", taskID, "memory") }
func (s *Store) TaskSessionsFile(taskID string) string      { return filepath.Join(s.dataDir, "tasks", taskID, "sessions.json") }
func (s *Store) TaskDraftPath(taskID string, version int) string {
	return filepath.Join(s.dataDir, "tasks", taskID, fmt.Sprintf("draft_v%d.md", version))
}

func (s *Store) GetSessionDir(taskID, sessionID string) string {
	return filepath.Join(s.dataDir, "tasks", taskID, "sessions", sessionID)
}

func (s *Store) GetSessionCWDDir(taskID, sessionID string) string {
	return filepath.Join(s.dataDir, "tasks", taskID, "sessions", sessionID, "cwd")
}

func (s *Store) EnsureTaskDir(taskID string) error {
	dir := s.TaskDir(taskID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	for _, sub := range []string{"sessions", "memory"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0755); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) CreateSessionDir(taskID, sessionID string) (string, error) {
	cwd := s.GetSessionCWDDir(taskID, sessionID)
	if err := os.MkdirAll(cwd, 0755); err != nil {
		return "", err
	}
	return cwd, nil
}

func (s *Store) GetTask(taskID string) (*models.Task, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.tasks[taskID]
	if !ok {
		return nil, fmt.Errorf("task not found: %s", taskID)
	}
	return t, nil
}

func (s *Store) TrySetActiveSession(taskID, sessionID string) (bool, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	t, ok := s.tasks[taskID]
	if !ok {
		return false, "", fmt.Errorf("task not found: %s", taskID)
	}
	if t.ActiveSessionID != "" {
		return false, t.ActiveSessionID, nil
	}
	t.ActiveSessionID = sessionID
	t.LastActiveAt = time.Now()
	if err := s.saveTasks(); err != nil {
		return false, "", err
	}
	return true, "", nil
}

func (s *Store) GetOrCreateTask(taskID, topic, uid, memoryModel, platform, skillID, model, accountID string) (*models.Task, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	t, ok := s.tasks[taskID]
	if ok {
		if t.AccountID == "" && accountID != "" {
			t.AccountID = accountID
			_ = s.saveTasks()
		}
		return t, false, nil
	}

	if err := s.EnsureTaskDir(taskID); err != nil {
		return nil, false, err
	}

	t = &models.Task{
		TaskID:      taskID,
		UID:         uid,
		Topic:       topic,
		Platform:    platform,
		SkillID:     skillID,
		Model:       model,
		MemoryModel: memoryModel,
		AccountID:   accountID,
		CreatedAt:   time.Now(),
		SessionIDs:  []string{},
	}
	s.tasks[taskID] = t
	if err := s.saveTasks(); err != nil {
		return nil, false, err
	}
	return t, true, nil
}

func (s *Store) UpdateTask(t *models.Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks[t.TaskID] = t
	return s.saveTasks()
}

func (s *Store) DeleteTask(taskID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tasks, taskID)
	if err := s.saveTasks(); err != nil {
		return err
	}
	// 删除任务目录（包含 sessions、drafts 等）
	taskDir := filepath.Join(s.dataDir, "tasks", taskID)
	_ = os.RemoveAll(taskDir)
	return nil
}

func (s *Store) ListTasks() []*models.Task {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*models.Task
	for _, t := range s.tasks {
		result = append(result, t)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].CreatedAt.After(result[j].CreatedAt)
	})
	return result
}

func (s *Store) ListTasksByUID(uid string) []*models.Task {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*models.Task
	for _, t := range s.tasks {
		if t.UID == uid {
			result = append(result, t)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].CreatedAt.After(result[j].CreatedAt)
	})
	return result
}

func (s *Store) LoadTaskSessions(taskID string) ([]*models.Session, error) {
	path := s.TaskSessionsFile(taskID)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var sessions []*models.Session
	if err := json.Unmarshal(data, &sessions); err != nil {
		return nil, err
	}
	return sessions, nil
}

func (s *Store) SaveTaskSessions(taskID string, sessions []*models.Session) error {
	path := s.TaskSessionsFile(taskID)
	data, err := json.MarshalIndent(sessions, "", "  ")
	if err != nil {
		return err
	}
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func (s *Store) UpsertSessionInTask(sess *models.Session) error {
	path := s.TaskSessionsFile(sess.TaskID)
	var sessions []*models.Session

	data, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(data, &sessions); err != nil {
			sessions = nil
		}
	}
	if sessions == nil {
		sessions = []*models.Session{}
	}

	found := false
	for i, existing := range sessions {
		if existing.SessionID == sess.SessionID {
			sessions[i] = sess
			found = true
			break
		}
	}
	if !found {
		sessions = append(sessions, sess)
	}

	return s.SaveTaskSessions(sess.TaskID, sessions)
}

func (s *Store) GetSession(taskID, sessionID string) (*models.Session, error) {
	path := s.TaskSessionsFile(taskID)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("session not found: %s", sessionID)
	}
	var sessions []*models.Session
	if err := json.Unmarshal(data, &sessions); err != nil {
		return nil, err
	}
	for _, sess := range sessions {
		if sess.SessionID == sessionID {
			return sess, nil
		}
	}
	return nil, fmt.Errorf("session not found: %s", sessionID)
}

func (s *Store) ListAllSessions() []*models.Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var all []*models.Session
	for taskID := range s.tasks {
		path := s.TaskSessionsFile(taskID)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var sessions []*models.Session
		if json.Unmarshal(data, &sessions) == nil {
			all = append(all, sessions...)
		}
	}
	return all
}

func (s *Store) SaveDraftVersion(taskID string, version int, content string) error {
	path := s.TaskDraftPath(taskID, version)
	return os.WriteFile(path, []byte(content), 0644)
}

func (s *Store) GetDraftVersion(taskID string, version int) (string, error) {
	path := s.TaskDraftPath(taskID, version)
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (s *Store) SaveSummary(taskID, sessionID string, epochNo int, content string) (string, error) {
	dir := s.GetSessionDir(taskID, sessionID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	filename := fmt.Sprintf("ep%d.md", epochNo)
	path := filepath.Join(dir, filename)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return "", err
	}
	return path, nil
}

func (s *Store) ReadShortTerm(taskID string) ([]byte, error) {
	path := filepath.Join(s.TaskMemoryDir(taskID), "short_term.md")
	return os.ReadFile(path)
}

func (s *Store) ReadMediumTerm(taskID string) ([]byte, error) {
	path := filepath.Join(s.TaskMemoryDir(taskID), "medium_term.md")
	return os.ReadFile(path)
}

func (s *Store) UpdateShortTerm(taskID string, sessions []*models.Session) error {
	dir := s.TaskMemoryDir(taskID)
	path := filepath.Join(dir, "short_term.md")

	start := 0
	if len(sessions) > models.ShortTermWindowSize {
		start = len(sessions) - models.ShortTermWindowSize
	}

	var content string
	for i := start; i < len(sessions); i++ {
		sess := sessions[i]
		cwd := s.GetSessionCWDDir(taskID, sess.SessionID)
		draftPath := filepath.Join(cwd, "current_draft.md")
		if data, err := os.ReadFile(draftPath); err == nil {
			content += fmt.Sprintf("## Session %s (会话 %d)\n\n", sess.SessionID, i+1)
			content += string(data)
			content += "\n\n"
		}
	}

	return os.WriteFile(path, []byte(content), 0644)
}

func (s *Store) AppendMediumTerm(taskID, summary string) error {
	dir := s.TaskMemoryDir(taskID)
	path := filepath.Join(dir, "medium_term.md")

	var content []byte
	if data, err := os.ReadFile(path); err == nil {
		content = data
	}

	content = append(content, []byte(summary)...)
	content = append(content, '\n', '\n')

	return os.WriteFile(path, content, 0644)
}

func (s *Store) HasTaskContext(taskID string) bool {
	dir := s.TaskMemoryDir(taskID)
	shortPath := filepath.Join(dir, "short_term.md")
	medPath := filepath.Join(dir, "medium_term.md")
	_, errShort := os.Stat(shortPath)
	_, errMed := os.Stat(medPath)
	return errShort == nil || errMed == nil
}

func (s *Store) EnsureTaskDirExists(taskID string) error {
	return s.EnsureTaskDir(taskID)
}
