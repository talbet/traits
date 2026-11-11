// Package trait parses, validates and selects traits: Markdown files in
// .agents/traits/<name>.md whose frontmatter states a reusable preference.
//
// The core fields (name, subject, requirement, applicability, purpose, and the
// Markdown body as context) are compatible with crosshatch/traits. The other
// fields are optional extensions used by this tool.
package trait

// Scope is how much of a change a trait is judged against.
type Scope string

const (
	// ScopeHunk judges changed hunks with surrounding context (default).
	ScopeHunk Scope = "hunk"
	// ScopeFile judges the whole post-change file.
	ScopeFile Scope = "file"
)

// Status controls whether a trait runs in checks.
type Status string

const (
	// StatusActive traits run in checks (default).
	StatusActive Status = "active"
	// StatusCandidate traits run only in eval.
	StatusCandidate Status = "candidate"
)

// Thresholds are the violation probabilities at or above which a finding is
// reported as "fix" or "review". Zero means use the default.
type Thresholds struct {
	Fix    float64 `yaml:"fix"`
	Review float64 `yaml:"review"`
}

// Trait is one parsed trait.
type Trait struct {
	// Core fields, compatible with crosshatch/traits.
	Name          string `yaml:"name"`
	Subject       string `yaml:"subject"`
	Requirement   string `yaml:"requirement"`
	Applicability string `yaml:"applicability,omitempty"`
	Purpose       string `yaml:"purpose"`
	// Context is the Markdown body after the frontmatter.
	Context string `yaml:"-"`

	// Extensions.
	AppliesTo  []string   `yaml:"appliesTo,omitempty"` // path globs, "**" allowed
	When       []string   `yaml:"when,omitempty"`      // regexes over changed lines
	Fix        string     `yaml:"fix,omitempty"`
	Scope      Scope      `yaml:"scope,omitempty"`
	Status     Status     `yaml:"status,omitempty"`
	Thresholds Thresholds `yaml:"thresholds,omitempty"`

	// Path is the file the trait was read from, relative to the traits dir.
	Path string `yaml:"-"`
}
