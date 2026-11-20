package main

import (
	"errors"
	"testing"
	"time"
)

func TestQueryDialogsExpireAndBoundMemory(t *testing.T) {
	now := time.Unix(1000, 0)
	s := newQueryDialogs(time.Minute)
	s.now = func() time.Time { return now }
	first, created, err := s.acquire("")
	if err != nil || !created {
		t.Fatal(err)
	}
	if same, fresh, err := s.acquire(first.id); err != nil || fresh || same != first {
		t.Fatal("known dialog was replaced")
	}
	for range 31 {
		if _, _, err := s.acquire(""); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.acquire(""); !errors.Is(err, errQueryDialogLimit) {
		t.Fatalf("unbounded sessions: %v", err)
	}
	s.finish(first.id, true)
	if _, _, err := s.acquire(""); err != nil {
		t.Fatalf("failed dialog retained its slot: %v", err)
	}
	now = now.Add(time.Minute)
	if _, _, err := s.acquire(first.id); !errors.Is(err, errMissingQueryDialog) {
		t.Fatalf("expired dialog silently restarted: %v", err)
	}
	if _, _, err := s.acquire(""); err != nil {
		t.Fatalf("expired sessions retained memory: %v", err)
	}
	if _, _, err := s.acquire("../../private"); !errors.Is(err, errInvalidQueryDialog) {
		t.Fatalf("invalid dialog accepted: %v", err)
	}
}
