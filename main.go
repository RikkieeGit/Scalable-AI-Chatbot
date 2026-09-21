package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/crypto/argon2"
	_ "modernc.org/sqlite"
)

const (
	defaultModel       = "openai/gpt-oss-20b"
	defaultListenAddr  = "127.0.0.1:8080"
	defaultMaxOutput   = 800
	defaultDailyTokens = 150000
	maxMessageBytes    = 16 * 1024
	requestTimeout     = 90 * time.Second
	maxStreamLineBytes = 1024 * 1024
	maxConcurrentAI    = 2
	maxTitleLength     = 72

	sessionCookieName = "sys32_session"
	csrfCookieName    = "sys32_csrf"
	sessionTTL        = 7 * 24 * time.Hour
	csrfTTL           = 7 * 24 * time.Hour
	minPasswordLen    = 12
	maxPasswordLen    = 128
)

var errProviderUnavailable = errors.New("provider unavailable")

type ProviderError struct {
	Status        int
	Code, Message string
}

func (e *ProviderError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("provider HTTP %d: %s", e.Status, e.Message)
	}
	return fmt.Sprintf("provider HTTP %d (%s): %s", e.Status, e.Code, e.Message)
}

type ChatRequest struct {
	Message string
	Mode    string
}
type Usage struct{ PromptTokens, CompletionTokens, TotalTokens int }
type ChatResponse struct {
	Content, Provider, Model, FinishReason string
	Usage                                  Usage
}
type AIProvider interface {
	Chat(context.Context, ChatRequest) (ChatResponse, error)
	Stream(context.Context, ChatRequest, func(string) error) (Usage, error)
	Name() string
	Model() string
}

type GroqClient struct {
	apiKey, model string
	httpClient    *http.Client
	maxOutput     int
}

