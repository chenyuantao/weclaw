package messaging

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fastclaw-ai/weclaw/agent"
	"github.com/fastclaw-ai/weclaw/ilink"
)

// AgentFactory creates an agent by config name. Returns nil if the name is unknown.
type AgentFactory func(ctx context.Context, name string) agent.Agent

// CommandInfo holds display information for a configured agent command.
type CommandInfo struct {
	Type  string // e.g. "acp", "cli", "http"
	Model string // e.g. "sonnet", "gpt-4o-mini"
	Cwd   string // working directory
}

// CommandsFunc returns the current command key → agent info mapping from config.
type CommandsFunc func() map[string]CommandInfo

// DefaultKeyFunc returns the current default command key from config.
type DefaultKeyFunc func() string

// ConfigHashFunc returns a fingerprint of the agent config for the given key.
// Used to detect config changes and recreate agents without restart.
type ConfigHashFunc func(name string) string

// Handler processes incoming WeChat messages and dispatches replies.
type Handler struct {
	mu            sync.RWMutex
	agents        map[string]agent.Agent // name -> running agent
	agentHash     map[string]string      // name -> config fingerprint at creation time
	factory       AgentFactory
	commands      CommandsFunc
	defaultKey    DefaultKeyFunc
	configHash    ConfigHashFunc
	sessions      *SessionManager
	contextTokens sync.Map // map[userID]contextToken
}

// NewHandler creates a new message handler.
func NewHandler(factory AgentFactory, commands CommandsFunc, defaultKey DefaultKeyFunc, configHash ConfigHashFunc, sessions *SessionManager) *Handler {
	return &Handler{
		agents:     make(map[string]agent.Agent),
		agentHash:  make(map[string]string),
		factory:    factory,
		commands:   commands,
		defaultKey: defaultKey,
		configHash: configHash,
		sessions:   sessions,
	}
}

// PreWarmAgent registers a pre-started agent for the given key.
func (h *Handler) PreWarmAgent(name string, ag agent.Agent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.agents[name] = ag
	if h.configHash != nil {
		h.agentHash[name] = h.configHash(name)
	}
	log.Printf("[handler] agent pre-warmed: %s (%s)", name, ag.Info())
}

// getAgent returns a running agent by name, or starts it on demand via factory.
func (h *Handler) getAgent(ctx context.Context, name string) (agent.Agent, error) {
	if h.factory == nil {
		return nil, fmt.Errorf("agent %q not found and no factory configured", name)
	}

	// Check current config fingerprint
	currentHash := ""
	if h.configHash != nil {
		currentHash = h.configHash(name)
	}

	// Fast path: cached and config unchanged
	h.mu.RLock()
	ag, cached := h.agents[name]
	storedHash := h.agentHash[name]
	h.mu.RUnlock()

	if cached && currentHash == storedHash {
		return ag, nil
	}

	// Slow path: create or recreate
	h.mu.Lock()
	defer h.mu.Unlock()

	// Double-check after acquiring write lock
	if ag, ok := h.agents[name]; ok {
		if currentHash == h.agentHash[name] {
			return ag, nil
		}
		// Config changed — stop old agent and recreate
		log.Printf("[handler] config changed for %q, recreating agent", name)
		if stopper, ok := ag.(interface{ Stop() }); ok {
			stopper.Stop()
		}
		delete(h.agents, name)
		delete(h.agentHash, name)
	}

	log.Printf("[handler] starting agent %q on demand...", name)
	ag = h.factory(ctx, name)
	if ag == nil {
		return nil, fmt.Errorf("agent %q not available", name)
	}

	h.agents[name] = ag
	h.agentHash[name] = currentHash
	log.Printf("[handler] agent started on demand: %s (%s)", name, ag.Info())
	return ag, nil
}

