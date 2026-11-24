package eval

import (
	"math"
	"reflect"
	"testing"
)

const eps = 1e-9

func approx(a, b float64) bool { return math.Abs(a-b) < eps }

func TestScorePartition(t *testing.T) {
	t.Parallel()
	gold4 := Assignment{"a": "X", "b": "X", "c": "Y", "d": "Y"}
	tests := []struct {
		name          string
		pred, gold    Assignment
		p, r, f1, ari float64
		missing       []string
		extra         []string
	}{
		{
			name: "identical",
			pred: Assignment{"a": "X", "b": "X", "c": "Y", "d": "Y"},
			gold: gold4,
			p:    1, r: 1, f1: 1, ari: 1,
		},
		{
			// Same partition, different labels: only co-membership counts.
			name: "label permutation",
			pred: Assignment{"a": "q", "b": "q", "c": "z", "d": "z"},
			gold: gold4,
			p:    1, r: 1, f1: 1, ari: 1,
		},
		{
			// No predicted positive pair, gold has 2 (ab, cd): P=0 by
			// definition, R=0/2=0. TP=FP=0, FN=2, TN=4:
			// ARI = 2(0*4 - 0*2) / ((0)(0+4) + (2)(2+4)) = 0/12 = 0.
			name: "all singletons",
			pred: Assignment{"a": "1", "b": "2", "c": "3", "d": "4"},
			gold: gold4,
			p:    0, r: 0, f1: 0, ari: 0,
		},
		{
			// Every pair predicted positive: TP=2 (ab, cd), FP=4, FN=0, TN=0.
			// P=2/6, R=1, F1=2*(1/3)/(4/3)=0.5.
			// ARI = 2(2*0 - 4*0) / ((6)(4) + (2)(0)) = 0.
			name: "everything in one",
			pred: Assignment{"a": "all", "b": "all", "c": "all", "d": "all"},
			gold: gold4,
			p:    1.0 / 3, r: 1, f1: 0.5, ari: 0,
		},
		{
			// gold {a,b,c | d,e}, pred {a,b | c,d,e}; 10 pairs.
			// gold positives: ab ac bc de (4); pred positives: ab cd ce de (4).
			// TP = ab, de = 2; FP = cd, ce = 2; FN = ac, bc = 2; TN = 10-6 = 4.
			// P = 2/4, R = 2/4, F1 = 0.5.
			// ARI = 2(TP*TN - FP*FN) / ((TP+FP)(FP+TN) + (TP+FN)(FN+TN))
			//     = 2(8 - 4) / (4*6 + 4*6) = 8/48 = 1/6.
			// Cross-check (Hubert & Arabie contingency form): index =
			// C(2,2)+C(1,2)+C(2,2) = 2; sum C(a_i,2) = 3+1 = 4; sum C(b_j,2) =
			// 1+3 = 4; expected = 4*4/C(5,2) = 1.6; max = 4;
			// ARI = (2-1.6)/(4-1.6) = 1/6.
			name: "hand computed",
			pred: Assignment{"a": "1", "b": "1", "c": "2", "d": "2", "e": "2"},
			gold: Assignment{"a": "G", "b": "G", "c": "G", "d": "H", "e": "H"},
			p:    0.5, r: 0.5, f1: 0.5, ari: 1.0 / 6,
		},
		{
			// gold {a,b,c | d}; pred has a,b together, c and d missing, plus
			// extra x. Pairs over a,b,c,d (6): gold positives ab ac bc;
			// pred positives ab. TP=1, FP=0, FN=2, TN=3.
			// P=1, R=1/3, F1=0.5; ARI = 2(3-0)/((1)(3)+(3)(5)) = 6/18 = 1/3.
			name: "missing and extra tables",
			pred: Assignment{"a": "1", "b": "1", "x": "1"},
			gold: Assignment{"a": "G", "b": "G", "c": "G", "d": "H"},
			p:    1, r: 1.0 / 3, f1: 0.5, ari: 1.0 / 3,
			missing: []string{"c", "d"},
			extra:   []string{"x"},
		},
		{
			// No positive pair on either side and a zero ARI denominator:
			// all metrics are 1 by definition.
			name: "single table",
			pred: Assignment{"a": "1"},
			gold: Assignment{"a": "G"},
			p:    1, r: 1, f1: 1, ari: 1,
		},
		{
			// Both all-singletons: no positive pairs anywhere, perfect match.
			name: "both singletons",
			pred: Assignment{"a": "1", "b": "2", "c": "3"},
			gold: Assignment{"a": "G", "b": "H", "c": "I"},
			p:    1, r: 1, f1: 1, ari: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ScorePartition(tt.pred, tt.gold)
			if !approx(got.Precision, tt.p) || !approx(got.Recall, tt.r) ||
				!approx(got.F1, tt.f1) || !approx(got.ARI, tt.ari) {
				t.Errorf("got P=%v R=%v F1=%v ARI=%v, want P=%v R=%v F1=%v ARI=%v",
					got.Precision, got.Recall, got.F1, got.ARI, tt.p, tt.r, tt.f1, tt.ari)
			}
			if got.Tables != len(tt.gold) {
				t.Errorf("Tables = %d, want %d", got.Tables, len(tt.gold))
			}
			if !reflect.DeepEqual(got.MissingInPrediction, tt.missing) {
				t.Errorf("MissingInPrediction = %v, want %v", got.MissingInPrediction, tt.missing)
			}
			if !reflect.DeepEqual(got.ExtraInPrediction, tt.extra) {
				t.Errorf("ExtraInPrediction = %v, want %v", got.ExtraInPrediction, tt.extra)
			}
		})
	}
}

func TestScorePartitionSymmetricPR(t *testing.T) {
	t.Parallel()
	// Swapping pred and gold swaps precision and recall; ARI is symmetric.
	a := Assignment{"a": "1", "b": "1", "c": "2", "d": "2", "e": "2"}
	b := Assignment{"a": "G", "b": "G", "c": "G", "d": "H", "e": "G"}
	ab, ba := ScorePartition(a, b), ScorePartition(b, a)
	if !approx(ab.Precision, ba.Recall) || !approx(ab.Recall, ba.Precision) || !approx(ab.ARI, ba.ARI) {
		t.Errorf("asymmetric: %+v vs %+v", ab, ba)
	}
}
