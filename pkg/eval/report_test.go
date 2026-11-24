package eval

import "testing"

func TestEvaluatePerfectPagila(t *testing.T) {
	t.Parallel()
	g, err := LoadGold(pagilaGold)
	if err != nil {
		t.Fatal(err)
	}
	var sens []PredictedSensitive
	for _, s := range g.Sensitive {
		sens = append(sens, PredictedSensitive(s))
	}
	r := Evaluate(g, g.Assignment(), sens)
	want := "eval pagila\n" +
		"partition: tables=15 precision=1.000 recall=1.000 f1=1.000 ari=1.000\n" +
		"sensitive: recall=1.000 precision=1.000 class_accuracy=1.000\n"
	if got := r.String(); got != want {
		t.Errorf("String() =\n%s\nwant\n%s", got, want)
	}
}

func TestReportString(t *testing.T) {
	t.Parallel()
	g := &Gold{
		Name:    "tiny",
		Domains: map[string][]string{"G": {"a", "b", "c"}, "H": {"d"}},
		Sensitive: []GoldSensitive{
			{"a", "pw", ClassCredential},
			{"d", "email", ClassPII},
		},
	}
	pred := Assignment{"a": "1", "b": "1", "x": "1"}
	sens := []PredictedSensitive{{"a", "pw", "credential"}, {"x", "y", "other"}}
	got := Evaluate(g, pred, sens).String()
	// Partition numbers: see "missing and extra tables" in partition_test.go.
	want := "eval tiny\n" +
		"partition: tables=4 precision=1.000 recall=0.333 f1=0.500 ari=0.333\n" +
		"  missing in prediction: c, d\n" +
		"  extra in prediction: x\n" +
		"sensitive: recall=0.500 precision=0.500 class_accuracy=1.000\n" +
		"  missed: d.email (pii)\n" +
		"  extra: x.y (other)\n"
	if got != want {
		t.Errorf("String() =\n%s\nwant\n%s", got, want)
	}
}
