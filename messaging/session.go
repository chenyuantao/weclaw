package messaging

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Session represents one active conversation session.
type Session struct {
	ID           string    `json:"id"`
	Nickname     string    `json:"nickname"`
	AgentName    string    `json:"agent_name"`
	ConvKey      string    `json:"conv_key"`
	AgentSession string    `json:"agent_session"`
	CreatedAt    time.Time `json:"created_at"`
	LastActiveAt time.Time `json:"last_active_at"`
}

// nicknamePool is a set of short, memorable nicknames for sessions.
var nicknamePool = []string{
	"ace", "bolt", "comet", "dash", "echo",
	"flux", "glide", "haze", "ivy", "jazz",
	"kite", "lark", "mist", "nova", "opus",
	"pulse", "quest", "reef", "spark", "tide",
	"ultra", "vibe", "warp", "xray", "zen",
}

// nicknameEmoji maps the first letter of a nickname to an emoji.
var nicknameEmoji = map[byte]string{
	'a': "\U0001F34E", // 🍎
	'b': "\U0001F34C", // 🍌
	'c': "\U0001F338", // 🌸
	'd': "\U0001F48E", // 💎
	'e': "\U000026A1", // ⚡
	'f': "\U0001F525", // 🔥
	'g': "\U0001F48E", // 💎 (replaced: 🌿 doesn't render well → use 💚)
	'h': "\U0001F33F", // 🌿
	'i': "\U0001F9CA", // 🧊
	'j': "\U0001F3B5", // 🎵
	'k': "\U0001F3C0", // 🏀 (kite → not a common emoji, use 🪁 or 🏀)
	'l': "\U0001F343", // 🍃
	'm': "\U0001F319", // 🌙
	'n': "\U00002B50", // ⭐
	'o': "\U0001F3B6", // 🎶
	'p': "\U0001F49C", // 💜
	'q': "\U0001F451", // 👑
	'r': "\U0001F308", // 🌈
	's': "\U00002728", // ✨
	't': "\U0001F30A", // 🌊
	'u': "\U0001F984", // 🦄
	'v': "\U0001F33A", // 🌺
	'w': "\U0001F300", // 🌀
	'x': "\U0001F52E", // 🔮
	'z': "\U000026A1", // ⚡
}

// NicknameDisplay returns the emoji-prefixed nickname, e.g. "🍎ace".
func NicknameDisplay(nickname string) string {
	if len(nickname) == 0 {
		return nickname
	}
	if emoji, ok := nicknameEmoji[nickname[0]]; ok {
		return emoji + nickname
	}
	return nickname
}

// SessionManager manages per-user sessions with persistence and TTL-based cleanup.
type SessionManager struct {
	mu       sync.RWMutex
	sessions map[string][]*Session // userID -> active sessions
	dataDir  string
	ttl      time.Duration
}

// NewSessionManager creates a new SessionManager.
func NewSessionManager(dataDir string) *SessionManager {
	return &SessionManager{
		sessions: make(map[string][]*Session),
		dataDir:  dataDir,
		ttl:      4 * time.Hour,
	}
}

// Create creates a new session for the given user and agent.
func (sm *SessionManager) Create(userID, agentName string) *Session {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	nickname := sm.pickNickname(userID)
	sess := &Session{
		ID:           uuid.New().String(),
		Nickname:     nickname,
		AgentName:    agentName,
		ConvKey:      userID + ":" + uuid.New().String(),
		CreatedAt:    time.Now(),
		LastActiveAt: time.Now(),
	}

	sm.sessions[userID] = append(sm.sessions[userID], sess)
	sm.saveLocked()
	log.Printf("[session] created session %s (@%s) for user %s on agent %s", sess.ID, nickname, userID, agentName)
	return sess
}

// Get returns a session by nickname for the given user.
func (sm *SessionManager) Get(userID, nickname string) *Session {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	for _, s := range sm.sessions[userID] {
		if s.Nickname == nickname {
			return s
		}
	}
	return nil
}

// List returns all active sessions for a user, sorted by lastActiveAt desc.
func (sm *SessionManager) List(userID string) []*Session {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	list := make([]*Session, len(sm.sessions[userID]))
	copy(list, sm.sessions[userID])
	sort.Slice(list, func(i, j int) bool {
		return list[i].LastActiveAt.After(list[j].LastActiveAt)
	})
	return list
}

// Touch updates the lastActiveAt timestamp for a session and saves.
func (sm *SessionManager) Touch(userID, nickname string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	for _, s := range sm.sessions[userID] {
		if s.Nickname == nickname {
			s.LastActiveAt = time.Now()
			sm.saveLocked()
			return
		}
	}
}

