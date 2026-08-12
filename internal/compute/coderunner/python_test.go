package coderunner_test

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/lajosdeme/mole/internal/compute/coderunner"
	"github.com/lajosdeme/mole/internal/compute/sandbox"
)

// The interpreter path, closed.
//
// Every other container test in this package runs a POSIX shell script, because
// python:3.13-slim could not be pulled where the package was written. The
// mechanism under test there is the container — mount, stdin, output cap,
// wallclock, exit code — and none of it is interpreter-specific, but "nothing has
// run actual Python against a connector database" was a stated gap, and a
// DefaultCommand of `python3 -` that has never been executed is a claim rather
// than a verified path.
//
// These tests run the real default command against the real image, reading the
// mounted database with Python's own sqlite3 module. Skipped, not failed, where
// the image is absent: it is a 130MB pull and CI that lacks it should say so
// rather than go red.

// pythonRunner is a Sandboxed with the DEFAULT command — the one a model's script
// will actually be run under.
func pythonRunner(t *testing.T) coderunner.Sandboxed {
	t.Helper()
	rep := sandbox.Detect(context.Background())
	if !rep.Usable {
		t.Skipf("no usable container runtime: %s", rep.Detail)
	}
	image := pythonImage(t, string(rep.Runtime))
	if image == "" {
		t.Skip("no python image present locally; `docker pull python:3.13-slim` " +
			"or set MOLE_PYTHON_TEST_IMAGE")
	}
	return coderunner.Sandboxed{
		Report: rep,
		Image:  image,
		// Command deliberately unset: DefaultCommand() is the thing being verified.
		Limits: sandbox.Limits{CPUs: 1, MemoryMB: 256, Pids: 64, Wallclock: 60 * time.Second},
	}
}

func pythonImage(t *testing.T, runtime string) string {
	t.Helper()
	if img := os.Getenv("MOLE_PYTHON_TEST_IMAGE"); img != "" {
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
		// The REPOSITORY component, not the whole reference. podman lists images
		// fully qualified — `docker.io/library/python:3.13-slim` — so a prefix test
		// against the whole string skipped every Python test on a podman machine
		// while reporting the suite green. Docker lists them short, which is why it
		// went unnoticed: the helper encoded one runtime's output format.
		repo := name
		if i := strings.LastIndex(repo, "/"); i >= 0 {
			repo = repo[i+1:]
		}
		// Still anchored, so `mypython-tools` does not qualify.
		if strings.HasPrefix(repo, "python:") || strings.HasPrefix(repo, "python@") {
			return name
		}
	}
	return ""
}

// TestPythonReadsTheMountedDatabaseAndReturnsOnlyItsMetrics.
//
// The whole path at once: DefaultCommand, the script on stdin, MOLE_DB pointing at
// the read-only mount, Python's stdlib sqlite3 opening it, and the gate letting
// exactly the declared numbers out.
func TestPythonReadsTheMountedDatabaseAndReturnsOnlyItsMetrics(t *testing.T) {
	runner := pythonRunner(t)

	out, err := runner.Analyze(context.Background(), coderunner.Request{
		DBPath: connectorDB(t),
		Query:  `SELECT region, SUM(spend) FROM samples GROUP BY 1`,
		Script: `
import json, os, sqlite3
db = sqlite3.connect("file:" + os.environ["MOLE_DB"] + "?mode=ro", uri=True)
rows = db.execute("SELECT region, SUM(spend) FROM samples GROUP BY 1").fetchall()
total = sum(r[1] for r in rows)
print(json.dumps({
    "metrics": {
        "groups": len(rows),
        "total_spend": total,
        # Undeclared, and holding a row value: the key is the channel a review
        # probe once got 12KB of records out through.
        "north": rows[0][0],
    },
    "tests": [{
        "name": "north_vs_south",
        "n": 2,
        "statistic": 3.1,
        "p": 0.02,
        "effect_size": 0.9,
    }],
}))
`,
		Contract: coderunner.Contract{
			Metrics: []string{"groups", "total_spend"},
			Tests:   []string{"north_vs_south"},
		},
	})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if out.Metrics["groups"] != 2 {
		t.Errorf("groups = %v, want 2", out.Metrics["groups"])
	}
	if out.Metrics["total_spend"] != 140 {
		t.Errorf("total_spend = %v, want 140", out.Metrics["total_spend"])
	}
	if len(out.Findings) != 1 || out.Findings[0].Name != "north_vs_south" {
		t.Fatalf("findings = %+v, want the declared test", out.Findings)
	}
	// The verdict is derived here rather than taken from the script: a model
	// reporting its own significance is a model grading its own homework.
	if out.Findings[0].Verdict == "" {
		t.Error("no verdict was derived for a declared test")
	}
	// The region NAME is what the gate exists to stop, and Python can serialise it
	// as easily as the shell could.
	if _, ok := out.Metrics["north"]; ok {
		t.Error("an undeclared key crossed out of a real Python container")
	}
	if out.Undeclared != 1 {
		t.Errorf("undeclared = %d, want 1", out.Undeclared)
	}
}

