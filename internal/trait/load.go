package trait

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"reflect"
	"slices"
	"strings"

	"github.com/goccy/go-yaml"
)

// ErrNoDir reports that the traits directory does not exist.
var ErrNoDir = errors.New("traits directory not found")

// Load reads every *.md trait under dir, recursively, skipping hidden files
// and directories (names starting with "."). Trait.Path is relative to dir.
// Traits are sorted by name. Errors from all files, including duplicate
// names, are joined; the traits that did parse are still returned. A missing
// dir returns an error wrapping ErrNoDir.
func Load(dir string) ([]Trait, error) {
	if info, err := os.Stat(dir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNoDir, dir)
		}
		return nil, err
	} else if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}

	fsys := os.DirFS(dir)
	var traits []Trait
	var errs []error
	walkErr := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p, err))
			return nil
		}
		if p != "." && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() || !strings.EqualFold(pathExt(d.Name()), ".md") {
			return nil
		}
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p, err))
			return nil
		}
		t, err := Parse(p, data)
		if err != nil {
			errs = append(errs, err)
			return nil
		}
		traits = append(traits, t)
		return nil
	})
	if walkErr != nil {
		errs = append(errs, walkErr)
	}

	slices.SortStableFunc(traits, func(a, b Trait) int { return strings.Compare(a.Name, b.Name) })
	out := traits[:0]
	for i, t := range traits {
		if i > 0 && traits[i-1].Name == t.Name {
			errs = append(errs, fmt.Errorf("%s: duplicate trait name %q (also in %s)", t.Path, t.Name, traits[i-1].Path))
			continue
		}
		out = append(out, t)
	}
	return out, errors.Join(errs...)
}

func pathExt(name string) string {
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		return name[i:]
	}
	return ""
}

// serialized fixes the frontmatter key order: core fields, then extensions.
type serialized struct {
	Name          string                `yaml:"name"`
	Subject       string                `yaml:"subject"`
	Requirement   string                `yaml:"requirement"`
	Applicability string                `yaml:"applicability,omitempty"`
	Purpose       string                `yaml:"purpose"`
	AppliesTo     []string              `yaml:"appliesTo,omitempty"`
	When          []string              `yaml:"when,omitempty"`
	Fix           string                `yaml:"fix,omitempty"`
	Scope         Scope                 `yaml:"scope,omitempty"`
	Status        Status                `yaml:"status,omitempty"`
	Thresholds    *serializedThresholds `yaml:"thresholds,omitempty"`
}

type serializedThresholds struct {
	Fix    float64 `yaml:"fix,omitempty"`
	Review float64 `yaml:"review,omitempty"`
}

// Serialize renders t as a trait file: frontmatter with the core fields first
// (name, subject, requirement, applicability, purpose), then the extensions,
// omitting empty values and the defaults scope: hunk and status: active,
// followed by the context as the body.
func Serialize(t Trait) []byte {
	s := serialized{
		Name:          t.Name,
		Subject:       t.Subject,
		Requirement:   t.Requirement,
		Applicability: t.Applicability,
		Purpose:       t.Purpose,
		AppliesTo:     t.AppliesTo,
		When:          t.When,
		Fix:           t.Fix,
	}
	if t.Scope != ScopeHunk {
		s.Scope = t.Scope
	}
	if t.Status != StatusActive {
		s.Status = t.Status
	}
	if t.Thresholds != (Thresholds{}) {
		th := serializedThresholds(t.Thresholds)
		s.Thresholds = &th
	}
	// Literal blocks read best, but go-yaml drops some trailing whitespace in
	// them, so fall back to JSON (valid YAML, every string quoted) when the block would not decode back
	// to the same values.
	fm, err := yaml.MarshalWithOptions(s, yaml.UseLiteralStyleIfMultiline(true))
	var back serialized
	if err != nil || decodeYAML(string(fm), &back) != nil || !reflect.DeepEqual(back, s) {
		fm, err = yaml.MarshalWithOptions(s, yaml.JSON())
	}
	if err != nil {
		// Marshalling plain strings, string slices and floats cannot fail.
		panic(fmt.Sprintf("trait: serialize %s: %v", t.Name, err))
	}
	var b bytes.Buffer
	b.WriteString("---\n")
	b.Write(fm)
	if len(fm) > 0 && fm[len(fm)-1] != '\n' {
		b.WriteByte('\n')
	}
	b.WriteString("---\n")
	if t.Context != "" {
		b.WriteString(t.Context)
		b.WriteByte('\n')
	}
	return b.Bytes()
}
