package api

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"clawstudios/pkg/logging"

	"session_manager/manager"
	"session_manager/models"
	"session_manager/store"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
)

type Server struct {
	sm       *manager.SessionManager
	router   *mux.Router
	upgrader websocket.Upgrader
}

func NewServer(sm *manager.SessionManager) *Server {
	s := &Server{
		sm:     sm,
		router: mux.NewRouter(),
		upgrader: websocket.Upgrader{
			CheckOrigin:     func(r *http.Request) bool { return true },
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
		},
	}
	s.registerRoutes()
	return s
}

func (s *Server) Router() *mux.Router {
	return s.router
}

func (s *Server) registerRoutes() {
	api := s.router.PathPrefix("/api").Subrouter()

	api.HandleFunc("/status", s.handleStatus).Methods("GET")
	api.HandleFunc("/pool/status", s.handlePoolStatus).Methods("GET")
	api.HandleFunc("/skill/list", s.handleListSkills).Methods("GET")

	api.HandleFunc("/task/list", s.handleTaskList).Methods("GET")
	api.HandleFunc("/task/{id}", s.handleTaskGet).Methods("GET")
	api.HandleFunc("/task/{id}/wake", s.handleTaskWake).Methods("POST")
	api.HandleFunc("/task/{id}/update", s.handleTaskUpdate).Methods("POST")
	api.HandleFunc("/task/{id}/sessions", s.handleTaskSessions).Methods("GET")
	api.HandleFunc("/task/{id}/messages", s.handleTaskMessages).Methods("GET")
	api.HandleFunc("/task/{id}/stream", s.handleTaskStream).Methods("GET")
	api.HandleFunc("/task/{id}/timeline", s.handleTaskTimeline).Methods("GET")
	api.HandleFunc("/task/{id}", s.handleTaskDelete).Methods("DELETE")

	api.HandleFunc("/session/create", s.handleCreate).Methods("POST")
	api.HandleFunc("/session/{id}/send", s.handleSend).Methods("POST")
	api.HandleFunc("/session/{id}/close", s.handleClose).Methods("POST")
	api.HandleFunc("/session/{id}", s.handleGetSession).Methods("GET")
	api.HandleFunc("/session/{id}/stream", s.handleStream).Methods("GET")
	api.HandleFunc("/session/{id}/draft", s.handleGetDraft).Methods("GET")

	api.HandleFunc("/sessions", s.handleListSessions).Methods("GET")

	if staticDir := s.findStaticDir(); staticDir != "" {
		fs := http.FileServer(http.Dir(staticDir))
		s.router.PathPrefix("/").Handler(http.StripPrefix("/", fs))
	}
}

func (s *Server) findStaticDir() string {
	candidates := []string{
		"static",
		filepath.Join(os.Getenv("HOME"), ".session_manager/static"),
	}
	for _, dir := range candidates {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return dir
		}
	}
	return ""
}

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (s *Server) handleTaskList(w http.ResponseWriter, r *http.Request) {
	uid := r.URL.Query().Get("uid")
	search := r.URL.Query().Get("q")

	page := 1
	size := 12
	if p := r.URL.Query().Get("page"); p != "" {
		if v, err := strconv.Atoi(p); err == nil && v > 0 {
			page = v
		}
	}
	if sz := r.URL.Query().Get("size"); sz != "" {
		if v, err := strconv.Atoi(sz); err == nil && v > 0 {
			size = v
		}
	}

	tasks, total := s.sm.ListTasksPage(uid, search, page, size)
	if tasks == nil {
		tasks = []models.TaskInfo{}
	}
	writeJSON(w, 200, map[string]interface{}{
		"tasks": tasks,
		"total": total,
	})
}

func (s *Server) handleTaskGet(w http.ResponseWriter, r *http.Request) {
	logger := logging.FromContext(r.Context())
	taskID := mux.Vars(r)["id"]
	task, err := s.sm.GetTask(taskID)
	if err != nil {
		if logger != nil {
			logger.Error(logging.ErrNotFound, "GetTask(%s) failed: %v", taskID, err)
		}
		writeError(w, 404, "task not found: "+taskID)
		return
	}
	if logger != nil {
		logger.Info("task fetched: task=%s chapter=%d volume=%s", taskID, task.ChapterNumber, task.VolumeName)
	}
	writeJSON(w, 200, task)
}

