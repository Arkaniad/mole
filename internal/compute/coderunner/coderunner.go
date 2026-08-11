// Package coderunner runs model-authored analysis inside the sandbox and
// decides what may come back (M8, §12.1).
//
// §12.1 is explicit about when this exists: "Where analysis genuinely needs
// row-level data (regression, seasonality decomposition), the computation
// happens inside the sandbox and only its output envelope crosses the gate."
//
// So the sandbox is where the privacy boundary moves to. Inside it there is no
// boundary at all — the script sees the connector's database, read-only, in
// full, because it has no network, no writable filesystem, no capabilities and
// no way out except stdout. Handing it the whole database rather than an
// extracted subset is deliberate: an extract would be a second copy of the
// user's data on the host, written by mole, for no gain.
//
// # The channel out is the whole design
//
// The script's output is UNTRUSTED. It was written by a model, and a model
// reading a prompt-injected page is the threat §3.2 already assumes. So stdout
// is not parsed as "whatever the script wanted to say" — it is matched against
// names the plan DECLARED before the script ran, and values that are finite
// numbers. An undeclared key is dropped and reported; a string value is
// dropped and reported.
//
// That mirrors §12.3's choice for SQL: the model declares, mole looks up.
// Declaring `mean_gap` and emitting `{"ada@example.org": 1}` gets nothing out,
// because the key is not on the list rather than because something inspected it
// for personal data.
//
// # What that does not stop
//
// A determined model could encode a value in the digits of a declared number.
// The channel is bounded — a fixed set of names, one float each — but it is not
// zero, and calling it zero would be the kind of claim this package exists to
// avoid making. What it stops is the realistic case: a script that prints its
// input, or an output shape nobody constrained.
package coderunner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/lajosdeme/mole/internal/compute/sandbox"
	"github.com/lajosdeme/mole/internal/compute/stats"
)

// DefaultImage is the interpreter.
//
// Pinned to a tag rather than a digest, because a tag is what a user will have
// pulled and a digest they do not have is an error message instead of a
// feature. `doctor` reports whether it is present.
const DefaultImage = "python:3.13-slim"

// DefaultCommand reads the script from stdin.
var DefaultCommand = []string{"python3", "-"}

// MountPath is where the connector's database appears inside the container.
const MountPath = "/data/connector.sqlite"

// ErrUnavailable means no usable runtime, which is a normal state and not a
// failure: §12.2 says the sandbox is not the control for the SQL path, so local
// analysis works without this entirely.
var ErrUnavailable = errors.New("coderunner: unavailable")

// Contract is what a plan declared it would emit, established before the script
// runs. Nothing outside it comes back.
type Contract struct {
	// Metrics are named scalars.
	Metrics []string `json:"metrics,omitempty"`
	// Tests are named statistical results.
	Tests []string `json:"tests,omitempty"`
}

func (c Contract) allows(kind, name string) bool {
	list := c.Metrics
	if kind == "test" {
		list = c.Tests
	}
	for _, n := range list {
		if n == name {
			return true
		}
	}
	return false
}

// Empty reports whether the contract permits nothing, in which case running the
// script could only waste a container.
func (c Contract) Empty() bool { return len(c.Metrics) == 0 && len(c.Tests) == 0 }

// Finding is one statistical result the script computed.
//
// The script supplies the numbers; the VERDICT is derived here, using the same
// thresholds as the SQL path. A script reporting its own verdict would be a
// model deciding whether its own result was significant.
type Finding struct {
	Name       string        `json:"name"`
	N          int64         `json:"n"`
	Statistic  float64       `json:"statistic"`
	P          float64       `json:"p"`
	EffectSize float64       `json:"effect_size"`
	Verdict    stats.Verdict `json:"verdict"`
}

// Output is what came back and is allowed to cross.
type Output struct {
	Metrics  map[string]float64 `json:"metrics,omitempty"`
	Findings []Finding          `json:"findings,omitempty"`

	// Dropped names refused outputs the plan DID declare — mole's own strings,
	// safe to repeat, and the case a model needs to see to fix its script.
	Dropped []string `json:"dropped,omitempty"`
	// Undeclared counts refused outputs the plan did not declare. A count and
	// not a list: an undeclared key is text the script chose, and repeating it
	// would make the key a channel out of the sandbox.
	Undeclared int      `json:"undeclared,omitempty"`
	Notes      []string `json:"notes,omitempty"`
}

