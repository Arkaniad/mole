// Package hypothesis renders the only SQL that may reach a connector (M8,
// §12.3).
//
// §12.3 is one paragraph and it is the whole design: "local leads are
// instantiated from hypothesis templates over the connector's known schema.
// Web-derived content can influence template and column choice; it cannot
// author SQL."
//
// So a model picks a template and fills its slots with column names. It never
// produces a fragment of SQL, and the names it produces are not escaped into a
// statement — they are looked up in the connector's profile and rejected if
// they are not there. Escaping would make a hostile name safe to interpolate;
// lookup makes it impossible to interpolate anything that is not already a
// column of a registered table, which is a stronger claim and a simpler one.
package hypothesis

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/lajosdeme/mole/internal/compute/connector"
	"github.com/lajosdeme/mole/internal/compute/stats"
)

// ErrInvalid matches every refusal to render a plan.
var ErrInvalid = errors.New("hypothesis: invalid plan")

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// Kind names a template.
type Kind string

const (
	KindDistribution Kind = "distribution"
	KindOverview     Kind = "overview"
	KindGroupCompare Kind = "group_comparison"
	KindTrend        Kind = "trend"
)

// Role is what a slot needs from a column.
type Role string

const (
	// RoleCategory is a column whose values are labels: text, not prose, and
	// with few enough distinct values to group by usefully.
	RoleCategory Role = "category"
	// RoleMeasure is a numeric column to summarize.
	RoleMeasure Role = "measure"
	// RoleTime is a time axis.
	RoleTime Role = "time"
)

// maxCategories bounds a grouping column.
//
// Not a safety limit — the k-anonymity floor is that, and the gate applies it
// whatever this says. It is a usefulness limit: grouping a thousand-row table
// by a column with nine hundred distinct values produces nine hundred buckets
// of one, every one of which the gate then suppresses. Refusing here means the
// model is told why instead of receiving an envelope with nothing in it.
const maxCategories = 200

// Slot is one hole in a template.
type Slot struct {
	Name string
	Role Role
	// Optional slots may be left unfilled.
	Optional bool
}

// Template is a question mole knows how to ask of a table.
type Template struct {
	Kind Kind
	// Question is what it answers, phrased for the model choosing between them.
	Question string
	Slots    []Slot

	render func(table string, cols map[string]string) string
}

