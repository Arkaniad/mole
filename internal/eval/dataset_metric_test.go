package eval

// In-package, because the metric it checks can no longer be reached through the
// store: migration 0009 refuses a row with an empty quote, which is the right
// place to stop one. The metric still has to fire for a dataset that contains one
// — a stored dataset from an older schema, say — so it is tested directly rather
// than exported for a test's convenience.

import (
	"testing"

	"github.com/lajosdeme/mole/internal/dataset"
)

// TestARowWithoutAQuoteIsAHardRegression, at the metric rather than through the
// store, since the store now refuses to hold one.
func TestARowWithoutAQuoteIsAHardRegression(t *testing.T) {
	d := dataset.Merge(
		dataset.Schema{Fields: []dataset.Field{{Name: "company", Type: dataset.TypeText, Key: true}}},
		[]dataset.Row{
			{Values: map[string]string{"company": "Acme Ltd"}, Source: "https://a.example",
				Quote: "Acme Ltd exists"},
			{Values: map[string]string{"company": "Beta GmbH"}, Source: "https://b.example"},
		},
		dataset.Options{})
	if d.Unquoted != 1 {
		t.Fatalf("unquoted = %d, want 1", d.Unquoted)
	}
	m := rowIntegrity(d)
	if !m.Regression {
		t.Error("a row with no quote is not a hard regression")
	}
	if m.Value != 50 {
		t.Errorf("integrity = %v, want 50", m.Value)
	}
}
