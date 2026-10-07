// Package engine holds the draft limits and the sentence shape the editor
// renders. Live scoring and rewriting go through the detector service.
package engine

import (
	"math"
	"regexp"
	"strings"
	"unicode/utf8"
)

const MaxRunes = 12000

type Mode string

const (
	Balanced Mode = "Balanced"
	Academic Mode = "Academic"
	Casual   Mode = "Casual"
)

type Sentence struct {
	Text    string `json:"text"`
	Score   int    `json:"score"`
	Flagged bool   `json:"flagged"`
	Note    string `json:"note"`
	Label   string `json:"label,omitempty"`
}

type Report struct {
	Score     int        `json:"score"`
	Label     string     `json:"label,omitempty"`
	Sentences []Sentence `json:"sentences"`
}

var aiWords = []string{
	"moreover", "furthermore", "in conclusion", "delve", "crucial", "leverage", "utilize",
	"it is important to note", "in today's", "landscape", "tapestry", "seamless", "robust",
	"pivotal", "comprehensive", "ensure", "additionally", "enhance", "facilitate",
	"multifaceted", "navigating", "i hope this email finds you well",
}

var (
	sentenceRE    = regexp.MustCompile(`[^.!?]+[.!?]*\s*`)
	contractionRE = regexp.MustCompile(`(?i)\b\w+'(s|t|re|ve|ll|d|m)\b`)
	spaceRE       = regexp.MustCompile(`\s{2,}`)
)

type rule struct {
	re *regexp.Regexp
	to string
}

func mustRules(pairs [][2]string) []rule {
	out := make([]rule, len(pairs))
	for i, p := range pairs {
		out[i] = rule{re: regexp.MustCompile(p[0]), to: p[1]}
	}
	return out
}

var swaps = mustRules([][2]string{
	{`(?i)\bit is important to note that\s*`, ""},
	{`(?i)\bIn today's [\w\- ]*?landscape,?\s*`, "These days, "},
	{`(?i)\bI hope this email finds you well\.?\s*`, "Hope your week's going well. "},
	{`(?i)\bMoreover,?\s*`, "Plus, "},
	{`(?i)\bFurthermore,?\s*`, "And "},
	{`(?i)\bAdditionally,?\s*`, "Also, "},
	{`(?i)\bIn conclusion,?\s*`, "So, "},
	{`(?i)\bdelve into\b`, "dig into"},
	{`(?i)\bleverage\b`, "use"},
	{`(?i)\butilize\b`, "use"},
	{`(?i)\bcrucial\b`, "key"},
	{`(?i)\bpivotal\b`, "big"},
	{`(?i)\bseamless\b`, "smooth"},
	{`(?i)\brobust\b`, "solid"},
	{`(?i)\bcomprehensive\b`, "full"},
	{`(?i)\bfacilitate\b`, "drive"},
	{`(?i)\benhance\b`, "improve"},
	{`(?i)\bensure\b`, "make sure"},
	{`(?i)\bmultifaceted\b`, "tricky"},
	{`(?i)\bnavigating\b`, "handling"},
	{`(?i)\bcannot\b`, "can't"},
	{`(?i)\bdo not\b`, "don't"},
	{`(?i)\bwill not\b`, "won't"},
	{`(?i)\bwe are\b`, "we're"},
	{`\bI am\b`, "I'm"},
})

var casual = mustRules([][2]string{
	{`(?i)\bHowever,?\s*`, "But "},
	{`(?i)\bTherefore,?\s*`, "So "},
	{`(?i)\bin order to\b`, "to"},
})

// ValidMode reports whether the editor sent a tone we know.
func ValidMode(mode string) bool {
	switch Mode(mode) {
	case Balanced, Academic, Casual:
		return true
	default:
		return false
	}
}

// CheckText trims a draft and rejects empty or oversized input.
func CheckText(text string) (string, error) {
	if !utf8.ValidString(text) || strings.ContainsRune(text, 0) {
		return "", errBadText
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", errEmpty
	}
	if utf8.RuneCountInString(text) > MaxRunes {
		return "", errTooLong
	}
	return text, nil
}

var (
	errEmpty   = strError("text is empty")
	errTooLong = strError("text is over 12,000 characters")
	errBadText = strError("text has characters we can't read")
)

type strError string

func (e strError) Error() string { return string(e) }

// Detect scores each sentence and the draft as a whole.
func Detect(text string) Report {
	parts := sentenceRE.FindAllString(text, -1)
	sentences := make([]Sentence, 0, len(parts))
	total := 0
	for _, part := range parts {
		words := countWords(part)
		total += words
		sentences = append(sentences, scoreSentence(part, words))
	}
	if total == 0 {
		total = 1
	}
	weighted := 0.0
	for _, s := range sentences {
		weighted += float64(s.Score) * (float64(countWords(s.Text)) / float64(total))
	}
	return Report{Score: int(math.Round(clamp(weighted, 2, 98))), Sentences: sentences}
}

func scoreSentence(s string, words int) Sentence {
	low := strings.ToLower(s)
	var hits []string
	for _, w := range aiWords {
		if strings.Contains(low, w) {
			hits = append(hits, w)
		}
	}
	sc := 20 + len(hits)*28
	if words > 26 {
		sc += 15
	}
	if words > 8 && !contractionRE.MatchString(low) {
		sc += 10
	}
	if words < 7 {
		sc -= 10
	}
	sc = int(clamp(float64(sc), 3, 98))
	note := "stiff, no contractions"
	if len(hits) > 0 {
		n := hits
		if len(n) > 2 {
			n = n[:2]
		}
		quoted := make([]string, len(n))
		for i, h := range n {
			quoted[i] = "“" + h + "”"
		}
		note = strings.Join(quoted, " + ") + " — classic AI phrasing"
	} else if words > 26 {
		note = "very long, perfectly even rhythm"
	}
	return Sentence{Text: s, Score: sc, Flagged: sc >= 50, Note: note}
}

// Humanize rewrites a draft in the chosen tone.
func Humanize(text string, mode Mode) string {
	rules := swaps
	if mode == Casual {
		rules = append(append([]rule{}, swaps...), casual...)
	}
	out := text
	for _, r := range rules {
		out = r.re.ReplaceAllString(out, r.to)
	}
	return strings.TrimSpace(spaceRE.ReplaceAllString(out, " "))
}

// Chunks splits s into pieces of about n runes, preferring word breaks,
// so a rewrite can be pushed to the editor as it is ready.
func Chunks(s string, n int) []string {
	if n < 8 {
		n = 8
	}
	runes := []rune(s)
	if len(runes) == 0 {
		return nil
	}
	var out []string
	start := 0
	for start < len(runes) {
		end := start + n
		if end >= len(runes) {
			out = append(out, string(runes[start:]))
			break
		}
		brk := end
		for i := end; i > start+n/2; i-- {
			if runes[i] == ' ' {
				brk = i + 1
				break
			}
		}
		out = append(out, string(runes[start:brk]))
		start = brk
	}
	return out
}

func countWords(t string) int {
	t = strings.TrimSpace(t)
	if t == "" {
		return 0
	}
	return len(strings.Fields(t))
}

// WordCount is the billable size of a draft.
func WordCount(t string) int {
	n := countWords(t)
	if strings.TrimSpace(t) != "" && n < 1 {
		return 1
	}
	return n
}

func clamp(n, a, b float64) float64 {
	if n < a {
		return a
	}
	if n > b {
		return b
	}
	return n
}
