package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/store"
	"github.com/spf13/cobra"
)

// `mole crossings` is §12.1's audit trail, read back.
//
// "Every crossing is logged, so a user can audit exactly what left their
// machine." The gate has emitted a structured log line since M8, which met the
// letter of that and not the use: logs rotate, Info is off in some setups, and a
// line cannot be queried per session. The question a user actually has —
// what did mole send about my sales data — is a query against a table.
//
// What this prints carries no value from the data. Counts, a hash, one of mole's
// own reason strings, and the statement — which is safe to show because §12.3
// forbids the model from authoring one: a query is a hypothesis template filled
// with identifiers the connector profile already published.

func newCrossingsCmd() *cobra.Command {
	var verbose bool
	c := &cobra.Command{
		Use:   "crossings <session-id>",
		Short: "Audit what local data left this machine",
		Long: "Every aggregate that crossed the gate for a session, plus every one that\n" +
			"was refused or withheld.\n\n" +
			"Refusals are the interesting half: they are the gate doing the thing it\n" +
			"exists to do. A trail listing only successes would let a reader conclude\n" +
			"that the questions mole answered are all it tried.\n\n" +
			"No value from your data appears here — counts, a query hash, and the\n" +
			"statement, which mole built from a template rather than a model.",
		Args: exactArgs(1, "mole crossings <session-id>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmdCrossings(cmd.Context(), cmd, dbPath(cmd), args[0], verbose)
		},
	}
	c.Flags().BoolVar(&verbose, "queries", false, "print the full statement for each crossing")
	return c
}

func cmdCrossings(
	ctx context.Context, cmd *cobra.Command, dbPath, sessionID string, verbose bool,
) error {
	db, err := openDBNoMigrate(ctx, dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	var list []core.Crossing
	if err := db.Read(ctx, func(ctx context.Context, q store.Queries) error {
		var err error
		list, err = q.ListCrossings(ctx, sessionID)
		return err
	}); err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	if len(list) == 0 {
		// Two different facts, and the distinction matters to somebody checking
		// whether their data was touched: a session that used no local data has
		// nothing to audit, and one whose trail is missing is a different problem.
		fmt.Fprintf(out, "no crossings recorded for %s — this session used no local data\n",
			sessionID)
		return nil
	}

	var crossed, refused, withheld int
	for _, c := range list {
		switch c.Outcome {
		case core.CrossingCrossed:
			crossed++
		case core.CrossingRefused:
			refused++
		case core.CrossingWithheld:
			withheld++
		}
	}

	w := newTabWriter()
	fmt.Fprintln(w, "WHEN\tCONNECTOR\tOUTCOME\tROWS\tBUCKETS\tSUPPRESSED\tWITHHELD COLS\tHASH")
	for _, c := range list {
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d\t%d\t%d\t%s\n",
			c.CreatedAt.Format(time.RFC3339), c.Connector, c.Outcome,
			c.RowsDescribed, c.Buckets, c.Suppressed, c.ColumnsWithheld,
			shortQueryHash(c.QueryHash))
	}
	if err := w.Flush(); err != nil {
		return err
	}

	fmt.Fprintf(out, "\n%d crossing(s): %d aggregate(s) crossed, %d refused, %d withheld.\n",
		len(list), crossed, refused, withheld)
	if withheld > 0 {
		// The one number here that is a bug rather than a design. The exfil check
		// is a backstop for rules that refuse first; if it fired, one of those
		// rules broke.
		fmt.Fprintf(out, "%d envelope(s) were WITHHELD because they carried row-level "+
			"data — that is a bug in mole, not a refused question. Please report it.\n", withheld)
	}
	if refused > 0 && !verbose {
		fmt.Fprintln(out, "Refusals are the gate working. --queries prints each statement "+
			"and the reason.")
	}

	if verbose {
		fmt.Fprintln(out)
		for _, c := range list {
			fmt.Fprintf(out, "%s  %s  %s\n", c.CreatedAt.Format(time.RFC3339),
				c.Outcome, c.QueryHash)
			fmt.Fprintf(out, "  %s\n", strings.Join(strings.Fields(c.Query), " "))
			if c.Detail != "" {
				fmt.Fprintf(out, "  reason: %s\n", c.Detail)
			}
		}
	}
	return nil
}

// shortQueryHash is what a local claim cites: connector:<name>#<16 hex>.
func shortQueryHash(h string) string {
	if len(h) > 16 {
		return h[:16]
	}
	return h
}
