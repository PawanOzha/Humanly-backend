package account

import (
	"sync"
	"testing"
	"time"
)

func TestApplySpendIsAtomicOnTheNumbers(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	sub := NewSubscription(PlanFree, now)
	next, err := ApplySpend(sub, 400, "Balanced", now)
	if err != nil || next.Used != 400 {
		t.Fatalf("spend = %+v %v", next, err)
	}
	if _, err := ApplySpend(next, 501, "Balanced", now); err == nil || err.(*LimitError).Code != "run" {
		t.Fatalf("run limit = %v", err)
	}
	if _, err := ApplySpend(next, 100, "Casual", now); err == nil || err.(*LimitError).Code != "tone" {
		t.Fatalf("tone = %v", err)
	}
	high := next
	high.Used = 1900
	if _, err := ApplySpend(high, 200, "Balanced", now); err == nil || err.(*LimitError).Code != "quota" {
		t.Fatalf("quota = %v", err)
	}
	rolled, err := ApplySpend(high, 10, "Balanced", now.AddDate(0, 1, 0))
	if err != nil || rolled.Used != 10 || rolled.Period != "2026-11" {
		t.Fatalf("rollover = %+v %v", rolled, err)
	}
}

func TestConcurrentSpendCannotOvershoot(t *testing.T) {
	svc := New(NewMemory(), Config{SessionSecret: "test-session-secret-value"})
	ctx := t.Context()
	user, _, _, err := svc.Signup(ctx, "Ada Lovelace", "ada@example.com", "correct-horse")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetPlan(ctx, user.ID, "team"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if _, err := svc.Spend(ctx, user.ID, 8000, "Balanced"); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = svc.Spend(ctx, user.ID, 8000, "Balanced")
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
		}
	}
	if ok != 1 {
		t.Fatalf("successes = %d errs = %v", ok, errs)
	}
	got, err := svc.store.WithSubscription(ctx, user.ID, func(sub Subscription) (Subscription, error) {
		return sub, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Used != 40000 {
		t.Fatalf("used = %d", got.Used)
	}
}

func TestStateRoundTrip(t *testing.T) {
	svc := New(NewMemory(), Config{SessionSecret: "test-session-secret-value"})
	signed, err := svc.SignState("/dashboard/plans")
	if err != nil {
		t.Fatal(err)
	}
	next, err := svc.OpenState(signed)
	if err != nil || next != "/dashboard/plans" {
		t.Fatalf("next = %q %v", next, err)
	}
	if _, err := svc.OpenState(signed + "x"); err == nil {
		t.Fatal("tampered state was accepted")
	}
}
