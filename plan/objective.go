package plan

import (
	"strings"

	"github.com/jerryschen31/minimal-agent/mdscan"
)

// ObjectiveSection returns an objective's Markdown ready to go under the plan's "## Objective" heading: every heading
// two levels deeper ("# Instructions" → "### Instructions"), everything else unchanged.
//
// Notes:
//   - [agent] The plan carries the objective so a run never needs the objective file (DECISIONS.md § 2026-11-19, the
//     plan file is auto mode's input). Two levels deeper keeps every copied heading at level 3 or more, which Cleanup
//     keeps inside the Objective section, so the copy reads back word for word.
//   - Lines inside code fences are never changed. A heading already at level 5 or 6 would go past 6 and stop being a
//     heading; it is written as "######" (level 6) instead.
//   - Trailing newlines are trimmed (the section body is stored without them).
func ObjectiveSection(objectiveMarkdown string) string {
	lines := strings.Split(strings.TrimRight(objectiveMarkdown, "\n"), "\n")
	inFence := false
	for i, l := range lines {
		if mdscan.IsCodeBlockFence(l) {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		level, text, ok := mdscan.ReadHeading(l, nil)
		if !ok {
			continue
		}
		lines[i] = strings.Repeat("#", min(level+2, 6)) + " " + text
	}
	return strings.Join(lines, "\n")
}
