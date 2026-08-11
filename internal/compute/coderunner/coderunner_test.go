package coderunner_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lajosdeme/mole/internal/compute/coderunner"
	"github.com/lajosdeme/mole/internal/compute/sandbox"
	"github.com/lajosdeme/mole/internal/compute/stats"
	_ "modernc.org/sqlite"
)

// M8 slice 7.
//
// The parsing tests run everywhere: the channel out is the design, so what it
// refuses is checked without needing a runtime. The container tests run a real
// sandbox and are skipped without one — they establish that the mount is
// read-only, that the wallclock limit kills a hanging script, and that a script
// printing its input is refused rather than parsed.
//
// The default image is python:3.13-slim and could not be pulled in the
// environment this was written in (no registry access), so the container tests
// use a locally present image with a shell. That verifies the MECHANISM —
// mount, stdin, stdout cap, timeout, exit code — and not the interpreter. Said
// plainly rather than left for somebody to discover.

// -----------------------------------------------------------------------------
// The channel out
// -----------------------------------------------------------------------------

func TestOnlyDeclaredNamesCross(t *testing.T) {
	contract := coderunner.Contract{
		Metrics: []string{"weekly_amplitude", "trend_slope"},
		Tests:   []string{"seasonality"},
	}

	out, err := coderunner.Parse(`{
	  "metrics": {
	    "weekly_amplitude": 12.5,
	    "trend_slope": -0.25,
	    "ada@example.org": 1,
	    "note_for_row_7": 3
	  },
	  "tests": [
	    {"name": "seasonality", "n": 8760, "statistic": 4.2, "p": 0.0003, "effect_size": 0.6},
	    {"name": "undeclared_test", "n": 100, "statistic": 1, "p": 0.4, "effect_size": 0.1}
	  ]
	}`, contract)
	if err != nil {
		t.Fatal(err)
	}

	if len(out.Metrics) != 2 {
		t.Fatalf("metrics = %v, want exactly the two declared", out.Metrics)
	}
	for _, leaked := range []string{"ada@example.org", "note_for_row_7"} {
		if _, ok := out.Metrics[leaked]; ok {
			t.Errorf("an undeclared key crossed: %q", leaked)
		}
	}
	if len(out.Findings) != 1 || out.Findings[0].Name != "seasonality" {
		t.Fatalf("findings = %+v, want only the declared test", out.Findings)
	}
	// Refusals are reported — as a COUNT for anything the plan did not declare.
	// The names are the script's own text, and repeating them made the key a
	// channel out of the sandbox: a review probe returned 12KB of records
	// through it.
	if out.Undeclared != 3 {
		t.Errorf("Undeclared = %d, want 3", out.Undeclared)
	}
	if len(out.Dropped) != 0 {
		t.Errorf("Dropped = %v, want empty — every refusal here was undeclared", out.Dropped)
	}
	if len(out.Notes) == 0 || !strings.Contains(out.Notes[0], "did not declare") {
		t.Errorf("nothing explains the refusals: %v", out.Notes)
	}

	// And nothing the script chose appears anywhere in what crosses.
	passage := out.Text("")
	for _, chosen := range []string{"ada@example.org", "note_for_row_7", "undeclared_test"} {
		if strings.Contains(passage, chosen) {
			t.Errorf("text the script chose reached the passage: %q\n%s", chosen, passage)
		}
	}
}

