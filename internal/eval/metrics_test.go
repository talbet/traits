package eval

import (
	"math"
	"testing"

	"github.com/talbet/traits/internal/check"
)

func out(pos bool, pv float64) Outcome {
	return Outcome{Positive: pos, Probabilities: map[check.Classification]float64{check.Violation: pv, check.Compliant: 1 - pv}}
}

func eq(t *testing.T, name string, got *float64, want float64) {
	t.Helper()
	if got == nil || math.Abs(*got-want) > 1e-9 {
		t.Errorf("%s = %v, want %v", name, deref(got), want)
	}
}

func isNil(t *testing.T, name string, got *float64) {
	t.Helper()
	if got != nil {
		t.Errorf("%s = %v, want nil", name, *got)
	}
}

func deref(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

func TestCountAndScore(t *testing.T) {
	outs := []Outcome{
		out(true, 0.9),                 // TP at both
		out(true, 0.6),                 // TP at review, FN at fix
		out(false, 0.85),               // FP at both
		out(false, 0.1),                // TN
		{Positive: true, Gated: true},  // FN: gated counts as not-applicable
		{Positive: false, Gated: true}, // TN
		{Positive: true, Error: true},  // ignored
	}
	c := Count(outs, 0.8)
	if c != (Confusion{TP: 1, FP: 1, FN: 2, TN: 2}) {
		t.Fatalf("fix confusion = %+v", c)
	}
	r := Score(c)
	eq(t, "precision", r.Precision, 0.5)
	eq(t, "recall", r.Recall, 1.0/3)
	eq(t, "f1", r.F1, 0.4)
	eq(t, "false alarm", r.FalseAlarmRate, 1.0/3)
	if c := Count(outs, 0.5); c != (Confusion{TP: 2, FP: 1, FN: 1, TN: 2}) {
		t.Errorf("review confusion = %+v", c)
	}
	// The threshold is inclusive.
	if c := Count([]Outcome{out(true, 0.8)}, 0.8); c.TP != 1 {
		t.Errorf("inclusive threshold: %+v", c)
	}
}

func TestScoreZeroDenominators(t *testing.T) {
	r := Score(Confusion{})
	isNil(t, "precision", r.Precision)
	isNil(t, "recall", r.Recall)
	isNil(t, "f1", r.F1)
	isNil(t, "false alarm", r.FalseAlarmRate)

	// Only clean cases, all correct: no positives at all.
	r = Score(Confusion{TN: 3})
	isNil(t, "precision", r.Precision)
	isNil(t, "recall", r.Recall)
	isNil(t, "f1", r.F1)
	eq(t, "false alarm", r.FalseAlarmRate, 0)

	// Positives all missed: precision undefined, recall and F1 zero.
	r = Score(Confusion{FN: 2})
	isNil(t, "precision", r.Precision)
	eq(t, "recall", r.Recall, 0)
	eq(t, "f1", r.F1, 0)
	isNil(t, "false alarm", r.FalseAlarmRate)

	// Only false alarms: recall undefined, F1 zero.
	r = Score(Confusion{FP: 1, TN: 1})
	eq(t, "precision", r.Precision, 0)
	isNil(t, "recall", r.Recall)
	eq(t, "f1", r.F1, 0)
	eq(t, "false alarm", r.FalseAlarmRate, 0.5)
}

func TestSummarizeAndOverall(t *testing.T) {
	inc := Outcome{Positive: false, Probabilities: map[check.Classification]float64{check.Inconclusive: 0.7, check.Violation: 0.2, check.Compliant: 0.1}}
	a := Summarize("a", 0.8, 0.5, []Outcome{out(true, 0.9), out(false, 0.1), inc, {Positive: true, Gated: true}, {Error: true}})
	if a.Cases != 5 || a.Positives != 2 || a.Negatives != 2 || a.Gated != 1 || a.GatedPositives != 1 || a.Errors != 1 || a.Inconclusive != 1 {
		t.Errorf("a = %+v", a)
	}
	eq(t, "inconclusive rate", a.InconclusiveRate, 1.0/3)
	if a.Fix.Threshold != 0.8 || a.Review.Threshold != 0.5 || a.Fix.TP != 1 || a.Fix.FN != 1 || a.Fix.TN != 2 {
		t.Errorf("a rates = %+v / %+v", a.Fix, a.Review)
	}
	a.Usage = check.Usage{Requests: 3, InputTokens: 30}

	b := Summarize("b", 0.9, 0.6, []Outcome{out(true, 0.85), out(false, 0.95)})
	b.Usage = check.Usage{Requests: 2, InputTokens: 20, CachedTokens: 5}
	o := Overall([]TraitResult{a, b})
	// fix: a TP1 FN1 TN2; b FN1 (0.85<0.9) FP1.
	if o.Fix.Confusion != (Confusion{TP: 1, FN: 2, FP: 1, TN: 2}) || o.Fix.Threshold != 0 {
		t.Errorf("overall fix = %+v", o.Fix)
	}
	// review: a TP1 FN1 TN2; b TP1 FP1.
	if o.Review.Confusion != (Confusion{TP: 2, FN: 1, FP: 1, TN: 2}) {
		t.Errorf("overall review = %+v", o.Review)
	}
	eq(t, "overall f1", o.Fix.F1, 0.4)
	eq(t, "overall inconclusive", o.InconclusiveRate, 1.0/5)
	if o.Usage != (check.Usage{Requests: 5, InputTokens: 50, CachedTokens: 5}) || o.Cases != 7 {
		t.Errorf("overall = %+v", o)
	}
	if e := Overall(nil); e.Fix.F1 != nil || e.InconclusiveRate != nil {
		t.Errorf("empty overall = %+v", e)
	}
}

func TestInconclusiveTies(t *testing.T) {
	tie := Outcome{Probabilities: map[check.Classification]float64{check.Inconclusive: 0.5, check.Violation: 0.5}}
	if tie.Inconclusive() {
		t.Error("a tie with violation is a violation")
	}
	tie = Outcome{Probabilities: map[check.Classification]float64{check.Inconclusive: 0.5, check.Compliant: 0.5}}
	if !tie.Inconclusive() {
		t.Error("a tie with compliant is inconclusive")
	}
	if (Outcome{Gated: true}).Inconclusive() {
		t.Error("gated is not inconclusive")
	}
}
