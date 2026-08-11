package sandbox_test

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/lajosdeme/mole/internal/compute/sandbox"
)

// M8 slice 6.
//
// Two kinds of test here, and the split matters. The parsing and the usability
// rules are unit tests over recorded runtime replies, so they run everywhere.
// The FLAGS are checked by running a real container and observing what it
// cannot do — because a flag list is a claim about a runtime's behaviour, and
// the only way to know `--cap-drop=ALL` drops capabilities is to look.
//
// The container test skips when there is no runtime and when no image is
// already present locally. It never pulls: a test suite that downloads 150MB
// on a cold cache is a test suite people turn off.

// -----------------------------------------------------------------------------
// What the runtime says about itself
// -----------------------------------------------------------------------------

// podmanReply and dockerReply are trimmed real replies. Only the fields the
// package reads are kept, which is also the point being tested: a runtime's
// info output is large and version-dependent, and decoding all of it would
// break on an upgrade that changed something irrelevant.
const podmanReply = `{
  "host": {
    "security": {"rootless": true, "seccompEnabled": true, "apparmorEnabled": false},
    "cgroupVersion": "v2",
    "arch": "amd64"
  },
  "version": {"Version": "5.2.3", "APIVersion": "5.2.3"},
  "store": {"graphDriverName": "overlay"}
}`

const dockerReply = `{
  "ServerVersion": "29.6.2",
  "SecurityOptions": ["name=seccomp,profile=builtin", "name=cgroupns"],
  "CgroupVersion": "2",
  "Driver": "overlayfs"
}`

// TestARuntimeReplyIsReadCorrectly goes through the exported surface, by putting
// a fake runtime on PATH that prints a recorded reply. Testing the unexported
// parsers directly would leave the argument construction unchecked — and podman
// and docker disagree about how to ask for JSON, which is exactly the kind of
// detail a parser test cannot catch.
func TestARuntimeReplyIsReadCorrectly(t *testing.T) {
	for _, tc := range []struct {
		runtime  string
		reply    string
		version  string
		rootless bool
	}{
		{"podman", podmanReply, "5.2.3", true},
		{"docker", dockerReply, "29.6.2", false},
	} {
		t.Run(tc.runtime, func(t *testing.T) {
			withFakeRuntime(t, tc.runtime, tc.reply, 0)

			got := sandbox.Detect(context.Background())
			if !got.Found {
				t.Fatalf("nothing found: %s", got.Detail)
			}
			if string(got.Runtime) != tc.runtime {
				t.Errorf("runtime = %q, want %q", got.Runtime, tc.runtime)
			}
			if got.Version != tc.version {
				t.Errorf("version = %q, want %q", got.Version, tc.version)
			}
			if got.Rootless != tc.rootless {
				t.Errorf("rootless = %v, want %v", got.Rootless, tc.rootless)
			}
			if !got.Seccomp || !got.CgroupV2 {
				t.Errorf("seccomp = %v, cgroup v2 = %v, want both true", got.Seccomp, got.CgroupV2)
			}
			if !got.Usable {
				t.Errorf("a runtime with seccomp and cgroup v2 is not usable: %+v", got)
			}
			if !strings.Contains(got.Detail, tc.version) {
				t.Errorf("the detail line omits the version: %q", got.Detail)
			}
		})
	}
}

// TestNoSeccompMakesItUnusable. A container without a syscall filter is a
// namespace trick around code somebody else wrote, and §3.6 names gVisor as the
// upgrade path for when namespaces are not enough — not as the floor.
func TestNoSeccompMakesItUnusable(t *testing.T) {
	withFakeRuntime(t, "podman",
		`{"host":{"security":{"rootless":true,"seccompEnabled":false},"cgroupVersion":"v2"},
		  "version":{"Version":"5.2.3"}}`, 0)

	got := sandbox.Detect(context.Background())
	if !got.Found {
		t.Fatalf("nothing found: %s", got.Detail)
	}
	if got.Usable {
		t.Error("a runtime with no seccomp was reported usable")
	}
	if !strings.Contains(strings.Join(got.Missing, " "), "seccomp") {
		t.Errorf("Missing does not name seccomp: %v", got.Missing)
	}
	// Found and unusable must read differently from absent, because the actions
	// are different: one is "install a runtime", the other is "your runtime
	// cannot do this".
	if !strings.Contains(got.Detail, "NO seccomp") {
		t.Errorf("the detail line does not say what is wrong: %q", got.Detail)
	}
}

// TestCgroupV1IsAWarningNotADisqualifier. Memory and pid limits are enforceable
// under v1; refusing to run at all would cost the feature for a difference that
// does not decide whether the sandbox holds.
func TestCgroupV1IsAWarningNotADisqualifier(t *testing.T) {
	withFakeRuntime(t, "docker",
		`{"ServerVersion":"24.0.7","SecurityOptions":["name=seccomp,profile=default"],
		  "CgroupVersion":"1"}`, 0)

	got := sandbox.Detect(context.Background())
	if !got.Usable {
		t.Error("cgroup v1 made the runtime unusable")
	}
	if got.CgroupV2 {
		t.Error("cgroup v1 was read as v2")
	}
	if !strings.Contains(strings.Join(got.Missing, " "), "cgroup v2") {
		t.Errorf("Missing does not mention cgroup v2: %v", got.Missing)
	}
}

