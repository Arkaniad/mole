package eval_test

import (
	"path/filepath"
	"testing"

	"github.com/lajosdeme/mole/internal/eval"
)

// TestCommittedCorporaLoad parses every corpus file in the repository.
//
// Cheap, and it guards something expensive: a corpus that does not parse is
// otherwise discovered by whoever runs it, after they have paid for a search
// key and however many model calls the first question took.
func TestCommittedCorporaLoad(t *testing.T) {
	files, err := filepath.Glob("../../testdata/corpus/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no corpus files found; the glob is wrong or they moved")
	}

	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			c, err := eval.LoadCorpus(f)
			if err != nil {
				t.Fatal(err)
			}
			for i, q := range c.Questions {
				// Notes are the labelling guide for a disputed set — they name the
				// disagreement a labeller is meant to find. A question without them
				// is one nobody can label consistently six months from now.
				if len(q.Tags) > 0 && hasTag(q.Tags, "disputed") && q.Notes == "" {
					t.Errorf("question %d (%s) is tagged disputed with no notes", i, q.ID)
				}
			}
		})
	}
}

func hasTag(tags []string, want string) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}
