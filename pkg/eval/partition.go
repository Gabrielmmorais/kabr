package eval

import "sort"

// Assignment maps a table ID to a domain label. Labels are arbitrary: only
// co-membership (which tables share a label) is scored.
type Assignment map[string]string

// PartitionScore compares a predicted grouping with the gold grouping.
type PartitionScore struct {
	// Precision, Recall and F1 are computed over pairs of gold tables: a pair
	// is positive when both tables share a label.
	Precision, Recall, F1 float64
	// ARI is the Adjusted Rand Index (Hubert & Arabie, 1985). 1 means
	// identical partitions, ~0 means chance-level agreement; it can be
	// negative.
	ARI float64
	// Tables is the number of gold tables scored.
	Tables int
	// MissingInPrediction lists gold tables absent from the prediction
	// (scored as singletons). Sorted.
	MissingInPrediction []string
	// ExtraInPrediction lists predicted tables absent from the gold (ignored
	// for scoring). Sorted.
	ExtraInPrediction []string
}

// ScorePartition scores pred against gold by pairwise co-membership over the
// tables present in gold.
//
// With TP/FP/FN/TN the pair counts (same-in-both, same-only-in-pred,
// same-only-in-gold, apart-in-both):
//   - Precision = TP/(TP+FP); when the prediction has no positive pair it is
//     1 if the gold has none either, else 0.
//   - Recall = TP/(TP+FN); when the gold has no positive pair it is 1 if the
//     prediction has none either, else 0.
//   - F1 is the harmonic mean (0 when P+R = 0).
//   - ARI = 2(TP*TN - FP*FN) / ((TP+FP)(FP+TN) + (TP+FN)(FN+TN)); when the
//     denominator is 0 (degenerate partitions, fewer than 2 tables) it is 1
//     if both partitions agree on every pair, else 0.
//
// Gold tables missing from pred are treated as singletons.
func ScorePartition(pred, gold Assignment) PartitionScore {
	tables := sortedKeys(gold)
	s := PartitionScore{Tables: len(tables)}

	for _, t := range tables {
		if _, ok := pred[t]; !ok {
			s.MissingInPrediction = append(s.MissingInPrediction, t)
		}
	}
	for t := range pred {
		if _, ok := gold[t]; !ok {
			s.ExtraInPrediction = append(s.ExtraInPrediction, t)
		}
	}
	sort.Strings(s.ExtraInPrediction)

	var tp, fp, fn, tn float64
	for i := 0; i < len(tables); i++ {
		for j := i + 1; j < len(tables); j++ {
			a, b := tables[i], tables[j]
			inGold := gold[a] == gold[b]
			pa, okA := pred[a]
			pb, okB := pred[b]
			inPred := okA && okB && pa == pb
			switch {
			case inGold && inPred:
				tp++
			case inPred:
				fp++
			case inGold:
				fn++
			default:
				tn++
			}
		}
	}

	predPos, goldPos := tp+fp, tp+fn
	s.Precision = ratioOr(tp, predPos, goldPos == 0)
	s.Recall = ratioOr(tp, goldPos, predPos == 0)
	if s.Precision+s.Recall > 0 {
		s.F1 = 2 * s.Precision * s.Recall / (s.Precision + s.Recall)
	}

	den := (tp+fp)*(fp+tn) + (tp+fn)*(fn+tn)
	switch {
	case den != 0:
		s.ARI = 2 * (tp*tn - fp*fn) / den
	case fp == 0 && fn == 0:
		s.ARI = 1
	default:
		s.ARI = 0
	}
	return s
}

// ratioOr returns num/den, or 1/0 (per emptyOK) when den is zero.
func ratioOr(num, den float64, emptyOK bool) float64 {
	if den == 0 {
		if emptyOK {
			return 1
		}
		return 0
	}
	return num / den
}
