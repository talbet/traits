package check

import (
	"context"
	"testing"

	"github.com/talbet/traits/internal/provider"
	"github.com/talbet/traits/internal/trait"
)

func TestEvaluateIgnoresStatusAndReportsGating(t *testing.T) {
	m := &fakeModel{answers: map[string]map[string]float64{
		"cand": {"violation": 0.85, "compliant": 0.15},
	}}
	cand := mkTrait("cand")
	cand.Status = trait.StatusCandidate
	tsOnly := mkTrait("ts-only")
	tsOnly.AppliesTo = []string{"*.ts"}
	opts := Options{Model: m, ProviderName: "fake", ModelName: "m", Traits: []trait.Trait{cand, tsOnly}, Cache: &memCache{m: map[string]provider.Answer{}}}
	got, usage, fileErr, err := Evaluate(context.Background(), opts, file("a.go", hunk(1, "x"), hunk(40, "y")))
	if err != nil || fileErr != nil {
		t.Fatal(err, fileErr)
	}
	if len(got) != 2 || got[0].Gated || got[0].Probabilities[Violation] != 0.85 || got[0].Cached {
		t.Errorf("cand = %+v", got)
	}
	if !got[1].Gated || got[1].Probabilities != nil {
		t.Errorf("ts-only = %+v", got[1])
	}
	// One pass-1 request; no localisation even though the file has two hunks.
	if usage.Requests != 1 || len(m.requests) != 1 {
		t.Errorf("usage %+v, %d requests", usage, len(m.requests))
	}
	got, usage, _, _ = Evaluate(context.Background(), opts, file("a.go", hunk(1, "x"), hunk(40, "y")))
	if !got[0].Cached || usage.Requests != 0 {
		t.Errorf("second run: %+v usage %+v", got[0], usage)
	}
}

func TestEvaluateFileError(t *testing.T) {
	m := &fakeModel{}
	ft := mkTrait("whole")
	ft.Scope = trait.ScopeFile
	got, _, fileErr, err := Evaluate(context.Background(), Options{Model: m, Traits: []trait.Trait{ft}}, file("a.go", hunk(1, "x")))
	if err != nil || fileErr == nil || got[0].Gated || got[0].Probabilities != nil {
		t.Errorf("answers %+v fileErr %v err %v", got, fileErr, err)
	}
}