// TestAnUndeclaredKeyIsNotRepeatedAnywhere is the review finding, kept as a
// property rather than as a note in a commit message.
//
// The whole output contract rests on a script being unable to choose what text
// comes back. A key is text the script chose, so echoing a refused key defeats
// the contract exactly as accepting its value would — and the package comment
// claimed the opposite in the specific case this asserts.
func TestAnUndeclaredKeyIsNotRepeatedAnywhere(t *testing.T) {
	const secret = "patient_7 ada7@example.org cardiology 1987-04-02 91234"
	out, err := coderunner.Parse(
		`{"metrics":{"ok":1,"`+secret+`":0},`+
			`"tests":[{"name":"`+secret+`","n":50,"statistic":1,"p":0.01,"effect_size":0.3}]}`,
		coderunner.Contract{Metrics: []string{"ok"}, Tests: []string{"seasonality"}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Undeclared != 2 {
		t.Errorf("Undeclared = %d, want 2", out.Undeclared)
	}
	rendered, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, where := range []string{string(rendered), out.Text("")} {
		if strings.Contains(where, "ada7@example.org") {
			t.Fatalf("the script's own text crossed:\n%s", where)
		}
	}
}

// TestAValueThatIsNotANumberIsRefused. A string where a number was declared is
// the shape a row arrives in, and the declaration is what makes that detectable
// without inspecting the value.
func TestAValueThatIsNotANumberIsRefused(t *testing.T) {
	contract := coderunner.Contract{Metrics: []string{"top_customer", "ratio"}}

	out, err := coderunner.Parse(`{"metrics": {
	  "top_customer": "ada@example.org",
	  "ratio": 0.5
	}}`, contract)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := out.Metrics["top_customer"]; ok {
		t.Error("a declared name carrying a string crossed")
	}
	if out.Metrics["ratio"] != 0.5 {
		t.Errorf("ratio = %v, want 0.5", out.Metrics["ratio"])
	}
	if !strings.Contains(strings.Join(out.Dropped, " "), "not a finite number") {
		t.Errorf("the refusal does not say why: %v", out.Dropped)
	}
}

// TestNonFiniteFiguresAreRefused.
//
// Python's json.dump writes Infinity and NaN as bare words, which are not JSON;
// a writer that quotes them produces a string. Either way a p-value of Infinity
// is not a result, and neither spelling crosses.
//
// What refuses them is the DECODER, not the explicit finite check beside it:
// JSON has no literal for either, and an overflowing number fails to unmarshal
// rather than saturating to ±Inf. Measured, and noted in finiteNumber — the
// check is kept as a guard against a library's overflow behaviour changing, not
// because anything reaches it today.
func TestNonFiniteFiguresAreRefused(t *testing.T) {
	for _, tc := range []struct{ why, raw string }{
		{"quoted by the writer", `{"metrics": {"a": "Infinity", "b": "NaN", "c": 1}}`},
		{"overflowing literal", `{"metrics": {"a": 1e999, "b": -1e999, "c": 1}}`},
	} {
		t.Run(tc.why, func(t *testing.T) {
			out, err := coderunner.Parse(tc.raw, coderunner.Contract{Metrics: []string{"a", "b", "c"}})
			if err != nil {
				t.Fatal(err)
			}
			if len(out.Metrics) != 1 {
				t.Errorf("metrics = %v, want only the finite one", out.Metrics)
			}
			if _, ok := out.Metrics["c"]; !ok {
				t.Error("the finite figure was dropped along with the others")
			}
		})
	}
}

// TestTheVerdictIsNotTheScriptsToDecide.
//
// The script supplies n, the statistic and p; the verdict is derived here with
// the same thresholds the SQL path uses. A script reporting its own verdict
// would be a model deciding whether its own result was significant.
func TestTheVerdictIsNotTheScriptsToDecide(t *testing.T) {
	for _, tc := range []struct {
		why  string
		n    int64
		p    float64
		want stats.Verdict
	}{
		{"significant with enough records", 500, 0.001, stats.Significant},
		{"not significant with enough records", 500, 0.4, stats.NotSignificant},
		// p well under alpha and only twelve records: the same refusal the SQL
		// path makes, so the two paths cannot disagree about what is evidence.
		{"underpowered whatever p says", 12, 0.001, stats.Underpowered},
	} {
		t.Run(tc.why, func(t *testing.T) {
			out, err := coderunner.Parse(mustTestJSON("seasonality", tc.n, tc.p),
				coderunner.Contract{Tests: []string{"seasonality"}})
			if err != nil {
				t.Fatal(err)
			}
			if len(out.Findings) != 1 {
				t.Fatalf("findings = %+v", out.Findings)
			}
			if got := out.Findings[0].Verdict; got != tc.want {
				t.Errorf("verdict = %q, want %q", got, tc.want)
			}
			// The sentence is what a claim must quote, so the verdict has to be
			// legible in it and not only in a field.
			summary := out.Findings[0].Summary()
			switch tc.want {
			case stats.Underpowered:
				if !strings.Contains(summary, "UNDERPOWERED") {
					t.Errorf("summary hides the verdict: %s", summary)
				}
			case stats.NotSignificant:
				if !strings.Contains(summary, "NOT distinguishable") {
					t.Errorf("summary hides the verdict: %s", summary)
				}
			}
		})
	}
}

func mustTestJSON(name string, n int64, p float64) string {
	return `{"tests":[{"name":"` + name + `","n":` + itoa(n) +
		`,"statistic":3.1,"p":` + ftoa(p) + `,"effect_size":0.4}]}`
}

// TestOutputWithNoJSONIsAnError, rather than an empty result. A script that
// crashed after printing a traceback must not read as one that found nothing.
func TestOutputWithNoJSONIsAnError(t *testing.T) {
	for _, raw := range []string{
		"",
		"Traceback (most recent call last):\n  File \"<stdin>\", line 3\nKeyError: 'spend'",
		"done",
	} {
		if _, err := coderunner.Parse(raw, coderunner.Contract{Metrics: []string{"a"}}); err == nil {
			t.Errorf("no error for %q", raw)
		}
	}
}

// TestAnEmptyContractIsRefusedBeforeAContainerStarts. A script that may emit
// nothing can only waste a container.
func TestAnEmptyContractIsRefusedBeforeAContainerStarts(t *testing.T) {
	s := coderunner.Sandboxed{Report: sandbox.Report{Usable: true, Runtime: "docker"}}
	_, err := s.Analyze(context.Background(), coderunner.Request{
		DBPath: "/nonexistent", Script: "print(1)",
	})
	if err == nil {
		t.Fatal("an empty contract was accepted")
	}
	if !strings.Contains(err.Error(), "declared no outputs") {
		t.Errorf("err = %v, want the contract to be named", err)
	}
}

// TestNoRuntimeIsReportedAsUnavailable, not as a failure — §12.2 says the
// sandbox is not the control for the SQL path, so local analysis works without
// this entirely and the caller has to be able to tell the difference.
func TestNoRuntimeIsReportedAsUnavailable(t *testing.T) {
	s := coderunner.Sandboxed{Report: sandbox.Report{Usable: false, Detail: "none found"}}
	_, err := s.Analyze(context.Background(), coderunner.Request{
		Script:   "print(1)",
		Contract: coderunner.Contract{Metrics: []string{"a"}},
	})
	if err == nil {
		t.Fatal("Analyze succeeded with no runtime")
	}
	if !errors.Is(err, coderunner.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable so a caller can tell this from a "+
			"failed analysis", err)
	}
}

// -----------------------------------------------------------------------------
// A real container
// -----------------------------------------------------------------------------

// shellRunner builds a Sandboxed that runs a POSIX shell script instead of
// Python, using an image already present locally.
//
// The mechanism under test is the container — mount, stdin, output cap,
// wallclock, exit code — and none of it is interpreter-specific. Python is what
// a model will actually write, and python:3.13-slim could not be pulled where
// this was written; that limit is in the package's known gaps rather than
// papered over with a test that pretends otherwise.
func shellRunner(t *testing.T) (coderunner.Sandboxed, bool) {
	t.Helper()
	rep := sandbox.Detect(context.Background())
	if !rep.Usable {
		t.Skipf("no usable container runtime: %s", rep.Detail)
	}
	image := localShellImage(t, string(rep.Runtime))
	if image == "" {
		t.Skip("no alpine/busybox image present locally; set MOLE_SANDBOX_TEST_IMAGE")
	}
	return coderunner.Sandboxed{
		Report:  rep,
		Image:   image,
		Command: []string{"sh"},
		Limits:  sandbox.Limits{CPUs: 1, MemoryMB: 256, Pids: 32, Wallclock: 20 * time.Second},
	}, true
}

func localShellImage(t *testing.T, runtime string) string {
	t.Helper()
	if img := os.Getenv("MOLE_SANDBOX_TEST_IMAGE"); img != "" {
		return img
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, runtime, "images", "--format", "{{.Repository}}:{{.Tag}}").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		name := strings.TrimSpace(line)
		if name == "" || strings.HasSuffix(name, ":<none>") {
			continue
		}
		if strings.Contains(name, "alpine") || strings.Contains(name, "busybox") {
			return name
		}
	}
	return ""
}

