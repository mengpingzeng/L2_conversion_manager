package manager

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"session_manager/store"
)

var (
	chapterHeadingLine = regexp.MustCompile(`(?m)^(?:#{1,3}\s*)?(?:\*\*)?(?:第[0-9一二三四五六七八九十百千]+章|\*\*第[0-9一二三四五六七八九十百千]+章)`)
	sectionHeadingLine = regexp.MustCompile(`(?m)^##\s+[一二三四五六七八九十百千\d]+(?:\s|$)`)
	volumeSectionLine  = regexp.MustCompile(`(?m)^#{1,3}\s*第[0-9一二三四五六七八九十百千]+[节部分]`)
	markdownTitleLine  = regexp.MustCompile(`(?m)^#{1,2}\s+[^\n#]{2,40}$`)
	processHintLine    = regexp.MustCompile(`已完成|已写入|概要|本章|风格|对应|写入|current_draft|草稿|章节信息|标题|指纹|落盘`)
	completionTailRe   = regexp.MustCompile(`(?is)(?:已完成|已写入)[\s\S]{0,800}?(?:current_draft\.md|current_draft|草稿)[。.!！\x60]?`)
	draftHeadingTitleRe = regexp.MustCompile(`^#\s*第[0-9一二三四五六七八九十百千]+章\s*(?:《([^》]+)》|(.+))?\s*$`)
)

var cnDigits = []string{"零", "一", "二", "三", "四", "五", "六", "七", "八", "九"}

// ChatDisplayText 从 AI 完整输出中提取适合会话区展示的文案，去掉章节正文。
func ChatDisplayText(full string, draftPath string) string {
	full = strings.TrimSpace(full)
	if full == "" {
		return ""
	}

	if draftPath != "" {
		if data, err := os.ReadFile(draftPath); err == nil {
			draft := strings.TrimSpace(string(data))
			if draft != "" {
				if strings.Contains(full, draft) {
					full = strings.TrimSpace(strings.Replace(full, draft, "", 1))
				} else if len(draft) >= 400 && len(full) >= len(draft)/2 {
					draftRunes := []rune(draft)
					prefixLen := min(len(draftRunes), 800)
					prefix := string(draftRunes[:prefixLen])
					if strings.HasPrefix(full, prefix) {
						full = strings.TrimSpace(full[len(prefix):])
					}
				}
			}
		}
	}

	if idx := findChapterBodyStart(full); idx >= 0 {
		return strings.TrimSpace(full[:idx])
	}

	full = extractProcessSummary(full)
	if looksLikeChapterBody(full) {
		return ""
	}

	return strings.TrimSpace(full)
}

func findChapterBodyStart(text string) int {
	candidates := []int{
		indexOrNeg1(chapterHeadingLine.FindStringIndex(text)),
		indexOrNeg1(sectionHeadingLine.FindStringIndex(text)),
		indexOrNeg1(volumeSectionLine.FindStringIndex(text)),
		indexOrNeg1(markdownTitleLine.FindStringIndex(text)),
	}
	best := -1
	for _, idx := range candidates {
		if idx >= 0 && (best < 0 || idx < best) {
			best = idx
		}
	}
	return best
}

func extractProcessSummary(text string) string {
	if loc := completionTailRe.FindStringIndex(text); loc != nil {
		end := loc[1]
		head := strings.TrimSpace(text[:end])
		tail := strings.TrimSpace(text[end:])
		if len(tail) > 80 && !processHintLine.MatchString(tail[:min(len(tail), 100)]) {
			return head
		}
	}

	blocks := strings.Split(text, "\n\n")
	if len(blocks) >= 2 {
		first := strings.TrimSpace(blocks[0])
		rest := strings.TrimSpace(strings.Join(blocks[1:], "\n\n"))
		if len(rest) > 350 && len(first) < 700 && (processHintLine.MatchString(first) || len(first) < 400) {
			return first
		}
	}

	if len(text) > 1200 {
		lines := strings.Split(text, "\n")
		var kept []string
		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				continue
			}
			if processHintLine.MatchString(line) ||
				strings.HasPrefix(trimmed, "-") ||
				strings.HasPrefix(trimmed, "*") ||
				strings.HasPrefix(trimmed, "•") ||
				len(trimmed) < 100 {
				kept = append(kept, line)
			}
		}
		joined := strings.TrimSpace(strings.Join(kept, "\n"))
		if joined != "" && len(joined) < len(text)*45/100 {
			return joined
		}
	}

	return text
}

func looksLikeChapterBody(text string) bool {
	text = strings.TrimSpace(text)
	if len(text) < 280 {
		return false
	}
	if findChapterBodyStart(text) >= 0 {
		return true
	}
	head := text
	if len(head) > 200 {
		head = head[:200]
	}
	if processHintLine.MatchString(head) {
		return false
	}
	var cn int
	for _, r := range text {
		if unicode.Is(unicode.Han, r) {
			cn++
		}
	}
	if len(text) == 0 {
		return false
	}
	return cn*100/len(text) > 35 && len(text) > 500
}