// parseCommand checks if text starts with "/command " and returns (command, rest).
// If no command prefix, returns ("", originalText).
func parseCommand(text string) (string, string) {
	if !strings.HasPrefix(text, "/") {
		return "", text
	}

	rest := text[1:] // strip leading "/"
	idx := strings.IndexByte(rest, ' ')
	if idx <= 0 {
		return rest, ""
	}

	return rest[:idx], strings.TrimSpace(rest[idx+1:])
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

	// --- Ref-msg session routing (highest priority) ---
	// When user replies to a bot message with header like [🍎admin-0],
	// automatically route to that session.
	if refNickname := extractRefNickname(msg); refNickname != "" {
		// /clear on ref-msg → archive that session
		if trimmed == "/clear" {
			log.Printf("[handler] ref-msg /clear for session @%s", refNickname)
			reply := h.handleClear(userID, "/clear @"+refNickname)
			h.sendReply(ctx, client, userID, reply, msg.ContextToken, clientID)
			return
		}

		sess := h.sessions.Get(userID, refNickname)
		if sess != nil {
			log.Printf("[handler] ref-msg routing to session @%s", refNickname)
			h.handleSessionChat(ctx, client, userID, sess, trimmed, msg.ContextToken, clientID)
			return
		}

		// Session not found — extract agent name from nickname and create a new session
		if agentName := ExtractAgentName(refNickname); agentName != "" {
			log.Printf("[handler] ref-msg session @%s expired, creating new %s session", refNickname, agentName)
			if agentName == "admin" {
				h.handleNewSessionAs(ctx, client, userID, "admin:"+h.defaultKey(), "admin", trimmed, msg.ContextToken, clientID)
			} else {
				h.handleNewSession(ctx, client, userID, agentName, trimmed, msg.ContextToken, clientID)
			}
			return
		}
		log.Printf("[handler] ref-msg nickname @%s not found, falling through", refNickname)
	}

	// --- Built-in commands ---

	// /status
	if trimmed == "/status" {
		reply := h.buildStatus()
		h.sendReply(ctx, client, userID, reply, msg.ContextToken, clientID)
		return
	}

	// /help
	if trimmed == "/help" {
		reply := h.buildHelpText()
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

	// /admin [message] — create session on default agent with cwd=~/.weclaw/ for config editing
	if trimmed == "/admin" || strings.HasPrefix(trimmed, "/admin ") {
		message := strings.TrimSpace(strings.TrimPrefix(trimmed, "/admin"))
		defaultKey := h.defaultKey()
		if defaultKey == "" {
			h.sendReply(ctx, client, userID, "No default agent configured.", msg.ContextToken, clientID)
			return
		}
		h.handleNewSessionAs(ctx, client, userID, "admin:"+defaultKey, "admin", message, msg.ContextToken, clientID)
		return
	}

	// /new [message] — create session on default agent
	if trimmed == "/new" || strings.HasPrefix(trimmed, "/new ") {
		message := strings.TrimSpace(strings.TrimPrefix(trimmed, "/new"))
		defaultKey := h.defaultKey()
		if defaultKey == "" {
			h.sendReply(ctx, client, userID, "No default agent configured.", msg.ContextToken, clientID)
			return
		}
		h.handleNewSession(ctx, client, userID, defaultKey, message, msg.ContextToken, clientID)
		return
	}

	// /key [message] — create session on a config-driven agent command
	if strings.HasPrefix(trimmed, "/") {
		cmdName, message := parseCommand(trimmed)
		if cmdName != "" {
			cmds := h.commands()
			if _, ok := cmds[cmdName]; ok {
				h.handleNewSession(ctx, client, userID, cmdName, message, msg.ContextToken, clientID)
				return
			}
		}
	}

	// Anything else — show help + active sessions
	reply := h.buildSessionHelp(userID, "")
	h.sendReply(ctx, client, userID, reply, msg.ContextToken, clientID)
}

// handleNewSession creates a new session and optionally chats.
func (h *Handler) handleNewSession(ctx context.Context, client *ilink.Client, userID, keyName, message, contextToken, clientID string) {
	h.handleNewSessionAs(ctx, client, userID, keyName, keyName, message, contextToken, clientID)
}

// handleNewSessionAs creates a new session using agentKey for agent lookup and displayName for session naming.
func (h *Handler) handleNewSessionAs(ctx context.Context, client *ilink.Client, userID, agentKey, displayName, message, contextToken, clientID string) {
	ag, err := h.getAgent(ctx, agentKey)
	if err != nil {
		h.sendReply(ctx, client, userID, fmt.Sprintf("Agent %q is not available: %v", agentKey, err), contextToken, clientID)
		return
	}

	sess := h.sessions.CreateWithKey(userID, displayName, agentKey)

	if message == "" {
		reply := fmt.Sprintf("[%s]\nNew session created.\nUse @%s <message> to chat.", NicknameDisplay(sess.Nickname), sess.Nickname)
		h.sendReply(ctx, client, userID, reply, contextToken, clientID)
		return
	}

	// Send immediate ack with agent info
	info := ag.Info()
	ack := fmt.Sprintf("[%s]\nNew session created (model: %s, cwd: %s)", NicknameDisplay(sess.Nickname), info.Model, info.Cwd)
	h.dispatchChat(ctx, client, userID, ag, sess, ack, message, contextToken, clientID)
}

// handleSessionChat routes a message to an existing session.
func (h *Handler) handleSessionChat(ctx context.Context, client *ilink.Client, userID string, sess *Session, message, contextToken, clientID string) {
	ag, err := h.getAgent(ctx, sess.AgentLookupKey())
	if err != nil {
		h.sendReply(ctx, client, userID, fmt.Sprintf("Agent %q for session @%s is not available: %v", sess.AgentLookupKey(), sess.Nickname, err), contextToken, clientID)
		return
	}

	ack := fmt.Sprintf("[%s]\nReceived, thinking...", NicknameDisplay(sess.Nickname))
	h.dispatchChat(ctx, client, userID, ag, sess, ack, message, contextToken, clientID)
}

// dispatchChat sends an ack, typing indicator, chats with the agent, and sends the reply.
func (h *Handler) dispatchChat(ctx context.Context, client *ilink.Client, userID string, ag agent.Agent, sess *Session, ack, message, contextToken, clientID string) {
	// Send ack and typing indicator concurrently (don't block the chat)
	go h.sendReply(ctx, client, userID, ack, contextToken, clientID)
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

	h.sendAndExtractImages(ctx, client, userID, reply, contextToken, NewClientID())
}

// handleClear archives sessions based on the /clear command.
func (h *Handler) handleClear(userID, trimmed string) string {
	arg := strings.TrimSpace(strings.TrimPrefix(trimmed, "/clear"))

	var result string

	// /clear @nickname — archive a specific session
	if strings.HasPrefix(arg, "@") {
		nickname := arg[1:]
		if h.sessions.Archive(userID, nickname) {
			result = fmt.Sprintf("Session @%s archived.", nickname)
		} else {
			return fmt.Sprintf("Session @%s not found.", nickname)
		}
	} else {
		// /clear — archive all sessions
		count := h.sessions.ArchiveAll(userID)
		if count == 0 {
			return "No active sessions."
		}
		result = fmt.Sprintf("Archived %d session(s).", count)
	}

	// Append remaining active sessions
	if extra := h.formatSessionList(userID); extra != "" {
		result += "\n\n" + extra
	}

	return result
}

// chatWithSession sends a message to an agent using the session's ConvKey.
func (h *Handler) chatWithSession(ctx context.Context, ag agent.Agent, sess *Session, message string) (string, error) {
	// Restore agent-side session if persisted (e.g. after weclaw restart).
	// RestoreSession is idempotent — agents skip if already known.
	if sess.AgentSession != "" {
		ag.RestoreSession(sess.ConvKey, sess.AgentSession)
	}

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
	var sb strings.Builder

	defaultKey := h.defaultKey()
	sb.WriteString(fmt.Sprintf("default: /%s", defaultKey))
	if defaultKey == "" {
		sb.WriteString(" (none)")
	}

	cmds := h.commands()
	if len(cmds) > 0 {
		keys := make([]string, 0, len(cmds))
		for k := range cmds {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		sb.WriteString("\n\nagents:")
		for _, key := range keys {
			info := cmds[key]
			marker := ""
			if key == defaultKey {
				marker = " *"
			}
			sb.WriteString(fmt.Sprintf("\n  /%s (%s)%s", key, info.Type, marker))
			if info.Model != "" {
				sb.WriteString(fmt.Sprintf("\n    model: %s", info.Model))
			}
			if info.Cwd != "" {
				sb.WriteString(fmt.Sprintf("\n    cwd: %s", info.Cwd))
			}
		}
	} else {
		sb.WriteString("\n\nNo agents configured.")
	}

	return sb.String()
}

// buildSessionHelp returns help text with active session list.
func (h *Handler) buildSessionHelp(userID, prefix string) string {
	var sb strings.Builder
	if prefix != "" {
		sb.WriteString(prefix)
		sb.WriteString("\n\n")
	}

	sb.WriteString(h.buildHelpText())

	if extra := h.formatSessionList(userID); extra != "" {
		sb.WriteString("\n\n")
		sb.WriteString(extra)
	}

	return sb.String()
}

// formatSessionList returns a formatted list of active sessions for the user,
// sorted by last active time descending. Returns empty string if no sessions.
func (h *Handler) formatSessionList(userID string) string {
	sessions := h.sessions.List(userID)
	if len(sessions) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("Active sessions:")
	for _, s := range sessions {
		sb.WriteString(fmt.Sprintf("\n  @%s (%s) %s", s.Nickname, s.AgentName, s.LastActiveAt.Format("01/02 15:04")))
	}
	return sb.String()
}

func (h *Handler) buildHelpText() string {
	var sb strings.Builder
	sb.WriteString("Commands:\n")
	sb.WriteString("@nickname message - Chat with a session\n")
	sb.WriteString("/new [message] - Create session on default agent\n")
	sb.WriteString("/admin [message] - Edit config via agent\n")
	sb.WriteString("/clear - Archive all sessions\n")
	sb.WriteString("/clear @nickname - Archive a specific session\n")
	sb.WriteString("/status - Show agent info\n")
	sb.WriteString("/help - Show this help")

	// Dynamic agent commands from config
	cmds := h.commands()
	if len(cmds) > 0 {
		sb.WriteString("\n----\nAgents:\n")
		keys := make([]string, 0, len(cmds))
		for k := range cmds {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, key := range keys {
			info := cmds[key]
			detail := info.Type
			if info.Model != "" {
				detail += ", " + info.Model
			}
			if info.Cwd != "" {
				detail += ", " + info.Cwd
			}
			sb.WriteString(fmt.Sprintf("/%s [message] (%s)\n", key, detail))
		}
	}

	return sb.String()
}

func extractText(msg ilink.WeixinMessage) string {
	for _, item := range msg.ItemList {
		if item.Type == ilink.ItemTypeText && item.TextItem != nil {
			return item.TextItem.Text
		}
	}
	return ""
}

// extractRefNickname extracts a session nickname from the ref_msg header.
// It looks for a pattern like [🍎admin-0] at the start of the referenced message text,
// strips the leading emoji, and returns the nickname (e.g. "admin-0").
func extractRefNickname(msg ilink.WeixinMessage) string {
	for _, item := range msg.ItemList {
		if item.RefMsg == nil || item.RefMsg.MessageItem == nil {
			continue
		}
		if item.RefMsg.MessageItem.Type != ilink.ItemTypeText || item.RefMsg.MessageItem.TextItem == nil {
			continue
		}
		refText := item.RefMsg.MessageItem.TextItem.Text
		nickname := ParseNicknameFromHeader(refText)
		if nickname != "" {
			return nickname
		}
	}
	return ""
}