// Empty reports whether anything crossed.
func (o Output) Empty() bool { return len(o.Metrics) == 0 && len(o.Findings) == 0 }

// -----------------------------------------------------------------------------
// Parsing what came back
// -----------------------------------------------------------------------------

// reply is the shape a script must print. Anything else is a parse failure,
// which is reported to the model as such — an unconstrained "print what you
// like" channel is the thing this package exists to not have.
type reply struct {
	Metrics map[string]json.RawMessage `json:"metrics"`
	Tests   []struct {
		Name       string          `json:"name"`
		N          int64           `json:"n"`
		Statistic  json.RawMessage `json:"statistic"`
		P          json.RawMessage `json:"p"`
		EffectSize json.RawMessage `json:"effect_size"`
	} `json:"tests"`
}

// Parse filters a script's stdout down to what the contract declared.
func Parse(raw string, c Contract) (Output, error) {
	body := extractObject(raw)
	if body == "" {
		return Output{}, fmt.Errorf("coderunner: the script printed no JSON object")
	}
	var r reply
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		return Output{}, fmt.Errorf("coderunner: parse output: %w", err)
	}

	out := Output{Metrics: map[string]float64{}}
	for name, rawVal := range r.Metrics {
		switch {
		case !c.allows("metric", name):
			// The NAME is not recorded, only the fact of a refusal.
			//
			// It used to be appended verbatim, joined into a note, and printed
			// into the passage handed to the miner — so a script could return
			// anything it liked by putting it in a KEY instead of a value. A
			// review probe got 12KB of records out through this, past a package
			// comment claiming that emitting {"ada@example.org": 1} "gets
			// nothing out, because the key is not on the list". The key was not
			// on the list and got out anyway.
			out.Undeclared++
		default:
			v, ok := finiteNumber(rawVal)
			if !ok {
				// A string where a number was declared. Dropped rather than
				// coerced: a value that is not a number is the shape a row
				// arrives in.
				out.Dropped = append(out.Dropped, name+" (not a finite number)")
				continue
			}
			out.Metrics[name] = v
		}
	}

	for _, t := range r.Tests {
		if !c.allows("test", t.Name) {
			out.Undeclared++
			continue
		}
		stat, okS := finiteNumber(t.Statistic)
		p, okP := finiteNumber(t.P)
		effect, okE := finiteNumber(t.EffectSize)
		if !okS || !okP || !okE {
			out.Dropped = append(out.Dropped, t.Name+" (a figure was not a finite number)")
			continue
		}
		if t.N < 2 {
			out.Dropped = append(out.Dropped, t.Name+" (n below 2)")
			continue
		}
		if p < 0 || p > 1 {
			// The verdict is derived from the script's own p and n, so "the
			// verdict is not the script's to decide" only holds while its inputs
			// are checked. They were not: p = -1 produced "statistically
			// significant (p = <0.001)" and p = 7 produced a p-value of 7.000 in
			// a sentence a claim would quote.
			out.Dropped = append(out.Dropped, t.Name+" (p is not a probability)")
			continue
		}
		out.Findings = append(out.Findings, Finding{
			Name: t.Name, N: t.N, Statistic: stat, P: p, EffectSize: effect,
			// §4's thresholds, applied here rather than taken from the script.
			Verdict: verdictFor(p, t.N),
		})
	}

	if out.Undeclared > 0 {
		out.Notes = append(out.Notes, fmt.Sprintf(
			"%d output(s) the plan did not declare were refused; their names are not "+
				"repeated here, because a name the script chose is text the script chose",
			out.Undeclared))
	}
	if len(out.Dropped) > 0 {
		sort.Strings(out.Dropped)
		out.Notes = append(out.Notes, fmt.Sprintf(
			"%d declared output(s) were refused for not being finite numbers: %s",
			len(out.Dropped), strings.Join(out.Dropped, ", ")))
	}
	return out, nil
}