// TestPythonCannotWriteTheMountedDatabase.
//
// The shell test asserts this with a shell append. Python is what will actually
// run, it has a sqlite3 module that opens files read-write by default, and a
// model asked to "analyse" data writes an UPDATE often enough that this is the
// interesting case rather than a duplicate.
func TestPythonCannotWriteTheMountedDatabase(t *testing.T) {
	runner := pythonRunner(t)
	path := connectorDB(t)
	// World-writable on purpose: the container's uid 65534 would make the file
	// unwritable on its own, which would let a :rw mount pass this test.
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}

	out, err := runner.Analyze(context.Background(), coderunner.Request{
		DBPath: path,
		Script: `
import json, os, sqlite3
readable = 0
try:
    sqlite3.connect("file:" + os.environ["MOLE_DB"] + "?mode=ro", uri=True) \
        .execute("SELECT COUNT(*) FROM samples").fetchone()
    readable = 1
except Exception:
    pass
wrote = 0
try:
    db = sqlite3.connect(os.environ["MOLE_DB"])
    db.execute("UPDATE samples SET spend = 0")
    db.commit()
    wrote = 1
except Exception:
    pass
print(json.dumps({"metrics": {"readable": readable, "wrote": wrote}}))
`,
		Contract: coderunner.Contract{Metrics: []string{"readable", "wrote"}},
	})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if out.Metrics["readable"] != 1 {
		t.Error("Python could not read the mounted database; the analysis path is useless")
	}
	if out.Metrics["wrote"] != 0 {
		t.Fatal("Python WROTE the user's database from inside the sandbox")
	}
}

// TestPythonHasNoNetwork, from the interpreter that can actually try.
//
// `--network=none` is asserted elsewhere by reading the flags. A shell image has
// no HTTP client to test it with; Python does, and a model writing an
// exfiltrating script would write it in Python.
func TestPythonHasNoNetwork(t *testing.T) {
	runner := pythonRunner(t)

	out, err := runner.Analyze(context.Background(), coderunner.Request{
		DBPath: connectorDB(t),
		Script: `
import json, socket, urllib.request
resolved = 0
try:
    socket.getaddrinfo("example.com", 80)
    resolved = 1
except Exception:
    pass
fetched = 0
try:
    urllib.request.urlopen("http://93.184.216.34/", timeout=3).read()
    fetched = 1
except Exception:
    pass
print(json.dumps({"metrics": {"resolved": resolved, "fetched": fetched}}))
`,
		Contract: coderunner.Contract{Metrics: []string{"resolved", "fetched"}},
	})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if out.Metrics["resolved"] != 0 {
		t.Error("DNS resolved inside the sandbox")
	}
	if out.Metrics["fetched"] != 0 {
		t.Fatal("the sandbox reached the network; a script can exfiltrate the user's data")
	}
}

// TestAPythonTracebackIsReportedAsAFailedRun, not as an empty result.
//
// A script that raises writes nothing to stdout and exits non-zero, which is the
// shape a parse failure and a crash share. The distinction matters to a caller
// deciding whether to retry.
func TestAPythonTracebackIsReportedAsAFailedRun(t *testing.T) {
	runner := pythonRunner(t)

	_, err := runner.Analyze(context.Background(), coderunner.Request{
		DBPath:   connectorDB(t),
		Script:   `raise ValueError("no such column: revenue")`,
		Contract: coderunner.Contract{Metrics: []string{"x"}},
	})
	if err == nil {
		t.Fatal("a script that raised was reported as a successful analysis")
	}
	// The interpreter's own message is what tells a caller which of the two
	// happened, so it has to survive.
	if !strings.Contains(err.Error(), "ValueError") && !strings.Contains(err.Error(), "no such column") {
		t.Errorf("the traceback did not reach the caller: %v", err)
	}
}