// TestARuntimeThatDoesNotAnswerIsNotAvailable.
//
// The case a PATH-only check gets wrong, and the common one: a Docker install
// whose daemon is not running has the binary and can run nothing.
func TestARuntimeThatDoesNotAnswerIsNotAvailable(t *testing.T) {
	withFakeRuntime(t, "docker",
		"Cannot connect to the Docker daemon at unix:///var/run/docker.sock.", 1)

	got := sandbox.Detect(context.Background())
	if got.Found || got.Usable {
		t.Fatalf("a runtime that failed to answer was reported present: %+v", got)
	}
	if !strings.Contains(got.Detail, "docker") {
		t.Errorf("the detail does not say which runtime failed: %q", got.Detail)
	}
	if got.Err == nil {
		t.Error("no error to distinguish this from a successful detection")
	}
}

// TestNothingInstalledSaysWhatWasTried. "No container runtime" with no detail
// sends somebody looking for a config option when the answer is that neither
// binary exists.
func TestNothingInstalledSaysWhatWasTried(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	got := sandbox.Detect(context.Background())
	if got.Found {
		t.Fatalf("something was found on an empty PATH: %+v", got)
	}
	for _, want := range []string{"podman", "docker"} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("the detail does not mention %s: %q", want, got.Detail)
		}
	}
}

// TestPodmanIsPreferred. Daemonless and rootless by default, so §3.6's
// non-root-uid and dropped-capability requirements start from a better place.
func TestPodmanIsPreferred(t *testing.T) {
	dir := t.TempDir()
	writeFakeRuntime(t, dir, "podman", podmanReply, 0)
	writeFakeRuntime(t, dir, "docker", dockerReply, 0)
	t.Setenv("PATH", dir)

	if got := sandbox.Detect(context.Background()); got.Runtime != sandbox.Podman {
		t.Errorf("runtime = %q, want podman when both are present", got.Runtime)
	}
}

// -----------------------------------------------------------------------------
// The flags
// -----------------------------------------------------------------------------

// TestEveryRequirementInTheSketchIsAFlag. §3.6 lists them; a missing one is a
// property `doctor` would report and nothing would enforce.
func TestEveryRequirementInTheSketchIsAFlag(t *testing.T) {
	flags := strings.Join(sandbox.DefaultLimits().Flags(), " ")
	for requirement, flag := range map[string]string{
		"no network namespace": "--network=none",
		"read-only rootfs":     "--read-only",
		"tmpfs scratch":        "--tmpfs=/tmp",
		"dropped capabilities": "--cap-drop=ALL",
		"no new privileges":    "--security-opt=no-new-privileges",
		"non-root uid":         "--user=65534",
		"cpu limit":            "--cpus=",
		"memory limit":         "--memory=",
		"pid limit":            "--pids-limit=",
	} {
		if !strings.Contains(flags, flag) {
			t.Errorf("§3.6 requires %s and no flag provides it (%s missing)", requirement, flag)
		}
	}
}

// TestZeroLimitsFallBackToTheDefaults, rather than rendering --memory=0m, which
// docker reads as unlimited. An unset limit turning into no limit is the worst
// available reading of a zero value.
func TestZeroLimitsFallBackToTheDefaults(t *testing.T) {
	flags := strings.Join(sandbox.Limits{}.Flags(), " ")
	for _, bad := range []string{"--cpus=0", "--memory=0m", "--pids-limit=0"} {
		if strings.Contains(flags, bad) {
			t.Errorf("a zero limit rendered as %s, which the runtime reads as unlimited", bad)
		}
	}
	if !strings.Contains(flags, "--memory=512m") {
		t.Errorf("the default memory limit is missing: %s", flags)
	}
}

