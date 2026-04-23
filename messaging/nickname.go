package messaging

import (
	"strconv"
	"strings"
)

// nicknameEmojis is an ordered palette. Sessions under the same agent rotate
// through this list via their numeric suffix so their emojis don't collide.
var nicknameEmojis = []string{
	"\U0001F34E", // 🍎
	"\U0001F34C", // 🍌
	"\U0001F338", // 🌸
	"\U0001F48E", // 💎
	"\U000026A1", // ⚡
	"\U0001F525", // 🔥
	"\U0001F33F", // 🌿
	"\U0001F9CA", // 🧊
	"\U0001F3B5", // 🎵
	"\U0001F3C0", // 🏀
	"\U0001F343", // 🍃
	"\U0001F319", // 🌙
	"\U00002B50", // ⭐
	"\U0001F3B6", // 🎶
	"\U0001F49C", // 💜
	"\U0001F451", // 👑
	"\U0001F308", // 🌈
	"\U00002728", // ✨
	"\U0001F30A", // 🌊
	"\U0001F984", // 🦄
	"\U0001F33A", // 🌺
	"\U0001F300", // 🌀
	"\U0001F52E", // 🔮
}

// --- Formatting (nickname → display string) ---

// NicknameDisplay returns the emoji-prefixed nickname, e.g. "🍎admin-0".
// The emoji is picked by offsetting the agent's first letter with the
// session's numeric suffix, so sessions under the same agent differ.
func NicknameDisplay(nickname string) string {
	if len(nickname) == 0 {
		return nickname
	}
	idx := letterIndex(nickname[0])
	if dash := strings.LastIndexByte(nickname, '-'); dash > 0 {
		if n, err := strconv.Atoi(nickname[dash+1:]); err == nil && n >= 0 {
			idx += n
		}
	}
	return nicknameEmojis[idx%len(nicknameEmojis)] + nickname
}

func letterIndex(b byte) int {
	switch {
	case b >= 'a' && b <= 'z':
		return int(b - 'a')
	case b >= 'A' && b <= 'Z':
		return int(b - 'A')
	}
	return 0
}

// --- Parsing (display string → nickname) ---

// ParseNicknameFromHeader parses a nickname from text starting with [emoji+nickname].
// e.g. "[🍎admin-0]\nSome content..." → "admin-0"
// This is the inverse of the header format produced by NicknameDisplay.
func ParseNicknameFromHeader(text string) string {
	if !strings.HasPrefix(text, "[") {
		return ""
	}
	end := strings.IndexByte(text, ']')
	if end < 0 {
		return ""
	}
	inside := text[1:end] // e.g. "🍎admin-0"

	// Strip leading non-letter runes (the emoji prefix added by NicknameDisplay)
	for i, r := range inside {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			return inside[i:]
		}
	}
	return ""
}

// ExtractAgentName returns the agent/display name from a session nickname.
// Nickname format is "{agentName}-{index}", e.g. "admin-0" → "admin".
func ExtractAgentName(nickname string) string {
	idx := strings.LastIndexByte(nickname, '-')
	if idx <= 0 {
		return ""
	}
	return nickname[:idx]
}
