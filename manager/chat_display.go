package manager

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"unicode"
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
					prefixLen := min(len(draft), 800)
					if strings.HasPrefix(full, draft[:prefixLen]) {
						full = strings.TrimSpace(full[prefixLen:])
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

// draftWrittenNotice 生成「某章已写入草稿」的会话提示；chapterNo<=0 时无法定位章节则用通用文案。
func draftWrittenNotice(chapterNo int, chapterTitle string) string {
	if chapterNo <= 0 {
		return "章节内容已写入草稿，请在左侧章节列表查看。"
	}
	label := formatChapterLabelCN(chapterNo)
	title := strings.TrimSpace(chapterTitle)
	if title != "" {
		return label + "《" + title + "》的内容已写入草稿，请在左侧章节列表查看。"
	}
	return label + "的内容已写入草稿，请在左侧章节列表查看。"
}

func isDraftWrittenNotice(msg string) bool {
	msg = strings.TrimSpace(msg)
	return strings.Contains(msg, "的内容已写入草稿，请在左侧章节列表查看。") ||
		strings.Contains(msg, "章节内容已写入草稿")
}

func parseDraftChapterTitle(draftPath string) string {
	if draftPath == "" {
		return ""
	}
	data, err := os.ReadFile(draftPath)
	if err != nil || len(data) == 0 {
		return ""
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

// chatDisplayOrDraftNotice 持久化到 messages.jsonl 的 assistant 文案。
func chatDisplayOrDraftNotice(full, draftPath string, chapterNo int, chapterTitle string) string {
	display := ChatDisplayText(full, draftPath)
	if display != "" {
		return display
	}
	if strings.TrimSpace(full) == "" {
		return ""
	}
	title := strings.TrimSpace(chapterTitle)
	if title == "" {
		title = parseDraftChapterTitle(draftPath)
	}
	if draftPath != "" {
		if info, err := os.Stat(draftPath); err == nil && info.Size() > 0 {
			return draftWrittenNotice(chapterNo, title)
		}
	}
	if looksLikeChapterBody(full) || len(strings.TrimSpace(full)) > 800 {
		return draftWrittenNotice(chapterNo, title)
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
