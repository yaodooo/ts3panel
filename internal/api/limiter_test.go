package api

import (
	"testing"
	"time"
)

func TestAttemptLimiterLocksAfterFiveFailures(t *testing.T) {
	base := time.Now()
	limiter := newAttemptLimiter(5, 15*time.Minute, 15*time.Minute)

	for i := 0; i < 4; i++ {
		if locked, _ := limiter.Fail("admin", base.Add(time.Duration(i)*time.Minute)); locked {
			t.Fatalf("failure %d locked too early", i+1)
		}
	}

	locked, retry := limiter.Fail("admin", base.Add(14*time.Minute))
	if !locked {
		t.Fatal("fifth failure did not lock the key")
	}
	if retry != 15*time.Minute {
		t.Fatalf("lock duration = %v, want 15m", retry)
	}
	if locked, _ := limiter.Check("admin", base.Add(28*time.Minute)); !locked {
		t.Fatal("key unlocked before the 15 minute lockout elapsed")
	}
}

func TestAttemptLimiterUnlocksAtLockExpiry(t *testing.T) {
	base := time.Now()
	limiter := newAttemptLimiter(5, 15*time.Minute, 15*time.Minute)

	for i := 0; i < 5; i++ {
		limiter.Fail("admin", base.Add(14*time.Minute))
	}

	// The fifth failure locks until base+29m. At exactly 29m the lock is over.
	if locked, _ := limiter.Check("admin", base.Add(29*time.Minute)); locked {
		t.Fatal("key is still locked exactly at lock expiry")
	}
	if locked, _ := limiter.Fail("admin", base.Add(29*time.Minute)); locked {
		t.Fatal("new failure after lock expiry should start a fresh window")
	}
}

func TestAttemptLimiterResetsExpiredWindow(t *testing.T) {
	base := time.Now()
	limiter := newAttemptLimiter(5, 15*time.Minute, 15*time.Minute)

	if locked, _ := limiter.Fail("admin", base); locked {
		t.Fatal("first failure locked")
	}
	// More than 15 minutes later the old failure count must be discarded.
	if locked, _ := limiter.Fail("admin", base.Add(16*time.Minute)); locked {
		t.Fatal("failure after window expiration locked")
	}
	for i := 2; i <= 4; i++ {
		if locked, _ := limiter.Fail("admin", base.Add(16*time.Minute+time.Duration(i)*time.Minute)); locked {
			t.Fatalf("failure %d in fresh window locked too early", i)
		}
	}
	if locked, _ := limiter.Fail("admin", base.Add(21*time.Minute)); !locked {
		t.Fatal("fifth failure in fresh window did not lock")
	}
}
