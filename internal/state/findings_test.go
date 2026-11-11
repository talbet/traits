package state

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFindingsFoldAndSummary(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".cache", FindingsFile)
	t0 := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	a := FindingID("main.go", "no-print", "fmt.Println(1)")
	b := FindingID("main.go", "no-print", "fmt.Println(2)")
	c := FindingID("x.go", "errors", "")
	d := FindingID("y.go", "errors", "")
	if a == b || len(a) != 16 {
		t.Fatalf("ids %q %q", a, b)
	}
	recs := []Record{
		{TS: at(0), ID: a, Path: "main.go", Trait: "no-print", Tier: "fix", Source: SourceHook, Session: "s1"},
		{TS: at(1), ID: b, Path: "main.go", Trait: "no-print", Tier: "review", Source: SourceHook, Session: "s1"},
		{TS: at(2), ID: c, Path: "x.go", Trait: "errors", Tier: "fix", Source: SourceCheck},
		{TS: at(3), ID: d, Path: "y.go", Trait: "errors", Tier: "fix", Source: SourceCheck},
		{TS: at(4), ID: a, Outcome: OutcomeFixed},
		{TS: at(5), ID: b, Outcome: OutcomeKept},
		{TS: at(6), ID: c, Outcome: OutcomeFixed},
		{TS: at(7), ID: c, Path: "x.go", Trait: "errors", Tier: "review", Source: SourceCheck}, // flagged again: open
		{TS: at(8), ID: "ffff", Outcome: OutcomeKept},                                          // outcome without a finding
	}
	if err := AppendFindings(path, recs); err != nil {
		t.Fatal(err)
	}
	// A torn line is skipped.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"ts":"2026-`)
	f.Close()
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), ".gitignore")); err != nil {
		t.Errorf("no .gitignore: %v", err)
	}

	got, err := ReadFindings(path)
	if err != nil || len(got) != len(recs) {
		t.Fatalf("read %d records, err %v", len(got), err)
	}
	folded := Fold(got)
	if len(folded) != 4 {
		t.Fatalf("folded = %+v", folded)
	}
	want := map[string]string{a: OutcomeFixed, b: OutcomeKept, c: "", d: ""}
	for _, fd := range folded {
		if fd.Outcome != want[fd.ID] {
			t.Errorf("%s %s: outcome %q, want %q", fd.Path, fd.ID, fd.Outcome, want[fd.ID])
		}
		if fd.ID == c && (fd.Times != 2 || fd.Tier != "review" || !fd.FirstSeen.Equal(at(2))) {
			t.Errorf("refolded finding = %+v", fd)
		}
	}
	sums := Summarize(folded)
	if len(sums) != 2 {
		t.Fatalf("summary = %+v", sums)
	}
	// Ties on flagged count sort by name.
	if s := sums[0]; s.Trait != "errors" || s.Flagged != 2 || s.Unknown != 2 || s.KeepRate != 0 {
		t.Errorf("errors = %+v", s)
	}
	if s := sums[1]; s.Trait != "no-print" || s.Flagged != 2 || s.Fixed != 1 || s.Kept != 1 || s.KeepRate != 0.5 {
		t.Errorf("no-print = %+v", s)
	}
}

func TestReadFindingsMissing(t *testing.T) {
	recs, err := ReadFindings(filepath.Join(t.TempDir(), "nope.jsonl"))
	if err != nil || recs != nil {
		t.Errorf("%v %v", recs, err)
	}
}

func TestAppendFindingsConcurrent(t *testing.T) {
	path := filepath.Join(t.TempDir(), FindingsFile)
	long := strings.Repeat("x", 3000) // records larger than a pipe buffer page
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Go(func() {
			for i := range 25 {
				r := Record{ID: fmt.Sprintf("%d-%d", w, i), Path: long, Trait: "t"}
				if err := AppendFindings(path, []Record{r}); err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()
	recs, err := ReadFindings(path)
	if err != nil || len(recs) != 200 {
		t.Fatalf("read %d records, err %v", len(recs), err)
	}
	if n := len(Fold(recs)); n != 200 {
		t.Errorf("folded %d", n)
	}
}
