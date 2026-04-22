package messaging

import "strings"

// nicknameEmoji maps the first letter of a nickname to an emoji.
var nicknameEmoji = map[byte]string{
	'a': "\U0001F34E", // 🍎
	'b': "\U0001F34C", // 🍌
	'c': "\U0001F338", // 🌸
	'd': "\U0001F48E", // 💎
	'e': "\U000026A1", // ⚡
	'f': "\U0001F525", // 🔥
	'g': "\U0001F48E", // 💎
	'h': "\U0001F33F", // 🌿
	'i': "\U0001F9CA", // 🧊
	'j': "\U0001F3B5", // 🎵
	'k': "\U0001F3C0", // 🏀
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

// --- Formatting (nickname → display string) ---

// NicknameDisplay returns the emoji-prefixed nickname, e.g. "🍎admin-0".
func NicknameDisplay(nickname string) string {
	if len(nickname) == 0 {
		return nickname
	}
	if emoji, ok := nicknameEmoji[nickname[0]]; ok {
		return emoji + nickname
	}
	return nickname
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