// Templates are the whole vocabulary. Every statement that reaches a connector
// comes from one of these.
//
// Each renders a COUNT(*), because the aggregation gate refuses a grouped
// result without one: the k-anonymity floor needs a record count per bucket,
// and a SUM of five is not five records.
var Templates = []Template{
	{
		Kind:     KindDistribution,
		Question: "How are the records distributed across the values of one column?",
		Slots:    []Slot{{Name: "key", Role: RoleCategory}},
		render: func(t string, c map[string]string) string {
			return fmt.Sprintf(
				"SELECT %s AS bucket, COUNT(*) AS n FROM %s GROUP BY 1 ORDER BY n DESC",
				c["key"], t)
		},
	},
	{
		Kind:     KindOverview,
		Question: "What is the overall shape of one numeric column across every record?",
		Slots:    []Slot{{Name: "measure", Role: RoleMeasure}},
		render: func(t string, c map[string]string) string {
			return fmt.Sprintf(
				"SELECT COUNT(*) AS n, AVG(%[1]s) AS mean, MIN(%[1]s) AS lowest, "+
					"MAX(%[1]s) AS highest, SUM(%[1]s) AS total FROM %[2]s",
				c["measure"], t)
		},
	},
	{
		Kind: KindGroupCompare,
		Question: "How does one numeric column differ between the groups of a " +
			"categorical column?",
		Slots: []Slot{
			{Name: "key", Role: RoleCategory},
			{Name: "measure", Role: RoleMeasure},
		},
		// The sums are not decoration. §4 asks for significance and effect size
		// on a local claim, and a mean per group supplies neither: a count, a
		// sum and a sum of squares do, and all three are aggregates §12.1
		// already permits.
		//
		// Two details are repairs rather than choices.
		//
		// COUNT(measure) as well as COUNT(*), because SUM skips NULL and
		// COUNT(*) does not — using the latter as n reported a large significant
		// difference between two groups that were identical apart from where
		// their blanks were.
		//
		// The measure is CENTRED on its own global mean before being summed.
		// Variance is shift-invariant, and Σx² over uncentred values around ten
		// million loses so much precision to cancellation that the computed
		// variance came out ten times too small — which the test then read as a
		// significant effect. The offset is carried in its own column so the
		// envelope can add it back rather than having to be told.
		render: func(t string, c map[string]string) string {
			centre := fmt.Sprintf("(SELECT AVG(%s) FROM %s)", c["measure"], t)
			return fmt.Sprintf(
				"SELECT %[1]s AS bucket, COUNT(*) AS n, COUNT(%[2]s) AS %[6]s, "+
					"AVG(%[2]s) AS mean, MIN(%[2]s) AS lowest, MAX(%[2]s) AS highest, "+
					"MIN(%[7]s) AS %[8]s, "+
					"SUM(%[2]s - %[7]s) AS %[4]s, "+
					"SUM((%[2]s - %[7]s) * (%[2]s - %[7]s)) AS %[5]s "+
					"FROM %[3]s GROUP BY 1 ORDER BY n DESC",
				c["key"], c["measure"], t,
				stats.SumColumn, stats.SumSqColumn, stats.CountColumn,
				centre, stats.OffsetColumn)
		},
	},
	{
		Kind:     KindTrend,
		Question: "How does the record count, and optionally a numeric column, move over time?",
		Slots: []Slot{
			{Name: "time", Role: RoleTime},
			{Name: "measure", Role: RoleMeasure, Optional: true},
		},
		render: func(t string, c map[string]string) string {
			measure := ""
			if m := c["measure"]; m != "" {
				measure = fmt.Sprintf(", AVG(%s) AS mean", m)
			}
			return fmt.Sprintf(
				"SELECT strftime('%%Y-%%m', %s) AS month, COUNT(*) AS n%s FROM %s "+
					"GROUP BY 1 ORDER BY 1",
				c["time"], measure, t)
		},
	},
}

// TemplateFor looks up a template by kind.
func TemplateFor(k Kind) (Template, bool) {
	for _, t := range Templates {
		if t.Kind == k {
			return t, true
		}
	}
	return Template{}, false
}

// Code is an analysis a template cannot express (§12.1: "regression,
// seasonality decomposition").
//
// This is the one place a model DOES author code, and the sandbox is why that is
// acceptable: §12.3 forbids it authoring SQL because SQL runs against the user's
// database with the user's privileges, and a script here runs with no network,
// no writable filesystem, no capabilities, and a read-only view of one file.
//
// Outputs must be declared. The script's stdout is filtered down to these names
// before anything crosses, so a declaration is not documentation — it is the
// channel.
type Code struct {
	Script  string   `json:"script"`
	Metrics []string `json:"metrics,omitempty"`
	Tests   []string `json:"tests,omitempty"`
}

// Plan is what a model returns: a choice, never a statement.
type Plan struct {
	Connector string            `json:"connector"`
	Table     string            `json:"table"`
	Template  Kind              `json:"template"`
	Columns   map[string]string `json:"columns"`
	// Question is the plan in words, carried into the claim's lead so a reader
	// can see what was asked as well as what was run.
	Question string `json:"question"`

	// Code, when set, replaces the template: the analysis runs in the sandbox
	// instead of as SQL. Only offered to the model when a runtime is usable.
	Code *Code `json:"code,omitempty"`
}

