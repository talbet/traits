package check

import (
	"context"
	"errors"

	"github.com/talbet/traits/internal/diff"
)

// TraitAnswer is one trait's pass-1 answer for one file.
type TraitAnswer struct {
	Trait string
	// Gated is set when appliesTo or when excluded the trait for this file,
	// so no question was asked.
	Gated bool
	// Probabilities is the four-way distribution, or nil when the trait was
	// gated or could not be asked (see Evaluate's file error).
	Probabilities map[Classification]float64
	// Cached reports that the answer came from the cache.
	Cached bool
}

// Evaluate asks the pass-1 classification question for every trait in
// opts.Traits about f, regardless of trait status, using the same gating,
// batching and cache as Run. It returns one answer per trait in opts.Traits
// order, the provider usage, and a file error for problems confined to f
// (e.g. a hunk over the request budget, or a missing post-image for a
// scope: file trait). err is a provider failure or cancellation. Pass 2
// (localisation) is not asked.
func Evaluate(ctx context.Context, opts Options, f diff.File) (answers []TraitAnswer, usage Usage, fileErr error, err error) {
	fr := fileResult{path: f.Path()}
	c, err := classify(ctx, opts, &fr, f, true)
	if err != nil {
		return nil, fr.usage, nil, err
	}
	changed := f.ChangedText()
	answers = make([]TraitAnswer, len(opts.Traits))
	for i, t := range opts.Traits {
		a := TraitAnswer{Trait: t.Name, Gated: !t.Matches(f.Path()) || !t.Gate(changed)}
		if b, ok := c.bests[t.Name]; ok {
			a.Probabilities, a.Cached = b.probs, b.cached
		}
		answers[i] = a
	}
	return answers, fr.usage, errors.Join(c.errs...), nil
}
