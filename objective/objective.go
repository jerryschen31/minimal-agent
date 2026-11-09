package objective

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Section headings: matched leniently by Parse (any level, any case, singular or plural, trailing ":"), written
// exactly like this by Markdown.
const (
	HeadingObjective    = "Objective"
	HeadingInstructions = "Instructions"
	HeadingContext      = "Context"
)

// Objective is what an objective file asks for: the user's own words, in three parts.
type Objective struct {
	ObjText      string // "# Objective": what to achieve; the whole file when it has no headings
	Instructions string // "# Instructions", then every other top-level section with its heading kept; "" if none
	Context      string // "# Context": background; "" if none
}

// Markdown renders o in the standard form: "# Objective", then "# Instructions" and "# Context" when not empty.
//
// Notes:
//   - Used for Hash and for the planner's prompt, never to rewrite the user's file (Go never rewrites it).
//   - Sections are separated by one blank line and the text ends in exactly one "\n"; Parse reads it back unchanged.
//   - "# Objective" is always written, even with no text.
func (o Objective) Markdown() string {
	blocks := []string{sectionBlock(HeadingObjective, o.ObjText)}
	if o.Instructions != "" {
		blocks = append(blocks, sectionBlock(HeadingInstructions, o.Instructions))
	}
	if o.Context != "" {
		blocks = append(blocks, sectionBlock(HeadingContext, o.Context))
	}
	return strings.Join(blocks, "\n\n") + "\n"
}

// sectionBlock returns "# <heading>" followed by its body, or just the heading when the body is empty.
//
// Notes: none.
func sectionBlock(heading, body string) string {
	if body == "" {
		return "# " + heading
	}
	return "# " + heading + "\n" + body
}

// Hash returns "sha256:<hex>" of o.Markdown().
//
// Notes:
//   - Taken over the standard form, so an edit that only changes heading spelling or level, or blank lines around a
//     section, is not a change; any change to the words is.
//   - A plan file stores it as objective_hash; a different hash means the plan is stale (plan.Plan.IsStale).
func (o Objective) Hash() string {
	sum := sha256.Sum256([]byte(o.Markdown()))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ChangedSections returns the headings ("# Instructions", …) of the sections whose text differs between old and o, in
// standard order; nil when none changed.
//
// Notes:
//   - For the plan summary's "Objective changed since plan 3: # Instructions changed" line. The planner gets both full
//     texts and works out the details itself, so no line diff is needed here.
func (o Objective) ChangedSections(old Objective) []string {
	var changed []string
	for _, s := range []struct {
		heading  string
		now, was string
	}{
		{HeadingObjective, o.ObjText, old.ObjText},
		{HeadingInstructions, o.Instructions, old.Instructions},
		{HeadingContext, o.Context, old.Context},
	} {
		if s.now != s.was {
			changed = append(changed, "# "+s.heading)
		}
	}
	return changed
}
