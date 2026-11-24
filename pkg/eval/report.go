package eval

import (
	"fmt"
	"strings"
)

// Report is the full evaluation of one prediction against one gold.
type Report struct {
	Gold      string // gold name
	Partition PartitionScore
	Sensitive SensitiveScore
}

// Evaluate scores a predicted grouping and sensitive-column list against g.
func Evaluate(g *Gold, pred Assignment, sens []PredictedSensitive) Report {
	return Report{
		Gold:      g.Name,
		Partition: ScorePartition(pred, g.Assignment()),
		Sensitive: ScoreSensitive(sens, g.Sensitive),
	}
}

// String renders a stable, human-readable multi-line summary.
func (r Report) String() string {
	var b strings.Builder
	p, s := r.Partition, r.Sensitive
	fmt.Fprintf(&b, "eval %s\n", r.Gold)
	fmt.Fprintf(&b, "partition: tables=%d precision=%.3f recall=%.3f f1=%.3f ari=%.3f\n",
		p.Tables, p.Precision, p.Recall, p.F1, p.ARI)
	if len(p.MissingInPrediction) > 0 {
		fmt.Fprintf(&b, "  missing in prediction: %s\n", strings.Join(p.MissingInPrediction, ", "))
	}
	if len(p.ExtraInPrediction) > 0 {
		fmt.Fprintf(&b, "  extra in prediction: %s\n", strings.Join(p.ExtraInPrediction, ", "))
	}
	fmt.Fprintf(&b, "sensitive: recall=%.3f precision=%.3f class_accuracy=%.3f\n",
		s.Recall, s.Precision, s.ClassAccuracy)
	for _, m := range s.Missed {
		fmt.Fprintf(&b, "  missed: %s.%s (%s)\n", m.Table, m.Column, m.Class)
	}
	for _, e := range s.Extra {
		fmt.Fprintf(&b, "  extra: %s.%s (%s)\n", e.Table, e.Column, e.Class)
	}
	return b.String()
}