func (s *Server) handleTaskWake(w http.ResponseWriter, r *http.Request) {
	logger := logging.FromContext(r.Context())
	taskID := mux.Vars(r)["id"]

	var req models.WakeTaskRequest
	if r.Body != nil {
		json.NewDecoder(r.Body).Decode(&req)
	}

	sess, err := s.sm.WakeTask(r.Context(), taskID, req)
	if err != nil {
		if busy, ok := err.(*models.TaskBusyError); ok {
			writeJSON(w, 409, map[string]interface{}{
				"error":               busy.Message,
				"task_id":             busy.TaskID,
				"existing_session_id": busy.SessionID,
				"hint":                "close the existing session or use /api/session/{id}/send to send a message",
			})
			return
		}
		if logger != nil {
			logger.Error(logging.ErrSessionError, "WakeTask(%s) failed: %v", taskID, err)
		}
		writeError(w, 500, "failed to wake task: "+err.Error())
		return
	}

	writeJSON(w, 201, map[string]interface{}{
		"session_id": sess.SessionID,
		"task_id":    sess.TaskID,
		"status":     sess.Status,
		"cwd_path":   sess.CWDPath,
	})
}

func (s *Server) handleTaskUpdate(w http.ResponseWriter, r *http.Request) {
	logger := logging.FromContext(r.Context())
	taskID := mux.Vars(r)["id"]

	var req struct {
		NovelName         string `json:"novel_name"`
		AccountID         string `json:"account_id"`
		VolumeName        string `json:"volume_name"`
		Title             string `json:"title"`
		ChapterNumber     int    `json:"chapter_number"`
		ChapterCountDelta int    `json:"chapter_count_delta"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		if logger != nil {
			logger.Error(logging.ErrInvalidParam, "decode request body failed: %v", err)
		}
		writeError(w, 400, "invalid request body: "+err.Error())
		return
	}

	if err := s.sm.UpdateTaskFields(taskID, req.NovelName, req.AccountID, req.VolumeName, req.Title, req.ChapterNumber, req.ChapterCountDelta); err != nil {
		if logger != nil {
			logger.Error(logging.ErrDatabaseError, "UpdateTaskFields(%s) failed: %v", taskID, err)
		}
		writeError(w, 500, "failed to update task: "+err.Error())
		return
	}

	writeJSON(w, 200, map[string]string{"status": "updated"})
}

func (s *Server) handleTaskDelete(w http.ResponseWriter, r *http.Request) {
	logger := logging.FromContext(r.Context())
	taskID := mux.Vars(r)["id"]
	if err := s.sm.DeleteTask(taskID); err != nil {
		if logger != nil {
			logger.Error(logging.ErrDatabaseError, "DeleteTask(%s) failed: %v", taskID, err)
		}
		writeError(w, 500, "failed to delete task: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}

func (s *Server) handleTaskSessions(w http.ResponseWriter, r *http.Request) {
	logger := logging.FromContext(r.Context())
	taskID := mux.Vars(r)["id"]
	sessions, err := s.sm.ListSessions(taskID)
	if err != nil {
		if logger != nil {
			logger.Error(logging.ErrDatabaseError, "ListSessions(%s) failed: %v", taskID, err)
		}
		writeError(w, 500, "failed to list sessions: "+err.Error())
		return
	}
	if sessions == nil {
		sessions = []*models.Session{}
	}
	if logger != nil {
		logger.Info("sessions listed: task=%s count=%d", taskID, len(sessions))
	}
	writeJSON(w, 200, map[string]interface{}{
		"sessions": sessions,
		"count":    len(sessions),
	})
}

func (s *Server) handleTaskMessages(w http.ResponseWriter, r *http.Request) {
	logger := logging.FromContext(r.Context())
	taskID := mux.Vars(r)["id"]
	messages, err := s.sm.ListTaskMessages(taskID)
	if err != nil {
		if logger != nil {
			logger.Error(logging.ErrDatabaseError, "ListTaskMessages(%s) failed: %v", taskID, err)
		}
		writeError(w, 500, "failed to list task messages: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]interface{}{
		"messages": messages,
		"count":    len(messages),
	})
}

func (s *Server) handleTaskStream(w http.ResponseWriter, r *http.Request) {
	logger := logging.FromContext(r.Context())
	taskID := mux.Vars(r)["id"]

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		if logger != nil {
			logger.Error(logging.ErrInternal, "ws upgrade failed for task %s: %v", taskID, err)
		}
		log.Printf("task ws upgrade failed: %v", err)
		return
	}
	defer conn.Close()

	ch := s.sm.SubscribeTask(taskID)
	defer s.sm.UnsubscribeTask(taskID, ch)

	for evt := range ch {
		data, err := json.Marshal(evt)
		if err != nil {
			continue
		}
		if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
			break
		}
	}
}

type timelineEvent struct {
	EventID       string `json:"event_id"`
	SessionID     string `json:"session_id"`
	EventType     string `json:"event_type"`
	ChapterNumber int    `json:"chapter_number"`
	VolumeName    string `json:"volume_name,omitempty"`
	Description   string `json:"description"`
	CreatedAt     string `json:"created_at"`
}

func (s *Server) handleTaskTimeline(w http.ResponseWriter, r *http.Request) {
	logger := logging.FromContext(r.Context())
	taskID := mux.Vars(r)["id"]

	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			limit = n
		}
	}
	cursor := r.URL.Query().Get("cursor")

	sessions, err := s.sm.ListSessions(taskID)
	if err != nil {
		if logger != nil {
			logger.Error(logging.ErrDatabaseError, "ListSessions for timeline(%s) failed: %v", taskID, err)
		}
		writeError(w, 500, "failed to list sessions: "+err.Error())
		return
	}

	events := make([]timelineEvent, 0)
	for _, sess := range sessions {
		if sess.SessionID == "" {
			continue
		}
		eventID := sess.SessionID + "_created"
		events = append(events, timelineEvent{
			EventID:       eventID,
			SessionID:     sess.SessionID,
			EventType:     "session_created",
			ChapterNumber: sess.ChapterNumber,
			VolumeName:    sess.VolumeName,
			Description:   "开始生成第" + strconv.Itoa(sess.ChapterNumber) + "章",
			CreatedAt:     sess.CreatedAt.Format("2006-01-02T15:04:05Z"),
		})

		if sess.ArchivedAt != nil {
			eventID := sess.SessionID + "_archived"
			events = append(events, timelineEvent{
				EventID:       eventID,
				SessionID:     sess.SessionID,
				EventType:     "session_archived",
				ChapterNumber: sess.ChapterNumber,
				VolumeName:    sess.VolumeName,
				Description:   "第" + strconv.Itoa(sess.ChapterNumber) + "章生成完成",
				CreatedAt:     sess.ArchivedAt.Format("2006-01-02T15:04:05Z"),
			})
		}
	}

	// 按时间降序排序
	for i := 0; i < len(events); i++ {
		for j := i + 1; j < len(events); j++ {
			if events[i].CreatedAt < events[j].CreatedAt {
				events[i], events[j] = events[j], events[i]
			}
		}
	}

	// cursor 分页
	startIdx := 0
	if cursor != "" {
		for i, e := range events {
			if e.EventID == cursor {
				startIdx = i + 1
				break
			}
		}
	}

	endIdx := startIdx + limit
	if endIdx > len(events) {
		endIdx = len(events)
	}

	paged := events[startIdx:endIdx]
	hasMore := endIdx < len(events)
	nextCursor := ""
	if hasMore && len(paged) > 0 {
		nextCursor = paged[len(paged)-1].EventID
	}

	if logger != nil {
		logger.Info("timeline for task=%s: total=%d returned=%d has_more=%v",
			taskID, len(events), len(paged), hasMore)
	}

	writeJSON(w, 200, map[string]interface{}{
		"events":      paged,
		"has_more":    hasMore,
		"next_cursor": nextCursor,
	})
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	logger := logging.FromContext(r.Context())
	var req models.CreateSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		if logger != nil {
			logger.Error(logging.ErrInvalidParam, "decode request body failed: %v", err)
		}
		writeError(w, 400, "invalid request body: "+err.Error())
		return
	}

	if req.TaskID == "" {
		writeError(w, 400, "task_id is required")
		return
	}
	if req.Topic == "" {
		writeError(w, 400, "topic is required")
		return
	}
	if req.SkillID == "" {
		req.SkillID = "general_fallback_v1"
	}

	sess, err := s.sm.Create(r.Context(), req)
	if err != nil {
		if busy, ok := err.(*models.TaskBusyError); ok {
			writeJSON(w, 409, map[string]interface{}{
				"error":               busy.Message,
				"task_id":             busy.TaskID,
				"existing_session_id": busy.SessionID,
				"hint":                "close the existing session or use /api/task/{id}/wake to create a new one",
			})
			return
		}
		if logger != nil {
			logger.Error(logging.ErrSessionError, "Create session for task %s failed: %v", req.TaskID, err)
		}
		writeError(w, 500, "failed to create session: "+err.Error())
		return
	}

	if logger != nil {
		logger.Info("session created: session=%s task=%s skill=%s model=%s",
			sess.SessionID, req.TaskID, req.SkillID, req.Model)
	}

	if req.NovelName != "" {
		_ = s.sm.UpdateTaskFields(req.TaskID, req.NovelName, "", "", "", 0, 0)
	}

	writeJSON(w, 201, map[string]interface{}{
		"session_id":    sess.SessionID,
		"task_id":       sess.TaskID,
		"status":        sess.Status,
		"cwd_path":      sess.CWDPath,
		"draft_version": sess.DraftVersion,
	})
}

func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	logger := logging.FromContext(r.Context())
	sessionID := mux.Vars(r)["id"]

	var req models.SendMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		if logger != nil {
			logger.Error(logging.ErrInvalidParam, "decode request body failed: %v", err)
		}
		writeError(w, 400, "invalid request body: "+err.Error())
		return
	}

	if req.Text == "" {
		writeError(w, 400, "text is required")
		return
	}

	if err := s.sm.Send(r.Context(), sessionID, req); err != nil {
		if logger != nil {
			logger.Error(logging.ErrSessionError, "Send to session %s failed: %v", sessionID, err)
		}
		writeError(w, 500, "failed to send message: "+err.Error())
		return
	}

	writeJSON(w, 200, map[string]interface{}{
		"accepted": true,
		"message":  "message queued, stream events via WebSocket",
	})
}

func (s *Server) handleClose(w http.ResponseWriter, r *http.Request) {
	logger := logging.FromContext(r.Context())
	sessionID := mux.Vars(r)["id"]

	if err := s.sm.Close(r.Context(), sessionID); err != nil {
		if logger != nil {
			logger.Error(logging.ErrSessionError, "Close session %s failed: %v", sessionID, err)
		}
		writeError(w, 500, "failed to close session: "+err.Error())
		return
	}

	if logger != nil {
		logger.Info("session closed: session=%s", sessionID)
	}
	writeJSON(w, 200, map[string]string{
		"session_id": sessionID,
		"status":     "archived",
	})
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	logger := logging.FromContext(r.Context())
	sessionID := mux.Vars(r)["id"]

	sess, _, err := s.sm.GetSession(sessionID)
	if err != nil {
		if logger != nil {
			logger.Error(logging.ErrNotFound, "GetSession(%s) failed: %v", sessionID, err)
		}
		writeError(w, 404, "session not found: "+sessionID)
		return
	}

	writeJSON(w, 200, sess)
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	logger := logging.FromContext(r.Context())
	taskID := r.URL.Query().Get("task_id")

	var sessions []*models.Session
	if taskID != "" {
		var err error
		sessions, err = s.sm.ListSessions(taskID)
		if err != nil {
			if logger != nil {
				logger.Error(logging.ErrDatabaseError, "ListSessions(%s) failed: %v", taskID, err)
			}
			writeError(w, 500, "failed to list sessions: "+err.Error())
			return
		}
	} else {
		sessions = s.sm.ListAllSessions()
	}

	if sessions == nil {
		sessions = []*models.Session{}
	}
	writeJSON(w, 200, map[string]interface{}{
		"sessions": sessions,
		"count":    len(sessions),
	})
}

func (s *Server) handleListSkills(w http.ResponseWriter, r *http.Request) {
	skills := s.sm.ListSkills()
	writeJSON(w, 200, map[string]interface{}{
		"skills": skills,
		"count":  len(skills),
	})
}

func (s *Server) handlePoolStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]string{
		"status": s.sm.PoolStatus(),
	})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]interface{}{
		"service": "session_manager",
		"status":  "running",
		"pool":    s.sm.PoolStatus(),
	})
}

func (s *Server) handleGetDraft(w http.ResponseWriter, r *http.Request) {
	logger := logging.FromContext(r.Context())
	sessionID := mux.Vars(r)["id"]

	draft, err := s.sm.ScanDraftFile(sessionID)
	if err != nil {
		if logger != nil {
			logger.Error(logging.ErrNotFound, "ScanDraftFile(%s) failed: %v", sessionID, err)
		}
		writeError(w, 404, "draft not found: "+err.Error())
		return
	}

	chapterTitle := store.ExtractChapterTitle(draft)

	resp := map[string]interface{}{
		"session_id":    sessionID,
		"draft":         draft,
		"chapter_title": chapterTitle,
	}

	sess, _, err := s.sm.GetSession(sessionID)
	if err == nil {
		resp["draft_version"] = sess.DraftVersion
		if logger != nil {
			logger.Info("draft returned: session=%s version=%d", sessionID, sess.DraftVersion)
		}
	}

	writeJSON(w, 200, resp)
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	logger := logging.FromContext(r.Context())
	sessionID := mux.Vars(r)["id"]

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		if logger != nil {
			logger.Error(logging.ErrInternal, "ws upgrade failed for session %s: %v", sessionID, err)
		}
		log.Printf("ws upgrade failed: %v", err)
		return
	}
	defer conn.Close()

	ch := s.sm.Subscribe(sessionID)
	defer s.sm.Unsubscribe(sessionID, ch)

	for evt := range ch {
		data, err := json.Marshal(evt)
		if err != nil {
			continue
		}
		if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
			break
		}
	}
}
