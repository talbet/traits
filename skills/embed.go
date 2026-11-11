// Package skills embeds the agent skills that `traits skill install` writes.
package skills

import _ "embed"

// Traits is skills/traits/SKILL.md: how a coding agent should use traits.
//
//go:embed traits/SKILL.md
var Traits string
