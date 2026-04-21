package messaging

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/fastclaw-ai/weclaw/agent"
	"github.com/fastclaw-ai/weclaw/ilink"
)

// AgentFactory creates an agent by config name. Returns nil if the name is unknown.
type AgentFactory func(ctx context.Context, name string) agent.Agent

// SaveDefaultFunc persists the default agent name to config file.
type SaveDefaultFunc func(name string) error

// AgentMeta holds static config info about an agent (for /status display).
type AgentMeta struct {
	Name    string
	Type    string // "acp", "cli", "http"
	Command string // binary path or endpoint
	Model   string
}

// Handler processes incoming WeChat messages and dispatches replies.
type Handler struct {
	mu            sync.RWMutex
	defaultName   string
	agents        map[string]agent.Agent // name -> running agent
	agentMetas    []AgentMeta            // all configured agents (for /status)
	factory       AgentFactory
	saveDefault   SaveDefaultFunc
	sessions      *SessionManager
	contextTokens sync.Map // map[userID]contextToken
}

// NewHandler creates a new message handler.
func NewHandler(factory AgentFactory, saveDefault SaveDefaultFunc, sessions *SessionManager) *Handler {
	return &Handler{
		agents:      make(map[string]agent.Agent),
		factory:     factory,
		saveDefault: saveDefault,
		sessions:    sessions,
	}
}

// SetAgentMetas sets the list of all configured agents (for /status).
func (h *Handler) SetAgentMetas(metas []AgentMeta) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.agentMetas = metas
}

// SetDefaultAgent sets the default agent (already started).
func (h *Handler) SetDefaultAgent(name string, ag agent.Agent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.defaultName = name
	h.agents[name] = ag
	log.Printf("[handler] default agent ready: %s (%s)", name, ag.Info())
}

// getAgent returns a running agent by name, or starts it on demand via factory.
func (h *Handler) getAgent(ctx context.Context, name string) (agent.Agent, error) {
	// Fast path: already running
	h.mu.RLock()
	ag, ok := h.agents[name]
	h.mu.RUnlock()
	if ok {
		return ag, nil
	}

	// Slow path: create on demand
	if h.factory == nil {
		return nil, fmt.Errorf("agent %q not found and no factory configured", name)
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	// Double-check after acquiring write lock
	if ag, ok := h.agents[name]; ok {
		return ag, nil
	}

	log.Printf("[handler] starting agent %q on demand...", name)
	ag = h.factory(ctx, name)
	if ag == nil {
		return nil, fmt.Errorf("agent %q not available", name)
	}

	h.agents[name] = ag
	log.Printf("[handler] agent started on demand: %s (%s)", name, ag.Info())
	return ag, nil
}

// getDefaultName returns the current default agent name.
func (h *Handler) getDefaultName() string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.defaultName
}

// agentAliases maps short aliases to agent config names.
var agentAliases = map[string]string{
	"cc":  "claude",
	"cx":  "codex",
	"oc":  "openclaw",
	"cs":  "cursor",
	"km":  "kimi",
	"gm":  "gemini",
	"ocd": "opencode",
}

// resolveAlias returns the full agent name for an alias, or the original name if no alias matches.
func resolveAlias(name string) string {
	if full, ok := agentAliases[name]; ok {
		return full
	}
	return name
}

// parseCommand checks if text starts with "/command " and returns (command, rest).
// Aliases are resolved for known agent names.
// If no command prefix, returns ("", originalText).
func parseCommand(text string) (string, string) {
	if !strings.HasPrefix(text, "/") {
		return "", text
	}

	rest := text[1:] // strip leading "/"
	idx := strings.IndexByte(rest, ' ')
	if idx <= 0 {
		return resolveAlias(rest), ""
	}

	name := resolveAlias(rest[:idx])
	return name, strings.TrimSpace(rest[idx+1:])
}

// parseAtNickname checks if text starts with "@nickname " and returns (nickname, message).
// Returns ("", originalText) if no @nickname prefix.
func parseAtNickname(text string) (string, string) {
	if !strings.HasPrefix(text, "@") {
		return "", text
	}

	rest := text[1:] // strip leading "@"
	idx := strings.IndexByte(rest, ' ')
	if idx <= 0 {
		return rest, ""
	}

	return rest[:idx], strings.TrimSpace(rest[idx+1:])
}

