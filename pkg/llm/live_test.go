//go:build llm_live

package llm

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Gabrielmmorais/kabr/pkg/plan"
)

// TestLiveAnthropicPagila refines the Pagila baseline against the real API.
// Run with: ANTHROPIC_API_KEY=... go test -tags llm_live -run Live ./pkg/llm/
func TestLiveAnthropicPagila(t *testing.T) {
	if os.Getenv("ANTHROPIC_API_KEY") == "" && os.Getenv("ANTHROPIC_AUTH_TOKEN") == "" {
		t.Skip("neither ANTHROPIC_API_KEY nor ANTHROPIC_AUTH_TOKEN is set")
	}
	r, err := New(Config{Provider: ProviderAnthropic})
	if err != nil {
		t.Fatal(err)
	}
	c := pagila(t)
	base := baselineFor(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	ref, err := r.Refine(ctx, c, base)
	if err != nil {
		t.Fatalf("Refine: %v", err)
	}
	t.Logf("model %s: %d renames, %d moves, %d sensitive columns",
		r.Model(), len(ref.Renames), len(ref.Moves), len(ref.SensitiveColumns))
	refined, warnings, err := plan.Apply(base, ref, c)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for _, w := range warnings {
		t.Logf("warning: %s", w)
	}
	for _, d := range refined.Spec.Domains {
		t.Logf("domain %s (%d tables): %s", d.Name, len(d.Tables), d.Description)
	}
}
