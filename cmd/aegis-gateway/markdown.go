package main

import (
	"strings"
)

// commonmarkToTelegramV2 converts CommonMark markdown to Telegram MarkdownV2 format.
//
// Key differences from CommonMark:
//   - Bold: **text** → *text*
//   - Italic: *text* or _text_ → _text_
//   - Strikethrough: ~~text~~ → ~text~
//   - Code blocks and inline code: same syntax, but content inside is NOT escaped
//   - All special characters outside formatting must be escaped with \
//
// Telegram MarkdownV2 special chars: _ * [ ] ( ) ~ ` > # + - = | { } . !
func commonmarkToTelegramV2(text string) string {
	var b strings.Builder
	b.Grow(len(text) + len(text)/4)

	runes := []rune(text)
	n := len(runes)
	i := 0

	for i < n {
		// Fenced code block: ``` ... ```
		if i+2 < n && runes[i] == '`' && runes[i+1] == '`' && runes[i+2] == '`' {
			// Find closing ```
			end := findTripleBacktick(runes, i+3)
			if end >= 0 {
				// Write the entire code block as-is (no escaping inside)
				b.WriteString(string(runes[i : end+3]))
				i = end + 3
				continue
			}
			// No closing — treat as literal, escape
			writeEscaped(&b, '`')
			writeEscaped(&b, '`')
			writeEscaped(&b, '`')
			i += 3
			continue
		}

		// Inline code: `...`
		if runes[i] == '`' {
			end := findChar(runes, '`', i+1)
			if end >= 0 {
				// Write inline code as-is (no escaping inside)
				b.WriteString(string(runes[i : end+1]))
				i = end + 1
				continue
			}
			writeEscaped(&b, '`')
			i++
			continue
		}

		// Bold: **text** → *text*
		if i+1 < n && runes[i] == '*' && runes[i+1] == '*' {
			end := findDoubleChar(runes, '*', i+2)
			if end >= 0 {
				b.WriteRune('*')
				writeEscapedSpan(&b, runes[i+2:end])
				b.WriteRune('*')
				i = end + 2
				continue
			}
		}

		// Italic with *: *text* (single asterisk, not double)
		if runes[i] == '*' && (i+1 >= n || runes[i+1] != '*') {
			end := findSingleNotDouble(runes, '*', i+1)
			if end >= 0 {
				b.WriteRune('_')
				writeEscapedSpan(&b, runes[i+1:end])
				b.WriteRune('_')
				i = end + 1
				continue
			}
		}

		// Italic with _: _text_
		if runes[i] == '_' && (i+1 < n && runes[i+1] != '_') {
			end := findSingleNotDouble(runes, '_', i+1)
			if end >= 0 {
				b.WriteRune('_')
				writeEscapedSpan(&b, runes[i+1:end])
				b.WriteRune('_')
				i = end + 1
				continue
			}
		}

		// Bold with __: __text__ → *text*
		if i+1 < n && runes[i] == '_' && runes[i+1] == '_' {
			end := findDoubleChar(runes, '_', i+2)
			if end >= 0 {
				b.WriteRune('*')
				writeEscapedSpan(&b, runes[i+2:end])
				b.WriteRune('*')
				i = end + 2
				continue
			}
		}

		// Strikethrough: ~~text~~ → ~text~
		if i+1 < n && runes[i] == '~' && runes[i+1] == '~' {
			end := findDoubleChar(runes, '~', i+2)
			if end >= 0 {
				b.WriteRune('~')
				writeEscapedSpan(&b, runes[i+2:end])
				b.WriteRune('~')
				i = end + 2
				continue
			}
		}

		// Links: [text](url) → [text](url) (same syntax, but escape inside text)
		if runes[i] == '[' {
			closeBracket := findChar(runes, ']', i+1)
			if closeBracket >= 0 && closeBracket+1 < n && runes[closeBracket+1] == '(' {
				closeParen := findChar(runes, ')', closeBracket+2)
				if closeParen >= 0 {
					b.WriteRune('[')
					writeEscapedSpan(&b, runes[i+1:closeBracket])
					b.WriteString("](")
					// URL: don't escape inside parens (Telegram handles it)
					b.WriteString(string(runes[closeBracket+2 : closeParen]))
					b.WriteRune(')')
					i = closeParen + 1
					continue
				}
			}
		}

		// Default: escape special characters
		if isTelegramSpecial(runes[i]) {
			writeEscaped(&b, runes[i])
		} else {
			b.WriteRune(runes[i])
		}
		i++
	}

	return b.String()
}

func isTelegramSpecial(r rune) bool {
	switch r {
	case '_', '*', '[', ']', '(', ')', '~', '`', '>', '#', '+', '-', '=', '|', '{', '}', '.', '!':
		return true
	}
	return false
}

func writeEscaped(b *strings.Builder, r rune) {
	b.WriteRune('\\')
	b.WriteRune(r)
}

// writeEscapedSpan writes runes with Telegram special char escaping.
func writeEscapedSpan(b *strings.Builder, runes []rune) {
	for _, r := range runes {
		if isTelegramSpecial(r) {
			writeEscaped(b, r)
		} else {
			b.WriteRune(r)
		}
	}
}

func findChar(runes []rune, ch rune, start int) int {
	for i := start; i < len(runes); i++ {
		if runes[i] == ch {
			return i
		}
	}
	return -1
}

func findDoubleChar(runes []rune, ch rune, start int) int {
	for i := start; i+1 < len(runes); i++ {
		if runes[i] == ch && runes[i+1] == ch {
			return i
		}
	}
	return -1
}

func findSingleNotDouble(runes []rune, ch rune, start int) int {
	for i := start; i < len(runes); i++ {
		if runes[i] == ch {
			if i+1 < len(runes) && runes[i+1] == ch {
				i++ // skip double
				continue
			}
			return i
		}
	}
	return -1
}

func findTripleBacktick(runes []rune, start int) int {
	for i := start; i+2 < len(runes); i++ {
		if runes[i] == '`' && runes[i+1] == '`' && runes[i+2] == '`' {
			return i
		}
	}
	return -1
}
