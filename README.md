# Mole

A Planner–Executor–Verifier deep research agent with budget as a first-class
primitive. Web research, academic literature, and local-data analysis behind one
Actor interface, exposed to coding agents over MCP.

**Status: M0 (foundations) complete.** There is no research loop yet — see
[the milestone table](#milestones). What exists is the substrate everything else
is built on: the schema, the budget ledger, the record/replay layer, and tracing.

Full design: [`mole-architecture-sketch.md`](./mole-architecture-sketch.md).

---

## Why the foundations came first

The order is deliberate: **build the ledger and the measurement before the
intelligence.** You cannot enforce a budget you can't reconcile, or tune a
verifier you can't score. Three properties are established here and relied on by
every later milestone.

**Budget is reserved before dispatch, not charged after.** Post-hoc charging
lets N workers each pass the "can I afford one more?" check before any of them
pays, so the session overshoots by up to N lead-costs. `budget.Ledger` takes the
hold and the availability check in one transaction, which makes the ceiling
hard. `TestConcurrentReserveNeverOvershoots` races 64 goroutines at a budget
that fits exactly 10 and asserts that exactly 10 win.

**The append-only ledger is the source of truth.** `sessions.spent` is a
materialized sum of `tool_calls`, written in the same transaction as each cost
row — so it can always be recomputed and proven. `Ledger.Verify` does exactly
that, and `mole doctor` runs it across every session.

**Everything outbound is recordable.** `internal/record` is an
`http.RoundTripper` that records request/response cassettes and replays them.
Built before the first actor, because adding it later means auditing every call
site. It also redacts credentials at the write boundary — cassettes get
committed, so a key written into one is a leaked key.

---

## Quick start

```bash
make build           # CGO_ENABLED=0 — one static binary, no cgo, no sqlite dep
./bin/mole migrate
./bin/mole dev seed  # writes a synthetic session through the real ledger
./bin/mole sessions
./bin/mole trace <session-id>
./bin/mole doctor
```

`mole trace` output:

```
s_06FVYMAVHFR3EAAS  $0.5015 / $3.0000  ·  status=done  mode=report

ROLE      SPEND    SHARE  TOKENS
planner   $0.0335  7%     3900
executor  $0.2165  43%    46300
verifier  $0.0505  10%    10100
output    $0.2010  40%    34200

tokens: 51400 in · 9100 out · 34000 cache-read · 0 cache-write
ledger: consistent (5 calls)
```

The role breakdown is a single `GROUP BY` over the ledger — the entire reason
`ToolCall.Role` exists. Reconstructing it later would be archaeology.

---

## Package map

| Package | What it owns |
|---|---|
| `internal/core` | Domain types, enums, `Cost`, ID generation |
| `internal/pricing` | Token usage → money. Nano-dollar rates, cache tiers |
| `internal/store` | Persistence interface (`Store` / `Tx` / `Queries`) |
| `internal/store/sqlite` | SQLite implementation, migrations, WAL + single writer |
| `internal/budget` | Reserve / Settle / Release, escrow, estimator, `Verify` |
| `internal/record` | Cassette record/replay transport with credential redaction |
| `internal/obs` | Structured logging, lead-level spans persisted to the DB |
| `cmd/mole` | CLI: `migrate`, `doctor`, `sessions`, `trace`, `dev seed` |

### Decisions worth knowing before you read the code

**Money is `int64` micro-dollars, never `float64`.** The architecture sketch
specifies `CostUSD float64`; that is the one place this implementation
deliberately diverges. Budget adherence is a tested metric ("max overshoot must
be ~0"), and float accumulation across thousands of reserve/settle operations
drifts. `pricing` goes further and holds *rates* in nano-dollars per token, so
`$5/MTok` is exactly `5000` and a cache read is exactly `500` — both integers.

**SQLite is the only backend, on purpose.** See
[§7.1 of the sketch](./mole-architecture-sketch.md) for the full argument. The short
version: a busy shared instance is ~3 writes/second, write serialization is
something the ledger *wants* rather than tolerates, and WAL covers multi-process
access. A second backend would cost a cgo-free build, a dependency, and two
dialects of every query to buy nothing this workload needs.

**One writer, many readers.** The write pool is capped at a single connection,
which serializes writes at the pool and removes `database is locked` entirely
once the executor runs multiple workers. Reads use a normal pool under WAL.
`TestConcurrentWritesDoNotLock` and `TestReadsProceedDuringOpenWrite` cover both
halves.

That property is only real if read paths genuinely don't write, so CLI read
commands (`sessions`, `trace`, `doctor`) open read-only and refuse with
`run: mole migrate` when the schema is behind. Only `migrate` and `dev seed`
take the writer.

**The driver is `modernc.org/sqlite` (pure Go).** "One static binary, no
separate service" is a stated product property; a cgo driver would trade it away
for marginal speed.

**Schema enforces the invariants.** `spent`, `held`, and `escrow` carry
non-negative CHECK constraints, and `ApplyBudgetDelta` repeats them in its
`WHERE` clause. A double-settle matches zero rows and surfaces as
`store.ErrConflict` rather than corrupting the ledger.

---

## Testing

```bash
make test    # fast
make race    # -race -count=2, what CI runs
make cover
```

Tests use real on-disk SQLite (not `:memory:`) so they exercise the actual WAL
and single-writer configuration the daemon runs. Nothing touches the network.

The suites that carry weight:

| Test | Guards against |
|---|---|
| `TestConcurrentReserveNeverOvershoots` | The N-worker budget overshoot |
| `TestConcurrentSettleKeepsSpentExact` | Lost updates in the materialized sum |
| `TestEscrowProtectsOutputBudget` | Research starving report generation |
| `TestDoubleSettleIsRejected` | Double-releasing a hold |
| `TestSweepExpiredReleasesStrandedHolds` | Budget held forever by a dead worker |
| `TestCeilingsAreUnitIndependent` | Token mode's free-fetch hole |
| `TestSecretsNeverReachDisk` | Credentials committed inside a cassette |
| `TestReplayMissIsAnError` | A cassette gap silently making a billable call |
| `TestRoundingDoesNotUnderBill` | Truncation quietly understating spend |

---

## Milestones

| | Milestone | Status |
|---|---|---|
| M0 | Foundations — store, ledger, cassettes, tracing | **done** |
| M1 | WebActor end to end + fetch failure classification | next |
| M2 | Eval harness + `mole stats --fetch` | |
| M3 | Planner loop, rolling digest, error policy | |
| M4 | Claim graph + Verifier | |
| M5 | Executor pool | |
| M6 | AcademicActor | |
| M7 | MCP daemon + stdio shim | |
| M8 | LocalComputeActor (sandbox → sqlguard → aggregation gate → actor) | |
| M9 | Dataset mode | |

---

## Known gaps in M0

Stated plainly rather than left to be discovered:

- **The estimator does not warm from history.** Attributing a settled cost to
  `(actor_type, depth)` needs a join to `leads`, which M3 populates. Guessing
  the actor type would poison the distribution — worse than the honestly
  conservative cold-start seeds.
- **`mole doctor` only checks what M0 owns.** Provider keys, contact email,
  socket permissions, and sandbox availability are reported as unconfigured and
  wired up in their own milestones.
