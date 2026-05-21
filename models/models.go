package models

import "time"

type SessionStatus string

const (
	StatusCreated  SessionStatus = "CREATED"
	StatusWarm     SessionStatus = "WARM"
	StatusCold     SessionStatus = "COLD"
	StatusArchived SessionStatus = "ARCHIVED"
)

const (
	DefaultStaleTimeoutMin = 60
	ShortTermWindowSize    = 3
)

type Task struct {
	TaskID                string    `json:"task_id"`
	UID                   string    `json:"uid"`
	Topic                 string    `json:"topic"`
	Platform              string    `json:"platform"`
	SkillID               string    `json:"skill_id"`
	Model                 string    `json:"model"`
	MemoryModel           string    `json:"memory_model"`
	ActiveSessionID       string    `json:"active_session_id"`
	LastActiveAt          time.Time `json:"last_active_at"`
	CreatedAt             time.Time `json:"created_at"`
	SessionIDs            []string  `json:"session_ids"`
	SessionCount          int       `json:"session_count"`
	MemoryVersion         int       `json:"memory_version"`
	NovelName             string    `json:"novel_name,omitempty"`
	AccountID             string    `json:"account_id,omitempty"`
	PublishedChapterCount int       `json:"published_chapter_count"`
}

type Session struct {
	SessionID    string        `json:"session_id"`
	TaskID       string        `json:"task_id"`
	Topic        string        `json:"topic"`
	SkillID      string        `json:"skill_id"`
	Model        string        `json:"model"`
	Status       SessionStatus `json:"status"`
	OpenCodeSID  string        `json:"opencode_sid,omitempty"`
	CWDPath      string        `json:"cwd_path"`
	LatestDraft  string        `json:"latest_draft,omitempty"`
	MessageCount int           `json:"message_count"`
	TotalTokens  int           `json:"total_tokens"`
	DraftVersion int           `json:"draft_version"`
	CreatedAt    time.Time     `json:"created_at"`
	LastActiveAt time.Time     `json:"last_active_at"`
	ArchivedAt   *time.Time    `json:"archived_at,omitempty"`
	Episodes     []Episode     `json:"episodes,omitempty"`
}

type Episode struct {
	EpisodeID    string    `json:"episode_id"`
	SessionID    string    `json:"session_id"`
	EpochNo      int       `json:"epoch_no"`
	UserIntent   string    `json:"user_intent"`
	Decisions    string    `json:"decisions"`
	SummaryPath  string    `json:"summary_path"`
	MessageCount int       `json:"message_count"`
	CreatedAt    time.Time `json:"created_at"`
}

type SessionEvent struct {
	Type       string      `json:"type"`
	SessionID  string      `json:"session_id"`
	TaskID     string      `json:"task_id,omitempty"`
	Seq        int         `json:"seq,omitempty"`
	Text       string      `json:"text,omitempty"`
	Tool       string      `json:"tool,omitempty"`
	ToolArgs   interface{} `json:"tool_args,omitempty"`
	ToolResult string      `json:"tool_result,omitempty"`
	DraftPath  string      `json:"draft_path,omitempty"`
	Tokens     *TokenInfo  `json:"tokens,omitempty"`
	Error      string      `json:"error,omitempty"`
	Reason     string      `json:"reason,omitempty"`
	NovelName  string      `json:"novel_name,omitempty"`
	DraftVersion int       `json:"draft_version,omitempty"`
}

type TokenInfo struct {
	Total     int `json:"total"`
	Input     int `json:"input"`
	Output    int `json:"output"`
	Reasoning int `json:"reasoning"`
}

type CreateSessionRequest struct {
	TaskID       string `json:"task_id"`
	Topic        string `json:"topic"`
	Platform     string `json:"platform"`
	SkillID      string `json:"skill_id"`
	Model        string `json:"model"`
	MemoryModel  string `json:"memory_model"`
	UID          string `json:"uid"`
	AccountID    string `json:"account_id"`
	InitialMsg   string `json:"initial_msg,omitempty"`
	DraftVersion int    `json:"draft_version"`
}

type SendMessageRequest struct {
	Text         string `json:"text"`
	DraftVersion int    `json:"draft_version"`
}

type WakeTaskRequest struct {
	Text         string `json:"text,omitempty"`
	DraftVersion int    `json:"draft_version"`
}

type SkillInfo struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	Description      string   `json:"description"`
	Category         string   `json:"category"`
	RecommendedFor   string   `json:"recommended_for"`
	ModelRecommended string   `json:"model_recommended"`
	TargetPlatforms  []string `json:"target_platforms"`
}

type TaskInfo struct {
	TaskID                string    `json:"task_id"`
	UID                   string    `json:"uid"`
	Topic                 string    `json:"topic"`
	Platform              string    `json:"platform"`
	SkillID               string    `json:"skill_id"`
	Model                 string    `json:"model"`
	MemoryModel           string    `json:"memory_model"`
	ActiveSessionID       string    `json:"active_session_id"`
	LastActiveAt          time.Time `json:"last_active_at"`
	CreatedAt             time.Time `json:"created_at"`
	SessionCount          int       `json:"session_count"`
	MemoryVersion         int       `json:"memory_version"`
	Stale                 bool      `json:"stale"`
	NovelName             string    `json:"novel_name,omitempty"`
	AccountID             string    `json:"account_id,omitempty"`
	PublishedChapterCount int       `json:"published_chapter_count"`
}

type TaskBusyError struct {
	TaskID    string `json:"task_id"`
	SessionID string `json:"existing_session_id"`
	Message   string `json:"error"`
}

func (e *TaskBusyError) Error() string {
	return e.Message
}

type MemorySummary struct {
	Topic    string `json:"topic"`
	Intent   string `json:"intent"`
	Summary  string `json:"summary"`
	KeyDecisions []string `json:"key_decisions"`
	DraftPreview string `json:"draft_preview"`
}
