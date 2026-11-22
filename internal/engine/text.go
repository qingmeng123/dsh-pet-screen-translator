package engine

import (
	"regexp"
	"strings"
	"unicode"
)

var wordRe = regexp.MustCompile(`[a-z0-9']+`)

// cleanText 把 OCR 原始文本压成一行：按行切、合并空白、丢掉过短的行。
func cleanText(raw string) string {
	var parts []string
	for _, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		line = strings.Join(strings.Fields(line), " ")
		if len([]rune(line)) >= 2 {
			parts = append(parts, line)
		}
	}
	return strings.TrimSpace(strings.Join(parts, " "))
}

// looksEnglish 判断是否值得送去翻译：字母够多、至少两个词、字母占比够高。
func looksEnglish(text string) bool {
	var latin, cjk, words int
	for _, r := range text {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
			latin++
		case unicode.Is(unicode.Han, r):
			cjk++
		}
	}
	words = len(regexp.MustCompile(`[A-Za-z]{2,}`).FindAllString(text, -1))
	if latin < 10 || words < 2 {
		return false
	}
	return float64(latin)/float64(latin+cjk) >= 0.55
}

// normalize 归一化文本，并把 OCR 常见混淆拉回正常字母，用于判稳/判重。
func normalize(text string) string {
	s := strings.ToLower(text)
	s = strings.ReplaceAll(s, "?", "h")
	s = strings.ReplaceAll(s, "rn", "m")
	s = strings.NewReplacer("0", "o", "1", "l", "5", "s").Replace(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', unicode.Is(unicode.Han, r):
			b.WriteRune(r)
		default:
			b.WriteByte(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// similarity 是词集合的 Jaccard 相似度。
func similarity(a, b string) float64 {
	sa := toSet(wordRe.FindAllString(a, -1))
	sb := toSet(wordRe.FindAllString(b, -1))
	if len(sa) == 0 || len(sb) == 0 {
		return 0
	}
	inter := 0
	for w := range sa {
		if sb[w] {
			inter++
		}
	}
	return float64(inter) / float64(len(sa)+len(sb)-inter)
}

func toSet(words []string) map[string]bool {
	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}
	return m
}

// prefix 取前 n 个字符（按 rune 计），用于「译文开头相同就不重复弹泡」。
func prefix(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// truncate 截断到 n 个字符（按 rune 计），超出加省略号。
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// charSimilarity 按字符集合算相似度（中文用）：OCR 抖动对译文影响很小，
// 而"换了新台词"几乎不会共享字符，所以这个指标区分度够。
func charSimilarity(a, b string) float64 {
	ra, rb := []rune(a), []rune(b)
	if len(ra) == 0 || len(rb) == 0 {
		return 0
	}
	set := func(rs []rune) map[rune]bool {
		m := make(map[rune]bool, len(rs))
		for _, r := range rs {
			m[r] = true
		}
		return m
	}
	sa, sb := set(ra), set(rb)
	inter := 0
	for r := range sa {
		if sb[r] {
			inter++
		}
	}
	return float64(inter) / float64(len(sa)+len(sb)-inter)
}
