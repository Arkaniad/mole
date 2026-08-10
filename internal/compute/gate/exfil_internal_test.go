package gate

import (
	"errors"
	"strings"
	"testing"
)

// The runtime exfil check needs a positive control of its own.
//
// TestNoRowLevelDataCrossesTheGate asserts that nothing leaks, which is also
// what a check that never fires would report. These tests hand it an envelope
// that definitely carries a value and require it to say so — and hand it
// envelopes that legitimately carry aggregates and require it to stay quiet,
// because a check that refuses everything is not a safe one, it is a broken one.

func fixture() *accumulator {
	a := &accumulator{
		names:       []string{"region", "note"},
		freeTextKey: map[int]bool{},
		shape:       shape{grouped: true, isKey: []bool{true, false}, countCol: -1},
	}
	for _, r := range [][2]string{
		{"north", "the invoice needs splitting across two cost centres"},
		{"south", "replacement unit failed on arrival, second one sent"},
	} {
		a.rows = append(a.rows, []cell{{text: r[0]}, {text: r[1]}})
	}
	return a
}

func TestTheExfilCheckFiresOnAValueThatCrossed(t *testing.T) {
	a := fixture()
	const leaked = "the invoice needs splitting across two cost centres"

	for _, tc := range []struct {
		why string
		env AggregateEnvelope
	}{
		{"in a range bound", AggregateEnvelope{
			Columns: []ColumnStats{
				{Name: "region", Kind: KindText},
				{Name: "note", Kind: KindText, Range: &TextRange{Min: leaked, Max: leaked}},
			},
		}},
		{"in a bucket key", AggregateEnvelope{
			TopK: []Bucket{{Key: []string{leaked}, Count: 9}},
		}},
		{"interpolated into a note", AggregateEnvelope{
			Notes: []string{"the most common value was " + leaked},
		}},
	} {
		t.Run(tc.why, func(t *testing.T) {
			err := a.verifyNoLeak(tc.env, []int{0, 1})
			if err == nil {
				t.Fatal("the check passed an envelope carrying a value from the data")
			}
			if !errors.Is(err, ErrLeak) {
				t.Fatalf("err = %v, want ErrLeak", err)
			}
			// The error must not be another copy of the thing that escaped.
			if strings.Contains(err.Error(), leaked) {
				t.Errorf("the error quotes the leaked value: %v", err)
			}
			if !strings.Contains(err.Error(), "note") {
				t.Errorf("the error does not name the column: %v", err)
			}
		})
	}
}

// TestTheExfilCheckIsQuietOnLegitimateAggregates. A check that fired on the
// answer would be indistinguishable in the property test from one that works,
// and would make the gate useless rather than safe.
func TestTheExfilCheckIsQuietOnLegitimateAggregates(t *testing.T) {
	a := fixture()

	for _, tc := range []struct {
		why string
		env AggregateEnvelope
	}{
		{"a bucket key that was reported as a range", AggregateEnvelope{
			Columns: []ColumnStats{
				{Name: "region", Kind: KindText, Range: &TextRange{Min: "north", Max: "south"}},
				{Name: "note", Kind: KindText, FreeText: true},
			},
			TopK: []Bucket{{Key: []string{"north"}, Count: 9}, {Key: []string{"south"}, Count: 7}},
		}},
		{"counts and moments", AggregateEnvelope{
			RowCount: 2,
			Columns: []ColumnStats{
				{Name: "n", Kind: KindNumber, Distinct: 2,
					Number: &NumberStats{Min: 7, Max: 9, Mean: 8, P50: 8}},
			},
		}},
		{"a note that names a column but quotes nothing", AggregateEnvelope{
			Notes: []string{`column "note" holds free text; its values were not read (§12.1)`},
		}},
	} {
		t.Run(tc.why, func(t *testing.T) {
			if err := a.verifyNoLeak(tc.env, []int{0, 1}); err != nil {
				t.Fatalf("the check fired on a legitimate envelope: %v", err)
			}
		})
	}
}

// TestTheExfilCheckIgnoresNumbers. Every aggregate is a number, so treating
// numeric cells as needles would report the answer as the leak.
func TestTheExfilCheckIgnoresNumbers(t *testing.T) {
	a := &accumulator{
		names:       []string{"spend"},
		freeTextKey: map[int]bool{},
		shape:       shape{isKey: []bool{false}, countCol: -1},
	}
	a.rows = append(a.rows, []cell{{text: "123456789.5", num: 123456789.5, isNum: true}})

	env := AggregateEnvelope{Columns: []ColumnStats{{
		Name: "spend", Kind: KindNumber,
		Number: &NumberStats{Min: 123456789.5, Max: 123456789.5, Mean: 123456789.5},
	}}}
	if err := a.verifyNoLeak(env, []int{0}); err != nil {
		t.Fatalf("a numeric aggregate was reported as a leak: %v", err)
	}
}

// TestTheExfilCheckSkipsShortValues records a limit rather than a behaviour.
//
// Substring matching cannot separate a short value from a coincidence, so
// anything under leakMinLen is left to the structural rules. Written down so
// the limit is a decision someone made and not something to be discovered by
// finding a three-character value in an envelope.
func TestTheExfilCheckSkipsShortValues(t *testing.T) {
	a := &accumulator{
		names:       []string{"code"},
		freeTextKey: map[int]bool{},
		shape:       shape{isKey: []bool{false}, countCol: -1},
	}
	a.rows = append(a.rows, []cell{{text: "n/a"}})

	env := AggregateEnvelope{Notes: []string{"n/a"}}
	if err := a.verifyNoLeak(env, []int{0}); err != nil {
		t.Fatalf("a value shorter than %d characters was treated as a needle, which "+
			"makes the check fire on coincidences: %v", leakMinLen, err)
	}
}
