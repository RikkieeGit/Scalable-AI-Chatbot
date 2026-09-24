package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestDailyTokenBudget(t *testing.T) {
	store, err := OpenStore(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	b := NewPersistentDailyTokenBudget(store, 100)
	if !b.reserve(80) {
		t.Fatal("expected first reservation to succeed")
	}
	if b.reserve(21) {
		t.Fatal("expected reservation over budget to fail")
	}
	b.refundUnused(80, 30)
	used, _ := b.status()
	if used != 30 {
		t.Fatalf("expected 30 used tokens after refund, got %d", used)
	}
}

func TestRateLimiter(t *testing.T) {
	r := NewRateLimiter(2, time.Minute)
	if !r.Allow("client") || !r.Allow("client") {
		t.Fatal("expected first two requests to be allowed")
	}
	if r.Allow("client") {
		t.Fatal("expected third request to be rate limited")
	}
	if !r.Allow("other-client") {
		t.Fatal("different client should have its own bucket")
	}
}

func TestMockStreamingHonorsCancellation(t *testing.T) {
	m := &MockClient{model: "mock"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := m.Stream(ctx, ChatRequest{Message: "hello"}, func(string) error { return nil })
	if err == nil {
		t.Fatal("expected canceled context")
	}
}

func TestMarkdownEscape(t *testing.T) {
	input := `<script>alert("x")</script> **safe**`
	out := renderMarkdownSafe(input)
	if strings.Contains(out, "<script>") {
		t.Fatal("raw HTML was not escaped")
	}
	if !strings.Contains(out, "<strong>safe</strong>") {
		t.Fatal("expected bold markdown")
	}
}