// connectorDB writes a small database to mount.
func connectorDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "connector.sqlite")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{
		`CREATE TABLE samples (region TEXT, spend REAL)`,
		`INSERT INTO samples VALUES ('north', 100), ('south', 40)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

// TestAnAnalysisRunsAndOnlyItsFiguresComeBack, in a real container.
func TestAnAnalysisRunsAndOnlyItsFiguresComeBack(t *testing.T) {
	runner, ok := shellRunner(t)
	if !ok {
		return
	}

	out, err := runner.Analyze(context.Background(), coderunner.Request{
		DBPath: connectorDB(t),
		Query:  `SELECT region, COUNT(*) FROM samples GROUP BY 1`,
		// Emits one declared metric and one undeclared key holding something
		// that looks like a row.
		Script:   `echo '{"metrics": {"rows_seen": 2, "leaked_note": 1}}'`,
		Contract: coderunner.Contract{Metrics: []string{"rows_seen"}},
	})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if out.Metrics["rows_seen"] != 2 {
		t.Errorf("rows_seen = %v, want 2", out.Metrics["rows_seen"])
	}
	if _, ok := out.Metrics["leaked_note"]; ok {
		t.Error("an undeclared key crossed out of a real container")
	}
}

// TestTheMountedDatabaseIsReadableAndNotWritable.
//
// Readable, because an analysis that cannot see the data is useless; not
// writable, because model-authored code must not be able to alter the user's
// database — and the connector's own read-only handle does not apply here, the
// container is what enforces it.
func TestTheMountedDatabaseIsReadableAndNotWritable(t *testing.T) {
	runner, ok := shellRunner(t)
	if !ok {
		return
	}
	path := connectorDB(t)
	// World-writable on purpose. The container runs as uid 65534, so a file
	// owned by the invoking user is unwritable whatever the mount says — which
	// made an earlier version of this test pass with the mount changed to :rw.
	// The uid and the read-only mount overlap; this makes the mount the only
	// thing left standing.
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}

	out, err := runner.Analyze(context.Background(), coderunner.Request{
		DBPath: path,
		Script: `
		  readable=0
		  [ -r "$MOLE_DB" ] && readable=1
		  writable=0
		  if echo x >> "$MOLE_DB" 2>/dev/null; then writable=1; fi
		  echo "{\"metrics\": {\"readable\": $readable, \"writable\": $writable}}"
		`,
		Contract: coderunner.Contract{Metrics: []string{"readable", "writable"}},
	})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if out.Metrics["readable"] != 1 {
		t.Error("the mounted database was not readable inside the container")
	}
	if out.Metrics["writable"] != 0 {
		t.Fatal("the mounted database was WRITABLE inside the container; " +
			"model-authored code can alter the user's data")
	}

	// And the file on the host is untouched.
	info, err := os.Stat(path)
	if err != nil || info.Size() == 0 {
		t.Fatalf("the host database is gone or empty: %v", err)
	}
}

// TestAScriptThatPrintsItsInputIsRefused.
//
// The realistic failure this package exists for: not a hostile script, but one
// that dumps what it read. Refused rather than parsed — the prefix of whatever
// it was doing is not a safe way to find out what.
func TestAScriptThatPrintsItsInputIsRefused(t *testing.T) {
	runner, ok := shellRunner(t)
	if !ok {
		return
	}
	out, err := runner.Analyze(context.Background(), coderunner.Request{
		DBPath: connectorDB(t),
		// Half a megabyte, past the 256KB cap.
		Script:   `i=0; while [ $i -lt 4000 ]; do printf '%0128d\n' $i; i=$((i+1)); done`,
		Contract: coderunner.Contract{Metrics: []string{"x"}},
	})
	if err == nil {
		t.Fatalf("a script that printed half a megabyte was accepted: %+v", out)
	}
	if !strings.Contains(err.Error(), "output limit") {
		t.Errorf("err = %v, want the output limit to be named", err)
	}
}

// TestAHangingScriptIsKilled. The wallclock limit is enforced outside the
// container, because a limit the script could choose to ignore is not one.
func TestAHangingScriptIsKilled(t *testing.T) {
	runner, ok := shellRunner(t)
	if !ok {
		return
	}
	runner.Limits.Wallclock = 3 * time.Second

	start := time.Now()
	_, err := runner.Analyze(context.Background(), coderunner.Request{
		DBPath:   connectorDB(t),
		Script:   `while true; do :; done`,
		Contract: coderunner.Contract{Metrics: []string{"x"}},
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a script that never finished was accepted")
	}
	if !strings.Contains(err.Error(), "wallclock") {
		t.Errorf("err = %v, want the wallclock limit to be named", err)
	}
	if elapsed > 30*time.Second {
		t.Errorf("took %v to give up on a 3s limit", elapsed)
	}
}

// TestAFailingScriptReportsItsStderr. A model whose code raised has to be told
// what it raised, or the next attempt is the same attempt.
func TestAFailingScriptReportsItsStderr(t *testing.T) {
	runner, ok := shellRunner(t)
	if !ok {
		return
	}
	_, err := runner.Analyze(context.Background(), coderunner.Request{
		DBPath:   connectorDB(t),
		Script:   `echo "column spend does not exist" >&2; exit 3`,
		Contract: coderunner.Contract{Metrics: []string{"x"}},
	})
	if err == nil {
		t.Fatal("a failing script was accepted")
	}
	if !strings.Contains(err.Error(), "column spend does not exist") {
		t.Errorf("the error does not carry the script's own message: %v", err)
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

func ftoa(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// -----------------------------------------------------------------------------
// M8 review regressions
// -----------------------------------------------------------------------------

// TestPythonNoiseAroundTheJSONIsTolerated.
//
// extractObject took the first `{` to the last `}`, which looked lenient and was
// not: a warning mentioning a dict, a print(dict) before the result, or a
// trailing "all done (ok}" each made the whole run unparseable. Python prints
// exactly those.
func TestPythonNoiseAroundTheJSONIsTolerated(t *testing.T) {
	for _, tc := range []struct{ why, raw string }{
		{"a brace in a preamble", "note {see below}\n" + `{"metrics":{"ok":1}}`},
		{"a brace after the result", `{"metrics":{"ok":1}}` + "\nall done (ok}"},
		{"a printed dict", "warning: dict {'a': 1} is deprecated\n" + `{"metrics":{"ok":1}}`},
		{"nothing around it", `{"metrics":{"ok":1}}`},
	} {
		t.Run(tc.why, func(t *testing.T) {
			out, err := coderunner.Parse(tc.raw, coderunner.Contract{Metrics: []string{"ok"}})
			if err != nil {
				t.Fatalf("the whole run was discarded: %v", err)
			}
			if out.Metrics["ok"] != 1 {
				t.Errorf("metrics = %v, want ok=1", out.Metrics)
			}
		})
	}
}

// TestAPValueMustBeAProbability.
//
// "The verdict is derived here rather than by the script" only holds while its
// inputs are checked, and they were not: p = -1 produced "statistically
// significant (p = <0.001)" and p = 7 produced a p-value of 7.000 in a sentence a
// claim would quote.
func TestAPValueMustBeAProbability(t *testing.T) {
	for _, p := range []string{"-1", "7", "-0.0001", "1.5"} {
		out, err := coderunner.Parse(
			`{"tests":[{"name":"s","n":50,"statistic":1,"p":`+p+`,"effect_size":0.1}]}`,
			coderunner.Contract{Tests: []string{"s"}})
		if err != nil {
			t.Fatal(err)
		}
		if len(out.Findings) != 0 {
			t.Errorf("p = %s was accepted: %+v", p, out.Findings)
		}
		if !strings.Contains(strings.Join(out.Dropped, " "), "not a probability") {
			t.Errorf("p = %s: the refusal does not say why: %v", p, out.Dropped)
		}
	}
}

// TestAFailingScriptShowsItsErrorEvenWhenItPrintedTooMuch.
//
// Truncated was checked before ExitCode, so a script that crashed after printing
// a lot reported "printed more than the output limit" and its traceback was never
// shown — leaving the model to make the same attempt again.
func TestAFailingScriptShowsItsErrorEvenWhenItPrintedTooMuch(t *testing.T) {
	runner, ok := shellRunner(t)
	if !ok {
		return
	}
	_, err := runner.Analyze(context.Background(), coderunner.Request{
		DBPath: connectorDB(t),
		Script: `i=0; while [ $i -lt 4000 ]; do printf '%0128d\n' $i; i=$((i+1)); done
		         echo "column spend does not exist" >&2; exit 3`,
		Contract: coderunner.Contract{Metrics: []string{"x"}},
	})
	if err == nil {
		t.Fatal("accepted")
	}
	if !strings.Contains(err.Error(), "column spend does not exist") {
		t.Errorf("the script's own message is missing: %v", err)
	}
}

// TestACancelledAnalysisIsNotReportedAsATimeout, because they mean opposite
// things about the script: one ran too long, the other never got the chance.
func TestACancelledAnalysisIsNotReportedAsATimeout(t *testing.T) {
	runner, ok := shellRunner(t)
	if !ok {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := runner.Analyze(ctx, coderunner.Request{
		DBPath:   connectorDB(t),
		Script:   `while true; do :; done`,
		Contract: coderunner.Contract{Metrics: []string{"x"}},
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("accepted")
	}
	if strings.Contains(err.Error(), "wallclock") {
		t.Errorf("cancellation was reported as a timeout: %v", err)
	}
	if !strings.Contains(err.Error(), "cancelled") {
		t.Errorf("err = %v, want it to say the run was cancelled", err)
	}
	// The caller must not wait on the container's cleanup. It used to block for
	// thirty seconds on pipe holders plus ten in killContainer.
	if elapsed > 15*time.Second {
		t.Errorf("a cancelled caller waited %v", elapsed)
	}
}

// TestAColonInTheMountPathIsRefusedWithTheFix. A data folder named 2024:Q1 used
// to fail with a raw `too many colons` runtime error.
func TestAColonInTheMountPathIsRefusedWithTheFix(t *testing.T) {
	rep := sandbox.Detect(context.Background())
	if !rep.Usable {
		t.Skipf("no usable runtime: %s", rep.Detail)
	}
	dir := filepath.Join(t.TempDir(), "2024:Q1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	runner := coderunner.Sandboxed{Report: rep, Image: "alpine", Command: []string{"sh"}}
	_, err := runner.Analyze(context.Background(), coderunner.Request{
		DBPath:   filepath.Join(dir, "connector.sqlite"),
		Script:   `echo '{"metrics":{"x":1}}'`,
		Contract: coderunner.Contract{Metrics: []string{"x"}},
	})
	if err == nil {
		t.Fatal("accepted a path a mount spec cannot express")
	}
	if !strings.Contains(err.Error(), "colon") {
		t.Errorf("the error does not name the problem: %v", err)
	}
	if !strings.Contains(err.Error(), "rename") {
		t.Errorf("the error does not say what to do: %v", err)
	}
}
