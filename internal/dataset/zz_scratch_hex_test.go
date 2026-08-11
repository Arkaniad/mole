package dataset

import (
	"encoding/hex"
	"strings"
	"testing"
)

func TestScratchHex(t *testing.T) {
	s := Schema{Fields: []Field{{Name: "company", Type: TypeText, Key: true}, {Name: "note", Type: TypeText}}}
	rows := []Row{{
		Values: map[string]string{"company": "Acme Ltd", "note": "=1+1"},
		Source: "https://evil.example/page",
		Quote:  "a sufficiently long verbatim quote from the page",
	}}
	d := Merge(s, rows, Options{})
	var b strings.Builder
	if err := d.WriteCSV(&b, false); err != nil {
		t.Fatal(err)
	}
	t.Logf("hex: %s", hex.EncodeToString([]byte(b.String())))
	t.Logf("raw: %q", b.String())
}