// verdictFor mirrors the SQL path's rule so the two cannot disagree about what
// counts as evidence. stats owns the thresholds.
func verdictFor(p float64, n int64) stats.Verdict {
	if n < stats.MinGroupN {
		return stats.Underpowered
	}
	if p < stats.Alpha {
		return stats.Significant
	}
	return stats.NotSignificant
}

func finiteNumber(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		return 0, false
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		// Unreachable through encoding/json, and kept anyway. Measured: JSON has
		// no literal for Infinity or NaN, and an overflowing number like 1e999
		// fails to unmarshal rather than saturating — so the decoder above is
		// what actually refuses these, and removing this branch breaks no test.
		//
		// It stays because that is an argument about somebody else's decoder. A
		// p-value of +Inf reaching a claim would be a number a reader trusts,
		// and one comparison is a small price for not depending on a library's
		// overflow behaviour staying the same.
		return 0, false
	}
	return f, true
}

// extractObject finds the outermost JSON object in the output.
//
// Lenient about surroundings for the same reason the claim miner is: a model
// told to print JSON prints a sentence and then JSON often enough that refusing
// would be a fight rather than a boundary. The CONTENT is not treated leniently.
func extractObject(raw string) string {
	// Every `{` as a candidate, longest match first, and the one that PARSES
	// wins.
	//
	// First-brace-to-last-brace looked lenient and was not: a Python warning
	// mentioning a dict, or a `print(dict)` before the result, or a trailing
	// "all done (ok}" each made the whole run unparseable — and Python prints
	// exactly those. The doc claimed leniency about surroundings; this delivers
	// it, while the CONTENT stays as strict as it was.
	for start := 0; start < len(raw); start++ {
		if raw[start] != '{' {
			continue
		}
		for end := len(raw) - 1; end > start; end-- {
			if raw[end] != '}' {
				continue
			}
			candidate := raw[start : end+1]
			if json.Valid([]byte(candidate)) {
				return candidate
			}
		}
	}
	return ""
}

// -----------------------------------------------------------------------------
// Rendering
// -----------------------------------------------------------------------------

// Text renders the output as the passage a claim is mined from and quoted
// against, exactly as an AggregateEnvelope does — so §11.5 applies to a claim
// about a regression the same way it applies to one about a web page.
func (o Output) Text(script string) string {
	var b strings.Builder
	b.WriteString("Result of an analysis run inside the sandbox, over local data.\n")
	b.WriteString("No network was available to it and its only output was the figures below.\n")

	if len(o.Metrics) > 0 {
		b.WriteString("\nMetrics:\n")
		names := make([]string, 0, len(o.Metrics))
		for n := range o.Metrics {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			// A sentence rather than `name = value`, and not for style: §11.5
			// requires a quote of at least minQuoteLen characters, and
			// "amplitude = 12.5000" is nineteen. Rendered that way, a claim
			// about a single metric could not be cited at all — the quote check
			// dropped every one of them, which is how this was found.
			fmt.Fprintf(&b, "  The analysis computed %s = %s over the local data.\n",
				n, num(o.Metrics[n]))
		}
	}

	if len(o.Findings) > 0 {
		b.WriteString("\nStatistical results:\n")
		for _, f := range o.Findings {
			fmt.Fprintf(&b, "  %s\n", f.Summary())
		}
	}

	if len(o.Notes) > 0 {
		b.WriteString("\nWhat was refused:\n")
		for _, n := range o.Notes {
			b.WriteString("  - " + n + "\n")
		}
	}
	return b.String()
}

// Summary is the sentence a claim has to quote, phrased like the SQL path's so
// that a reader cannot tell from the wording which one produced it — the
// evidence differs, the standard does not.
func (f Finding) Summary() string {
	head := fmt.Sprintf("%s: statistic %s, n = %d", f.Name, num(f.Statistic), f.N)
	switch f.Verdict {
	case stats.Underpowered:
		return head + fmt.Sprintf("; UNDERPOWERED — fewer than %d records, so this is "+
			"not evidence either way", stats.MinGroupN)
	case stats.Significant:
		return head + fmt.Sprintf("; statistically significant (p = %s), effect size %s",
			pval(f.P), num(f.EffectSize))
	default:
		return head + fmt.Sprintf("; NOT distinguishable from chance (p = %s)", pval(f.P))
	}
}