// Render validates a plan against a connector's profile and returns the SQL.
//
// Every identifier in the output came from the profile. The model's strings are
// only ever used as lookup keys, so a column name carrying a quote, a semicolon
// or a whole second statement fails to match a column and the plan is refused —
// there is no path by which it reaches the rendered text.
func Render(c connector.Connector, p Plan) (string, error) {
	tpl, ok := TemplateFor(p.Template)
	if !ok {
		return "", invalid("no template named %q; known: %s", p.Template, strings.Join(kinds(), ", "))
	}
	table, ok := c.Table(p.Table)
	if !ok {
		return "", invalid("connector %q has no table %q; known: %s",
			c.Name, p.Table, strings.Join(tableNames(c), ", "))
	}

	cols := map[string]string{}
	for _, slot := range tpl.Slots {
		name := strings.TrimSpace(p.Columns[slot.Name])
		if name == "" {
			if slot.Optional {
				continue
			}
			return "", invalid("template %s needs a column for %q", tpl.Kind, slot.Name)
		}
		col, ok := table.Column(name)
		if !ok {
			return "", invalid("table %s has no column %q; known: %s",
				table.Name, name, strings.Join(columnNames(table), ", "))
		}
		if err := satisfies(col, slot.Role); err != nil {
			return "", invalid("column %q cannot fill %q: %s", name, slot.Name, err)
		}
		// Taken from the profile rather than from the plan. The two are equal
		// whenever the lookup succeeded, so this is hygiene and not a second
		// defense — the LOOKUP is the defense, and it is what the injection
		// tests exercise.
		quoted, err := ident(col.Name)
		if err != nil {
			return "", err
		}
		cols[slot.Name] = quoted
	}

	qt, err := ident(table.Name)
	if err != nil {
		return "", err
	}
	return tpl.render(qt, cols), nil
}

// satisfies reports whether a column can fill a role.
func satisfies(col connector.Column, role Role) error {
	switch role {
	case RoleMeasure:
		if !col.Type.Numeric() {
			return fmt.Errorf("it holds %s, and a measure must be numeric", col.Type)
		}
	case RoleTime:
		if col.Type != connector.TypeTime {
			return fmt.Errorf("it holds %s, and a time axis must be a timestamp", col.Type)
		}
	case RoleCategory:
		if col.FreeText {
			// §12.1 would withhold every bucket anyway. Refusing here says why.
			return errors.New("it holds free text, and grouping on prose produces " +
				"one bucket per record")
		}
		if col.Type == connector.TypeReal {
			return errors.New("it holds continuous numbers, which do not group")
		}
		if col.Distinct > maxCategories {
			return fmt.Errorf("it has %d distinct values, past the %d a grouping "+
				"column can usefully carry", col.Distinct, maxCategories)
		}
		if col.Distinct < 2 {
			return errors.New("it has fewer than two distinct values, so grouping " +
				"on it says nothing")
		}
	default:
		return fmt.Errorf("unknown role %q", role)
	}
	return nil
}

// identPattern is what a rendered identifier must look like.
//
// Unreachable by construction, and deliberately kept. Every name that gets here
// came out of a connector profile, and both routes into one already constrain
// it: an imported file's headers go through the sanitizer, and an attached
// database's tables are skipped unless they pass the same check. So no input
// reaches this and fails it, and a mutation that makes it accept everything
// breaks no test.
//
// It stays because that is an argument about two other packages rather than
// about this one. If either of them loosens, the failure surfaces here as a
// refusal instead of as a quoted string spliced into a statement.
//
// The character class matches the connector's own rule rather than being
// stricter than it. It was stricter — no leading underscore — which meant a
// table legitimately named `_staging` in someone's database passed
// registration and then could not be queried, with the refusal blaming the
// identifier rather than the disagreement between two rules.
var identPattern = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,63}$`)

func ident(name string) (string, error) {
	if !identPattern.MatchString(name) {
		return "", invalid("refusing to render %q as an identifier", name)
	}
	return `"` + name + `"`, nil
}

func kinds() []string {
	out := make([]string, 0, len(Templates))
	for _, t := range Templates {
		out = append(out, string(t.Kind))
	}
	sort.Strings(out)
	return out
}

func tableNames(c connector.Connector) []string {
	out := make([]string, 0, len(c.Tables))
	for _, t := range c.Tables {
		out = append(out, t.Name)
	}
	return out
}

func columnNames(t connector.Table) []string {
	out := make([]string, 0, len(t.Columns))
	for _, c := range t.Columns {
		out = append(out, c.Name)
	}
	return out
}
