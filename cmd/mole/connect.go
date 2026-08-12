package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/lajosdeme/mole/internal/compute/connector"
	"github.com/spf13/cobra"
)

// `mole connect` registers local data sources (M8, §12).
//
// Registration is the only moment mole writes anything derived from the user's
// data, and the only moment it reads a row. Everything after this — the
// aggregation gate, the actor, the report — works from the profile written
// here, which holds shape and no contents.

func newConnectCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "connect",
		Short: "Register a local file or folder for analysis",
		Long: "Registers a file, a folder of files, or an existing SQLite database\n" +
			"as a named source that research can query.\n\n" +
			"Rows never reach a model. Queries run through a read-only handle and\n" +
			"only aggregates cross to anything that can reach an LLM (§12.1), so\n" +
			"what registration records is a profile — types, null rates, distinct\n" +
			"counts, ranges — and not the data.",
	}
	c.AddCommand(newConnectAddCmd(), newConnectListCmd(), newConnectSchemaCmd(), newConnectRemoveCmd())
	return c
}

// registryPath and scratchPath sit beside the session database, so --db keeps
// a test or a second install fully separate rather than half of it.
func registryPath(cmd *cobra.Command) string {
	return connectorRegistryPath(dbPath(cmd))
}

// connectorRegistryPath is the same rule without a cobra command, for the
// research path — which has the database path and not the flag set it came
// from. One function, so a session and `mole connect list` cannot disagree
// about which registry they are reading.
func connectorRegistryPath(db string) string {
	return filepath.Join(filepath.Dir(db), "connectors.json")
}

func scratchPath(cmd *cobra.Command, name string) string {
	return filepath.Join(filepath.Dir(dbPath(cmd)), "connectors", name+".db")
}

func newConnectAddCmd() *cobra.Command {
	var replace bool

	c := &cobra.Command{
		Use:   "add <name> <path>",
		Short: "Register a file, a folder, or a SQLite database",
		Long: "  mole connect add sales ./exports/sales.csv\n" +
			"  mole connect add exports ./exports          # one table per file\n" +
			"  mole connect add warehouse ./warehouse.db   # attached, never copied\n\n" +
			"A folder is read one level deep: .csv, .tsv, .jsonl and .ndjson become\n" +
			"one table each, anything else is ignored. Subdirectories are not\n" +
			"followed — a research tool that walks into folders nobody meant to\n" +
			"expose is the wrong shape for a privacy boundary.\n\n" +
			"Delimited and JSON files are copied into a database mole owns. An\n" +
			"existing SQLite file is attached where it is.",
		Args: exactArgs(2, "mole connect add <name> <path>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmdConnectAdd(cmd, args[0], args[1], replace)
		},
	}
	c.Flags().BoolVar(&replace, "replace", false, "re-import over an existing registration")
	return c
}

func cmdConnectAdd(cmd *cobra.Command, name, path string, replace bool) error {
	reg, err := connector.LoadRegistry(registryPath(cmd))
	if err != nil {
		return err
	}
	if _, exists := reg.Get(name); exists {
		if !replace {
			return fmt.Errorf("connector %q is already registered; "+
				"pass --replace to re-import it, or `mole connect remove %s` first", name, name)
		}
		reg.Remove(name)
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "reading %s …\n", path)

	c, err := connector.Ingest(cmd.Context(), name, path, scratchPath(cmd, name))
	if err != nil {
		return err
	}
	if err := reg.Add(c); err != nil {
		return err
	}
	if err := reg.Save(); err != nil {
		return err
	}

	var rows int64
	for _, t := range c.Tables {
		rows += t.Rows
	}
	fmt.Fprintf(out, "registered %s — %d table(s), %d row(s)\n", c.Name, len(c.Tables), rows)
	// Anything registration could not use, said out loud. Uppercase identifiers
	// were once rejected, so a CamelCase database registered with no error and a
	// profile of one table with one column — and the model then planned over a
	// schema that was not the user's data.
	for _, note := range c.Skipped {
		fmt.Fprintf(out, "  ! %s\n", note)
	}
	if c.Kind == connector.KindImport {
		fmt.Fprintf(out, "  imported into %s\n", c.DBPath)
	} else {
		fmt.Fprintf(out, "  attached in place, read-only\n")
	}
	printSchema(out, c)
	return nil
}

func newConnectListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List registered sources",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			reg, err := connector.LoadRegistry(registryPath(cmd))
			if err != nil {
				return err
			}
			list := reg.List()
			out := cmd.OutOrStdout()
			if len(list) == 0 {
				fmt.Fprintln(out, "no sources registered — `mole connect add <name> <path>`")
				return nil
			}
			w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tKIND\tTABLES\tROWS\tSOURCE")
			for _, c := range list {
				var rows int64
				for _, t := range c.Tables {
					rows += t.Rows
				}
				fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%s\n", c.Name, c.Kind, len(c.Tables), rows, c.Source)
			}
			return w.Flush()
		},
	}
}

func newConnectSchemaCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "schema <name>",
		Short: "Show the profile a query would be planned against",
		Args:  exactArgs(1, "mole connect schema <name>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, err := connector.LoadRegistry(registryPath(cmd))
			if err != nil {
				return err
			}
			c, ok := reg.Get(args[0])
			if !ok {
				return fmt.Errorf("%w: %s", connector.ErrNoSuchConnector, args[0])
			}
			printSchema(cmd.OutOrStdout(), c)
			return nil
		},
	}
}

func newConnectRemoveCmd() *cobra.Command {
	var purge bool

	c := &cobra.Command{
		Use:     "remove <name>",
		Aliases: []string{"rm"},
		Short:   "Unregister a source",
		Args:    exactArgs(1, "mole connect remove <name>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, err := connector.LoadRegistry(registryPath(cmd))
			if err != nil {
				return err
			}
			c, ok := reg.Remove(args[0])
			if !ok {
				return fmt.Errorf("%w: %s", connector.ErrNoSuchConnector, args[0])
			}
			if err := reg.Save(); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "removed %s\n", c.Name)

			// Deleting is opt-in. For an attached database the file is the
			// user's own and removing a registration must never touch it; for
			// an imported one it is mole's, but a copy of someone's data is
			// still theirs to decide about.
			switch {
			case !purge:
				if c.Kind == connector.KindImport {
					fmt.Fprintf(out, "  the imported copy is still at %s (--purge to delete it)\n", c.DBPath)
				}
			case c.Kind != connector.KindImport:
				fmt.Fprintf(out, "  --purge ignored: %s is your own database, attached in place\n", c.DBPath)
			default:
				if err := os.Remove(c.DBPath); err != nil && !errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("delete %s: %w", c.DBPath, err)
				}
				fmt.Fprintf(out, "  deleted %s\n", c.DBPath)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&purge, "purge", false, "also delete the imported copy")
	return c
}

// printSchema renders the profile.
//
// This is close to what §12.3's templates get to see, so it is worth being able
// to look at: if a column reads as text here, no numeric hypothesis will ever
// be planned over it, and the reason will be a comma in a spreadsheet.
func printSchema(out io.Writer, c connector.Connector) {
	for _, t := range c.Tables {
		origin := ""
		if t.Origin != "" {
			origin = "  ← " + t.Origin
		}
		fmt.Fprintf(out, "\n%s.%s — %d row(s)%s\n", c.Name, t.Name, t.Rows, origin)

		w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "  COLUMN\tTYPE\tNULLS\tDISTINCT\tRANGE")
		for _, col := range t.Columns {
			name := col.Name
			if col.Label != "" {
				name += " (" + col.Label + ")"
			}
			rng := ""
			switch {
			case col.FreeText:
				// Not "unknown". The values exist and were deliberately not
				// read, and saying so is the difference between a profile that
				// looks incomplete and one that is doing its job.
				rng = "free text — excluded from top-values (§12.1)"
			case col.Min != "" || col.Max != "":
				rng = col.Min + " … " + col.Max
			}
			fmt.Fprintf(w, "  %s\t%s\t%d\t%d\t%s\n", name, col.Type, col.Nulls, col.Distinct, rng)
		}
		_ = w.Flush()
	}
}
