package check

// Classification is the four-way answer to a trait question.
type Classification string

const (
	Compliant     Classification = "compliant"
	Violation     Classification = "violation"
	NotApplicable Classification = "not-applicable"
	Inconclusive  Classification = "inconclusive"
)

// Tier is how urgently a finding should be acted on.
type Tier string

const (
	// TierFix: violation probability >= the fix threshold (default 0.8).
	TierFix Tier = "fix"
	// TierReview: violation probability >= the review threshold (default
	// 0.5), or inconclusive is the most likely answer.
	TierReview Tier = "review"
)

// TraitInfo is the part of a trait repeated in output so a reader can act on
// a finding without opening the trait file.
type TraitInfo struct {
	Requirement string `json:"requirement"`
	Purpose     string `json:"purpose"`
	Fix         string `json:"fix,omitempty"`
	Path        string `json:"path,omitempty"`
}

// Location is where in the new file a finding most likely is.
type Location struct {
	Hunk      string  `json:"hunk,omitempty"`
	StartLine int     `json:"startLine"`
	EndLine   int     `json:"endLine"`
	// Probability is the model's probability for this location; 1 when the
	// file had a single hunk and no localisation question was asked.
	Probability float64 `json:"probability"`
}

// Finding is one trait's reportable answer for one file.
type Finding struct {
	Trait          string                     `json:"trait"`
	Tier           Tier                       `json:"tier"`
	Classification Classification             `json:"classification"`
	Probabilities  map[Classification]float64 `json:"probabilities"`
	// Confidence is the probability of Classification.
	Confidence     float64   `json:"confidence"`
	Location       *Location `json:"location,omitempty"`
	Interpretation string    `json:"interpretation"`
	Cached         bool      `json:"cached,omitempty"`
}

// FileResult is the findings for one file, most severe first.
type FileResult struct {
	Path     string    `json:"path"`
	Findings []Finding `json:"findings"`
}

// Skipped counts trait/file pairs that were not reported.
type Skipped struct {
	Gated         int `json:"gated"`         // filtered by appliesTo/when/status
	NotApplicable int `json:"notApplicable"` // model answered not-applicable
	Compliant     int `json:"compliant"`
	Cached        int `json:"cached"` // answered from the local cache
}

// FileError is a file that could not be checked (e.g. too large).
type FileError struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

// Usage aggregates provider token accounting.
type Usage struct {
	Requests     int `json:"requests"`
	InputTokens  int `json:"inputTokens"`
	CachedTokens int `json:"cachedTokens"`
}

// Report is the complete result of a check run.
type Report struct {
	Version  int                  `json:"version"` // always 1
	Summary  string               `json:"summary"`
	Provider string               `json:"provider"`
	Model    string               `json:"model"`
	Traits   map[string]TraitInfo `json:"traits"` // only traits referenced by findings
	Files    []FileResult         `json:"files"`  // only files with findings, sorted by path
	Skipped  Skipped              `json:"skipped"`
	Errors   []FileError          `json:"errors"`
	Usage    Usage                `json:"usage"`
}
