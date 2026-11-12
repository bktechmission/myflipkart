// Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
//
// Trust-weighted rating. The shown rating uses only verified, trusted reviews
// (status published and counted), as a Bayesian average pulled toward the
// category mean, so a handful of reviews can't swing it:
//
//	shown = (m*C + sum of trusted stars) / (m + n)      m = 5, C = category mean
//
// During a burst the rating stays at its last trusted value (reviews from the
// burst window are left out until a person clears it); a person can also freeze it.
// Admins also see the naive average of every submitted review, for comparison.

package reviews

import (
	"math"

	"asksauby/internal/model"
)

const PriorWeight = 5.0

type Rating struct {
	ProductID string  `json:"product_id"`
	Shown     float64 `json:"shown"`
	N         int     `json:"n"`     // verified reviews counted
	Prior     float64 `json:"prior"` // category mean the average is pulled toward
	Naive     float64 `json:"naive"` // plain average of every submitted review
	NaiveN    int     `json:"naive_n"`
	Paused    bool    `json:"paused,omitempty"` // burst: last trusted value, waiting for a person
	Frozen    bool    `json:"frozen,omitempty"` // a person froze it
	Note      string  `json:"note"`
}

// Rating computes a product's shown rating from the store.
func (g *Guard) Rating(pid string) Rating {
	p, _, _ := g.catalog(pid)
	return ComputeRating(pid, g.categoryMean(p.Category), g.S.Reviews(func(r model.Review) bool { return r.ProductID == pid }),
		g.S.ProductReviewState(pid))
}

// ComputeRating is the pure rating function (tests call it directly).
func ComputeRating(pid string, prior float64, reviews []model.Review, ps model.ProductReviewState) Rating {
	out := Rating{ProductID: pid, Prior: round1(prior)}
	sum, nsum := 0.0, 0.0
	for _, r := range reviews {
		if r.Status != "pending" {
			nsum += float64(r.Stars)
			out.NaiveN++
		}
		if r.Status != "published" || !r.Counted {
			continue
		}
		if !ps.BurstSince.IsZero() && !r.CreatedAt.Before(ps.BurstSince) {
			continue // burst window: not trusted until a person clears it
		}
		sum += float64(r.Stars)
		out.N++
	}
	out.Shown = round1((PriorWeight*prior + sum) / (PriorWeight + float64(out.N)))
	if out.NaiveN > 0 {
		out.Naive = round1(nsum / float64(out.NaiveN))
	}
	switch {
	case ps.Frozen:
		out.Frozen, out.Shown, out.N = true, ps.FrozenRating, ps.FrozenN
		out.Note = "Rating frozen by our review team while they investigate."
	case !ps.BurstSince.IsZero():
		out.Paused = true
		out.Note = "New reviews are being checked; the rating shows its last trusted value."
	}
	return out
}

func (g *Guard) categoryMean(category string) float64 {
	sum, n := 0.0, 0
	for _, p := range g.S.Products() {
		if p.Category == category {
			sum += p.Rating
			n++
		}
	}
	if n == 0 {
		return 4
	}
	return sum / float64(n)
}

func round1(x float64) float64 { return math.Round(x*10) / 10 }