func NewGroqClient(apiKey, model string, maxOutput int) *GroqClient {
	return &GroqClient{apiKey: apiKey, model: model, httpClient: &http.Client{Timeout: requestTimeout + 10*time.Second}, maxOutput: maxOutput}
}
func (g *GroqClient) Name() string     { return "Groq" }
func (g *GroqClient) Model() string    { return g.model }
func (g *GroqClient) endpoint() string { return "https://api.groq.com/openai/v1/chat/completions" }
func (g *GroqClient) buildPayload(req ChatRequest, stream bool) map[string]any {
	payload := map[string]any{
		"model":                 g.model,
		"messages":              []map[string]string{{"role": "user", "content": req.Message}},
		"max_completion_tokens": g.maxOutput,
		"stream":                stream,
	}

	// GPT-OSS only accepts low/medium/high for reasoning_effort.
	// Qwen 3.x accepts none/default (and Qwen 3.8 also accepts low/medium/high).
	if strings.HasPrefix(strings.ToLower(g.model), "openai/gpt-oss-") {
		if req.Mode == "think" {
			payload["reasoning_effort"] = "high"
		} else {
			payload["reasoning_effort"] = "low"
		}
		payload["include_reasoning"] = false
	} else {
		reasoning := "none"
		if req.Mode == "think" {
			reasoning = "default"
		}
		payload["reasoning_effort"] = reasoning
	}

	return payload
}
func (g *GroqClient) newRequest(ctx context.Context, req ChatRequest, stream bool) (*http.Request, error) {
	body, err := json.Marshal(g.buildPayload(req, stream))
	if err != nil {
		return nil, err
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, g.endpoint(), strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	r.Header.Set("Authorization", "Bearer "+g.apiKey)
	r.Header.Set("Content-Type", "application/json")
	if stream {
		r.Header.Set("Accept", "text/event-stream")
	} else {
		r.Header.Set("Accept", "application/json")
	}
	return r, nil
}
func (g *GroqClient) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	r, err := g.newRequest(ctx, req, false)
	if err != nil {
		return ChatResponse{}, err
	}
	resp, err := g.httpClient.Do(r)
	if err != nil {
		return ChatResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ChatResponse{}, parseProviderHTTPError(resp)
	}
	var payload struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage Usage `json:"usage"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxStreamLineBytes*2)).Decode(&payload); err != nil {
		return ChatResponse{}, err
	}
	if len(payload.Choices) == 0 {
		return ChatResponse{}, errProviderUnavailable
	}
	return ChatResponse{Content: payload.Choices[0].Message.Content, Provider: g.Name(), Model: g.Model(), FinishReason: payload.Choices[0].FinishReason, Usage: payload.Usage}, nil
}
func (g *GroqClient) Stream(ctx context.Context, req ChatRequest, emit func(string) error) (Usage, error) {
	r, err := g.newRequest(ctx, req, true)
	if err != nil {
		return Usage{}, err
	}
	resp, err := g.httpClient.Do(r)
	if err != nil {
		return Usage{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Usage{}, parseProviderHTTPError(resp)
	}
	var usage Usage
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), maxStreamLineBytes)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Usage Usage `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return usage, err
		}
		if chunk.Usage.TotalTokens > 0 {
			usage = chunk.Usage
		}
		if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
			if err := emit(chunk.Choices[0].Delta.Content); err != nil {
				return usage, err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return usage, err
	}
	return usage, nil
}
func parseProviderHTTPError(resp *http.Response) error {
	var payload struct {
		Error struct {
			Message, Code string `json:"message"`
		} `json:"error"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
	_ = json.Unmarshal(raw, &payload)
	msg := strings.TrimSpace(payload.Error.Message)
	if msg == "" {
		msg = strings.TrimSpace(string(raw))
	}
	if msg == "" {
		msg = resp.Status
	}
	return &ProviderError{Status: resp.StatusCode, Code: payload.Error.Code, Message: msg}
}

// MockClient avoids consuming external quota during local development/tests.
type MockClient struct{ model string }

func (m *MockClient) Name() string  { return "Mock" }
func (m *MockClient) Model() string { return m.model }
func (m *MockClient) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	select {
	case <-ctx.Done():
		return ChatResponse{}, ctx.Err()
	default:
	}
	content := "Mock response: " + req.Message
	return ChatResponse{Content: content, Provider: m.Name(), Model: m.Model(), Usage: Usage{TotalTokens: len(strings.Fields(content))}}, nil
}
func (m *MockClient) Stream(ctx context.Context, req ChatRequest, emit func(string) error) (Usage, error) {
	content := "Mock response: " + req.Message
	parts := strings.Fields(content)
	total := 0
	for _, p := range parts {
		select {
		case <-ctx.Done():
			return Usage{TotalTokens: total}, ctx.Err()
		default:
		}
		piece := p + " "
		if err := emit(piece); err != nil {
			return Usage{TotalTokens: total}, err
		}
		total++
		time.Sleep(5 * time.Millisecond)
	}
	return Usage{TotalTokens: total}, nil
}

type TokenBudget interface {
	reserve(int) bool
	refundUnused(int, int)
	status() (int, int)
}
type PersistentDailyTokenBudget struct {
	store *Store
	limit int
	mu    sync.Mutex
}

func NewPersistentDailyTokenBudget(store *Store, limit int) *PersistentDailyTokenBudget {
	return &PersistentDailyTokenBudget{store: store, limit: limit}
}
func (b *PersistentDailyTokenBudget) reserve(tokens int) bool {
	if tokens <= 0 {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.store.reserveDailyTokens(todayKey(), tokens, b.limit)
}
func (b *PersistentDailyTokenBudget) refundUnused(reserved, actual int) {
	if reserved <= 0 || actual >= reserved {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	_ = b.store.adjustDailyTokens(todayKey(), -(reserved - actual))
}
func (b *PersistentDailyTokenBudget) status() (int, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.store.dailyUsage(todayKey()), b.limit
}
func todayKey() string { return time.Now().UTC().Format("2006-01-02") }

type RateLimiter struct {
	mu          sync.Mutex
	limit       int
	window      time.Duration
	clients     map[string]*rateState
	lastCleanup time.Time
}
type rateState struct {
	start time.Time
	count int
}

func NewRateLimiter(limit int, window time.Duration) *RateLimiter {
	return &RateLimiter{limit: limit, window: window, clients: map[string]*rateState{}, lastCleanup: time.Now()}
}
func (l *RateLimiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if now.Sub(l.lastCleanup) >= l.window {
		cutoff := now.Add(-l.window * 2)
		for k, st := range l.clients {
			if st.start.Before(cutoff) {
				delete(l.clients, k)
			}
		}
		l.lastCleanup = now
	}
	st := l.clients[key]
	if st == nil || now.Sub(st.start) >= l.window {
		l.clients[key] = &rateState{start: now, count: 1}
		return true
	}
	if st.count >= l.limit {
		return false
	}
	st.count++
	return true
}

type ConversationRunGuard struct {
	mu     sync.Mutex
	active map[string]context.CancelFunc
}

func NewConversationRunGuard() *ConversationRunGuard {
	return &ConversationRunGuard{active: make(map[string]context.CancelFunc)}
}

func (g *ConversationRunGuard) key(userID, conversationID int64) string {
	return fmt.Sprintf("%d:%d", userID, conversationID)
}

func (g *ConversationRunGuard) TryAcquire(userID, conversationID int64) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	key := g.key(userID, conversationID)
	if _, ok := g.active[key]; ok {
		return false
	}
	g.active[key] = nil
	return true
}

func (g *ConversationRunGuard) SetCancel(userID, conversationID int64, cancel context.CancelFunc) {
	g.mu.Lock()
	defer g.mu.Unlock()
	key := g.key(userID, conversationID)
	if _, ok := g.active[key]; ok {
		g.active[key] = cancel
	}
}

func (g *ConversationRunGuard) Cancel(userID, conversationID int64) bool {
	g.mu.Lock()
	cancel, ok := g.active[g.key(userID, conversationID)]
	g.mu.Unlock()
	if !ok || cancel == nil {
		return false
	}
	cancel()
	return true
}

func (g *ConversationRunGuard) Release(userID, conversationID int64) {
	g.mu.Lock()
	delete(g.active, g.key(userID, conversationID))
	g.mu.Unlock()
}

// Store persists users, sessions, conversations, messages and usage in SQLite.
type Store struct{ db *sql.DB }
type User struct {
	ID        int64     `json:"id"`
	Username  string    `json:"username"`
	Email     string    `json:"email"`
	CreatedAt time.Time `json:"created_at"`
}
type Session struct {
	UserID    int64
	ExpiresAt time.Time
	CSRFToken string
}
type Conversation struct {
	ID        int64     `json:"id"`
	UserID    int64     `json:"user_id"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
type StoredMessage struct {
	ID             int64     `json:"id"`
	ConversationID int64     `json:"conversation_id"`
	Role           string    `json:"role"`
	Content        string    `json:"content"`
	Provider       string    `json:"provider"`
	Model          string    `json:"model"`
	CreatedAt      time.Time `json:"created_at"`
	TotalTokens    int       `json:"total_tokens"`
	Status         string    `json:"status"`
}

func OpenStore(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	st := &Store{db: db}
	if err := st.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return st, nil
}
func (s *Store) migrate() error {
	_, err := s.db.Exec(`PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000; PRAGMA synchronous=NORMAL;
CREATE TABLE IF NOT EXISTS users (id INTEGER PRIMARY KEY AUTOINCREMENT, username TEXT NOT NULL UNIQUE, email TEXT NOT NULL UNIQUE, password_hash TEXT NOT NULL, created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS sessions (id_hash TEXT PRIMARY KEY, user_id INTEGER NOT NULL, csrf_token TEXT NOT NULL, expires_at TEXT NOT NULL, created_at TEXT NOT NULL, last_seen_at TEXT NOT NULL, FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE);
CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user_id); CREATE INDEX IF NOT EXISTS idx_sessions_expiry ON sessions(expires_at);
CREATE TABLE IF NOT EXISTS conversations (id INTEGER PRIMARY KEY AUTOINCREMENT, user_id INTEGER, title TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL, FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE);
CREATE TABLE IF NOT EXISTS messages (id INTEGER PRIMARY KEY AUTOINCREMENT, conversation_id INTEGER NOT NULL, role TEXT NOT NULL CHECK(role IN ('user','assistant','system')), content TEXT NOT NULL, provider TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '', total_tokens INTEGER NOT NULL DEFAULT 0, status TEXT NOT NULL DEFAULT 'completed' CHECK(status IN ('pending','completed','cancelled','failed')), created_at TEXT NOT NULL, FOREIGN KEY(conversation_id) REFERENCES conversations(id) ON DELETE CASCADE);
CREATE TABLE IF NOT EXISTS ai_usage (day TEXT PRIMARY KEY, used_tokens INTEGER NOT NULL DEFAULT 0);`)
	if err != nil {
		return err
	}
	// Stage 3/4 compatibility: add fields to old databases when possible.
	if _, e := s.db.Exec(`ALTER TABLE conversations ADD COLUMN user_id INTEGER`); e != nil && !strings.Contains(strings.ToLower(e.Error()), "duplicate column") {
		return e
	}
	if _, e := s.db.Exec(`ALTER TABLE messages ADD COLUMN status TEXT NOT NULL DEFAULT 'completed'`); e != nil && !strings.Contains(strings.ToLower(e.Error()), "duplicate column") {
		return e
	}
	if _, e := s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_messages_conversation_created ON messages(conversation_id,created_at,id); CREATE INDEX IF NOT EXISTS idx_conversations_user_updated ON conversations(user_id,updated_at DESC);`); e != nil {
		return e
	}
	return nil
}
func (s *Store) Close() error { return s.db.Close() }
func (s *Store) CreateUser(username, email, passwordHash string) (User, error) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := s.db.Exec(`INSERT INTO users(username,email,password_hash,created_at) VALUES(?,?,?,?)`, username, email, passwordHash, now)
	if err != nil {
		return User{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return User{}, err
	}
	return User{ID: id, Username: username, Email: email, CreatedAt: parseDBTime(now)}, nil
}
func (s *Store) FindUserByLogin(login string) (int64, string, User, error) {
	var id int64
	var username, email, hash, created string
	err := s.db.QueryRow(`SELECT id,username,email,password_hash,created_at FROM users WHERE lower(username)=lower(?) OR lower(email)=lower(?) LIMIT 1`, login, login).Scan(&id, &username, &email, &hash, &created)
	if err != nil {
		return 0, "", User{}, err
	}
	return id, hash, User{ID: id, Username: username, Email: email, CreatedAt: parseDBTime(created)}, nil
}
func (s *Store) userCount() int {
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n
}
func (s *Store) ClaimLegacyConversations(userID int64) error {
	_, err := s.db.Exec(`UPDATE conversations SET user_id=? WHERE user_id IS NULL`, userID)
	return err
}
func (s *Store) CreateSession(userID int64, sessionHash, csrf string, expires time.Time) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.Exec(`INSERT INTO sessions(id_hash,user_id,csrf_token,expires_at,created_at,last_seen_at) VALUES(?,?,?,?,?,?)`, sessionHash, userID, csrf, expires.UTC().Format(time.RFC3339Nano), now, now)
	return err
}
func (s *Store) GetSession(sessionHash string) (Session, error) {
	var uid int64
	var csrf, exp string
	err := s.db.QueryRow(`SELECT user_id,csrf_token,expires_at FROM sessions WHERE id_hash=?`, sessionHash).Scan(&uid, &csrf, &exp)
	if err != nil {
		return Session{}, err
	}
	e := parseDBTime(exp)
	if e.Before(time.Now().UTC()) {
		_, _ = s.db.Exec(`DELETE FROM sessions WHERE id_hash=?`, sessionHash)
		return Session{}, sql.ErrNoRows
	}
	return Session{UserID: uid, ExpiresAt: e, CSRFToken: csrf}, nil
}
func (s *Store) DeleteSession(sessionHash string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE id_hash=?`, sessionHash)
	return err
}

func (s *Store) DeleteSessionsForUser(userID int64) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE user_id=?`, userID)
	return err
}

func (s *Store) UpdatePassword(userID int64, passwordHash string) error {
	_, err := s.db.Exec(
		`UPDATE users SET password_hash=? WHERE id=?`,
		passwordHash,
		userID,
	)
	return err
}

func (s *Store) CleanupSessions() {
	_, _ = s.db.Exec(`DELETE FROM sessions WHERE expires_at < ?`, time.Now().UTC().Format(time.RFC3339Nano))
}
func (s *Store) CreateConversation(userID int64, title string) (Conversation, error) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if strings.TrimSpace(title) == "" {
		title = "New chat"
	}
	res, err := s.db.Exec(`INSERT INTO conversations(user_id,title,created_at,updated_at) VALUES(?,?,?,?)`, userID, title, now, now)
	if err != nil {
		return Conversation{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Conversation{}, err
	}
	return Conversation{ID: id, UserID: userID, Title: title, CreatedAt: parseDBTime(now), UpdatedAt: parseDBTime(now)}, nil
}
func (s *Store) GetConversation(userID, id int64) (Conversation, error) {
	var c Conversation
	var cr, up string
	err := s.db.QueryRow(`SELECT id,user_id,title,created_at,updated_at FROM conversations WHERE id=? AND user_id=?`, id, userID).Scan(&c.ID, &c.UserID, &c.Title, &cr, &up)
	if err != nil {
		return Conversation{}, err
	}
	c.CreatedAt = parseDBTime(cr)
	c.UpdatedAt = parseDBTime(up)
	return c, nil
}
func (s *Store) ListConversations(userID int64) ([]Conversation, error) {
	rows, err := s.db.Query(`SELECT id,user_id,title,created_at,updated_at FROM conversations WHERE user_id=? ORDER BY datetime(updated_at) DESC,id DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Conversation, 0)
	for rows.Next() {
		var c Conversation
		var cr, up string
		if err := rows.Scan(&c.ID, &c.UserID, &c.Title, &cr, &up); err != nil {
			return nil, err
		}
		c.CreatedAt = parseDBTime(cr)
		c.UpdatedAt = parseDBTime(up)
		out = append(out, c)
	}
	return out, rows.Err()
}
func (s *Store) DeleteConversation(userID, id int64) error {
	_, err := s.db.Exec(`DELETE FROM conversations WHERE id=? AND user_id=?`, id, userID)
	return err
}
func (s *Store) HasConversation(userID, id int64) bool {
	var n int
	err := s.db.QueryRow(`SELECT 1 FROM conversations WHERE id=? AND user_id=?`, id, userID).Scan(&n)
	return err == nil
}
func (s *Store) AddMessage(userID, conversationID int64, role, content, provider, model string, tokens int, status string) (StoredMessage, error) {
	if !s.HasConversation(userID, conversationID) {
		return StoredMessage{}, sql.ErrNoRows
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := s.db.Exec(`INSERT INTO messages(conversation_id,role,content,provider,model,total_tokens,status,created_at) VALUES(?,?,?,?,?,?,?,?)`, conversationID, role, content, provider, model, tokens, status, now)
	if err != nil {
		return StoredMessage{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return StoredMessage{}, err
	}
	_, err = s.db.Exec(`UPDATE conversations SET updated_at=? WHERE id=? AND user_id=?`, now, conversationID, userID)
	if err != nil {
		return StoredMessage{}, err
	}
	return StoredMessage{ID: id, ConversationID: conversationID, Role: role, Content: content, Provider: provider, Model: model, TotalTokens: tokens, Status: status, CreatedAt: parseDBTime(now)}, nil
}
func (s *Store) UpdateMessageLifecycle(userID, messageID int64, status, content, provider, model string, tokens int) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := s.db.Exec(`
		UPDATE messages
		SET content=?, provider=?, model=?, total_tokens=?, status=?
		WHERE id=? AND role='assistant' AND conversation_id IN (SELECT id FROM conversations WHERE user_id=?)`,
		content, provider, model, tokens, status, messageID, userID,
	)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return sql.ErrNoRows
	}
	_, err = s.db.Exec(`
		UPDATE conversations
		SET updated_at=?
		WHERE id=(SELECT conversation_id FROM messages WHERE id=? ) AND user_id=?`,
		now, messageID, userID,
	)
	return err
}