func indexOrNeg1(pair []int) int {
	if len(pair) == 0 {
		return -1
	}
	return pair[0]
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func formatChapterLabelCN(chapterNo int) string {
	if chapterNo <= 0 {
		return ""
	}
	return "第" + toChineseNumber(chapterNo) + "章"
}

func toChineseNumber(n int) string {
	if n < 0 {
		return fmt.Sprintf("%d", n)
	}
	if n == 0 {
		return "零"
	}
	if n < 10 {
		return cnDigits[n]
	}
	if n < 20 {
		if n == 10 {
			return "十"
		}
		return "十" + cnDigits[n%10]
	}
	if n < 100 {
		tens := n / 10
		ones := n % 10
		if ones == 0 {
			return cnDigits[tens] + "十"
		}
		return cnDigits[tens] + "十" + cnDigits[ones]
	}
	if n < 1000 {
		hundreds := n / 100
		rest := n % 100
		restStr := ""
		if rest > 0 {
			if rest < 10 {
				restStr = "零" + toChineseNumber(rest)
			} else {
				restStr = toChineseNumber(rest)
			}
		}
		return cnDigits[hundreds] + "百" + restStr
	}
	if n < 10000 {
		thousands := n / 1000
		rest := n % 1000
		restStr := ""
		if rest > 0 {
			if rest < 100 {
				restStr = "零" + toChineseNumber(rest)
			} else {
				restStr = toChineseNumber(rest)
			}
		}
		return cnDigits[thousands] + "千" + restStr
	}
	return fmt.Sprintf("%d", n)
}

const chapterWrittenActionSuffix = "请点击章节列表查看正文。"

// normalizeVolumeLabel 保证卷名带「第」前缀，如「一卷」→「第一卷」。
func normalizeVolumeLabel(volumeName string) string {
	vol := strings.TrimSpace(volumeName)
	if vol == "" {
		return ""
	}
	if strings.HasPrefix(vol, "第") && strings.HasSuffix(vol, "卷") {
		return vol
	}
	if strings.HasSuffix(vol, "卷") {
		body := strings.TrimSuffix(vol, "卷")
		if body != "" {
			return "第" + body + "卷"
		}
	}
	return vol
}

// draftWrittenNotice 生成章节写完的用户向提示（不含 current_draft 等技术信息）。
// 示例：第一卷第八章《深部对话》已写好，请点击章节列表查看正文。
func draftWrittenNotice(chapterNo int, volumeName, chapterTitle string) string {
	vol := normalizeVolumeLabel(volumeName)
	title := strings.TrimSpace(chapterTitle)
	if chapterNo <= 0 && vol == "" {
		return "章节已写好，" + chapterWrittenActionSuffix
	}
	var b strings.Builder
	if vol != "" {
		b.WriteString(vol)
	}
	if chapterNo > 0 {
		b.WriteString(formatChapterLabelCN(chapterNo))
	}
	if title != "" {
		b.WriteString("《")
		b.WriteString(title)
		b.WriteString("》")
	}
	if b.Len() == 0 {
		return "章节已写好，" + chapterWrittenActionSuffix
	}
	return b.String() + "已写好，" + chapterWrittenActionSuffix
}

var (
	draftFileInTextRe     = regexp.MustCompile(`(?i)current_draft(?:\.md)?`)
	chapterCompletionRe   = regexp.MustCompile(`(?i)(?:已完成|已写入|已写好|写入|完成).*(?:章|草稿)|(?:章|草稿).*(?:已完成|已写入|已写好|写入)`)
	chapterTitleInNoticeRe = regexp.MustCompile(`第\s*([0-9一二三四五六七八九十百千零两\d]+)\s*章\s*[《「]([^》」]+)[》」]`)
)

func isDraftCompletionLike(msg string) bool {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return false
	}
	if draftFileInTextRe.MatchString(msg) {
		return true
	}
	if isDraftWrittenNotice(msg) {
		return true
	}
	return chapterCompletionRe.MatchString(msg)
}

func isDraftWrittenNotice(msg string) bool {
	msg = strings.TrimSpace(msg)
	return strings.Contains(msg, chapterWrittenActionSuffix) ||
		strings.Contains(msg, "已写好，请在章节列表中查看") ||
		strings.Contains(msg, "的内容已写入草稿，请在左侧章节列表查看。") ||
		strings.Contains(msg, "章节内容已写入草稿")
}

var englishChapterNoRe = regexp.MustCompile(`(?i)chapter\s*([0-9]+)\b`)

func isEnglishProcessNarration(msg string) bool {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return false
	}
	cn := 0
	for _, r := range msg {
		if unicode.Is(unicode.Han, r) {
			cn++
		}
	}
	if cn >= 4 {
		return false
	}
	lower := strings.ToLower(msg)
	if englishChapterNoRe.MatchString(lower) {
		return true
	}
	return strings.Contains(lower, "let me") || strings.Contains(lower, "i'll") ||
		strings.Contains(lower, "i will") || strings.Contains(lower, "craft it")
}

