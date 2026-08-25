package auth

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestIssueReturnsURLAndRawToken(t *testing.T) {
	s := NewTokenStore(0) // use default TTL

	urlOut, raw, err := s.Issue("http://127.0.0.1:7600")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if raw == "" {
		t.Error("raw token is empty")
	}
	if !strings.HasPrefix(urlOut, "http://127.0.0.1:7600/?bootstrap=") {
		t.Errorf("URL = %q, want bootstrap query param", urlOut)
	}
	if !strings.Contains(urlOut, raw) {
		t.Errorf("URL %q must embed the raw token %q", urlOut, raw)
	}
}

func TestConsumeValidTokenSucceeds(t *testing.T) {
	s := NewTokenStore(DefaultTokenTTL)
	_, raw, _ := s.Issue("http://x")
	if err := s.Consume(raw); err != nil {
		t.Errorf("Consume(valid): %v", err)
	}
}

func TestSecondConsumeIsRefused(t *testing.T) {
	s := NewTokenStore(DefaultTokenTTL)
	_, raw, _ := s.Issue("http://x")
	if err := s.Consume(raw); err != nil {
		t.Fatalf("first Consume: %v", err)
	}
	if err := s.Consume(raw); !errors.Is(err, ErrTokenUnknown) {
		t.Errorf("second Consume err = %v, want ErrTokenUnknown", err)
	}
}

func TestExpiredTokenRefused(t *testing.T) {
	s := NewTokenStore(2 * time.Millisecond)
	_, raw, _ := s.Issue("http://x")
	time.Sleep(10 * time.Millisecond)
	err := s.Consume(raw)
	if !errors.Is(err, ErrTokenExpired) {
		t.Errorf("expired Consume err = %v, want ErrTokenExpired", err)
	}
	// Expired tokens still burn from the table — replay yields Unknown
	if err := s.Consume(raw); !errors.Is(err, ErrTokenUnknown) {
		t.Errorf("expired token must not be reusable; got err = %v", err)
	}
}

func TestUnknownTokenRefused(t *testing.T) {
	s := NewTokenStore(DefaultTokenTTL)
	if err := s.Consume("not-a-real-token"); !errors.Is(err, ErrTokenUnknown) {
		t.Errorf("unknown Consume err = %v, want ErrTokenUnknown", err)
	}
}

func TestSweepExpired(t *testing.T) {
	s := NewTokenStore(2 * time.Millisecond)
	for i := 0; i < 3; i++ {
		_, _, _ = s.Issue("http://x")
	}
	if got := s.Outstanding(); got != 1 {
		t.Fatalf("Outstanding before sweep = %d, want 1", got)
	}
	time.Sleep(10 * time.Millisecond)
	if n := s.SweepExpired(time.Now()); n != 1 {
		t.Errorf("SweepExpired removed %d, want 1", n)
	}
	if got := s.Outstanding(); got != 0 {
		t.Errorf("Outstanding after sweep = %d, want 0", got)
	}
}

func TestIssueIsConcurrencySafe(t *testing.T) {
	const N = 50
	s := NewTokenStore(DefaultTokenTTL)
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			_, _, err := s.Issue("http://x")
			if err != nil {
				t.Errorf("Issue: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := s.Outstanding(); got != 1 {
		t.Errorf("Outstanding = %d, want 1", got)
	}
}

func TestDefaultTTLAppliedWhenZero(t *testing.T) {
	s := NewTokenStore(0)
	if got := s.TTL(); got != DefaultTokenTTL {
		t.Errorf("TTL = %v, want DefaultTokenTTL=%v", got, DefaultTokenTTL)
	}
}
