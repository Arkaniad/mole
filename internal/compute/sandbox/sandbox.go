// Package sandbox detects a container runtime and describes what it can
// enforce (M8, §3.6).
//
// §3.6 decides the technology before CodeRunner is written: "OCI container, no
// network namespace, read-only rootfs, tmpfs scratch, dropped capabilities,
// seccomp default profile, non-root uid, and hard CPU/memory/wallclock/pid
// limits." This package is the half of that which can be answered before any
// code runs — is there a runtime, does it work, and does it enforce the things
// the list depends on.
//
// # Nothing here is required for mole to run
//
// mole is one static binary and stays one. A container runtime is needed by
// CodeRunner alone — the component that executes model-authored Python against
// real data — and that is one feature, not the tool. The SQL path needs no
// sandbox at all: §12.2 says outright that "the sandbox is not the control
// here", and the aggregation gate is what makes local analysis safe.
//
// So Detect reports a capability. It never fails a startup, and `doctor` prints
// what is missing alongside what still works without it.
//
// # Measured, not assumed
//
// A binary on PATH is not a working runtime: a Docker install with a wedged
// daemon has the binary and cannot run anything. Detect asks the runtime about
// itself and reports what it says — version, rootless, seccomp, cgroup version
// — rather than inferring capability from a file existing.
package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Runtime is a container runtime mole knows how to interrogate.
type Runtime string

const (
	Podman Runtime = "podman"
	Docker Runtime = "docker"
)

// Order is the preference. Podman first: it is daemonless and rootless by
// default, so the non-root-uid and dropped-capability half of §3.6 starts from
// a better place and needs no privileged service running to get there.
var Order = []Runtime{Podman, Docker}

// probeTimeout bounds the interrogation.
//
// Short on purpose. `docker info` talks to a daemon, and a daemon that is not
// answering is exactly the case this has to report rather than hang on —
// `doctor` and a session start both call this, and neither may block on
// somebody else's socket.
const probeTimeout = 5 * time.Second

// Report is what was found.
type Report struct {
	// Runtime and Version are set when a runtime answered.
	Runtime Runtime `json:"runtime,omitempty"`
	Version string  `json:"version,omitempty"`

	// Found means a runtime binary answered an info request.
	Found bool `json:"found"`
	// Usable means it answered AND enforces what §3.6 depends on. A runtime
	// that is present but cannot filter syscalls is reported separately from
	// one that is absent, because trusting it would be worse than having none.
	Usable bool `json:"usable"`

	Rootless bool `json:"rootless"`
	Seccomp  bool `json:"seccomp"`
	CgroupV2 bool `json:"cgroup_v2"`

	// Detail is one line for `doctor`.
	Detail string `json:"detail"`
	// Missing names each §3.6 requirement the runtime does not meet.
	Missing []string `json:"missing,omitempty"`
	// Err is why nothing was found, if nothing was.
	Err error `json:"-"`
}

// Detect interrogates the runtimes in Order and returns the first that answers.
//
// The FIRST that answers, not the first that is usable: a present-but-weakened
// runtime is a finding worth reporting, and silently moving on to the next one
// would hide it. Somebody who installed podman deliberately should be told
// their podman cannot filter syscalls, not handed docker instead.
func Detect(ctx context.Context) Report {
	var tried []string
	for _, rt := range Order {
		bin, err := exec.LookPath(string(rt))
		if err != nil {
			tried = append(tried, string(rt)+": not on PATH")
			continue
		}
		rep, err := interrogate(ctx, rt, bin)
		if err != nil {
			// Present and not answering. Reported rather than skipped: this is
			// the case a LookPath-only check gets wrong.
			tried = append(tried, fmt.Sprintf("%s: %v", rt, err))
			continue
		}
		return rep
	}
	return Report{
		Detail: "no container runtime found (" + strings.Join(tried, "; ") + ")",
		Err:    errors.New("sandbox: no container runtime"),
	}
}

