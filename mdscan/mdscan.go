package mdscan

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// MaxBytes is the largest Markdown input Lines accepts.
const MaxBytes = 1024 * 1024 // 1 MB

// Line is one line of the input with its 1-based line number, so a caller's fixes and errors can point at it.
type Line struct {
	Num  int
	Text string
}

// Lines checks data and splits it into numbered lines.
//
// Notes:
//   - what names the file in error messages ("objective file", "plan file").
//   - Errors: larger than MaxBytes, not UTF-8, or empty (only whitespace).
//   - A UTF-8 byte-order mark (added by some Windows editors) is dropped; "\r\n" and a lone "\r" (old Mac line ending)
//     are read as "\n". None of these is reported: they don't change what the file says.
func Lines(data []byte, what string) ([]Line, error) {
	if len(data) > MaxBytes {
		return nil, fmt.Errorf("%s is %d bytes; the limit is %d", what, len(data), MaxBytes)
	}
	if !utf8.Valid(data) {
		return nil, errors.New(what + " is not UTF-8 text")
	}
	text := strings.TrimPrefix(string(data), "\uFEFF")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	if strings.TrimSpace(text) == "" {
		return nil, errors.New(what + " is empty")
	}
	var lines []Line
	for i, s := range strings.Split(text, "\n") {
		lines = append(lines, Line{Num: i + 1, Text: s})
	}
	return lines, nil
}

// FirstTextLine returns the line number of the first non-blank line, or 0 when all are blank.
//
// Notes: none.
func FirstTextLine(lines []Line) int {
	for _, l := range lines {
		if strings.TrimSpace(l.Text) != "" {
			return l.Num
		}
	}
	return 0
}

// LastTextLine returns the line number of the last non-blank line, or 0 when all are blank.
//
// Notes: none.
func LastTextLine(lines []Line) int {
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i].Text) != "" {
			return lines[i].Num
		}
	}
	return 0
}

// IsCodeBlockFence reports whether s opens or closes a code fence: "```" or "~~~" after optional indentation.
//
// Notes:
//   - [agent] simplified: any fence line toggles, so a ~~~ line inside a ``` block would end it. Markdown pairs a fence
//     with the same character; fine to tighten later if a real file needs it.
func IsCodeBlockFence(s string) bool {
	t := strings.TrimSpace(s)
	return strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~")
}

// ReadHeading reports whether s is a heading and returns its level (1–6) and text.
//
// Notes:
//   - "# Title"; up to three leading spaces are allowed (four or more is an indented code block in Markdown).
//   - A closing "#" run after a space is dropped ("## Tasks ##" → "Tasks"); "### C#" keeps its "#".
//   - No space after the "#"s counts only when known(text) is true: the caller passes its own check (a known section
//     name, a task ID), so "#Objective" is a heading and "#hashtag" stays text. A nil known accepts none.
//   - Seven or more "#", or a heading with no text ("# ") or only "#"s as text ("# # #"), is not a heading.
func ReadHeading(s string, known func(text string) bool) (level int, text string, ok bool) {
	trimmed := strings.TrimLeft(s, " ")
	if len(s)-len(trimmed) > 3 {
		return 0, "", false
	}
	for level < len(trimmed) && trimmed[level] == '#' {
		level++
	}
	if level == 0 || level > 6 {
		return 0, "", false
	}
	rest := trimmed[level:]

	if rest != "" && (rest[0] == ' ' || rest[0] == '\t') {
		text = StripClosingHashes(rest)
		if strings.Trim(text, "#") == "" {
			return 0, "", false // [agent] no text, or only "#"s ("# # #"), which "## #" could not be read back as
		}
		return level, text, true
	}

	text = StripClosingHashes(rest)
	if known != nil && known(text) {
		return level, text, true
	}
	return 0, "", false
}

// StripClosingHashes trims s and drops closing runs of "#" after a space ("Tasks ##" → "Tasks", "x # #" → "x"); text
// that is only "#"s becomes "".
//
// Notes:
//   - "C#" keeps its "#" (no space before it).
//   - Readers use it for every heading and for any title that is written back as a heading: a "#" run this would drop
//     must already be gone, or a round trip changes the title.
func StripClosingHashes(s string) string {
	s = strings.TrimSpace(s)
	for {
		t := strings.TrimRight(s, "#")
		switch {
		case t == "":
			return ""
		case t != s && (strings.HasSuffix(t, " ") || strings.HasSuffix(t, "\t")):
			s = strings.TrimSpace(t) // [agent] and again: "x # #" → "x #" → "x"
		default:
			return s
		}
	}
}
