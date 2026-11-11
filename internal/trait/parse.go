package trait

import (
	"errors"
	"fmt"
	"math"
	"path"
	"regexp"
	"strings"

	"github.com/goccy/go-yaml"
)

const (
	// DefaultFix is the violation probability at or above which a finding is
	// reported as "fix" when the trait sets no threshold.
	DefaultFix = 0.8
	// DefaultReview is the violation probability at or above which a finding is
	// reported as "review" when the trait sets no threshold.
	DefaultReview = 0.5

	maxNameLen = 80
)

var namePattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

const frontmatterDelimiter = "---"

var (
	errNoFrontmatter       = errors.New("missing frontmatter: a trait must begin with a --- line")
	errUnclosedFrontmatter = errors.New("unclosed frontmatter: no closing --- line")
)

// stringList accepts either a single string or a list of strings.
// Adapted from madxmike/gallant (MIT).
type stringList []string

func (l *stringList) UnmarshalYAML(unmarshal func(any) error) error {
	var list []string
	if err := unmarshal(&list); err == nil {
		if len(list) > 0 {
			*l = list
		}
		return nil
	}
	var s string
	if err := unmarshal(&s); err == nil {
		if s != "" {
			*l = stringList{s}
		}
		return nil
	}
	return errors.New("expected a string or a list of strings")
}

// frontmatter is the decoded YAML before validation.
type frontmatter struct {
	Name          string     `yaml:"name"`
	Subject       string     `yaml:"subject"`
	Requirement   string     `yaml:"requirement"`
	Applicability string     `yaml:"applicability"`
	Purpose       string     `yaml:"purpose"`
	AppliesTo     stringList `yaml:"appliesTo"`
	When          stringList `yaml:"when"`
	Fix           string     `yaml:"fix"`
	Scope         string     `yaml:"scope"`
	Status        string     `yaml:"status"`
	Thresholds    Thresholds `yaml:"thresholds"`
}

// Parse parses and validates one trait file. path is the file's path, used
// in error messages, stored as Trait.Path, and checked against the trait's
// name (the name must equal the filename stem); an empty path skips the stem
// check. Unknown frontmatter keys are ignored. Scope and Status default to
// ScopeHunk and StatusActive.
func Parse(path string, data []byte) (Trait, error) {
	t, err := parse(path, data)
	if err != nil {
		if path == "" {
			return Trait{}, err
		}
		return Trait{}, fmt.Errorf("%s: %w", path, err)
	}
	return t, nil
}

func parse(file string, data []byte) (Trait, error) {
	src := strings.TrimPrefix(string(data), "\ufeff")
	fm, body, err := splitFrontmatter(src)
	if err != nil {
		return Trait{}, err
	}
	var raw frontmatter
	if err := decodeYAML(fm, &raw); err != nil {
		return Trait{}, fmt.Errorf("invalid frontmatter: %w", err)
	}
	t := Trait{
		Name:          raw.Name,
		Subject:       raw.Subject,
		Requirement:   raw.Requirement,
		Applicability: raw.Applicability,
		Purpose:       raw.Purpose,
		Context:       strings.TrimSpace(strings.ReplaceAll(body, "\r\n", "\n")),
		AppliesTo:     raw.AppliesTo,
		When:          raw.When,
		Fix:           raw.Fix,
		Scope:         Scope(raw.Scope),
		Status:        Status(raw.Status),
		Thresholds:    raw.Thresholds,
		Path:          file,
	}
	if err := t.validate(); err != nil {
		return Trait{}, err
	}
	if t.Scope == "" {
		t.Scope = ScopeHunk
	}
	if t.Status == "" {
		t.Status = StatusActive
	}
	return t, nil
}

// decodeYAML unmarshals src into v, turning a decoder panic into an error:
// go-yaml v1.19.2 panics on some malformed input (e.g. a tagged empty value
// decoded into a slice, found by FuzzParse).
func decodeYAML(src string, v any) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("yaml decoder failed: %v", r)
		}
	}()
	return yaml.Unmarshal([]byte(src), v)
}