func num(f float64) string {
	if f == math.Trunc(f) && math.Abs(f) < 1e15 {
		return fmt.Sprintf("%.0f", f)
	}
	return fmt.Sprintf("%.4f", f)
}

func pval(p float64) string {
	if p < 0.001 {
		return "<0.001"
	}
	return fmt.Sprintf("%.3f", p)
}

// -----------------------------------------------------------------------------
// The runner
// -----------------------------------------------------------------------------

// Runner executes an analysis. Separate from the parsing so a caller can be
// tested without a container.
type Runner interface {
	Analyze(ctx context.Context, req Request) (Output, error)
}

// Request is one analysis.
type Request struct {
	// DBPath is the connector database, mounted read-only.
	DBPath string
	// Query is the statement the script is told to run. Already through
	// sqlguard: the script could run anything against the mounted database, so
	// this is guidance rather than a control — the container is the control.
	Query    string
	Script   string
	Contract Contract
}

// Sandboxed is the real Runner.
type Sandboxed struct {
	Report sandbox.Report
	Image  string
	// Command defaults to DefaultCommand.
	Command []string
	Limits  sandbox.Limits
}

// Analyze runs the script and returns only what the contract declared.
func (s Sandboxed) Analyze(ctx context.Context, req Request) (Output, error) {
	if !s.Report.Usable {
		return Output{}, fmt.Errorf("%w: %s", ErrUnavailable, s.Report.Detail)
	}
	if req.Contract.Empty() {
		return Output{}, errors.New("coderunner: the plan declared no outputs, so the " +
			"script could produce nothing that may cross")
	}

	image := s.Image
	if image == "" {
		image = DefaultImage
	}
	command := s.Command
	if len(command) == 0 {
		command = DefaultCommand
	}

	res, err := s.Report.Run(ctx, sandbox.Spec{
		Image:   image,
		Command: command,
		Script:  req.Script,
		Mounts:  []sandbox.Mount{{HostPath: req.DBPath, ContainerPath: MountPath}},
		Env: []string{
			"MOLE_DB=" + MountPath,
			"MOLE_QUERY=" + req.Query,
		},
		Limits: s.Limits,
	})
	if err != nil {
		return Output{}, err
	}

	switch {
	case res.Cancelled:
		return Output{}, fmt.Errorf("coderunner: the analysis was cancelled before it finished")
	case res.TimedOut:
		return Output{}, fmt.Errorf("coderunner: the analysis exceeded its wallclock limit")
	case res.ExitCode != 0:
		// Before Truncated, and the order matters: a script that crashed after
		// printing a lot reported "printed more than the output limit" and its
		// traceback was never shown, so the model could not fix what it broke.
		return Output{}, fmt.Errorf("coderunner: the analysis failed (exit %d): %s",
			res.ExitCode, lastLines(res.Stderr))
	case res.Truncated:
		// Refused rather than parsed. A script that printed more than the cap
		// was not producing a handful of statistics, and parsing the prefix of
		// whatever it was doing is not a safe way to find out what.
		return Output{}, fmt.Errorf("coderunner: the analysis printed more than the " +
			"output limit; it was not producing a summary")
	}

	out, err := Parse(res.Stdout, req.Contract)
	if err != nil {
		return Output{}, err
	}
	return out, nil
}

// lastLines is the tail of stderr, which is where a traceback's cause is. It was
// called firstLines and returned the last three, which is the more useful
// behaviour and the wrong name.
func lastLines(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(no output)"
	}
	lines := strings.Split(s, "\n")
	if len(lines) > 3 {
		lines = lines[len(lines)-3:]
	}
	joined := strings.Join(lines, " / ")
	if len(joined) > 300 {
		joined = joined[:300] + "…"
	}
	return joined
}
