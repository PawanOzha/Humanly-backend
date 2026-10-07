package account

import (
	"context"
	"sync"
	"testing"
)

func TestGuestAllowanceDependsOnConsent(t *testing.T) {
	svc := New(NewMemory(), Config{SessionSecret: "test-session-secret-value"})
	ctx := context.Background()

	pass, err := svc.TakeGuest(ctx, "203.0.113.10", false)
	if err != nil || pass.Used != 1 || pass.Allowance != 1 || pass.Left != 0 {
		t.Fatalf("first declined pass = %+v %v", pass, err)
	}
	if _, err := svc.TakeGuest(ctx, "203.0.113.10", false); !errorsIs(err, ErrGuestLimit) {
		t.Fatalf("second declined = %v", err)
	}

	pass, err = svc.PeekGuest(ctx, "203.0.113.10", true)
	if err != nil || pass.Allowance != 3 || pass.Left != 2 {
		t.Fatalf("upgrade = %+v %v", pass, err)
	}
	for i := 0; i < 2; i++ {
		if _, err := svc.TakeGuest(ctx, "203.0.113.10", true); err != nil {
			t.Fatalf("consented take %d: %v", i, err)
		}
	}
	if _, err := svc.TakeGuest(ctx, "203.0.113.10", true); !errorsIs(err, ErrGuestLimit) {
		t.Fatalf("fourth = %v", err)
	}

	other, err := svc.TakeGuest(ctx, "203.0.113.11", true)
	if err != nil || other.Left != 2 || other.Allowance != 3 {
		t.Fatalf("other network = %+v %v", other, err)
	}
}

func TestGuestTakeDoesNotOvershoot(t *testing.T) {
	svc := New(NewMemory(), Config{SessionSecret: "test-session-secret-value"})
	ctx := context.Background()
	var wg sync.WaitGroup
	ok := 0
	var mu sync.Mutex
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := svc.TakeGuest(ctx, "198.51.100.8", false); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ok != 1 {
		t.Fatalf("successes = %d", ok)
	}
	pass, err := svc.PeekGuest(ctx, "198.51.100.8", false)
	if err != nil || pass.Used != 1 {
		t.Fatalf("used = %+v %v", pass, err)
	}
}

func TestGuestRefundRestoresTheRun(t *testing.T) {
	svc := New(NewMemory(), Config{SessionSecret: "test-session-secret-value"})
	ctx := context.Background()
	if _, err := svc.TakeGuest(ctx, "192.0.2.4", true); err != nil {
		t.Fatal(err)
	}
	if err := svc.RefundGuest(ctx, "192.0.2.4"); err != nil {
		t.Fatal(err)
	}
	pass, err := svc.PeekGuest(ctx, "192.0.2.4", true)
	if err != nil || pass.Used != 0 || pass.Left != 3 {
		t.Fatalf("refunded = %+v %v", pass, err)
	}
}

func errorsIs(err, target error) bool {
	return err == target
}
