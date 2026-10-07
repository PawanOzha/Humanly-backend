package account

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

type PlanID string

const (
	PlanFree PlanID = "free"
	PlanPro  PlanID = "pro"
	PlanTeam PlanID = "team"
)

type Plan struct {
	ID        PlanID
	Name      string
	Allowance int
	PerRun    int
}

var Plans = map[PlanID]Plan{
	PlanFree: {ID: PlanFree, Name: "Free", Allowance: 2000, PerRun: 500},
	PlanPro:  {ID: PlanPro, Name: "Pro", Allowance: 15000, PerRun: 4000},
	PlanTeam: {ID: PlanTeam, Name: "Team", Allowance: 40000, PerRun: 8000},
}

type Subscription struct {
	Plan      PlanID `json:"plan"`
	Used      int    `json:"used"`
	Allowance int    `json:"allowance"`
	Period    string `json:"period"`
	PerRun    int    `json:"perRun"`
}

type LimitError struct {
	Code string
	Msg  string
}

func (e *LimitError) Error() string { return e.Msg }

var (
	errTone    = &LimitError{Code: "tone", Msg: "Academic and Casual are on Pro."}
	errBadPlan = &LimitError{Code: "plan", Msg: "unknown plan"}
)

func Period(now time.Time) string {
	return now.UTC().Format("2006-01")
}

func NewSubscription(plan PlanID, now time.Time) Subscription {
	spec := Plans[plan]
	return Subscription{
		Plan:      plan,
		Used:      0,
		Allowance: spec.Allowance,
		Period:    Period(now),
		PerRun:    spec.PerRun,
	}
}

func normalize(sub Subscription, now time.Time) Subscription {
	spec, ok := Plans[sub.Plan]
	if !ok {
		spec = Plans[PlanFree]
		sub.Plan = PlanFree
	}
	if sub.Period != Period(now) {
		sub.Period = Period(now)
		sub.Used = 0
	}
	if sub.Used < 0 {
		sub.Used = 0
	}
	sub.Allowance = spec.Allowance
	sub.PerRun = spec.PerRun
	return sub
}

func ApplyPlan(sub Subscription, plan PlanID, now time.Time) (Subscription, error) {
	if _, ok := Plans[plan]; !ok {
		return sub, errBadPlan
	}
	sub = normalize(sub, now)
	sub.Plan = plan
	return normalize(sub, now), nil
}

func ApplySpend(sub Subscription, words int, mode string, now time.Time) (Subscription, error) {
	sub = normalize(sub, now)
	if words < 1 {
		words = 1
	}
	spec := Plans[sub.Plan]
	if mode != "" && mode != "Balanced" && sub.Plan == PlanFree {
		return sub, errTone
	}
	if words > spec.PerRun {
		return sub, &LimitError{
			Code: "run",
			Msg:  fmt.Sprintf("%s takes %s words at a time. This draft is %s.", spec.Name, formatInt(spec.PerRun), formatInt(words)),
		}
	}
	left := spec.Allowance - sub.Used
	if left < 0 {
		left = 0
	}
	if words > left {
		return sub, &LimitError{
			Code: "quota",
			Msg:  fmt.Sprintf("%s words left this month. This draft is %s.", formatInt(left), formatInt(words)),
		}
	}
	sub.Used += words
	return sub, nil
}

func ApplyRefund(sub Subscription, words int, period string) Subscription {
	if words < 1 || sub.Period != period {
		return sub
	}
	sub.Used -= words
	if sub.Used < 0 {
		sub.Used = 0
	}
	return sub
}

func formatInt(n int) string {
	if n < 0 {
		return "-" + formatInt(-n)
	}
	s := strconv.Itoa(n)
	if len(s) <= 3 {
		return s
	}
	var parts []string
	for len(s) > 3 {
		parts = append([]string{s[len(s)-3:]}, parts...)
		s = s[:len(s)-3]
	}
	return strings.Join(append([]string{s}, parts...), ",")
}
