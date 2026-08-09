<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="banner-dark.png">
    <img src="banner.png" alt="Mole — a deep research agent in Go, exposed over MCP" width="820">
  </picture>
</p>

# Mole

A Planner–Executor–Verifier deep research agent with budget as a first-class
primitive. Web research, academic literature, and local-data analysis behind one
Actor interface, exposed to coding agents over MCP.

**Status: M7 complete — the loop runs end to end and coding agents can drive
it.** Decompose → fetch → mine claims → digest → cross-check → replan → report,
with the budget ledger binding at every step, behind a local daemon speaking MCP
over a unix socket. Verified against a live provider rather than only fakes: a
nine-lead run held to 114,654 of 400,000 tokens with 0% overshoot, replanned
twice, served eight sources from cache without a fetch, and reconciled a 40-call
ledger.

M5's executor pool has since landed: a session runs four leads at once by
default, per session rather than per process.

What is not done: M2's question corpus, and the Verifier's contradiction
*recall* — its precision is measured, but no labelled set of true contradictions
exists to measure against. M6, M8 and M9 are untouched. See
[the milestone table](#milestones) and [known gaps](#known-gaps).

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
./bin/mole --help    # commands and flags, generated from their definitions
./bin/mole migrate
./bin/mole dev seed  # writes a synthetic session through the real ledger
./bin/mole sessions
./bin/mole trace <session-id>
./bin/mole doctor
```

Running an actual research question needs a search key and a model:

```bash
./bin/mole config set search.provider tavily
./bin/mole config set search.tavily-key tvly-...
./bin/mole doctor                              # exits non-zero until this is green

./bin/mole research "what is the consensus on byte-level LLMs?" --usd 0.50
./bin/mole stats --fetch                       # the §17.1 headless-browser gate
./bin/mole eval --last --verbose               # score the run against §14.3
```

Once a run has finished, its claims can be re-questioned without researching
again — retrieval plus one model call, no search and no fetch (§13):

```bash
./bin/mole ask <session-id> "how does it compare to MegaByte?"
./bin/mole ask <session-id> "..." --json       # answer, claims and citations
```

An ask costs cents and is charged to a small session of its own rather than to
the run it queries — that run's ledger is settled, and §8 does not allow cost
rows against a closed account, so the ask shows up in `mole sessions` separately.
This is the same operation an agent gets over MCP as `research.ask`.

`ask` writes, and it is safe to run against a live daemon. SQLite serialises
writers per *transaction*, not per connection, so the daemon holds no lock
between its own transactions and an ask's are short. The one command that should
not be run against a running daemon is `mole migrate`.

No model key is needed if `ant auth login` has run or a local runtime is up —
`doctor` says which one it found. `--usd` and `--tokens` are mutually exclusive
and there is no built-in default: a number nobody chose is still money spent.

A session runs `--workers` leads at once, four by default, clamped at 16. The
pool is per session and multiplies with the daemon's session limit, so `serve`
prints both numbers and their product — four sessions of four workers is sixteen
concurrent leads leaving the machine. Lead execution is ~91% of a session's wall
clock, measured on the pre-pool baseline, which is what the pool is aimed at.

Concurrency and deterministic replay do not mix, and the boundary is one
environment variable. A cassette is keyed on the request body; with a pool the
bodies stop being reproducible, because the synthesis prompt is built from claims
in order and lead completion order is whatever the network decided. So a cassette
recorded under concurrency cannot be replayed at any worker count — not even
against itself — and `MOLE_RECORD` with `--workers > 1` is refused rather than
silently serialised. Everything that touches a cassette runs serial; every real
research run does not.

Every outbound call can go through a cassette, which is what makes the eval
harness deterministic and free:

```bash
MOLE_RECORD=record MOLE_CASSETTE_DIR=./testdata/cassettes ./bin/mole research "..." --usd 0.50 --workers 1
MOLE_RECORD=replay MOLE_CASSETTE_DIR=./testdata/cassettes ./bin/mole research "..." --usd 0.50 --workers 1
```

Replay never opens a socket — a miss is an error, not a quiet billable call.
That is what makes citation accuracy affordable: `mole eval --citations` re-reads
every cited source to check the quote is really in it, which costs a fetch per
source live and nothing at all replayed.

```bash
MOLE_RECORD=record MOLE_CASSETTE_DIR=./testdata/cassettes \
  ./bin/mole research "..." --tokens 200000 --always-fetch --workers 1
MOLE_RECORD=replay MOLE_CASSETTE_DIR=./testdata/cassettes \
  ./bin/mole eval --last --citations          # 32ms, no network
```

`--always-fetch` matters more than it looks. Tavily returns page text, so by
default nothing is fetched — which means §10.4 has no denominator, §17.1's gate
reads "no data", and citation accuracy cannot be checked at all, because
re-reading the page runs a different extractor than the one that produced the
text. An eval run on a content-supplying provider silently collects none of it.
Mode is an environment variable rather than a config field on purpose: left on
in a config file it would slowly write every API response to disk.

`mole trace` output:

```
s_06FWWN0FBRZ9V8QR  114654 tok / 400000 tok  ·  status=budget_exhausted  mode=report

ROLE      SPEND       SHARE  TOKENS
planner   2099 tok    2%     2099
executor  111529 tok  97%    111529
output    1026 tok    1%     1026

tokens: 104403 in · 10251 out · 0 cache-read · 0 cache-write
ledger: consistent (40 calls)
```

Real output from a live run predating M4, so there is no `verifier` row.
The 2%/97% split is the number the rolling digest exists to protect: planning cost
scales with the number of *replans*, not the number of leads (§9.1).

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
| `internal/llm` | Provider boundary — Anthropic and any OpenAI-compatible endpoint |
| `internal/tools` | Search providers, fetch + robots, extraction, rate limiter |
| `internal/actors` | `Actor` interface, WebActor, chunk mining, quote verification |
| `internal/planner` | Decomposition, the rolling digest, replan |
| `internal/queue` | Lease-based lead queue: heartbeat, crash recovery |
| `internal/executor` | The loop — sub-budgets, error policy, replan batching |
| `internal/verifier` | Claim graph: clustering, contradiction adjudication, grounding |
| `internal/cache` | Artifact-level result-returning cache (URL, DOI, query) |
| `internal/output` | Report synthesis, `ask` answers, citation rendering |
| `internal/session` | Runner and supervisor — session lifecycle, crash recovery |
| `internal/daemon` | Unix socket listener, peer credential checks, graceful stop |
| `internal/mcpserver` | The MCP tool surface the daemon speaks |
| `internal/eval` | Mechanical scorecard, citation re-verification |
| `internal/config` | Config file and environment resolution |
| `cmd/mole` | CLI: `research`, `ask`, `serve`, `eval`, `stats`, `trace`, `sessions`, `doctor`, `config`, `migrate`, `dev` |
| `cmd/mole-mcp` | The disposable stdio shim — pumps bytes to the daemon's socket |

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

The CLI tests drive the real command tree, so they redirect `MOLE_DB` and
`MOLE_CONFIG_DIR` to temporary paths — enforced by
`TestCLITestsCannotReachTheRealEnvironment`. Without that they used the
developer's live config and database, and once a reachable local model was
configured they created sessions in it and blocked on real model calls. A test
that is safe only because a provider happens to be unreachable is not safe.

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
| `TestReplayMakesNoNetworkCalls` | Replay falling through to a live origin |
| `TestProviderKeysNeverReachDisk` | A vendor auth header no redaction rule names |
| `TestPageCannotCloseItsOwnFence` | Page content escaping into instruction position |
| `TestEscapeHatchDoesNotReachMetadataByAnotherName` | Metadata reachable via NAT64/6to4 |
| `TestBacklogQueuesRatherThanCollapsing` | A rate limiter that enforces no rate |
| `TestSplitTerminatesOnInvalidUTF8` | A malformed page hanging the chunker |
| `TestOffsetsSurviveExoticWhitespace` | Quote offsets drifting off the source |
| `TestClaimCapIsPerSourceNotPerChunk` | One verbose page dominating the graph |
| `TestProviderContentIsExcludedFromEveryRate` | The §17.1 gate reading a padded denominator |
| `TestBudgetOvershootIsARegression` | A ceiling that stopped binding, scored as fine |
| `TestBlockedMetricsAreNamedNotOmitted` | Partial scoring read as full coverage |
| `TestNothingIsLeftHeldOnAnyPath` | Budget stranded by a skipped settle |
| `TestDigestStaysBoundedWhenNothingIsAnswered` | Quadratic planner cost returning |
| `TestNoLeadIsDispatchedTwice` | Paying twice for one lead |
| `TestCachedLeadReturnsAResultRatherThanSkipping` | Rev 1's replan livelock |
| `TestRoundingDoesNotUnderBill` | Truncation quietly understating spend |

---

## Milestones

| | Milestone | Status |
|---|---|---|
| M0 | Foundations — store, ledger, cassettes, tracing | **done** |
| M1 | WebActor end to end + fetch failure classification | **done** |
| M2 | Eval harness + `mole stats --fetch` | scorer **done**, corpus (§14.2) open |
| M3 | Planner loop, rolling digest, error policy | **done** |
| M4 | Claim graph + Verifier | **done**, contradiction recall unmeasured |
| M5 | Executor pool | **done**, real-run speedup unmeasured |
| M6 | AcademicActor | |
| M7 | MCP daemon + stdio shim | **done** |
| M8 | LocalComputeActor (sandbox → sqlguard → aggregation gate → actor) | |
| M9 | Dataset mode | |

---

## Known gaps

Stated plainly rather than left to be discovered:

- **The cache is session-scoped and in memory.** A cross-session cache has to
  answer "how stale is too stale", and the answer differs per question type — a
  settled fact keeps for months, a "current consensus" for days. §14.2's corpus
  is what would settle it, so the durable version waits for data rather than a
  guess.
- **M2 is open while M3 is done, on purpose.** `mole stats --fetch`, the cassette
  wiring, and the mechanical scorer (`mole eval`) are in; the question corpus
  (§14.2) is not. The milestones are not a strict chain — the corpus is labelled
  data, and building it before there was a loop worth measuring would have meant
  guessing at what to label. Two of the scorer's metrics still read zero:
  staleness detection is unimplemented, and contradiction recall has a verifier
  behind it now but nothing labelled to score against — see below.
- **The planner never converges by answering; it stops at a cap.**
  `MaxNewLeadsPerReplan` equals `ReplanEvery`, so the queue drains at exactly the
  rate it refills, and the two quality-driven exits — a drained queue, and the
  planner declaring itself done — cannot fire in practice. Termination is still
  bounded and safe (the depth cap holds a default run to ten leads), and the
  planner now sees how much allowance is left so `done` is at least an informed
  choice. Tapering the fan-out is the obvious next move and is a cost/quality
  tradeoff that cannot be evaluated without the corpus above.
- **Reasoning models are unusable through the OpenAI-compatible endpoint.** qwen3,
  gemma4 and others emit their reasoning in a `reasoning` field, which mole does
  not read, and charge it against the same output allowance — so the whole budget
  can go to reasoning and `content` comes back empty. `llm.ErrEmptyOutput` names
  this rather than surfacing it as a JSON parse failure. Ollama's `/v1` endpoint
  ignored every documented way to disable it (`think:false`, `/no_think`,
  `chat_template_kwargs.enable_thinking`). Use a non-reasoning model, or the
  native API. Supporting them properly — reading the `reasoning` field and
  giving it its own allowance so it cannot eat the output budget — is planned,
  not merely worked around.
- **Contradiction recall has never been measured.** The Verifier's precision was
  checked against a labelled 37-pair set, but that set contained no true
  contradictions, so the recall §14.3 asks for has no denominator — it is `0/0`,
  not zero. Precision alone cannot catch the failure that matters here: a
  Verifier that answers `neither` to everything scores perfect precision.

  `testdata/corpus/contradictions.json` is the denominator, drafted and not yet
  run: ten questions chosen because sources genuinely disagree, biased toward
  numeric disagreements so a labeller can call them without a judgment call.
  Each carries notes naming the specific competing figures. The procedure is

  ```bash
  MOLE_RECORD=record MOLE_CASSETTE_DIR=./testdata/cassettes \
    mole corpus testdata/corpus/contradictions.json --usd 0.40 --max-sources 6 --max-depth 1 --workers 1
  mole pairs dump <session-id> --all -o pairs.json   # --all is what makes recall measurable
  # label each pair: contradicts | duplicate | neither
  mole pairs score pairs.json
  ```

  One caveat worth knowing before reading the number. Pairs are formed by the
  lexical retriever, not exhaustively, so two contradicting claims that share few
  content words are never paired and never judged — a miss indistinguishable
  from a judge error. What this measures is the pipeline's recall, not the
  judge's. Re-dumping at a raised `--max-candidates` separates the two, and costs
  nothing once the cassettes exist.

  The full §14.2 corpus is deliberately not being built. Claim-precision
  labelling is ~2400 human judgments; if that number is ever needed, sample 200
  and report an error bar.
- **A fatal error costs up to one batch, not one lead.** `Fatal` means the
  failure repeats — a bad key fails on every lead — so serially the next lead
  never starts and exactly one is charged. With a pool its siblings are already
  in flight. The batch is cancelled as soon as any worker reports a fatal, and
  the loop stops before leasing more, so the blast radius is bounded at one
  batch; it is not bounded at one lead, and `--workers 1` is the only way to get
  that back.
- **The estimator does not warm from history.** Attributing a settled cost to
  `(actor_type, depth)` needs a join to `leads`, which M3 now populates, so the
  blocker is gone and this is simply unimplemented. Guessing the actor type would
  poison the distribution — worse than the honestly conservative cold-start seeds.
- **The binary is ~24MB, up 8.3MB after the cobra port.** Cobra itself is only
  ~0.3MB on top of this dependency set — measured, not assumed. The rest is
  retained type metadata: cobra and `text/template` use reflection, which stops
  the linker pruning type information across the whole graph, and the
  reflection-heavy Anthropic SDK accounts for most of it. Symbol count nearly
  doubled (22.9k → 40.7k) while symbol *bytes* grew only 3.5MB, which is the
  signature of metadata rather than code.

  Not a problem for how this ships. Every cobra-based CLI is in the same range
  — `docker` 27.8MB, `gh` 38.6MB, `kubectl` 84.8MB. It would matter for a
  per-invocation container image or an edge target, and neither is the plan.
- **`mole doctor` checks what has landed, and says so about the rest.** The store,
  schema, pricing table, ledger reconciliation, config permissions, and both
  provider credentials are checked for real — including whether the search provider
  returns page content, which decides whether §17.1's gate has a denominator. The
  contact email (Unpaywall/NCBI, M6), MCP socket permissions (M7), and sandbox
  availability (M8) are reported as informational until their milestone lands.

---

## Before a public release

Deliberately deferred, tracked here rather than in a scratch file:

- **Comment pass.** The code is commented at the density of something being
  reasoned about in the open — every non-obvious decision carries its argument.
  Some of that is scaffolding for the build rather than for a reader, and should
  be cut once the shape has stopped moving.
- **Reasoning-model support.** See the known gap above. Working around it is
  acceptable while the only local models available reason by default; shipping a
  research tool that silently spends a whole budget on hidden tokens is not.
- **Separate the dev commands from the product.** `mole dev`, `corpus`, `pairs`
  and parts of `eval` exist to build and check this thing, not to use it. They
  should be behind a build tag or a hidden group before the CLI is presented as
  a stable surface — an accidental `mole dev seed` against a real database is a
  bad first impression. `cmd/fixorphans` is a one-off repair for a mistake that
  can no longer happen and should simply go.
- **Choose a licence.** There is no `LICENSE` file yet. The choice is between
  plain MIT and a source-available/fair-code licence in the shape n8n uses —
  which turns on whether a hosted mole run by someone else is a problem worth
  preventing. Not decided.
