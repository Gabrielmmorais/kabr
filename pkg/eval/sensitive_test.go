package eval

import (
	"reflect"
	"testing"
)

func TestScoreSensitive(t *testing.T) {
	t.Parallel()
	gold := []GoldSensitive{
		{"public.staff", "password", ClassCredential},
		{"public.staff", "email", ClassPII},
		{"public.customer", "email", ClassPII},
		{"public.payment", "amount", ClassFinancial},
	}
	tests := []struct {
		name         string
		pred         []PredictedSensitive
		gold         []GoldSensitive
		recall, prec float64
		classAcc     float64
		missed       []GoldSensitive
		extra        []PredictedSensitive
	}{
		{
			// found = 3 (staff.password, staff.email, customer.email) of 4 gold
			// -> recall 0.75. Distinct predicted = 4 (duplicate
			// staff.password ignored) -> precision 3/4. Class matches:
			// staff.password ok, staff.email wrong (credential != pii),
			// customer.email ok (case-insensitive) -> 2/3.
			name: "mixed",
			pred: []PredictedSensitive{
				{"public.staff", "password", "credential"},
				{"public.staff", "email", "credential"},
				{"public.customer", "email", "PII"},
				{"public.film", "title", "other"},
				{"public.staff", "password", "pii"},
			},
			gold: gold, recall: 0.75, prec: 0.75, classAcc: 2.0 / 3,
			missed: []GoldSensitive{{"public.payment", "amount", ClassFinancial}},
			extra:  []PredictedSensitive{{"public.film", "title", "other"}},
		},
		{
			name:   "perfect",
			pred:   []PredictedSensitive{{"public.payment", "amount", "financial"}, {"public.customer", "email", "pii"}, {"public.staff", "email", "pii"}, {"public.staff", "password", "credential"}},
			gold:   gold,
			recall: 1, prec: 1, classAcc: 1,
		},
		{
			// Nothing predicted: recall 0, precision 0, class accuracy 0;
			// missed sorted by table then column.
			name: "no predictions",
			gold: gold,
			missed: []GoldSensitive{
				{"public.customer", "email", ClassPII},
				{"public.payment", "amount", ClassFinancial},
				{"public.staff", "email", ClassPII},
				{"public.staff", "password", ClassCredential},
			},
		},
		{
			name:   "empty gold and prediction",
			recall: 1, prec: 1, classAcc: 1,
		},
		{
			// Empty gold: nothing to miss, but every prediction is extra.
			name:   "empty gold with predictions",
			pred:   []PredictedSensitive{{"public.a", "b", "pii"}},
			recall: 1, prec: 0, classAcc: 1,
			extra: []PredictedSensitive{{"public.a", "b", "pii"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ScoreSensitive(tt.pred, tt.gold)
			if !approx(got.Recall, tt.recall) || !approx(got.Precision, tt.prec) || !approx(got.ClassAccuracy, tt.classAcc) {
				t.Errorf("got R=%v P=%v C=%v, want R=%v P=%v C=%v",
					got.Recall, got.Precision, got.ClassAccuracy, tt.recall, tt.prec, tt.classAcc)
			}
			if !reflect.DeepEqual(got.Missed, tt.missed) {
				t.Errorf("Missed = %v, want %v", got.Missed, tt.missed)
			}
			if !reflect.DeepEqual(got.Extra, tt.extra) {
				t.Errorf("Extra = %v, want %v", got.Extra, tt.extra)
			}
		})
	}
}
