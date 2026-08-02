package extract_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/lajosdeme/mole/internal/tools/extract"
)

// TestExtractHonoursCancellation. The HTML parse is superlinear on pathological
// markup — a 330KB page has been measured at 53s — and it has no cancellation
// of its own. Without a cancellation point here, one page outlasts the lead's
// deadline and every ceiling the budget layer enforces.
func TestExtractHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	page := "<html><body>" + strings.Repeat("<p>text</p>", 100) + "</body></html>"
	_, err := extract.New().Extract(ctx, []byte(page), "text/html", mustURL(t, "https://example.com/x"))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

// TestExtractHonoursDeadlineOnPathologicalMarkup uses input shaped to make the
// parser work: deeply nested tags are the realistic version of the slow case.
func TestExtractHonoursDeadlineOnPathologicalMarkup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var b strings.Builder
	b.WriteString("<html><body>")
	for i := 0; i < 5000; i++ {
		b.WriteString("<div>")
	}
	b.WriteString("deeply buried text")
	for i := 0; i < 5000; i++ {
		b.WriteString("</div>")
	}
	b.WriteString("</body></html>")

	if _, err := extract.New().Extract(ctx, []byte(b.String()), "text/html", mustURL(t, "https://example.com/y")); err == nil {
		t.Error("a cancelled extraction returned success")
	}
}

// TestExtractStillWorksWithALiveContext: the cancellation path must not break
// the ordinary one.
func TestExtractStillWorksWithALiveContext(t *testing.T) {
	page := "<html><head><title>T</title></head><body><article><p>" +
		strings.Repeat("Substantive sentence that carries meaning. ", 20) +
		"</p></article></body></html>"

	doc, err := extract.New().Extract(t.Context(), []byte(page), "text/html", mustURL(t, "https://example.com/z"))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if !doc.Usable() {
		t.Errorf("extracted %d bytes, want a usable document", len(doc.Text))
	}
}
