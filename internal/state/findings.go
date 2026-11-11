package state

import (
	"bufio"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// FindingsFile is the findings log's name inside the cache directory.
const FindingsFile = "findings.jsonl"

// Sources of a finding record.
const (
	SourceCheck = "check"
	SourceHook  = "hook"
	SourceCI    = "ci"
)

// Outcomes recorded for a finding.
const (
	OutcomeFixed = "fixed"
	OutcomeKept  = "kept"
)

// Record is one line of the findings log: either a finding (Trait set) or an
// outcome for an earlier finding (Outcome set, identified by ID).
type Record struct {
	TS             time.Time          `json:"ts"`
	ID             string             `json:"id"`
	Path           string             `json:"path,omitempty"`
	Trait          string             `json:"trait,omitempty"`
	Tier           string             `json:"tier,omitempty"`
	Classification string             `json:"classification,omitempty"`
	Probabilities  map[string]float64 `json:"probabilities,omitempty"`
	StartLine      int                `json:"startLine,omitempty"`
	EndLine        int                `json:"endLine,omitempty"`
	Provider       string             `json:"provider,omitempty"`
	Model          string             `json:"model,omitempty"`
	Source         string             `json:"source,omitempty"`
	Session        string             `json:"session,omitempty"`
	Outcome        string             `json:"outcome,omitempty"`
}

// FindingID identifies a finding by its path, trait and the text at its
// location, so the same problem keeps its id when lines move.
func FindingID(path, trait, locationText string) string {
	sum := sha256.Sum256([]byte(path + "\x00" + trait + "\x00" + locationText))
	return hex.EncodeToString(sum[:8])
}

// FindingsPath returns the findings log path for a traits directory.
func FindingsPath(traitsDir string) string {
	return filepath.Join(traitsDir, ".cache", FindingsFile)
}

// AppendFindings appends records to the log at path, creating the cache
// directory (with a .gitignore covering it) as needed. Each record is one
// write to a file opened with O_APPEND, so concurrent appenders do not
// interleave within a line.
func AppendFindings(path string, recs []Record) error {
	if len(recs) == 0 {
		return nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	ignore := filepath.Join(dir, ".gitignore")
	if _, err := os.Stat(ignore); errors.Is(err, fs.ErrNotExist) {
		_ = os.WriteFile(ignore, []byte("*\n"), 0o644)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range recs {
		if r.TS.IsZero() {
			r.TS = time.Now().UTC()
		}
		line, err := json.Marshal(r)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if _, err := f.Write(append(line, '\n')); err != nil {
			errs = append(errs, err)
			break
		}
	}
	return errors.Join(append(errs, f.Close())...)
}

// ReadFindings reads every well-formed record in the log at path, in file
// order. Malformed lines (e.g. a torn final line) are skipped. A missing log
// is empty, not an error.
func ReadFindings(path string) ([]Record, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var recs []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	for sc.Scan() {
		var r Record
		if json.Unmarshal(sc.Bytes(), &r) != nil || r.ID == "" {
			continue
		}
		recs = append(recs, r)
	}
	return recs, sc.Err()
}

// Finding is a finding's current state after folding the log.
type Finding struct {
	Record
	// FirstSeen is when the finding was first logged; Record.TS is the last.
	FirstSeen time.Time `json:"firstSeen"`
	// Times is how many times it was logged.
	Times int `json:"times"`
}

// Fold folds records into one Finding per id, in order of first appearance.
// The finding's fields come from its latest record; its Outcome is the
// latest outcome logged after that record, so a finding flagged again after
// being fixed is open again.
func Fold(recs []Record) []Finding {
	byID := map[string]int{}
	var out []Finding
	for _, r := range recs {
		i, seen := byID[r.ID]
		if r.Outcome != "" {
			if seen {
				out[i].Outcome = r.Outcome
			}
			continue
		}
		if r.Trait == "" {
			continue
		}
		if !seen {
			byID[r.ID] = len(out)
			out = append(out, Finding{Record: r, FirstSeen: r.TS, Times: 1})
			continue
		}
		times, first := out[i].Times+1, out[i].FirstSeen
		out[i] = Finding{Record: r, FirstSeen: first, Times: times}
	}
	return out
}

// TraitSummary counts one trait's findings by outcome.
type TraitSummary struct {
	Trait   string `json:"trait"`
	Flagged int    `json:"flagged"`
	Fixed   int    `json:"fixed"`
	Kept    int    `json:"kept"`
	Unknown int    `json:"unknown"`
	// KeepRate is kept / (fixed + kept); 0 when nothing has an outcome. A
	// high keep rate suggests a noisy trait worth refining.
	KeepRate float64 `json:"keepRate"`
}

// Summarize returns per-trait counts for folded findings, most flagged first.
func Summarize(fs []Finding) []TraitSummary {
	byTrait := map[string]*TraitSummary{}
	for _, f := range fs {
		s := byTrait[f.Trait]
		if s == nil {
			s = &TraitSummary{Trait: f.Trait}
			byTrait[f.Trait] = s
		}
		s.Flagged++
		switch f.Outcome {
		case OutcomeFixed:
			s.Fixed++
		case OutcomeKept:
			s.Kept++
		default:
			s.Unknown++
		}
	}
	out := make([]TraitSummary, 0, len(byTrait))
	for _, s := range byTrait {
		if d := s.Fixed + s.Kept; d > 0 {
			s.KeepRate = float64(s.Kept) / float64(d)
		}
		out = append(out, *s)
	}
	slices.SortFunc(out, func(a, b TraitSummary) int {
		return cmp.Or(cmp.Compare(b.Flagged, a.Flagged), cmp.Compare(a.Trait, b.Trait))
	})
	return out
}
