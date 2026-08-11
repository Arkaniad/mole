package store

import (
	"context"
	"fmt"

	"github.com/lajosdeme/mole/internal/dataset"
)

// Assembling a stored dataset (M9, §13).
//
// One function, because there were three copies of it: `mole dataset`, the eval
// scorecard and the session's own finish step each read the schema, read the rows
// and merged them, in that order, with their own error handling. Three copies of a
// four-line sequence is not a maintenance complaint — it is three places for the
// merge options to drift apart, and a threshold that means one thing in the CLI
// and another in the scorecard makes the eval measure something the user never
// sees.

// ErrNotDataset reports a session that stored no schema.
//
// Its own error because the two callers want different things from it: the CLI
// tells the user they ran the wrong command, and the eval treats it as "not this
// mode" and moves on. Both need to tell it apart from a failed read, which is the
// distinction a nil-or-empty return loses.
var ErrNotDataset = fmt.Errorf("store: session is not a dataset session")

// LoadDataset reads a session's schema and rows and merges them.
//
// Merging on read rather than storing the merged form is the design: the rows and
// the schema are what the session paid for, the merge is deterministic arithmetic
// over them, so a later fix to the matching rules improves every dataset already
// collected rather than only the next one.
//
// Returns ErrNotDataset when the session stored no schema.
func LoadDataset(
	ctx context.Context,
	st Store,
	sessionID string,
	opts dataset.Options,
) (dataset.Dataset, error) {
	var (
		schema dataset.Schema
		rows   []dataset.Row
	)
	err := st.Read(ctx, func(ctx context.Context, q Queries) error {
		s, ok, err := q.DatasetSchema(ctx, sessionID)
		if err != nil {
			return err
		}
		if !ok {
			return ErrNotDataset
		}
		schema = s
		rows, err = q.ListRows(ctx, sessionID)
		return err
	})
	if err != nil {
		return dataset.Dataset{}, err
	}
	return dataset.MergeContext(ctx, schema, rows, opts), nil
}

// MergeStoredRows assembles a session's rows against a schema the caller already
// holds.
//
// The session runner's path, and separate for one reason: it must not depend on
// the schema having been written. It has the validated schema in hand, so a failed
// or racing schema write costs the run its stored copy but not its dataset.
func MergeStoredRows(
	ctx context.Context,
	st Store,
	sessionID string,
	schema dataset.Schema,
	opts dataset.Options,
) (dataset.Dataset, error) {
	var rows []dataset.Row
	if err := st.Read(ctx, func(ctx context.Context, q Queries) error {
		var err error
		rows, err = q.ListRows(ctx, sessionID)
		return err
	}); err != nil {
		return dataset.Dataset{}, err
	}
	return dataset.MergeContext(ctx, schema, rows, opts), nil
}