func normalizeDraftCompletionDisplay(display string, chapterNo int, volumeName, chapterTitle, draftPath string) string {
	if isEnglishProcessNarration(display) {
		title := strings.TrimSpace(chapterTitle)
		if title == "" {
			title = parseDraftChapterTitle(draftPath)
		}
		vol := strings.TrimSpace(volumeName)
		if m := englishChapterNoRe.FindStringSubmatch(display); len(m) >= 2 {
			if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
				return draftWrittenNotice(n, vol, title)
			}
		}
		if chapterNo > 0 || title != "" || vol != "" {
			return draftWrittenNotice(chapterNo, vol, title)
		}
		return "正在撰写章节，完成后" + chapterWrittenActionSuffix
	}
	if !isDraftCompletionLike(display) {
		return display
	}
	title := strings.TrimSpace(chapterTitle)
	if title == "" {
		title = parseDraftChapterTitle(draftPath)
	}
	if m := chapterTitleInNoticeRe.FindStringSubmatch(display); len(m) >= 3 && chapterNo <= 0 {
		if n := parseChapterNoToken(m[1]); n > 0 {
			chapterNo = n
		}
		if title == "" {
			title = strings.TrimSpace(m[2])
		}
	}
	if m := englishChapterNoRe.FindStringSubmatch(display); len(m) >= 2 && chapterNo <= 0 {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			chapterNo = n
		}
	}
	return draftWrittenNotice(chapterNo, volumeName, title)
}

func parseChapterNoToken(token string) int {
	token = strings.TrimSpace(strings.ReplaceAll(token, " ", ""))
	if n, err := strconv.Atoi(token); err == nil && n > 0 {
		return n
	}
	token = strings.TrimPrefix(token, "第")
	token = strings.TrimSuffix(token, "章")
	if n, err := strconv.Atoi(token); err == nil && n > 0 {
		return n
	}
	return 0
}

func parseDraftChapterTitle(draftPath string) string {
	if draftPath == "" {
		return ""
	}
	data, err := os.ReadFile(draftPath)
	if err != nil || len(data) == 0 {
		return ""
	}
	if title := store.ExtractChapterTitle(string(data)); title != "" {
		return title
	}
	firstLine := strings.TrimSpace(strings.SplitN(string(data), "\n", 2)[0])
	m := draftHeadingTitleRe.FindStringSubmatch(firstLine)
	if len(m) < 2 {
		return ""
	}
	if m[1] != "" {
		return strings.TrimSpace(m[1])
	}
	if len(m) >= 3 && m[2] != "" {
		return strings.Trim(strings.TrimSpace(m[2]), "《》")
	}
	return ""
}

// DraftFileChangedSince 本轮开始前草稿基线；用于判断本轮是否已落盘章节正文。
func DraftFileChangedSince(draftPath string, baselineMod time.Time, baselineSize int64) bool {
	if draftPath == "" {
		return false
	}
	st, err := os.Stat(draftPath)
	if err != nil || st.Size() == 0 {
		return false
	}
	return st.ModTime().After(baselineMod) || st.Size() != baselineSize
}

// chatDisplayOrDraftNotice 持久化到 messages.jsonl 的 assistant 文案。
// draftWrittenThisTurn：本轮已通过 write 工具或更新了 current_draft.md 时，仅展示章节写完提示，不展示正文片段。
func chatDisplayOrDraftNotice(full, draftPath string, chapterNo int, volumeName, chapterTitle string, draftWrittenThisTurn bool) string {
	vol := strings.TrimSpace(volumeName)
	title := strings.TrimSpace(chapterTitle)
	if title == "" {
		title = parseDraftChapterTitle(draftPath)
	}
	if draftWrittenThisTurn {
		return draftWrittenNotice(chapterNo, vol, title)
	}

	display := ChatDisplayText(full, draftPath)
	if display != "" {
		return normalizeDraftCompletionDisplay(display, chapterNo, vol, title, draftPath)
	}
	if strings.TrimSpace(full) == "" {
		return ""
	}
	if draftPath != "" {
		if info, err := os.Stat(draftPath); err == nil && info.Size() > 0 {
			return draftWrittenNotice(chapterNo, vol, title)
		}
	}
	if looksLikeChapterBody(full) || len(strings.TrimSpace(full)) > 800 {
		return draftWrittenNotice(chapterNo, vol, title)
	}
	return ""
}

func isWriteToolName(tool string) bool {
	lower := strings.ToLower(tool)
	return lower == "write" || lower == "write_file" || lower == "write_to_file" ||
		lower == "filewrite" || lower == "edit"
}

func chatDisplayTextDelta(fullAccumulated, draftPath, delta string) string {
	if delta == "" {
		return ""
	}
	prevLen := len(fullAccumulated) - len(delta)
	if prevLen < 0 {
		prevLen = 0
	}
	prev := ChatDisplayText(fullAccumulated[:prevLen], draftPath)
	curr := ChatDisplayText(fullAccumulated, draftPath)
	if curr == prev {
		return ""
	}
	if strings.HasPrefix(curr, prev) {
		return curr[len(prev):]
	}
	return ""
}