// validate reports every rule the trait breaks, joined.
func (t Trait) validate() error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	switch {
	case t.Name == "":
		bad("name is required")
	case len(t.Name) > maxNameLen:
		bad("name %q is longer than %d characters", t.Name, maxNameLen)
	case !namePattern.MatchString(t.Name):
		bad("name %q is not kebab-case (lowercase letters, digits, single hyphens)", t.Name)
	case strings.HasPrefix(t.Name, "trait-"):
		bad("name %q must not start with \"trait-\"", t.Name)
	}
	if t.Path != "" && t.Name != "" {
		if stem := strings.TrimSuffix(path.Base(t.Path), path.Ext(t.Path)); stem != t.Name {
			bad("name %q does not match filename stem %q", t.Name, stem)
		}
	}
	for _, f := range []struct{ key, val string }{
		{"subject", t.Subject}, {"requirement", t.Requirement}, {"purpose", t.Purpose},
	} {
		if strings.TrimSpace(f.val) == "" {
			bad("%s is required", f.key)
		}
	}
	if t.Applicability != "" && strings.TrimSpace(t.Applicability) == "" {
		bad("applicability is blank")
	}
	switch t.Scope {
	case "", ScopeHunk, ScopeFile:
	default:
		bad("scope %q must be %q or %q", t.Scope, ScopeHunk, ScopeFile)
	}
	switch t.Status {
	case "", StatusActive, StatusCandidate:
	default:
		bad("status %q must be %q or %q", t.Status, StatusActive, StatusCandidate)
	}
	for _, p := range []struct {
		key string
		val float64
	}{{"fix", t.Thresholds.Fix}, {"review", t.Thresholds.Review}} {
		if math.IsNaN(p.val) || p.val < 0 || p.val > 1 {
			bad("thresholds.%s %v must be between 0 and 1", p.key, p.val)
		}
	}
	if fix, review := t.Thresholds.Fix, t.Thresholds.Review; fix != 0 && review != 0 && review > fix {
		bad("thresholds.review %v must not exceed thresholds.fix %v", review, fix)
	}
	for _, w := range t.When {
		if w == "" {
			bad("when: empty pattern")
			continue
		}
		if _, err := compileWhen(w); err != nil {
			bad("when %q: %v", w, err)
		}
	}
	for _, g := range t.AppliesTo {
		if err := validGlob(g); err != nil {
			bad("appliesTo %q: %v", g, err)
		}
	}
	return errors.Join(errs...)
}

// splitFrontmatter separates a frontmatter block from the Markdown body. The
// block must start on the first line, which is exactly "---", and ends at the
// next line that is exactly "---" (ignoring a trailing "\r"). Later "---"
// lines belong to the body.
// Adapted from madxmike/gallant (MIT).
func splitFrontmatter(src string) (fm, body string, err error) {
	line, next := readLine(src, 0)
	if strings.TrimRight(line, "\r") != frontmatterDelimiter {
		return "", src, errNoFrontmatter
	}
	start := next
	for pos := next; pos < len(src); pos = next {
		line, next = readLine(src, pos)
		if strings.TrimRight(line, "\r") == frontmatterDelimiter {
			return src[start:pos], src[next:], nil
		}
	}
	return "", src, errUnclosedFrontmatter
}

// readLine returns the line at pos without its "\n" and the start of the next
// line. Adapted from madxmike/gallant (MIT).
func readLine(s string, pos int) (line string, next int) {
	i := strings.IndexByte(s[pos:], '\n')
	if i < 0 {
		return s[pos:], len(s)
	}
	return s[pos : pos+i], pos + i + 1
}

// Effective returns the trait's fix and review thresholds, substituting
// DefaultFix and DefaultReview for unset (zero) values. Review is lowered to
// fix if a default would otherwise put it above fix.
func (t Trait) Effective() (fix, review float64) {
	fix, review = t.Thresholds.Fix, t.Thresholds.Review
	if fix == 0 {
		fix = DefaultFix
	}
	if review == 0 {
		review = DefaultReview
	}
	return fix, min(review, fix)
}
