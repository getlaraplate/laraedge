package serial

import (
	"bytes"
	"testing"
	"time"
)

var schedT0 = time.Date(2026, 11, 19, 8, 0, 0, 0, time.UTC)

func twoRequests(every time.Duration) []Request {
	return []Request{
		{Command: []byte("A"), Every: every},
		{Command: []byte("B"), Every: every},
	}
}

func TestSchedulerOneAtATime(t *testing.T) {
	s := newScheduler(twoRequests(time.Second), 500*time.Millisecond, schedT0)

	cmd, ok := s.next(schedT0)
	if !ok || !bytes.Equal(cmd, []byte("A")) {
		t.Fatalf("first next = %q, %v; want A", cmd, ok)
	}
	if cmd, ok := s.next(schedT0); ok {
		t.Fatalf("next while awaiting = %q; want none", cmd)
	}
	s.frame(schedT0.Add(10 * time.Millisecond))
	cmd, ok = s.next(schedT0.Add(10 * time.Millisecond))
	if !ok || !bytes.Equal(cmd, []byte("B")) {
		t.Fatalf("second next = %q, %v; want B", cmd, ok)
	}
}

func TestSchedulerNoResponse(t *testing.T) {
	s := newScheduler(twoRequests(200*time.Millisecond), 100*time.Millisecond, schedT0)
	sent := map[string]int{}
	expired := 0
	awaiting := false

	for at := schedT0; at.Before(schedT0.Add(5 * time.Second)); at = at.Add(100 * time.Millisecond) {
		if s.expired(at) {
			expired++
			awaiting = false
		}
		cmd, ok := s.next(at)
		if !ok {
			continue
		}
		if awaiting {
			t.Fatalf("two commands awaited at once at %v", at)
		}
		awaiting = true
		sent[string(cmd)]++
		if string(cmd) == "B" {
			s.frame(at.Add(10 * time.Millisecond))
			awaiting = false
		}
	}
	if sent["A"] != 25 || sent["B"] != 25 {
		t.Fatalf("sent = %v; want 25 each (one A+B cycle per 200ms)", sent)
	}
	if expired != sent["A"] {
		t.Fatalf("expired = %d; want %d (one per unanswered command)", expired, sent["A"])
	}
}

func TestSchedulerLateTurnSentOnce(t *testing.T) {
	reqs := []Request{
		{Command: []byte("silent"), Every: time.Hour},
		{Command: []byte("fast"), Every: 100 * time.Millisecond},
	}
	s := newScheduler(reqs, time.Second, schedT0)

	cmd, _ := s.next(schedT0)
	if string(cmd) != "silent" {
		t.Fatalf("first = %q; want silent", cmd)
	}
	free := schedT0.Add(time.Second)
	if !s.expired(free) {
		t.Fatal("silent request should have expired")
	}
	count := 0
	for at := free; at.Before(free.Add(50 * time.Millisecond)); at = at.Add(10 * time.Millisecond) {
		if cmd, ok := s.next(at); ok {
			if string(cmd) != "fast" {
				t.Fatalf("sent %q; want fast", cmd)
			}
			count++
			s.frame(at)
		}
	}
	if count != 1 {
		t.Fatalf("late request sent %d times; want 1", count)
	}
}

func TestSchedulerLateFrameStillAnswers(t *testing.T) {
	s := newScheduler(twoRequests(time.Second), 100*time.Millisecond, schedT0)
	s.next(schedT0)
	at := schedT0.Add(200 * time.Millisecond)
	if !s.expired(at) {
		t.Fatal("want expired")
	}
	if s.expired(at) {
		t.Fatal("expired reported twice")
	}
	s.frame(at.Add(time.Millisecond))
	s.frame(at.Add(2 * time.Millisecond))
	if cmd, ok := s.next(at); !ok || string(cmd) != "B" {
		t.Fatalf("next = %q, %v; want B", cmd, ok)
	}
}

func TestSchedulerNoRequests(t *testing.T) {
	s := newScheduler(nil, time.Second, schedT0)
	if _, ok := s.next(schedT0); ok {
		t.Fatal("next with no requests")
	}
	if s.expired(schedT0.Add(time.Hour)) {
		t.Fatal("expired with no requests")
	}
}

func TestSchedulerEarliestDueFirst(t *testing.T) {
	s := newScheduler(twoRequests(time.Second), 10*time.Millisecond, schedT0)
	s.next(schedT0) // A, next due t0+1s
	s.frame(schedT0)
	s.next(schedT0) // B, next due t0+1s
	s.frame(schedT0)
	// Both due at t0+1s again: ties go to configuration order.
	if cmd, _ := s.next(schedT0.Add(time.Second)); string(cmd) != "A" {
		t.Fatalf("tie = %q; want A", cmd)
	}
}