// HandleMessage processes a single incoming message.
func (h *Handler) HandleMessage(ctx context.Context, client *ilink.Client, msg ilink.WeixinMessage) {
	// Only process user messages that are finished
	if msg.MessageType != ilink.MessageTypeUser {
		return
	}
	if msg.MessageState != ilink.MessageStateFinish {
		return
	}

	// Extract text from item list
	text := extractText(msg)
	if text == "" {
		log.Printf("[handler] received non-text message from %s, skipping", msg.FromUserID)
		return
	}

	log.Printf("[handler] received from %s: %q", msg.FromUserID, truncate(text, 80))

	// Store context token for this user
	h.contextTokens.Store(msg.FromUserID, msg.ContextToken)

	clientID := NewClientID()
	userID := msg.FromUserID
	trimmed := strings.TrimSpace(text)

	// --- Built-in commands ---

	// /status
	if trimmed == "/status" {
		reply := h.buildStatus()
		h.sendReply(ctx, client, userID, reply, msg.ContextToken, clientID)
		return
	}

	// /help
	if trimmed == "/help" {
		reply := buildHelpText()
		h.sendReply(ctx, client, userID, reply, msg.ContextToken, clientID)
		return
	}

	// /clear [@nickname] — archive sessions
	if trimmed == "/clear" || strings.HasPrefix(trimmed, "/clear ") {
		reply := h.handleClear(userID, trimmed)
		h.sendReply(ctx, client, userID, reply, msg.ContextToken, clientID)
		return
	}

	// @nickname message — route to existing session
	if strings.HasPrefix(trimmed, "@") {
		nickname, message := parseAtNickname(trimmed)
		if message == "" {
			h.sendReply(ctx, client, userID, fmt.Sprintf("Usage: @%s <message>", nickname), msg.ContextToken, clientID)
			return
		}
		sess := h.sessions.Get(userID, nickname)
		if sess == nil {
			h.sendReply(ctx, client, userID, fmt.Sprintf("Session @%s not found. Use /new to create one.", nickname), msg.ContextToken, clientID)
			return
		}
		h.handleSessionChat(ctx, client, userID, sess, message, msg.ContextToken, clientID)
		return
	}

	// /new [message] — create session on default agent
	if trimmed == "/new" || strings.HasPrefix(trimmed, "/new ") {
		message := strings.TrimSpace(strings.TrimPrefix(trimmed, "/new"))
		defaultName := h.getDefaultName()
		if defaultName == "" {
			h.sendReply(ctx, client, userID, "No default agent configured.", msg.ContextToken, clientID)
			return
		}
		h.handleNewSession(ctx, client, userID, defaultName, message, msg.ContextToken, clientID)
		return
	}

	// /cc [message], /cx [message], etc. — create session on specific agent
	if strings.HasPrefix(trimmed, "/") {
		cmdName, message := parseCommand(trimmed)
		if cmdName != "" {
			h.handleNewSession(ctx, client, userID, cmdName, message, msg.ContextToken, clientID)
			return
		}
	}

	// Anything else — show help + active sessions
	reply := h.buildSessionHelp(userID, "")
	h.sendReply(ctx, client, userID, reply, msg.ContextToken, clientID)
}

// handleNewSession creates a new session and optionally chats.
func (h *Handler) handleNewSession(ctx context.Context, client *ilink.Client, userID, agentName, message, contextToken, clientID string) {
	ag, err := h.getAgent(ctx, agentName)
	if err != nil {
		h.sendReply(ctx, client, userID, fmt.Sprintf("Agent %q is not available: %v", agentName, err), contextToken, clientID)
		return
	}

	sess := h.sessions.Create(userID, agentName)

	if message == "" {
		reply := fmt.Sprintf("New %s session created: @%s\nUse @%s <message> to chat.", agentName, sess.Nickname, sess.Nickname)
		h.sendReply(ctx, client, userID, reply, contextToken, clientID)
		return
	}

	// Chat immediately
	go func() {
		if typingErr := SendTypingState(ctx, client, userID, contextToken); typingErr != nil {
			log.Printf("[handler] failed to send typing state: %v", typingErr)
		}
	}()

	reply, chatErr := h.chatWithSession(ctx, ag, sess, message)
	if chatErr != nil {
		reply = fmt.Sprintf("Error: %v", chatErr)
	} else {
		reply = fmt.Sprintf("[%s]\n%s", NicknameDisplay(sess.Nickname), reply)
	}

	h.sendAndExtractImages(ctx, client, userID, reply, contextToken, clientID)
}

// handleSessionChat routes a message to an existing session.
func (h *Handler) handleSessionChat(ctx context.Context, client *ilink.Client, userID string, sess *Session, message, contextToken, clientID string) {
	ag, err := h.getAgent(ctx, sess.AgentName)
	if err != nil {
		h.sendReply(ctx, client, userID, fmt.Sprintf("Agent %q for session @%s is not available: %v", sess.AgentName, sess.Nickname, err), contextToken, clientID)
		return
	}

	go func() {
		if typingErr := SendTypingState(ctx, client, userID, contextToken); typingErr != nil {
			log.Printf("[handler] failed to send typing state: %v", typingErr)
		}
	}()

	reply, chatErr := h.chatWithSession(ctx, ag, sess, message)
	if chatErr != nil {
		reply = fmt.Sprintf("Error: %v", chatErr)
	} else {
		reply = fmt.Sprintf("[%s]\n%s", NicknameDisplay(sess.Nickname), reply)
	}

	h.sendAndExtractImages(ctx, client, userID, reply, contextToken, clientID)
}