func (s *Store) RecoverStalePendingMessages(userID, conversationID int64, maxAge time.Duration) error {
	if !s.HasConversation(userID, conversationID) {
		return sql.ErrNoRows
	}
	cutoff := time.Now().UTC().Add(-maxAge).Format(time.RFC3339Nano)
	_, err := s.db.Exec(`
		UPDATE messages
		SET content=CASE WHEN TRIM(content)='' THEN 'Generation was interrupted before completion.' ELSE content END,
			status='failed'
		WHERE conversation_id=? AND role='assistant' AND status='pending' AND created_at < ?`,
		conversationID, cutoff,
	)
	return err
}

func (s *Store) ListMessages(userID, conversationID int64) ([]StoredMessage, error) {
	if !s.HasConversation(userID, conversationID) {
		return nil, sql.ErrNoRows
	}
	// A stale pending assistant message can remain after an unexpected process
	// restart. Reconcile it before returning history so the UI never shows an
	// indefinitely running generation.
	if err := s.RecoverStalePendingMessages(userID, conversationID, 2*time.Minute); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT id,conversation_id,role,content,provider,model,total_tokens,status,created_at FROM messages WHERE conversation_id=? ORDER BY id ASC`, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]StoredMessage, 0)
	for rows.Next() {
		var m StoredMessage
		var cr string
		if err := rows.Scan(&m.ID, &m.ConversationID, &m.Role, &m.Content, &m.Provider, &m.Model, &m.TotalTokens, &m.Status, &cr); err != nil {
			return nil, err
		}
		m.CreatedAt = parseDBTime(cr)
		out = append(out, m)
	}
	return out, rows.Err()
}
func (s *Store) RenameConversationFromFirstUserMessage(userID, id int64, message string) {
	_, _ = s.db.Exec(`UPDATE conversations SET title=?,updated_at=? WHERE id=? AND user_id=? AND title='New chat'`, makeTitle(message), time.Now().UTC().Format(time.RFC3339Nano), id, userID)
}
func (s *Store) reserveDailyTokens(day string, tokens, limit int) bool {
	tx, err := s.db.Begin()
	if err != nil {
		return false
	}
	defer tx.Rollback()
	var used int
	if err = tx.QueryRow(`SELECT used_tokens FROM ai_usage WHERE day=?`, day).Scan(&used); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false
	}
	if used+tokens > limit {
		return false
	}
	if errors.Is(err, sql.ErrNoRows) {
		_, err = tx.Exec(`INSERT INTO ai_usage(day,used_tokens) VALUES(?,?)`, day, tokens)
	} else {
		_, err = tx.Exec(`UPDATE ai_usage SET used_tokens=used_tokens+? WHERE day=?`, tokens, day)
	}
	if err != nil {
		return false
	}
	return tx.Commit() == nil
}
func (s *Store) adjustDailyTokens(day string, delta int) error {
	if delta == 0 {
		return nil
	}
	_, err := s.db.Exec(`UPDATE ai_usage SET used_tokens=MAX(0,used_tokens+?) WHERE day=?`, delta, day)
	return err
}
func (s *Store) dailyUsage(day string) int {
	var used int
	if err := s.db.QueryRow(`SELECT used_tokens FROM ai_usage WHERE day=?`, day).Scan(&used); err != nil {
		return 0
	}
	return used
}
func makeTitle(message string) string {
	t := strings.Join(strings.Fields(message), " ")
	r := []rune(t)
	if len(r) > maxTitleLength {
		return string(r[:maxTitleLength-1]) + "…"
	}
	if t == "" {
		return "New chat"
	}
	return t
}
func parseDBTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t, _ = time.Parse(time.RFC3339, s)
	}
	return t
}

// Authentication helpers.
type authContextKey struct{}

func newRandomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
func hashToken(value string) string {
	sum := sha256.Sum256([]byte(value))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
func validateUsername(v string) error {
	v = strings.TrimSpace(v)
	if len(v) < 3 || len(v) > 32 {
		return errors.New("username must be 3-32 characters")
	}
	for _, r := range v {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return errors.New("username may contain letters, numbers, _ and - only")
		}
	}
	return nil
}
func validateEmail(v string) error {
	v = strings.TrimSpace(v)
	if len(v) < 5 || len(v) > 254 || !strings.Contains(v, "@") {
		return errors.New("valid email is required")
	}
	return nil
}
func validatePassword(v string) error {
	if len(v) < minPasswordLen || len(v) > maxPasswordLen {
		return fmt.Errorf("password must be %d-%d characters", minPasswordLen, maxPasswordLen)
	}
	return nil
}
func hashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	dk := argon2.IDKey([]byte(password), salt, 3, 32*1024, 2, 32)
	return fmt.Sprintf("$argon2id$v=19$m=32768,t=3,p=2$%s$%s", base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(dk)), nil
}
func verifyPassword(password, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != "v=19" {
		return false
	}
	var m, t, p uint32
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, t, m, uint8(p), uint32(len(want)))
	if len(got) != len(want) {
		return false
	}
	var diff byte
	for i := range want {
		diff |= got[i] ^ want[i]
	}
	return diff == 0
}
func currentSession(r *http.Request, store *Store) (Session, error) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return Session{}, err
	}
	return store.GetSession(hashToken(cookie.Value))
}
func setAuthCookies(w http.ResponseWriter, sessionToken, csrf string, secure bool, expires time.Time) {
	same := http.SameSiteLaxMode
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: sessionToken, Path: "/", Expires: expires, MaxAge: int(time.Until(expires).Seconds()), HttpOnly: true, Secure: secure, SameSite: same})
	http.SetCookie(w, &http.Cookie{Name: csrfCookieName, Value: csrf, Path: "/", Expires: expires, MaxAge: int(time.Until(expires).Seconds()), HttpOnly: false, Secure: secure, SameSite: same})
}
func clearAuthCookies(w http.ResponseWriter, secure bool) {
	for _, name := range []string{sessionCookieName, csrfCookieName} {
		http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1, Expires: time.Unix(1, 0), HttpOnly: name == sessionCookieName, Secure: secure, SameSite: http.SameSiteLaxMode})
	}
}
func requireAuth(store *Store, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess, err := currentSession(r, store)
		if err != nil {
			if r.URL.Path == "/" {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
				return
			}
			writeJSONError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		ctx := context.WithValue(r.Context(), authContextKey{}, sess)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
func sessionFromContext(r *http.Request) Session {
	s, _ := r.Context().Value(authContextKey{}).(Session)
	return s
}
func verifyCSRF(r *http.Request, sess Session) bool {
	got := strings.TrimSpace(r.Header.Get("X-CSRF-Token"))
	if got == "" {
		got = strings.TrimSpace(r.FormValue("csrf_token"))
	}
	cookie, err := r.Cookie(csrfCookieName)
	if err != nil {
		return false
	}
	return got != "" && got == sess.CSRFToken && cookie.Value == sess.CSRFToken
}

// HTTP and configuration.
type Config struct {
	Addr, ProviderName, Model, GroqAPIKey, DatabasePath string
	MaxOutputTokens, DailyTokenLimit, RateLimit         int
	CookieSecure                                        bool
}

func loadConfig() (Config, error) {
	cfg := Config{Addr: getenv("APP_ADDR", defaultListenAddr), ProviderName: strings.ToLower(strings.TrimSpace(getenv("AI_PROVIDER", "mock"))), Model: getenv("GROQ_MODEL", defaultModel), GroqAPIKey: strings.TrimSpace(os.Getenv("GROQ_API_KEY")), DatabasePath: getenv("DATABASE_PATH", "data/sys32.db"), MaxOutputTokens: envInt("MAX_OUTPUT_TOKENS", defaultMaxOutput), DailyTokenLimit: envInt("MAX_DAILY_TOKENS", defaultDailyTokens), RateLimit: envInt("RATE_LIMIT_PER_MINUTE", 10), CookieSecure: strings.EqualFold(os.Getenv("COOKIE_SECURE"), "true")}
	if strings.EqualFold(os.Getenv("ALLOW_PAID_APIS"), "true") {
		return Config{}, errors.New("ALLOW_PAID_APIS=true is unsupported in this zero-cost build")
	}
	if cfg.MaxOutputTokens < 1 || cfg.MaxOutputTokens > 1000 {
		return Config{}, errors.New("MAX_OUTPUT_TOKENS must be between 1 and 1000")
	}
	if cfg.DailyTokenLimit < cfg.MaxOutputTokens {
		return Config{}, errors.New("MAX_DAILY_TOKENS must be >= MAX_OUTPUT_TOKENS")
	}
	if cfg.RateLimit < 1 {
		return Config{}, errors.New("RATE_LIMIT_PER_MINUTE must be >= 1")
	}
	return cfg, nil
}
func getenv(k, d string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return d
}
func envInt(k string, d int) int {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return d
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return d
	}
	return n
}

var requestIDCounter atomic.Uint64

type requestIDContextKey struct{}

func requestID(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("X-Request-ID")); v != "" && len(v) <= 128 {
		return v
	}
	return fmt.Sprintf("req-%d-%d", time.Now().UnixNano(), requestIDCounter.Add(1))
}
func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := requestID(r)
		ctx := context.WithValue(r.Context(), requestIDContextKey{}, id)
		rw := &responseWriter{ResponseWriter: w, status: 200}
		rw.Header().Set("X-Request-ID", id)
		start := time.Now()
		next.ServeHTTP(rw, r.WithContext(ctx))
		log.Printf("request_id=%s method=%s path=%s status=%d duration=%s ip=%s", id, r.Method, r.URL.Path, rw.status, time.Since(start), clientIP(r))
	})
}

type responseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}
func (rw *responseWriter) Write(p []byte) (int, error) { return rw.ResponseWriter.Write(p) }
func (rw *responseWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' https://fonts.googleapis.com; font-src 'self' https://fonts.gstatic.com; script-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'")
		next.ServeHTTP(w, r)
	})
}
func clientIP(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); v != "" {
		parts := strings.Split(v, ",")
		return strings.TrimSpace(parts[0])
	}
	if v := strings.TrimSpace(r.Header.Get("X-Real-IP")); v != "" {
		return v
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}
func clientKey(r *http.Request) string { return clientIP(r) }
func getRequestID(r *http.Request) string {
	if id, ok := r.Context().Value(requestIDContextKey{}).(string); ok && id != "" {
		return id
	}
	return requestID(r)
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	provider, err := buildProvider(cfg.ProviderName, cfg.Model, cfg.MaxOutputTokens)
	if err != nil {
		log.Fatal(err)
	}
	store, err := OpenStore(cfg.DatabasePath)
	if err != nil {
		log.Fatal("open database: ", err)
	}
	defer store.Close()
	budget := NewPersistentDailyTokenBudget(store, cfg.DailyTokenLimit)
	limiter := NewRateLimiter(cfg.RateLimit, time.Minute)
	authLimiter := NewRateLimiter(8, time.Minute)
	aiSlots := make(chan struct{}, maxConcurrentAI)
	runGuard := NewConversationRunGuard()
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthHandler)
	mux.HandleFunc("/readyz", readyHandler(store))
	mux.HandleFunc("/login", loginPageHandler(store))
	mux.HandleFunc("/register", registerPageHandler(store))
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))
	authAPI := authAPIHandler(store, authLimiter, cfg.CookieSecure)
	mux.Handle("/api/auth/", authAPI)
	protected := requireAuth(store, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.ServeFile(w, r, "templates/index.html") }))
	mux.Handle("/", protected)
	mux.Handle("/api/conversations", requireAuth(store, conversationsHandler(store, cfg.CookieSecure)))
	mux.Handle("/api/conversations/", requireAuth(store, conversationHandler(store, cfg.CookieSecure)))
	mux.Handle("/api/chat", requireAuth(store, chatHandler(provider, store, limiter, aiSlots, budget, runGuard)))
	mux.Handle("/api/chat/stream", requireAuth(store, streamHandler(provider, store, limiter, aiSlots, budget, cfg.MaxOutputTokens, runGuard)))
	mux.Handle("/api/chat/stop", requireAuth(store, stopHandler(store, runGuard)))
	mux.Handle("/api/usage", requireAuth(store, usageHandler(provider, budget)))
	handler := securityHeaders(requestLogger(mux))
	server := &http.Server{Addr: cfg.Addr, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 95 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 * 1024}
	go func() {
		log.Printf("Sys32.AI Stage 5 running at http://%s", server.Addr)
		log.Printf("provider=%s model=%s database=%s daily_token_limit=%d max_output=%d rate_limit=%d concurrent_ai=%d", provider.Name(), provider.Model(), cfg.DatabasePath, cfg.DailyTokenLimit, cfg.MaxOutputTokens, cfg.RateLimit, maxConcurrentAI)
		serverErr := server.ListenAndServe()
		if serverErr != nil && !errors.Is(serverErr, http.ErrServerClosed) {
			log.Printf("server error: %v", serverErr)
		}
	}()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stop)
	<-stop
	log.Printf("received shutdown signal")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Printf("graceful shutdown failed: %v", err)
		_ = server.Close()
	}
}
func buildProvider(name, model string, maxOutput int) (AIProvider, error) {
	switch name {
	case "mock":
		return &MockClient{model: "mock"}, nil
	case "groq":
		apiKey := os.Getenv("GROQ_API_KEY")
		if apiKey == "" {
			return nil, errors.New("GROQ_API_KEY is not set")
		}
		return NewGroqClient(apiKey, model, maxOutput), nil
	default:
		return nil, fmt.Errorf("unsupported AI_PROVIDER %q (use mock or groq)", name)
	}
}
func healthHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, 405, "method not allowed")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}
func readyHandler(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSONError(w, 405, "method not allowed")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := store.db.PingContext(ctx); err != nil {
			writeJSONError(w, 503, "database is not ready")
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ready\n"))
	}
}

func loginPageHandler(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, err := currentSession(r, store); err == nil {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		http.ServeFile(w, r, "templates/login.html")
	}
}
func registerPageHandler(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, err := currentSession(r, store); err == nil {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		http.ServeFile(w, r, "templates/register.html")
	}
}

func authAPIHandler(store *Store, limiter *RateLimiter, secure bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/me":
			if r.Method != http.MethodGet {
				writeJSONError(w, 405, "method not allowed")
				return
			}
			sess, err := currentSession(r, store)
			if err != nil {
				writeJSONError(w, 401, "not authenticated")
				return
			}
			var user User
			var createdAt string
			err = store.db.QueryRow(`SELECT id,username,email,created_at FROM users WHERE id=?`, sess.UserID).Scan(&user.ID, &user.Username, &user.Email, &createdAt)
			if err != nil {
				writeJSONError(w, 401, "not authenticated")
				return
			}
			user.CreatedAt = parseDBTime(createdAt)
			writeJSON(w, 200, map[string]any{"user": user, "csrf_token": sess.CSRFToken})
		case "/api/auth/register":
			if r.Method != http.MethodPost {
				writeJSONError(w, 405, "method not allowed")
				return
			}
			if !limiter.Allow(clientKey(r)) {
				writeJSONError(w, 429, "too many authentication attempts")
				return
			}
			var in struct{ Username, Email, Password string }
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8*1024)).Decode(&in); err != nil {
				writeJSONError(w, 400, "invalid request")
				return
			}
			in.Username = strings.TrimSpace(in.Username)
			in.Email = strings.TrimSpace(strings.ToLower(in.Email))
			if err := validateUsername(in.Username); err != nil {
				writeJSONError(w, 400, err.Error())
				return
			}
			if err := validateEmail(in.Email); err != nil {
				writeJSONError(w, 400, err.Error())
				return
			}
			if err := validatePassword(in.Password); err != nil {
				writeJSONError(w, 400, err.Error())
				return
			}
			hash, err := hashPassword(in.Password)
			if err != nil {
				writeJSONError(w, 500, "could not create account")
				return
			}
			user, err := store.CreateUser(in.Username, in.Email, hash)
			if err != nil {
				msg := "could not create account"
				if strings.Contains(strings.ToLower(err.Error()), "unique") {
					msg = "username or email already exists"
				}
				writeJSONError(w, 409, msg)
				return
			}
			if store.userCount() == 1 {
				_ = store.ClaimLegacyConversations(user.ID)
			}
			if err := issueSession(w, store, user.ID, secure); err != nil {
				writeJSONError(w, 500, "could not create session")
				return
			}
			writeJSON(w, 201, map[string]any{"user": user})
		case "/api/auth/login":
			if r.Method != http.MethodPost {
				writeJSONError(w, 405, "method not allowed")
				return
			}
			if !limiter.Allow(clientKey(r)) {
				writeJSONError(w, 429, "too many authentication attempts")
				return
			}
			var in struct{ Login, Password string }
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8*1024)).Decode(&in); err != nil {
				writeJSONError(w, 400, "invalid request")
				return
			}
			login := strings.TrimSpace(in.Login)
			if login == "" || in.Password == "" {
				writeJSONError(w, 400, "login and password are required")
				return
			}
			uid, hash, user, err := store.FindUserByLogin(login)
			if err != nil || !verifyPassword(in.Password, hash) {
				writeJSONError(w, 401, "invalid username/email or password")
				return
			}
			if err := issueSession(w, store, uid, secure); err != nil {
				writeJSONError(w, 500, "could not create session")
				return
			}
			writeJSON(w, 200, map[string]any{"user": user})

		case "/api/auth/change-password":
			if r.Method != http.MethodPost {
				writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
				return
			}

			sess, err := currentSession(r, store)
			if err != nil {
				writeJSONError(w, http.StatusUnauthorized, "not authenticated")
				return
			}

			if !verifyCSRF(r, sess) {
				writeJSONError(w, http.StatusForbidden, "invalid CSRF token")
				return
			}

			var in struct {
				CurrentPassword string `json:"current_password"`
				NewPassword     string `json:"new_password"`
				ConfirmPassword string `json:"confirm_password"`
			}

			if err := json.NewDecoder(
				http.MaxBytesReader(w, r.Body, 8*1024),
			).Decode(&in); err != nil {
				writeJSONError(w, http.StatusBadRequest, "invalid request")
				return
			}

			if in.CurrentPassword == "" ||
				in.NewPassword == "" ||
				in.ConfirmPassword == "" {
				writeJSONError(w, http.StatusBadRequest, "all password fields are required")
				return
			}

			if in.NewPassword != in.ConfirmPassword {
				writeJSONError(w, http.StatusBadRequest, "new passwords do not match")
				return
			}

			if err := validatePassword(in.NewPassword); err != nil {
				writeJSONError(w, http.StatusBadRequest, err.Error())
				return
			}

			var passwordHash string
			err = store.db.QueryRow(
				`SELECT password_hash FROM users WHERE id=?`,
				sess.UserID,
			).Scan(&passwordHash)

			if err != nil {
				writeJSONError(w, http.StatusUnauthorized, "not authenticated")
				return
			}

			if !verifyPassword(in.CurrentPassword, passwordHash) {
				writeJSONError(w, http.StatusBadRequest, "current password is incorrect")
				return
			}

			newHash, err := hashPassword(in.NewPassword)
			if err != nil {
				writeJSONError(w, http.StatusInternalServerError, "could not update password")
				return
			}

			if err := store.UpdatePassword(sess.UserID, newHash); err != nil {
				writeJSONError(w, http.StatusInternalServerError, "could not update password")
				return
			}

			if err := store.DeleteSessionsForUser(sess.UserID); err != nil {
				writeJSONError(w, http.StatusInternalServerError, "password changed but sessions could not be cleared")
				return
			}

			clearAuthCookies(w, secure)

			w.WriteHeader(http.StatusNoContent)
			return

		case "/api/auth/logout":
			if r.Method != http.MethodPost {
				writeJSONError(w, 405, "method not allowed")
				return
			}
			sess, err := currentSession(r, store)
			if err == nil {
				if !verifyCSRF(r, sess) {
					writeJSONError(w, 403, "invalid CSRF token")
					return
				}
				if c, e := r.Cookie(sessionCookieName); e == nil {
					_ = store.DeleteSession(hashToken(c.Value))
				}
			}
			clearAuthCookies(w, secure)
			w.WriteHeader(204)
		default:
			writeJSONError(w, 404, "not found")
		}
	})
}
func issueSession(w http.ResponseWriter, store *Store, userID int64, secure bool) error {
	token, err := newRandomToken(32)
	if err != nil {
		return err
	}
	csrf, err := newRandomToken(32)
	if err != nil {
		return err
	}
	expires := time.Now().UTC().Add(sessionTTL)
	if err := store.CreateSession(userID, hashToken(token), csrf, expires); err != nil {
		return err
	}
	setAuthCookies(w, token, csrf, secure, expires)
	store.CleanupSessions()
	return nil
}

func conversationsHandler(store *Store, secure bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := sessionFromContext(r)
		if r.Method == http.MethodGet {
			list, err := store.ListConversations(sess.UserID)
			if err != nil {
				writeJSONError(w, 500, "could not list conversations")
				return
			}
			writeJSON(w, 200, list)
			return
		}
		if r.Method == http.MethodPost {
			if !verifyCSRF(r, sess) {
				writeJSONError(w, 403, "invalid CSRF token")
				return
			}
			var input struct {
				Title string `json:"title"`
			}
			if r.Body != nil {
				_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4*1024)).Decode(&input)
			}
			c, err := store.CreateConversation(sess.UserID, input.Title)
			if err != nil {
				writeJSONError(w, 500, "could not create conversation")
				return
			}
			writeJSON(w, 201, c)
			return
		}
		writeJSONError(w, 405, "method not allowed")
	}
}
func conversationHandler(store *Store, secure bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := sessionFromContext(r)
		id, ok := pathID(r.URL.Path, "/api/conversations/")
		if !ok {
			writeJSONError(w, 400, "invalid conversation id")
			return
		}
		switch r.Method {
		case http.MethodGet:
			c, err := store.GetConversation(sess.UserID, id)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					writeJSONError(w, 404, "conversation not found")
				} else {
					writeJSONError(w, 500, "could not load conversation")
				}
				return
			}
			messages, err := store.ListMessages(sess.UserID, id)
			if err != nil {
				writeJSONError(w, 500, "could not load messages")
				return
			}
			writeJSON(w, 200, map[string]any{"conversation": c, "messages": messages})
		case http.MethodDelete:
			if !verifyCSRF(r, sess) {
				writeJSONError(w, 403, "invalid CSRF token")
				return
			}
			if err := store.DeleteConversation(sess.UserID, id); err != nil {
				writeJSONError(w, 500, "could not delete conversation")
				return
			}
			w.WriteHeader(204)
		default:
			writeJSONError(w, 405, "method not allowed")
		}
	}
}

func chatHandler(provider AIProvider, store *Store, limiter *RateLimiter, slots chan struct{}, budget TokenBudget, runGuard *ConversationRunGuard) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := sessionFromContext(r)
		if !verifyCSRF(r, sess) {
			writeJSONError(w, 403, "invalid CSRF token")
			return
		}
		reqID := getRequestID(r)
		if r.Method != http.MethodPost {
			writeJSONError(w, 405, "method not allowed")
			return
		}
		if !limiter.Allow(clientKey(r)) {
			writeJSONError(w, 429, "request rate limit exceeded")
			return
		}
		if !acquireAISlot(w, slots) {
			return
		}
		defer releaseAISlot(slots)
		req, convID, err := parseChatAndConversation(w, r, store, sess.UserID)
		if err != nil {
			writeJSONError(w, 400, err.Error())
			return
		}
		if !runGuard.TryAcquire(sess.UserID, convID) {
			writeJSONError(w, http.StatusConflict, "a response is already being generated for this conversation")
			return
		}
		defer runGuard.Release(sess.UserID, convID)
		reserved := envInt("MAX_OUTPUT_TOKENS", defaultMaxOutput)
		if !budget.reserve(reserved) {
			writeJSONError(w, 429, "the app's free daily AI budget has been reached")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()
		runGuard.SetCancel(sess.UserID, convID, cancel)
		start := time.Now()
		if _, err = store.AddMessage(sess.UserID, convID, "user", req.Message, "", "", 0, "completed"); err != nil {
			budget.refundUnused(reserved, 0)
			writeJSONError(w, 500, "could not save message")
			return
		}
		result, err := provider.Chat(ctx, req)
		actual := result.Usage.TotalTokens
		if err != nil {
			// A failed provider call that reports no usage consumed no confirmed AI tokens.
			// Refund the full reservation instead of charging the request the max output.
			actual = 0
		} else if actual <= 0 {
			// Some providers may omit usage on a successful response. Keep the existing
			// conservative reservation behavior in that case.
			actual = reserved
		}
		budget.refundUnused(reserved, actual)
		if err != nil {
			log.Printf("request_id=%s chat error provider=%s err=%v", reqID, provider.Name(), err)
			writeProviderError(w, err)
			return
		}
		if _, err := store.AddMessage(sess.UserID, convID, "assistant", result.Content, result.Provider, result.Model, result.Usage.TotalTokens, "completed"); err != nil {
			log.Printf("request_id=%s save assistant error: %v", reqID, err)
		}
		store.RenameConversationFromFirstUserMessage(sess.UserID, convID, req.Message)
		w.Header().Set("X-Request-ID", reqID)
		log.Printf("request_id=%s chat provider=%s model=%s conversation=%d user=%d latency=%s tokens=%d", reqID, result.Provider, result.Model, convID, sess.UserID, time.Since(start), actual)
		writeAssistantFragment(w, result.Content, result.Provider, result.Model, result.Usage)
	}
}
func stopHandler(store *Store, runGuard *ConversationRunGuard) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := sessionFromContext(r)
		if !verifyCSRF(r, sess) {
			writeJSONError(w, http.StatusForbidden, "invalid CSRF token")
			return
		}
		if r.Method != http.MethodPost {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if err := r.ParseForm(); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid request")
			return
		}
		convID, err := strconv.ParseInt(strings.TrimSpace(r.FormValue("conversation_id")), 10, 64)
		if err != nil || convID <= 0 {
			writeJSONError(w, http.StatusBadRequest, "conversation_id is required")
			return
		}
		if !store.HasConversation(sess.UserID, convID) {
			writeJSONError(w, http.StatusNotFound, "conversation not found")
			return
		}
		_ = runGuard.Cancel(sess.UserID, convID)
		w.WriteHeader(http.StatusNoContent)
	}
}

func streamHandler(provider AIProvider, store *Store, limiter *RateLimiter, slots chan struct{}, budget TokenBudget, maxOutput int, runGuard *ConversationRunGuard) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := sessionFromContext(r)
		if !verifyCSRF(r, sess) {
			writeJSONError(w, 403, "invalid CSRF token")
			return
		}
		reqID := getRequestID(r)
		if r.Method != http.MethodPost {
			writeJSONError(w, 405, "method not allowed")
			return
		}
		if !limiter.Allow(clientKey(r)) {
			writeJSONError(w, 429, "request rate limit exceeded")
			return
		}
		if !acquireAISlot(w, slots) {
			return
		}
		defer releaseAISlot(slots)
		req, convID, err := parseChatAndConversation(w, r, store, sess.UserID)
		if err != nil {
			writeJSONError(w, 400, err.Error())
			return
		}
		if !runGuard.TryAcquire(sess.UserID, convID) {
			writeJSONError(w, http.StatusConflict, "a response is already being generated for this conversation")
			return
		}
		defer runGuard.Release(sess.UserID, convID)
		if !budget.reserve(maxOutput) {
			writeJSONError(w, 429, "the app's free daily AI budget has been reached")
			return
		}
		if _, err := store.AddMessage(sess.UserID, convID, "user", req.Message, "", "", 0, "completed"); err != nil {
			budget.refundUnused(maxOutput, 0)
			writeJSONError(w, 500, "could not save message")
			return
		}
		store.RenameConversationFromFirstUserMessage(sess.UserID, convID, req.Message)
		pending, err := store.AddMessage(sess.UserID, convID, "assistant", "", provider.Name(), provider.Model(), 0, "pending")
		if err != nil {
			budget.refundUnused(maxOutput, 0)
			writeJSONError(w, 500, "could not prepare response")
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			budget.refundUnused(maxOutput, 0)
			_ = store.UpdateMessageLifecycle(sess.UserID, pending.ID, "failed", "Streaming is not supported by this client.", provider.Name(), provider.Model(), 0)
			http.Error(w, "streaming unsupported", 500)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.Header().Set("X-Request-ID", reqID)
		w.WriteHeader(200)
		flusher.Flush()
		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()
		runGuard.SetCancel(sess.UserID, convID, cancel)
		start := time.Now()
		var full strings.Builder
		usage, err := provider.Stream(ctx, req, func(piece string) error {
			full.WriteString(piece)
			if writeErr := writeSSE(w, flusher, map[string]any{"type": "token", "content": piece}); writeErr != nil {
				return fmt.Errorf("client disconnected: %w", context.Canceled)
			}
			return nil
		})
		actual := usage.TotalTokens
		if err != nil {
			// No reported usage on a failed/cancelled stream means no confirmed AI tokens.
			// Refund the entire reservation.
			actual = 0
		} else if actual <= 0 {
			// Successful responses without usage remain conservatively charged at maxOutput.
			actual = maxOutput
		}
		budget.refundUnused(maxOutput, actual)
		content := full.String()
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				message := content
				if strings.TrimSpace(message) == "" {
					message = "Generation cancelled."
				}
				if saveErr := store.UpdateMessageLifecycle(sess.UserID, pending.ID, "cancelled", message, provider.Name(), provider.Model(), usage.TotalTokens); saveErr != nil {
					log.Printf("request_id=%s update cancelled stream error: %v", reqID, saveErr)
				}
				_ = writeSSE(w, flusher, map[string]any{"type": "done", "conversation_id": convID, "provider": provider.Name(), "model": provider.Model(), "mode": req.Mode, "total_tokens": usage.TotalTokens, "status": "cancelled"})
				return
			}
			message := content
			if strings.TrimSpace(message) == "" {
				message = friendlyProviderError(err)
			}
			if saveErr := store.UpdateMessageLifecycle(sess.UserID, pending.ID, "failed", message, provider.Name(), provider.Model(), usage.TotalTokens); saveErr != nil {
				log.Printf("request_id=%s update failed stream error: %v", reqID, saveErr)
			}
			_ = writeSSE(w, flusher, map[string]any{"type": "error", "message": friendlyProviderError(err)})
			_ = writeSSE(w, flusher, map[string]any{"type": "done", "conversation_id": convID, "provider": provider.Name(), "model": provider.Model(), "mode": req.Mode, "total_tokens": usage.TotalTokens, "status": "failed"})
			return
		}
		if err := store.UpdateMessageLifecycle(sess.UserID, pending.ID, "completed", content, provider.Name(), provider.Model(), usage.TotalTokens); err != nil {
			log.Printf("request_id=%s save assistant stream error: %v", reqID, err)
		}
		_ = writeSSE(w, flusher, map[string]any{"type": "done", "conversation_id": convID, "provider": provider.Name(), "model": provider.Model(), "mode": req.Mode, "total_tokens": usage.TotalTokens, "status": "completed"})
		log.Printf("request_id=%s stream completed provider=%s model=%s conversation=%d user=%d latency=%s tokens=%d", reqID, provider.Name(), provider.Model(), convID, sess.UserID, time.Since(start), usage.TotalTokens)
	}
}
func parseChatAndConversation(w http.ResponseWriter, r *http.Request, store *Store, userID int64) (ChatRequest, int64, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxMessageBytes)
	if err := r.ParseForm(); err != nil {
		return ChatRequest{}, 0, errors.New("invalid request")
	}
	message := strings.TrimSpace(r.FormValue("message"))
	if message == "" {
		return ChatRequest{}, 0, errors.New("message is required")
	}
	if len([]byte(message)) > maxMessageBytes {
		return ChatRequest{}, 0, fmt.Errorf("message is too long (maximum %d bytes)", maxMessageBytes)
	}
	mode := strings.ToLower(strings.TrimSpace(r.FormValue("mode")))
	if mode == "" {
		mode = "fast"
	}
	if mode != "fast" && mode != "think" {
		return ChatRequest{}, 0, errors.New("invalid mode")
	}
	convID := int64(0)
	if raw := strings.TrimSpace(r.FormValue("conversation_id")); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			return ChatRequest{}, 0, errors.New("invalid conversation_id")
		}
		if !store.HasConversation(userID, id) {
			return ChatRequest{}, 0, errors.New("conversation not found")
		}
		convID = id
	} else {
		c, err := store.CreateConversation(userID, "New chat")
		if err != nil {
			return ChatRequest{}, 0, errors.New("could not create conversation")
		}
		convID = c.ID
	}
	return ChatRequest{Message: message, Mode: mode}, convID, nil
}

func usageHandler(provider AIProvider, budget TokenBudget) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSONError(w, 405, "method not allowed")
			return
		}
		used, limit := budget.status()
		writeJSON(w, 200, map[string]any{"provider": provider.Name(), "model": provider.Model(), "used_tokens": used, "daily_limit": limit, "remaining_tokens": max(0, limit-used), "paid_apis": false})
	}
}
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
func acquireAISlot(w http.ResponseWriter, slots chan struct{}) bool {
	select {
	case slots <- struct{}{}:
		return true
	default:
		writeJSONError(w, 429, "AI concurrency limit reached")
		return false
	}
}
func releaseAISlot(slots chan struct{}) { <-slots }
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}
func writeSSE(w http.ResponseWriter, f http.Flusher, payload map[string]any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
		return err
	}
	f.Flush()
	return nil
}
func writeAssistantFragment(w http.ResponseWriter, content, provider, model string, usage Usage) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprintf(w, `<article class="message assistant-message" data-provider="%s" data-model="%s"><div class="message-avatar">✦</div><div class="message-body"><div class="message-meta"><strong>Sys32.AI</strong><span class="stream-meta">%s · %s</span></div><div class="message-content">%s</div><div class="message-actions"><button type="button" class="action-link copy-message">Copy</button></div></div></article>`, html.EscapeString(provider), html.EscapeString(model), html.EscapeString(model), html.EscapeString(provider), renderMarkdownSafe(content))
}
func renderMarkdownSafe(input string) string {
	text := html.EscapeString(strings.ReplaceAll(input, "\r\n", "\n"))
	lines := strings.Split(text, "\n")
	var out strings.Builder
	inCode := false
	var code strings.Builder
	language := ""
	flush := func(s string) {
		if strings.TrimSpace(s) == "" {
			return
		}
		out.WriteString("<p>")
		out.WriteString(applyInlineMarkdown(s))
		out.WriteString("</p>")
	}
	for _, line := range lines {
		if strings.HasPrefix(line, "```") {
			if !inCode {
				inCode = true
				language = strings.TrimSpace(strings.TrimPrefix(line, "```"))
				code.Reset()
			} else {
				writeCodeBlock(&out, languageOrText(language), code.String())
				inCode = false
				language = ""
			}
			continue
		}
		if inCode {
			code.WriteString(line)
			code.WriteByte('\n')
			continue
		}
		switch {
		case strings.HasPrefix(line, "### "):
			out.WriteString("<h3>")
			out.WriteString(applyInlineMarkdown(strings.TrimPrefix(line, "### ")))
			out.WriteString("</h3>")
		case strings.HasPrefix(line, "## "):
			out.WriteString("<h2>")
			out.WriteString(applyInlineMarkdown(strings.TrimPrefix(line, "## ")))
			out.WriteString("</h2>")
		case strings.HasPrefix(line, "# "):
			out.WriteString("<h1>")
			out.WriteString(applyInlineMarkdown(strings.TrimPrefix(line, "# ")))
			out.WriteString("</h1>")
		case strings.HasPrefix(line, "- "):
			out.WriteString(`<div class="md-list-item">• `)
			out.WriteString(applyInlineMarkdown(strings.TrimPrefix(line, "- ")))
			out.WriteString(`</div>`)
		case strings.TrimSpace(line) == "":
		case strings.HasPrefix(strings.TrimSpace(line), "> "):
			out.WriteString("<blockquote>")
			out.WriteString(applyInlineMarkdown(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "> "))))
			out.WriteString("</blockquote>")
		default:
			flush(line)
		}
	}
	if inCode {
		writeCodeBlock(&out, languageOrText(language), code.String())
	}
	return out.String()
}
func writeCodeBlock(out *strings.Builder, language, code string) {
	out.WriteString(`<div class="code-wrap"><div class="code-header"><span>`)
	out.WriteString(html.EscapeString(language))
	out.WriteString(`</span><button type="button" class="copy-code">Copy</button></div><pre><code>`)
	out.WriteString(code)
	out.WriteString(`</code></pre></div>`)
}
func languageOrText(v string) string {
	if strings.TrimSpace(v) == "" {
		return "code"
	}
	return v
}
func applyInlineMarkdown(s string) string {
	s = replacePaired(s, "**")
	s = replacePaired(s, "__")
	return s
}
func replacePaired(s, marker string) string {
	var out strings.Builder
	for {
		start := strings.Index(s, marker)
		if start < 0 {
			out.WriteString(s)
			break
		}
		endRel := strings.Index(s[start+len(marker):], marker)
		if endRel < 0 {
			out.WriteString(s)
			break
		}
		end := start + len(marker) + endRel
		out.WriteString(s[:start])
		inner := s[start+len(marker) : end]
		out.WriteString("<strong>")
		out.WriteString(inner)
		out.WriteString("</strong>")
		s = s[end+len(marker):]
	}
	return out.String()
}
func friendlyProviderError(err error) string {
	var pe *ProviderError
	if errors.As(err, &pe) {
		if pe.Status == 429 {
			return "The AI provider rate limit was reached. Please try again shortly."
		}
		if pe.Status >= 500 {
			return "The AI provider is temporarily unavailable. Please try again."
		}
		return "The AI provider rejected the request."
	}
	if errors.Is(err, context.Canceled) {
		return "Generation cancelled."
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "The generation timed out."
	}
	return "The AI request failed. Please try again."
}
func writeProviderError(w http.ResponseWriter, err error) {
	status := 502
	var pe *ProviderError
	if errors.As(err, &pe) {
		if pe.Status == 429 {
			status = 429
		}
		if pe.Status >= 400 && pe.Status < 500 && pe.Status != 429 {
			status = 502
		}
	}
	writeJSONError(w, status, friendlyProviderError(err))
}
func pathID(path, prefix string) (int64, bool) {
	if !strings.HasPrefix(path, prefix) {
		return 0, false
	}
	raw := strings.Trim(strings.TrimPrefix(path, prefix), "/")
	if raw == "" || strings.Contains(raw, "/") {
		return 0, false
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	return id, err == nil && id > 0
}