func interrogate(ctx context.Context, rt Runtime, bin string) (Report, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	args := []string{"info", "--format", "json"}
	if rt == Docker {
		// Docker's --format is a Go template over the info struct; podman's
		// takes the literal word json. Same intent, different spelling.
		args = []string{"info", "--format", "{{json .}}"}
	}
	out, err := exec.CommandContext(ctx, bin, args...).Output()
	if err != nil {
		if ctx.Err() != nil {
			return Report{}, fmt.Errorf("did not answer within %s", probeTimeout)
		}
		return Report{}, fmt.Errorf("info failed: %w", firstLine(err))
	}

	rep := Report{Runtime: rt, Found: true}
	switch rt {
	case Podman:
		err = readPodman(out, &rep)
	case Docker:
		err = readDocker(out, &rep)
	}
	if err != nil {
		return Report{}, err
	}
	finish(&rep)
	return rep, nil
}

// finish decides usability and writes the summary.
func finish(rep *Report) {
	// Seccomp is required. It is the syscall boundary, and without it the
	// container is a namespace trick around code somebody else wrote — §3.6
	// names gVisor as the upgrade path for when namespaces are not enough, not
	// as the floor.
	if !rep.Seccomp {
		rep.Missing = append(rep.Missing, "seccomp filtering")
	}
	// cgroup v2 is preferred and not required. Memory and pid limits are
	// enforceable under v1; they are just less reliably reported, and the
	// difference does not change whether the sandbox holds.
	if !rep.CgroupV2 {
		rep.Missing = append(rep.Missing, "cgroup v2 (limits still apply under v1)")
	}
	rep.Usable = rep.Seccomp

	var parts []string
	parts = append(parts, string(rep.Runtime)+" "+rep.Version)
	if rep.Rootless {
		parts = append(parts, "rootless")
	} else {
		parts = append(parts, "rootful")
	}
	if rep.Seccomp {
		parts = append(parts, "seccomp available")
	} else {
		parts = append(parts, "NO seccomp")
	}
	if rep.CgroupV2 {
		parts = append(parts, "cgroup v2")
	} else {
		parts = append(parts, "cgroup v1")
	}
	rep.Detail = strings.Join(parts, ", ")
}

// -----------------------------------------------------------------------------
// Runtime replies
// -----------------------------------------------------------------------------

// Only the fields that decide something are decoded. A runtime's info output is
// large, changes between versions, and none of the rest of it affects whether
// model-authored code can be run safely.

type podmanInfo struct {
	Host struct {
		Security struct {
			Rootless       bool `json:"rootless"`
			SeccompEnabled bool `json:"seccompEnabled"`
		} `json:"security"`
		CgroupVersion string `json:"cgroupVersion"`
	} `json:"host"`
	Version struct {
		Version string `json:"Version"`
	} `json:"version"`
}

func readPodman(raw []byte, rep *Report) error {
	var info podmanInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return fmt.Errorf("parse podman info: %w", err)
	}
	rep.Version = info.Version.Version
	rep.Rootless = info.Host.Security.Rootless
	rep.Seccomp = info.Host.Security.SeccompEnabled
	rep.CgroupV2 = strings.Contains(info.Host.CgroupVersion, "2")
	return nil
}

type dockerInfo struct {
	ServerVersion string `json:"ServerVersion"`
	// SecurityOptions is a list of "name=seccomp,profile=builtin" strings. The
	// presence of a name is the whole signal; the shape is stable across the
	// versions that matter and the parse is deliberately forgiving.
	SecurityOptions []string `json:"SecurityOptions"`
	CgroupVersion   string   `json:"CgroupVersion"`
}

func readDocker(raw []byte, rep *Report) error {
	var info dockerInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return fmt.Errorf("parse docker info: %w", err)
	}
	rep.Version = info.ServerVersion
	for _, opt := range info.SecurityOptions {
		if strings.Contains(opt, "name=seccomp") {
			rep.Seccomp = true
		}
		if strings.Contains(opt, "name=rootless") {
			rep.Rootless = true
		}
	}
	if v, err := strconv.Atoi(strings.TrimSpace(info.CgroupVersion)); err == nil {
		rep.CgroupV2 = v >= 2
	}
	return nil
}

