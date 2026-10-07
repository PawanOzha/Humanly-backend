package api

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"humanly/backend/internal/account"
	"humanly/backend/internal/engine"
)

type inbound struct {
	Op      string `json:"op"`
	ID      string `json:"id"`
	Text    string `json:"text"`
	Mode    string `json:"mode"`
	Consent bool   `json:"consent"`
}

type outbound struct {
	Op          string                `json:"op"`
	ID          string                `json:"id,omitempty"`
	Phase       string                `json:"phase,omitempty"`
	Score       int                   `json:"score,omitempty"`
	Before      int                   `json:"before,omitempty"`
	Label       string                `json:"label,omitempty"`
	BeforeLabel string                `json:"beforeLabel,omitempty"`
	Sentence    *engine.Sentence      `json:"sentence,omitempty"`
	Sentences   []engine.Sentence     `json:"sentences,omitempty"`
	Original    []engine.Sentence     `json:"original,omitempty"`
	Output      string                `json:"output,omitempty"`
	Chunk       string                `json:"chunk,omitempty"`
	Message     string                `json:"message,omitempty"`
	Code        string                `json:"code,omitempty"`
	Usage       *account.Subscription `json:"usage,omitempty"`
	Guest       *account.GuestPass    `json:"guest,omitempty"`
}

func (s *Server) live(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin != "" && !s.allowOrigin(origin) {
		writeErr(w, http.StatusForbidden, "origin not allowed")
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // origin is checked above
	})
	if err != nil {
		s.Log.Warn("websocket accept", "err", err)
		return
	}
	conn.SetReadLimit(maxBody)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	defer conn.Close(websocket.StatusNormalClosure, "")

	user, _, signedIn := s.sessionUser(r)
	userID := ""
	if signedIn {
		userID = user.ID
	}
	c := &client{conn: conn, srv: s, userID: userID, ip: clientIP(r)}
	if err := c.write(ctx, outbound{Op: "ready"}); err != nil {
		return
	}

	var job context.CancelFunc
	var jobMu sync.Mutex
	stopJob := func() {
		jobMu.Lock()
		defer jobMu.Unlock()
		if job != nil {
			job()
			job = nil
		}
	}
	defer stopJob()

	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-ping.C:
				pctx, done := context.WithTimeout(ctx, 5*time.Second)
				err := c.ping(pctx)
				done()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			if !isClosed(err) && ctx.Err() == nil {
				s.Log.Info("websocket closed", "err", err)
			}
			return
		}
		var msg inbound
		if err := json.Unmarshal(data, &msg); err != nil || msg.ID == "" || msg.Op == "" {
			_ = c.write(ctx, outbound{Op: "error", ID: msg.ID, Message: "invalid message"})
			continue
		}
		stopJob()
		jobCtx, jobCancel := context.WithCancel(ctx)
		jobMu.Lock()
		job = jobCancel
		jobMu.Unlock()
		go func(msg inbound) {
			defer jobCancel()
			c.run(jobCtx, msg)
		}(msg)
	}
}

type client struct {
	conn   *websocket.Conn
	srv    *Server
	userID string
	ip     string
	mu     sync.Mutex
}

func (c *client) ping(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.Ping(ctx)
}

func (c *client) write(ctx context.Context, msg outbound) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return c.conn.Write(wctx, websocket.MessageText, b)
}

func (c *client) run(ctx context.Context, msg inbound) {
	key := jobKey(c.userID, c.ip)
	if ok, _ := c.srv.jobLimit.allow(key, time.Now()); !ok {
		_ = c.write(ctx, outbound{Op: "error", ID: msg.ID, Message: "Too many runs in a row. Try again in a moment.", Code: "rate"})
		return
	}
	switch msg.Op {
	case "scan":
		c.scan(ctx, msg)
	case "humanize":
		c.humanize(ctx, msg)
	default:
		_ = c.write(ctx, outbound{Op: "error", ID: msg.ID, Message: "unknown operation"})
	}
}