// TestTheFlagsActuallyHoldRunsARealContainer.
//
// The flag list is a claim about what a runtime does, and the only way to know
// `--cap-drop=ALL` drops capabilities is to look. Every assertion below was
// observed before it was written:
//
//	uid=65534(nobody)                        --user
//	Network unreachable                      --network=none
//	Read-only file system                    --read-only
//	/tmp writable, /tmp/x refuses to execute --tmpfs=...,noexec
//	CapEff: 0000000000000000                 --cap-drop=ALL
//
// Skipped without a usable runtime, and skipped without an image already
// present — it never pulls, because a suite that downloads on a cold cache is a
// suite people switch off.
func TestTheFlagsActuallyHoldRunsARealContainer(t *testing.T) {
	rep := sandbox.Detect(context.Background())
	if !rep.Usable {
		t.Skipf("no usable container runtime: %s", rep.Detail)
	}
	image := localImage(t, string(rep.Runtime))
	if image == "" {
		t.Skip("no image present locally; set MOLE_SANDBOX_TEST_IMAGE to one with a shell")
	}

	run := func(t *testing.T, script string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()

		args := append([]string{"run", "--rm"}, sandbox.DefaultLimits().Flags()...)
		args = append(args, image, "sh", "-c", script)
		out, err := exec.CommandContext(ctx, string(rep.Runtime), args...).CombinedOutput()
		if ctx.Err() != nil {
			t.Skipf("the runtime did not finish in time: %v", ctx.Err())
		}
		// A non-zero exit is expected for most of these: the point is what the
		// container could not do.
		_ = err
		return string(out)
	}

	for _, tc := range []struct {
		why    string
		script string
		want   string
	}{
		{"runs as a non-root uid", `id -u`, "65534"},
		{"cannot reach the network",
			`wget -q -T2 -O- http://1.1.1.1 2>&1 || echo BLOCKED`, "BLOCKED"},
		{"cannot write the root filesystem",
			`touch /should-fail 2>&1 || echo READONLY`, "READONLY"},
		{"can write the scratch mount", `echo hi > /tmp/x && cat /tmp/x`, "hi"},
		{"cannot execute from the scratch mount",
			`cp /bin/echo /tmp/e 2>/dev/null; chmod +x /tmp/e 2>/dev/null; /tmp/e ran 2>&1 || echo NOEXEC`,
			"NOEXEC"},
		{"holds no capabilities",
			`grep CapEff /proc/self/status`, "0000000000000000"},
		// Seccomp, which decides Usable and was asserted by nothing.
		//
		// The detection reports AVAILABILITY: a docker daemon configured
		// `seccomp-profile: unconfined` still answers `name=seccomp`, so the
		// only place the real answer exists is inside the container. Kernel
		// values: 0 disabled, 1 strict, 2 filter.
		{"runs under a seccomp filter",
			`grep Seccomp: /proc/self/status`, "2"},
	} {
		t.Run(tc.why, func(t *testing.T) {
			out := run(t, tc.script)
			if !strings.Contains(out, tc.want) {
				t.Errorf("expected %q in the output\n%s", tc.want, out)
			}
		})
	}
}

// localImage finds an image that is already pulled and has a shell.
func localImage(t *testing.T, runtime string) string {
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
	// Alpine-based and busybox images are the ones certain to have /bin/sh and
	// wget. Anything else might be a distroless or scratch image, where the
	// assertions would fail for the wrong reason.
	for _, line := range strings.Split(string(out), "\n") {
		name := strings.TrimSpace(line)
		switch {
		case name == "", strings.HasSuffix(name, ":<none>"):
			continue
		case strings.Contains(name, "alpine"), strings.Contains(name, "busybox"):
			return name
		}
	}
	return ""
}

// -----------------------------------------------------------------------------
// A fake runtime on PATH
// -----------------------------------------------------------------------------

func withFakeRuntime(t *testing.T, name, output string, exitCode int) {
	t.Helper()
	dir := t.TempDir()
	writeFakeRuntime(t, dir, name, output, exitCode)
	t.Setenv("PATH", dir)
}

// wantFormat is the argument each runtime must be asked for.
//
// podman takes the literal word `json`; docker takes a Go template. The fake
// refuses anything else, and that refusal is the only thing that makes swapping
// the two a test failure — an earlier version accepted any arguments at all, so
// the code could ask podman for a Go template and every test still passed.
//
// Only the docker spelling is verified against a real runtime; there is no
// podman on the machine this was written on. Stated rather than implied.
func wantFormat(runtime string) string {
	if runtime == "docker" {
		return "{{json .}}"
	}
	return "json"
}

// writeFakeRuntime installs a script that answers like the real thing.
//
// A script rather than a mocked exec, so what is under test is the whole path
// including how the command is invoked — podman wants `--format json` and
// docker wants `--format {{json .}}`, and a mock at the parser boundary would
// not notice the two being swapped.
func writeFakeRuntime(t *testing.T, dir, name, output string, exitCode int) {
	t.Helper()
	// echo, not cat or printf: PATH is replaced with the temp directory for
	// these tests, so the script has no external commands available. That is
	// deliberate — a fake runtime that needed /bin to work would not prove the
	// detection runs on an unusual PATH — but it took a round of failures to
	// notice, because `cat` is invisible until it is missing.
	script := "#!/bin/sh\n" +
		"if [ \"$1\" != \"info\" ]; then echo \"unexpected verb: $*\" >&2; exit 2; fi\n" +
		"if [ \"$2\" != \"--format\" ] || [ \"$3\" != '" + wantFormat(name) + "' ]; then\n" +
		"  echo \"wrong format argument: $*\" >&2; exit 3\nfi\n"
	body := "'" + strings.ReplaceAll(output, "'", "") + "'"
	if exitCode == 0 {
		script += "echo " + body + "\n"
	} else {
		script += "echo " + body + " >&2\nexit " + itoa(exitCode) + "\n"
	}
	path := dir + "/" + name
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