// handleClear archives sessions based on the /clear command.
func (h *Handler) handleClear(userID, trimmed string) string {
	arg := strings.TrimSpace(strings.TrimPrefix(trimmed, "/clear"))

	// /clear @nickname — archive a specific session
	if strings.HasPrefix(arg, "@") {
		nickname := arg[1:]
		if h.sessions.Archive(userID, nickname) {
			return fmt.Sprintf("Session @%s archived.", nickname)
		}
		return fmt.Sprintf("Session @%s not found.", nickname)
	}

	// /clear — archive all sessions
	count := h.sessions.ArchiveAll(userID)
	if count == 0 {
		return "No active sessions."
	}
	return fmt.Sprintf("Archived %d session(s).", count)
}

// chatWithSession sends a message to an agent using the session's ConvKey.
func (h *Handler) chatWithSession(ctx context.Context, ag agent.Agent, sess *Session, message string) (string, error) {
	info := ag.Info()
	log.Printf("[handler] dispatching to agent (%s) for session @%s (convKey=%s)", info, sess.Nickname, sess.ConvKey)

	start := time.Now()
	reply, err := ag.Chat(ctx, sess.ConvKey, message)
	elapsed := time.Since(start)

	if err != nil {
		log.Printf("[handler] agent error (%s, elapsed=%s): %v", info, elapsed, err)
		return "", err
	}

	log.Printf("[handler] agent replied (%s, session=@%s, elapsed=%s): %q", info, sess.Nickname, elapsed, truncate(reply, 100))

	// Update session metadata
	h.sessions.Touch(sess.ConvKey[:strings.Index(sess.ConvKey, ":")], sess.Nickname)
	if agentSessID := ag.SessionID(sess.ConvKey); agentSessID != "" && agentSessID != sess.AgentSession {
		h.sessions.UpdateAgentSession(sess.ConvKey[:strings.Index(sess.ConvKey, ":")], sess.Nickname, agentSessID)
	}

	return reply, nil
}

// sendReply sends a text reply to the user.
func (h *Handler) sendReply(ctx context.Context, client *ilink.Client, userID, reply, contextToken, clientID string) {
	if err := SendTextReply(ctx, client, userID, reply, contextToken, clientID); err != nil {
		log.Printf("[handler] failed to send reply to %s: %v", userID, err)
	}
}

// sendAndExtractImages sends a text reply and any extracted images.
func (h *Handler) sendAndExtractImages(ctx context.Context, client *ilink.Client, userID, reply, contextToken, clientID string) {
	imageURLs := ExtractImageURLs(reply)

	if err := SendTextReply(ctx, client, userID, reply, contextToken, clientID); err != nil {
		log.Printf("[handler] failed to send reply to %s: %v", userID, err)
	}

	for _, imgURL := range imageURLs {
		if err := SendMediaFromURL(ctx, client, userID, imgURL, contextToken); err != nil {
			log.Printf("[handler] failed to send image to %s: %v", userID, err)
		}
	}
}

// buildStatus returns a short status string showing the current default agent.
func (h *Handler) buildStatus() string {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if h.defaultName == "" {
		return "agent: none (echo mode)"
	}

	ag, ok := h.agents[h.defaultName]
	if !ok {
		return fmt.Sprintf("agent: %s (not started)", h.defaultName)
	}

	info := ag.Info()
	return fmt.Sprintf("agent: %s\ntype: %s\nmodel: %s", h.defaultName, info.Type, info.Model)
}

// buildSessionHelp returns help text with active session list.
func (h *Handler) buildSessionHelp(userID, prefix string) string {
	var sb strings.Builder
	if prefix != "" {
		sb.WriteString(prefix)
		sb.WriteString("\n\n")
	}

	sb.WriteString(buildHelpText())

	sessions := h.sessions.List(userID)
	if len(sessions) > 0 {
		sb.WriteString("\n\nActive sessions:\n")
		for _, s := range sessions {
			sb.WriteString(fmt.Sprintf("  @%s (%s)\n", s.Nickname, s.AgentName))
		}
	}

	return sb.String()
}

func buildHelpText() string {
	return `Commands:
@nickname message - Chat with a session
/new [message] - Create session on default agent
/cc [message] - Create session on claude
/cx [message] - Create session on codex
/cs [message] - Create session on cursor
/km [message] - Create session on kimi
/gm [message] - Create session on gemini
/oc [message] - Create session on openclaw
/clear - Archive all sessions
/clear @nickname - Archive a specific session
/status - Show agent info
/help - Show this help`
}

func extractText(msg ilink.WeixinMessage) string {
	for _, item := range msg.ItemList {
		if item.Type == ilink.ItemTypeText && item.TextItem != nil {
			return item.TextItem.Text
		}
	}
	return ""
}