func (c *client) scan(ctx context.Context, msg inbound) {
	text, err := engine.CheckText(msg.Text)
	if err != nil {
		_ = c.write(ctx, outbound{Op: "error", ID: msg.ID, Message: err.Error()})
		return
	}
	pass, refund, err := c.srv.beginGuest(ctx, c.ip, c.userID != "", text, msg.Consent)
	if err != nil {
		c.guestError(ctx, msg.ID, err, pass)
		return
	}
	defer func() { refund() }()
	report, err := c.srv.score(ctx, text)
	if err != nil {
		_ = c.write(ctx, outbound{Op: "error", ID: msg.ID, Message: err.Error()})
		return
	}
	var sum int
	for i, s := range report.Sentences {
		if err := ctx.Err(); err != nil {
			return
		}
		sum += s.Score
		sent := s
		if err := c.write(ctx, outbound{
			Op: "progress", ID: msg.ID, Phase: "scanning", Score: int(math.Round(float64(sum) / float64(i+1))), Sentence: &sent,
		}); err != nil {
			return
		}
		if err := c.srv.sleep(ctx); err != nil {
			return
		}
	}
	if err := ctx.Err(); err != nil {
		return
	}
	if err := c.write(ctx, outbound{
		Op: "result", ID: msg.ID, Phase: "scanned", Score: report.Score, Label: report.Label, Sentences: report.Sentences, Guest: pass,
	}); err != nil {
		return
	}
	refund = func() {}
}

func (c *client) humanize(ctx context.Context, msg inbound) {
	text, err := engine.CheckText(msg.Text)
	if err != nil {
		_ = c.write(ctx, outbound{Op: "error", ID: msg.ID, Message: err.Error()})
		return
	}
	if !engine.ValidMode(msg.Mode) {
		_ = c.write(ctx, outbound{Op: "error", ID: msg.ID, Message: "unknown tone"})
		return
	}
	pass, refundGuest, err := c.srv.beginGuest(ctx, c.ip, c.userID != "", text, msg.Consent)
	if err != nil {
		c.guestError(ctx, msg.ID, err, pass)
		return
	}
	defer func() { refundGuest() }()
	var usage *account.Subscription
	words := 0
	if c.userID != "" && c.srv.Accounts != nil {
		words = engine.WordCount(text)
		sub, err := c.srv.Accounts.Spend(ctx, c.userID, words, msg.Mode)
		if err != nil {
			_ = c.write(ctx, outbound{Op: "error", ID: msg.ID, Message: c.srv.publicMessage(limitOr(err)), Code: limitCode(err)})
			return
		}
		usage = &sub
		defer func() {
			if usage != nil {
				_ = c.srv.Accounts.Refund(context.Background(), c.userID, words, usage.Period)
			}
		}()
	}
	before, out, after, err := c.srv.rewrite(ctx, text, msg.Mode)
	if err != nil {
		_ = c.write(ctx, outbound{Op: "error", ID: msg.ID, Message: err.Error()})
		return
	}
	var built string
	for _, chunk := range engine.Chunks(out, 72) {
		if err := ctx.Err(); err != nil {
			return
		}
		built += chunk
		if err := c.write(ctx, outbound{
			Op: "chunk", ID: msg.ID, Phase: "humanizing", Chunk: chunk, Output: built,
		}); err != nil {
			return
		}
		if err := c.srv.sleep(ctx); err != nil {
			return
		}
	}
	if err := ctx.Err(); err != nil {
		return
	}
	spent := usage
	usage = nil
	if err := c.write(ctx, outbound{
		Op: "result", ID: msg.ID, Phase: "done",
		Output: out, Score: after.Score, Before: before.Score,
		Label: after.Label, BeforeLabel: before.Label,
		Sentences: after.Sentences, Original: before.Sentences,
		Usage: spent, Guest: pass,
	}); err != nil {
		return
	}
	refundGuest = func() {}
}

func (c *client) guestError(ctx context.Context, id string, err error, pass *account.GuestPass) {
	if !isGuestLimit(err) {
		_ = c.write(ctx, outbound{Op: "error", ID: id, Message: c.srv.publicMessage(err)})
		return
	}
	code := "guest"
	if errors.Is(err, account.ErrGuestWords) {
		code = "words"
	}
	_ = c.write(ctx, outbound{Op: "error", ID: id, Message: err.Error(), Code: code, Guest: pass})
}

func limitCode(err error) string {
	var lim *account.LimitError
	if errors.As(err, &lim) {
		return lim.Code
	}
	return ""
}

// limitOr keeps a plan limit's own wording and lets publicMessage replace
// anything else.
func limitOr(err error) error {
	var lim *account.LimitError
	if errors.As(err, &lim) {
		return account.InputError(lim.Error())
	}
	return err
}