// UpdateAgentSession saves the agent-side session ID for a session.
func (sm *SessionManager) UpdateAgentSession(userID, nickname, agentSessionID string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	for _, s := range sm.sessions[userID] {
		if s.Nickname == nickname {
			s.AgentSession = agentSessionID
			sm.saveLocked()
			return
		}
	}
}

// Archive removes a specific session by nickname and writes it to the archive directory.
func (sm *SessionManager) Archive(userID, nickname string) bool {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	sessions := sm.sessions[userID]
	for i, s := range sessions {
		if s.Nickname == nickname {
			sm.archiveSession(userID, s)
			sm.sessions[userID] = append(sessions[:i], sessions[i+1:]...)
			sm.saveLocked()
			log.Printf("[session] archived session @%s for user %s", nickname, userID)
			return true
		}
	}
	return false
}

// ArchiveAll archives all sessions for a user. Returns the count archived.
func (sm *SessionManager) ArchiveAll(userID string) int {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	sessions := sm.sessions[userID]
	count := len(sessions)
	for _, s := range sessions {
		sm.archiveSession(userID, s)
	}
	delete(sm.sessions, userID)
	sm.saveLocked()
	if count > 0 {
		log.Printf("[session] archived all %d sessions for user %s", count, userID)
	}
	return count
}

// StartCleanup runs a background goroutine that archives expired sessions.
func (sm *SessionManager) StartCleanup(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sm.cleanupExpired()
		}
	}
}

// Load reads sessions from the persistent JSON file.
func (sm *SessionManager) Load() error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	path := filepath.Join(sm.dataDir, "sessions.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read sessions file: %w", err)
	}

	var sessions map[string][]*Session
	if err := json.Unmarshal(data, &sessions); err != nil {
		return fmt.Errorf("parse sessions file: %w", err)
	}

	sm.sessions = sessions
	total := 0
	for _, list := range sessions {
		total += len(list)
	}
	if total > 0 {
		log.Printf("[session] loaded %d sessions from %s", total, path)
	}
	return nil
}

// saveLocked persists sessions to disk. Must be called with sm.mu held.
func (sm *SessionManager) saveLocked() {
	path := filepath.Join(sm.dataDir, "sessions.json")
	data, err := json.MarshalIndent(sm.sessions, "", "  ")
	if err != nil {
		log.Printf("[session] failed to marshal sessions: %v", err)
		return
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		log.Printf("[session] failed to save sessions to %s: %v", path, err)
	}
}

// pickNickname selects an unused nickname for the given user. Must be called with sm.mu held.
func (sm *SessionManager) pickNickname(userID string) string {
	used := make(map[string]bool)
	for _, s := range sm.sessions[userID] {
		used[s.Nickname] = true
	}

	for _, name := range nicknamePool {
		if !used[name] {
			return name
		}
	}

	// Fallback: s1, s2, ...
	for i := 1; ; i++ {
		name := fmt.Sprintf("s%d", i)
		if !used[name] {
			return name
		}
	}
}

// archiveSession writes a session to the archive directory. Must be called with sm.mu held.
func (sm *SessionManager) archiveSession(userID string, s *Session) {
	archiveDir := filepath.Join(sm.dataDir, "sessions", "archive")
	if err := os.MkdirAll(archiveDir, 0o700); err != nil {
		log.Printf("[session] failed to create archive dir: %v", err)
		return
	}

	filename := fmt.Sprintf("%s_%s_%s.json", userID, s.Nickname, time.Now().Format("20060102T150405"))
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		log.Printf("[session] failed to marshal session for archive: %v", err)
		return
	}
	path := filepath.Join(archiveDir, filename)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		log.Printf("[session] failed to write archive %s: %v", path, err)
	}
}

// cleanupExpired archives sessions that have exceeded the TTL.
func (sm *SessionManager) cleanupExpired() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	now := time.Now()
	changed := false

	for userID, sessions := range sm.sessions {
		var kept []*Session
		for _, s := range sessions {
			if now.Sub(s.LastActiveAt) > sm.ttl {
				sm.archiveSession(userID, s)
				log.Printf("[session] expired session @%s for user %s (idle %s)", s.Nickname, userID, now.Sub(s.LastActiveAt).Round(time.Minute))
				changed = true
			} else {
				kept = append(kept, s)
			}
		}
		if len(kept) == 0 {
			delete(sm.sessions, userID)
		} else {
			sm.sessions[userID] = kept
		}
	}

	if changed {
		sm.saveLocked()
	}
}
