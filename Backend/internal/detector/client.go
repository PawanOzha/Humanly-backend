// Package detector calls the AI detection and humanization service and
// maps its response onto the editor's sentence shape.
package detector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"humanly/backend/internal/engine"
)

// detectThreshold is the confidence the detector needs before it commits to
// AI-generated or Human-written. 0.6 is the cutoff where the AI essay and
// the AI email land on AI-generated, a plainly human note stays
// Human-written, and a mixed paragraph stays Inconclusive. The service
// default of 0.8 left that essay Inconclusive.
const (
	detectThreshold = 0.6
	detectBatch     = 16
)

// Client talks to the FastAPI detector.
type Client struct {
	Base string
	HTTP *http.Client
}

func New(base string) *Client {
	return &Client{
		Base: strings.TrimRight(strings.TrimSpace(base), "/"),
		HTTP: &http.Client{Timeout: 2 * time.Minute},
	}
}

// Warm loads the models so the first draft does not wait on a cold start.
func (c *Client) Warm(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+"/models/warmup", nil)
	if err != nil {
		return err
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("detector is offline")
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, res.Body)
	if res.StatusCode >= 300 {
		return fmt.Errorf("models are still loading")
	}
	return nil
}

// Detect scores text with the service's own threshold and document average.
func (c *Client) Detect(ctx context.Context, text string) (engine.Report, error) {
	var raw detectRes
	if err := c.post(ctx, "/detect/text", map[string]any{
		"text":       text,
		"threshold":  detectThreshold,
		"batch_size": detectBatch,
	}, &raw); err != nil {
		return engine.Report{}, err
	}
	if len(raw.Sentences) == 0 {
		return engine.Report{}, fmt.Errorf("detector returned no sentences")
	}
	parts := make([]engine.Sentence, len(raw.Sentences))
	for i, s := range raw.Sentences {
		score := pct(s.AI)
		label, note, flagged := mark(s.Prediction, score)
		parts[i] = engine.Sentence{
			Text:    s.Sentence,
			Score:   score,
			Flagged: flagged,
			Note:    note,
			Label:   label,
		}
	}
	return engine.Report{
		Score:     pct(raw.Document.AI),
		Label:     raw.Document.Prediction,
		Sentences: stitch(text, parts),
	}, nil
}

// Humanize rewrites text. Tone maps onto the service's synonym and transition rates.
func (c *Client) Humanize(ctx context.Context, text, mode string) (string, error) {
	pSyn, pTrans := settings(mode)
	var raw humanizeRes
	err := c.post(ctx, "/humanize", map[string]any{
		"text":                text,
		"p_syn":               pSyn,
		"p_trans":             pTrans,
		"preserve_linebreaks": true,
	}, &raw)
	if err != nil {
		return "", err
	}
	out := strings.TrimSpace(raw.Text)
	if out == "" {
		return "", fmt.Errorf("humanizer returned an empty draft")
	}
	return out, nil
}

// settings maps a tone onto the humanizer's synonym rate. Transition
// insertion stays at 0: those inserts ("Moreover,", "Hence,") raised the
// AI score and broke the sentence. Synonym rate 0.10 had the best average
// score drop that still left the draft readable. Academic stays lighter.
// Casual swaps a little more.
func settings(mode string) (pSyn, pTrans float64) {
	switch mode {
	case "Academic":
		return 0.06, 0
	case "Casual":
		return 0.12, 0
	default:
		return 0.10, 0
	}
}

func mark(prediction string, score int) (label, note string, flagged bool) {
	switch prediction {
	case "AI-generated":
		return prediction, "Feels AI", true
	case "Human-written":
		return prediction, "Reads human", false
	default:
		if score >= 60 {
			return "Inconclusive", "Leans AI", false
		}
		if score <= 40 {
			return "Inconclusive", "Leans human", false
		}
		return "Inconclusive", "", false
	}
}

func pct(p float64) int {
	if p < 0 {
		p = 0
	}
	if p > 1 {
		p = 1
	}
	return int(math.Round(p * 100))
}

// stitch keeps the service's sentence strings inside the original draft,
// including the spaces between them, so the editor shows the same text.
func stitch(source string, parts []engine.Sentence) []engine.Sentence {
	if len(parts) == 0 {
		return parts
	}
	pos := 0
	for i := range parts {
		raw := strings.TrimSpace(parts[i].Text)
		if raw == "" {
			continue
		}
		idx := strings.Index(source[pos:], raw)
		if idx < 0 {
			continue
		}
		start := pos + idx
		end := start + len(raw)
		switch {
		case i == 0:
			parts[i].Text = source[:end]
		case start > pos:
			parts[i-1].Text += source[pos:start]
			parts[i].Text = source[start:end]
		default:
			parts[i].Text = source[start:end]
		}
		pos = end
	}
	if pos < len(source) {
		parts[len(parts)-1].Text += source[pos:]
	}
	return parts
}

func (c *Client) post(ctx context.Context, path string, body any, dest any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+path, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.HTTP.Do(req)
	if err != nil {
		if strings.Contains(path, "humanize") {
			return fmt.Errorf("humanizer is offline")
		}
		return fmt.Errorf("detector is offline")
	}
	defer res.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("detector returned an unreadable response")
	}
	if res.StatusCode >= 300 {
		return callError(path, res.StatusCode, payload)
	}
	if err := json.Unmarshal(payload, dest); err != nil {
		return fmt.Errorf("detector returned an unexpected response")
	}
	return nil
}

func callError(path string, status int, payload []byte) error {
	detail := errorDetail(payload)
	offline := "detector"
	if strings.Contains(path, "humanize") {
		offline = "humanizer"
	}
	switch status {
	case http.StatusUnprocessableEntity, http.StatusBadRequest:
		if detail != "" {
			return fmt.Errorf("%s", detail)
		}
	case http.StatusServiceUnavailable:
		return fmt.Errorf("models are still loading")
	}
	if detail != "" && len(detail) < 140 {
		return fmt.Errorf("%s", detail)
	}
	return fmt.Errorf("%s could not finish that draft", offline)
}

func errorDetail(payload []byte) string {
	var body struct {
		Detail any `json:"detail"`
	}
	if json.Unmarshal(payload, &body) != nil || body.Detail == nil {
		return ""
	}
	switch d := body.Detail.(type) {
	case string:
		return d
	default:
		return ""
	}
}

type detectRes struct {
	Sentences []struct {
		Sentence   string  `json:"sentence"`
		Prediction string  `json:"prediction"`
		AI         float64 `json:"ai_probability"`
	} `json:"sentences"`
	Document struct {
		Prediction string  `json:"prediction"`
		AI         float64 `json:"ai_probability"`
	} `json:"document_summary"`
}

type humanizeRes struct {
	Text string `json:"humanized_text"`
}
