package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDailyTokenBudget(t *testing.T) {
	b := NewPersistentDailyTokenBudget(nil, 100)
	if b.limit != 100 {
		t.Fatal("unexpected budget limit")
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

func TestPasswordPolicy(t *testing.T) {
	if err := validatePassword("too-short"); err == nil {
		t.Fatal("expected short password to fail")
	}
	if err := validatePassword(strings.Repeat("a", 12)); err != nil {
		t.Fatalf("expected 12-char password to pass: %v", err)
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

func TestAuthMeReturnsUserFromSQLiteTimestamp(t *testing.T) {
	store, err := OpenStore(t.TempDir() + "/sys32.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	user, err := store.CreateUser("authmetest", "authmetest@example.com", "hash")
	if err != nil {
		t.Fatal(err)
	}

	token, err := newRandomToken(32)
	if err != nil {
		t.Fatal(err)
	}
	csrf, err := newRandomToken(32)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(user.ID, hashToken(token), csrf, time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	authAPIHandler(store, NewRateLimiter(100, time.Minute), false).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"username":"authmetest"`) {
		t.Fatalf("expected user in response: %s", rec.Body.String())
	}
}

func TestConversationRunGuard(t *testing.T) {
	g := NewConversationRunGuard()
	if !g.TryAcquire(1, 42) {
		t.Fatal("expected first acquisition to succeed")
	}
	if g.TryAcquire(1, 42) {
		t.Fatal("expected duplicate acquisition for same user/conversation to fail")
	}
	if !g.TryAcquire(2, 42) {
		t.Fatal("different user should have an independent lock")
	}
	if !g.TryAcquire(1, 43) {
		t.Fatal("different conversation should have an independent lock")
	}
	g.Release(1, 42)
	if !g.TryAcquire(1, 42) {
		t.Fatal("expected released conversation to be acquirable")
	}
}

func TestConversationOwnership(t *testing.T) {
	store, err := OpenStore(t.TempDir() + "/sys32.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	one, err := store.CreateUser("ownerone", "ownerone@example.com", "hash")
	if err != nil {
		t.Fatal(err)
	}
	two, err := store.CreateUser("ownertwo", "ownertwo@example.com", "hash")
	if err != nil {
		t.Fatal(err)
	}

	conv, err := store.CreateConversation(one.ID, "private")
	if err != nil {
		t.Fatal(err)
	}
	if !store.HasConversation(one.ID, conv.ID) {
		t.Fatal("owner should access their conversation")
	}
	if store.HasConversation(two.ID, conv.ID) {
		t.Fatal("other user must not access conversation")
	}
	if _, err := store.GetConversation(two.ID, conv.ID); err == nil {
		t.Fatal("other user should not be able to fetch conversation")
	}
	if _, err := store.AddMessage(two.ID, conv.ID, "user", "nope", "", "", 0, "completed"); err == nil {
		t.Fatal("other user should not be able to write to conversation")
	}
}

func TestMessageLifecycleUpdate(t *testing.T) {
	store, err := OpenStore(t.TempDir() + "/sys32.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	user, err := store.CreateUser("lifecycleuser", "lifecycle@example.com", "hash")
	if err != nil {
		t.Fatal(err)
	}
	conv, err := store.CreateConversation(user.ID, "lifecycle")
	if err != nil {
		t.Fatal(err)
	}
	pending, err := store.AddMessage(user.ID, conv.ID, "assistant", "", "Mock", "mock", 0, "pending")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateMessageLifecycle(user.ID, pending.ID, "completed", "done", "Mock", "mock", 3); err != nil {
		t.Fatal(err)
	}
	messages, err := store.ListMessages(user.ID, conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 {
		t.Fatalf("expected one assistant message, got %d", len(messages))
	}
	if messages[0].Status != "completed" || messages[0].Content != "done" || messages[0].TotalTokens != 3 {
		t.Fatalf("unexpected lifecycle state: %+v", messages[0])
	}
}

func TestRecoverStalePendingMessages(t *testing.T) {
	store, err := OpenStore(t.TempDir() + "/sys32.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	user, err := store.CreateUser("recoveryuser", "recovery@example.com", "hash")
	if err != nil {
		t.Fatal(err)
	}
	conv, err := store.CreateConversation(user.ID, "recovery")
	if err != nil {
		t.Fatal(err)
	}
	pending, err := store.AddMessage(user.ID, conv.ID, "assistant", "", "Mock", "mock", 0, "pending")
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339Nano)
	if _, err := store.db.Exec(`UPDATE messages SET created_at=? WHERE id=?`, old, pending.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.RecoverStalePendingMessages(user.ID, conv.ID, 2*time.Minute); err != nil {
		t.Fatal(err)
	}
	messages, err := store.ListMessages(user.ID, conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 {
		t.Fatalf("expected one message, got %d", len(messages))
	}
	if messages[0].Status != "failed" || !strings.Contains(messages[0].Content, "interrupted") {
		t.Fatalf("expected recovered failed message, got %+v", messages[0])
	}
}

func TestConversationRunGuardCancel(t *testing.T) {
	g := NewConversationRunGuard()
	var cancelled bool
	cancel := func() { cancelled = true }
	if !g.TryAcquire(1, 2) {
		t.Fatal("first acquire should succeed")
	}
	g.SetCancel(1, 2, cancel)
	if !g.Cancel(1, 2) {
		t.Fatal("expected active run to be cancellable")
	}
	if !cancelled {
		t.Fatal("cancel function was not called")
	}
	g.Release(1, 2)
	if g.Cancel(1, 2) {
		t.Fatal("released run should not remain cancellable")
	}
}
