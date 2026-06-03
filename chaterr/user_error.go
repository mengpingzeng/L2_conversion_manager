package chaterr

import "strings"

// UserFacing 将后台/进程英文错误转为会话区中文提示（原始文案仍打在日志里）。
func UserFacing(raw string) string {
	msg := strings.TrimSpace(raw)
	if msg == "" {
		return "发生未知错误，请稍后重试。"
	}
	lower := strings.ToLower(msg)

	switch {
	case strings.Contains(lower, "opencode exited"),
		strings.Contains(lower, "exit status"),
		strings.Contains(lower, "failed to start opencode"),
		strings.Contains(lower, "start failed"):
		return "AI 创作进程异常退出，请稍后重试。"
	case strings.Contains(lower, "process timeout"),
		strings.Contains(lower, "timeout or cancelled"),
		strings.Contains(lower, "context deadline"):
		return "AI 处理超时，请稍后重试。"
	case strings.Contains(lower, "server busy"):
		return "服务繁忙，请稍后再试。"
	case strings.Contains(lower, "stdout pipe"),
		strings.Contains(lower, "stderr pipe"):
		return "AI 服务连接异常，请稍后重试。"
	case msg == "AI 未返回内容，请重试":
		return msg
	case strings.Contains(msg, "当前任务没有可修改的章节草稿"),
		strings.Contains(msg, "当前章节已发布"),
		strings.Contains(msg, "上一条消息还在处理中"):
		return msg
	}

	if isMostlyTechnicalEnglish(msg) {
		return "AI 服务异常，请稍后重试。若反复出现请联系管理员。"
	}
	return msg
}

func isMostlyTechnicalEnglish(msg string) bool {
	han := 0
	for _, r := range msg {
		if r >= 0x4e00 && r <= 0x9fff {
			han++
		}
	}
	if han >= 2 {
		return false
	}
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "error") ||
		strings.Contains(lower, "failed") ||
		strings.Contains(lower, "exit") ||
		strings.Contains(lower, "opencode") ||
		strings.Contains(lower, "timeout")
}
