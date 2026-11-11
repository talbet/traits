package eval

import "github.com/talbet/traits/internal/check"

// Outcome is a model's answer for one case.
type Outcome struct {
	Positive bool // expect violation
	Split    Split
	// Gated: appliesTo/when excluded the case, which counts as a predicted
	// not-applicable (violation probability 0).
	Gated bool
	// Error: the case could not be asked (e.g. over the request budget); it
	// is left out of the confusion counts.
	Error         bool
	Probabilities map[check.Classification]float64
}

// PViolation is the predicted violation probability (0 when gated).
func (o Outcome) PViolation() float64 { return o.Probabilities[check.Violation] }

// Inconclusive reports whether inconclusive was the most likely answer,
// breaking ties towards violation as check does.
func (o Outcome) Inconclusive() bool {
	p := o.Probabilities
	i := p[check.Inconclusive]
	return i > p[check.Violation] && i >= p[check.Compliant] && i >= p[check.NotApplicable] && i > 0
}

// Confusion counts cases by label (positive = expect violation) and
// prediction (positive = violation probability >= threshold).
type Confusion struct {
	TP int `json:"tp"`
	FP int `json:"fp"`
	FN int `json:"fn"`
	TN int `json:"tn"`
}

// Count tallies outcomes at threshold, skipping errors.
func Count(outs []Outcome, threshold float64) Confusion {
	var c Confusion
	for _, o := range outs {
		if o.Error {
			continue
		}
		pred := !o.Gated && o.PViolation() >= threshold
		switch {
		case o.Positive && pred:
			c.TP++
		case o.Positive:
			c.FN++
		case pred:
			c.FP++
		default:
			c.TN++
		}
	}
	return c
}

// Rates is a confusion matrix and the scores derived from it. A score whose
// denominator is zero is nil (JSON null): precision with no predicted
// positives, recall with no positive cases, false-alarm rate with no clean
// cases, and F1 when there are neither positives nor predicted positives.
type Rates struct {
	// Threshold is the violation probability counted as a positive
	// prediction; 0 in the overall row, where each trait uses its own.
	Threshold float64 `json:"threshold,omitempty"`
	Confusion
	Precision *float64 `json:"precision"`
	Recall    *float64 `json:"recall"`
	F1        *float64 `json:"f1"`
	// FalseAlarmRate is FP over clean (compliant or not-applicable) cases.
	FalseAlarmRate *float64 `json:"falseAlarmRate"`
}

// Score derives the rates from c.
func Score(c Confusion) Rates {
	r := Rates{Confusion: c}
	r.Precision = ratio(c.TP, c.TP+c.FP)
	r.Recall = ratio(c.TP, c.TP+c.FN)
	r.FalseAlarmRate = ratio(c.FP, c.FP+c.TN)
	// F1 = 2TP / (2TP + FP + FN), the harmonic mean of precision and
	// recall. It is defined (possibly 0) whenever a case was labelled or
	// predicted positive, even if precision or recall alone is not.
	r.F1 = ratio(2*c.TP, 2*c.TP+c.FP+c.FN)
	return r
}

func ratio(num, den int) *float64 {
	if den == 0 {
		return nil
	}
	v := float64(num) / float64(den)
	return &v
}

func (c *Confusion) add(o Confusion) {
	c.TP += o.TP
	c.FP += o.FP
	c.FN += o.FN
	c.TN += o.TN
}

// TraitResult is one trait's scores for one model on a set of cases.
type TraitResult struct {
	Trait  string `json:"trait"`
	Status string `json:"status,omitempty"`
	// Cases counts all cases, including gated and errored ones.
	Cases     int `json:"cases"`
	Positives int `json:"positives"` // expect violation, excluding errors
	Negatives int `json:"negatives"` // compliant or not-applicable, excluding errors
	// Gated cases were excluded by appliesTo/when; GatedPositives of them
	// were labelled violation, which a gate should never drop.
	Gated          int `json:"gated"`
	GatedPositives int `json:"gatedPositives"`
	Errors         int `json:"errors"`
	// Inconclusive counts asked cases whose most likely answer was
	// inconclusive; the rate is over asked (non-gated, non-error) cases.
	Inconclusive     int         `json:"inconclusive"`
	InconclusiveRate *float64    `json:"inconclusiveRate"`
	Review           Rates       `json:"review"`
	Fix              Rates       `json:"fix"`
	Usage            check.Usage `json:"usage"`
}

// Summarize scores outs for one trait at its fix and review thresholds.
func Summarize(name string, fix, review float64, outs []Outcome) TraitResult {
	r := TraitResult{Trait: name, Cases: len(outs)}
	asked := 0
	for _, o := range outs {
		switch {
		case o.Error:
			r.Errors++
			continue
		case o.Positive:
			r.Positives++
		default:
			r.Negatives++
		}
		if o.Gated {
			r.Gated++
			if o.Positive {
				r.GatedPositives++
			}
			continue
		}
		asked++
		if o.Inconclusive() {
			r.Inconclusive++
		}
	}
	r.InconclusiveRate = ratio(r.Inconclusive, asked)
	r.Review = Score(Count(outs, review))
	r.Review.Threshold = review
	r.Fix = Score(Count(outs, fix))
	r.Fix.Threshold = fix
	return r
}

// Overall is the micro-averaged row: counts summed over traits (each at its
// own thresholds) and rates recomputed from the sums.
func Overall(rs []TraitResult) TraitResult {
	o := TraitResult{Trait: "overall"}
	var review, fix Confusion
	asked := 0
	for _, r := range rs {
		o.Cases += r.Cases
		o.Positives += r.Positives
		o.Negatives += r.Negatives
		o.Gated += r.Gated
		o.GatedPositives += r.GatedPositives
		o.Errors += r.Errors
		o.Inconclusive += r.Inconclusive
		asked += r.Positives + r.Negatives - r.Gated
		review.add(r.Review.Confusion)
		fix.add(r.Fix.Confusion)
		o.Usage.Requests += r.Usage.Requests
		o.Usage.InputTokens += r.Usage.InputTokens
		o.Usage.CachedTokens += r.Usage.CachedTokens
	}
	o.InconclusiveRate = ratio(o.Inconclusive, asked)
	o.Review = Score(review)
	o.Fix = Score(fix)
	return o
}
