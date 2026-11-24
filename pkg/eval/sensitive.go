package eval

import (
	"sort"
	"strings"
)

// PredictedSensitive is one column a planner flagged as sensitive.
type PredictedSensitive struct {
	Table  string `json:"table"`
	Column string `json:"column"`
	Class  string `json:"class"`
}

// SensitiveScore measures sensitive-column detection against the gold
// must-flag set.
type SensitiveScore struct {
	// Recall = gold columns found / gold columns.
	Recall float64
	// Precision = gold columns found / distinct predicted columns. The gold
	// lists the columns that MUST be flagged, not every column that MAY be:
	// extra predictions are allowed but lower precision, so recall is the
	// primary signal.
	Precision float64
	// ClassAccuracy = among found columns, the fraction whose predicted class
	// matches the gold class (case-insensitive).
	ClassAccuracy float64
	// Missed lists gold columns not predicted, sorted by table then column.
	Missed []GoldSensitive
	// Extra lists predicted columns not in the gold, sorted by table then
	// column.
	Extra []PredictedSensitive
}

// ScoreSensitive scores pred against gold, matching on (table, column).
// Duplicate predictions for the same column count once (the first wins).
//
// Edge cases: with an empty gold set Recall and ClassAccuracy are 1; with no
// predictions Precision is 1 only if the gold set is empty too; with gold
// columns but none found ClassAccuracy is 0.
func ScoreSensitive(pred []PredictedSensitive, gold []GoldSensitive) SensitiveScore {
	type key struct{ table, column string }
	predicted := map[key]PredictedSensitive{}
	var order []key
	for _, p := range pred {
		k := key{p.Table, p.Column}
		if _, dup := predicted[k]; dup {
			continue
		}
		predicted[k] = p
		order = append(order, k)
	}
	inGold := map[key]bool{}

	var s SensitiveScore
	found, classOK := 0, 0
	for _, g := range gold {
		k := key{g.Table, g.Column}
		inGold[k] = true
		p, ok := predicted[k]
		if !ok {
			s.Missed = append(s.Missed, g)
			continue
		}
		found++
		if strings.EqualFold(strings.TrimSpace(p.Class), strings.TrimSpace(g.Class)) {
			classOK++
		}
	}
	for _, k := range order {
		if !inGold[k] {
			s.Extra = append(s.Extra, predicted[k])
		}
	}
	sort.Slice(s.Missed, func(i, j int) bool {
		return lessTC(s.Missed[i].Table, s.Missed[i].Column, s.Missed[j].Table, s.Missed[j].Column)
	})
	sort.Slice(s.Extra, func(i, j int) bool {
		return lessTC(s.Extra[i].Table, s.Extra[i].Column, s.Extra[j].Table, s.Extra[j].Column)
	})

	s.Recall = ratioOr(float64(found), float64(len(gold)), true)
	s.Precision = ratioOr(float64(found), float64(len(predicted)), len(gold) == 0)
	s.ClassAccuracy = ratioOr(float64(classOK), float64(found), len(gold) == 0)
	return s
}

func lessTC(t1, c1, t2, c2 string) bool {
	if t1 != t2 {
		return t1 < t2
	}
	return c1 < c2
}
