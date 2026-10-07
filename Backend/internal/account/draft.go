package account

import (
	"context"
	"math"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const maxDrafts = 12
const maxDraftRunes = 12000

// Draft is one saved document. It belongs to a single user.
type Draft struct {
	ID    string   `json:"id"`
	Title string   `json:"title"`
	Text  string   `json:"text"`
	Kind  string   `json:"kind"`
	Words int      `json:"words"`
	Score *float64 `json:"score"`
	At    int64    `json:"at"`
}

// Drafts lists the documents saved on this account.
func (s *Service) Drafts(ctx context.Context, userID string) ([]Draft, error) {
	rows, err := s.store.ListDrafts(ctx, userID)
	if err != nil || rows == nil {
		if rows == nil && err == nil {
			return []Draft{}, nil
		}
		return nil, err
	}
	return rows, nil
}

// AddDraft stores a document on this account and returns that account's list.
func (s *Service) AddDraft(ctx context.Context, userID, title, text, kind string, score *float64) ([]Draft, error) {
	if !validText(text, true) || !validText(title, false) {
		return nil, InputError("That text has characters we can't save.")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, InputError("Write something before saving.")
	}
	if utf8.RuneCountInString(text) > maxDraftRunes {
		text = string([]rune(text)[:maxDraftRunes])
	}
	switch kind {
	case "write", "pdf":
	default:
		return nil, InputError("Unknown document.")
	}
	title = strings.Join(strings.Fields(title), " ")
	if title == "" {
		title = "Untitled"
	}
	if utf8.RuneCountInString(title) > 80 {
		title = string([]rune(title)[:79]) + "…"
	}
	var stored *float64
	if score != nil {
		n := *score
		if math.IsNaN(n) || n < 0 || n > 100 {
			return nil, InputError("Score is out of range.")
		}
		rounded := math.Round(n)
		stored = &rounded
	}
	words := len(strings.Fields(text))
	if words < 1 {
		words = 1
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	return s.store.SaveDraft(ctx, userID, Draft{
		ID:    id,
		Title: title,
		Text:  text,
		Kind:  kind,
		Words: words,
		Score: stored,
	})
}

// DeleteDraft removes one document from this account and returns what is left.
func (s *Service) DeleteDraft(ctx context.Context, userID, id string) ([]Draft, error) {
	return s.store.DeleteDraft(ctx, userID, id)
}

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func validUUID(id string) bool { return uuidRE.MatchString(id) }

// validText rejects invalid UTF-8, NUL bytes (PostgreSQL cannot store them)
// and control characters. Multi-line text may keep newlines and tabs.
func validText(value string, multiline bool) bool {
	if !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if r == 0 {
			return false
		}
		if unicode.IsControl(r) && !(multiline && (r == '\n' || r == '\r' || r == '\t')) {
			return false
		}
	}
	return true
}