func firstLine(err error) error {
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(ee.Stderr) > 0 {
		line := strings.TrimSpace(strings.SplitN(string(ee.Stderr), "\n", 2)[0])
		if line != "" {
			if len(line) > 120 {
				line = line[:120] + "…"
			}
			return errors.New(line)
		}
	}
	return err
}

// -----------------------------------------------------------------------------
// The flags CodeRunner will use
// -----------------------------------------------------------------------------

// Limits are §3.6's "hard CPU/memory/wallclock/pid limits".
type Limits struct {
	CPUs      float64
	MemoryMB  int64
	Pids      int64
	Wallclock time.Duration
}

// DefaultLimits are enough for a pandas job over a few hundred thousand rows and
// not enough to matter if the code is hostile instead.
func DefaultLimits() Limits {
	return Limits{CPUs: 1, MemoryMB: 512, Pids: 64, Wallclock: 30 * time.Second}
}

// Flags renders §3.6's requirements as runtime arguments.
//
// Exported and shared, so `doctor` prints the arguments CodeRunner will actually
// pass rather than a description somebody wrote alongside them. A doctor line
// that claims "netns disabled, seccomp default" while the runner forgot
// --network=none is worse than no line at all: it is a check that reports a
// property nothing enforces.
// withDefaults fills in only the fields that were left unset.
//
// Per field, and one implementation. Flags replaced the WHOLE struct when any
// single field was zero — so Limits{CPUs: 4} silently ran at one cpu — and
// Summary tested a different set of fields, so `doctor` could print a summary
// that did not match the flags. That is exactly the failure Flags's own comment
// warns about.
func (l Limits) withDefaults() Limits {
	d := DefaultLimits()
	if l.CPUs <= 0 {
		l.CPUs = d.CPUs
	}
	if l.MemoryMB <= 0 {
		l.MemoryMB = d.MemoryMB
	}
	if l.Pids <= 0 {
		l.Pids = d.Pids
	}
	if l.Wallclock <= 0 {
		l.Wallclock = d.Wallclock
	}
	return l
}

func (l Limits) Flags() []string {
	l = l.withDefaults()
	return []string{
		// §12.2's fourth defense, and the one that matters most here: code that
		// cannot open a socket cannot exfiltrate whatever it was given.
		"--network=none",
		"--read-only",
		"--tmpfs=/tmp:rw,noexec,nosuid,size=64m",
		"--cap-drop=ALL",
		// No seccomp flag. Both runtimes apply their default profile when none is
		// given, and naming one is a portability risk on a runtime this could not
		// be tested against — `seccomp=builtin` is docker's spelling.
		//
		// What was missing was never the flag: it was the CHECK. Usable is
		// decided by rep.Seccomp, which reports availability, and a daemon
		// configured `seccomp-profile: unconfined` still reports
		// `name=seccomp` — so detection cannot tell whether a filter is
		// actually installed. TestSeccompIsActuallyEnforced reads
		// /proc/self/status inside a real container instead, which is the only
		// place the answer exists.
		"--security-opt=no-new-privileges",
		"--user=65534:65534",
		fmt.Sprintf("--cpus=%g", l.CPUs),
		fmt.Sprintf("--memory=%dm", l.MemoryMB),
		fmt.Sprintf("--pids-limit=%d", l.Pids),
	}
}

// Summary describes the configuration in one line, for `doctor`.
func (l Limits) Summary() string {
	l = l.withDefaults()
	return fmt.Sprintf("no network, read-only rootfs, all capabilities dropped, "+
		"the runtime's default seccomp profile, uid 65534, "+
		"%g cpu, %dMB, %d pids, %s wallclock",
		l.CPUs, l.MemoryMB, l.Pids, l.Wallclock)
}
