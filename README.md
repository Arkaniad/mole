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

**Status: M9 complete — the loop runs end to end and coding agents can drive
it.** Decompose → fetch → mine claims → digest → cross-check → replan → report,
with the budget ledger binding at every step, behind a local daemon speaking MCP
over a unix socket. Verified against a live provider rather than only fakes: a
nine-lead run held to 114,654 of 400,000 tokens with 0% overshoot, replanned
twice, served eight sources from cache without a fetch, and reconciled a 40-call
ledger.

M5's executor pool has since landed: a session runs three leads at once by
default, per session rather than per process.

M6, the AcademicActor, has landed: arXiv and PubMed as keyless defaults,
Unpaywall for DOI to legal open-access copy, and an escalation ladder that reads
an abstract before it reads anything longer.

```bash
./bin/mole config set contact-email you@example.com   # required by §10.3
./bin/mole research "..." --actors web,academic --tokens 200000
# sub-questions are distributed across the actors round-robin, not researched twice
```

What is not done: M2's question corpus, and the Verifier's contradiction
*recall* — its precision is measured, but no labelled set of true contradictions
exists to measure against. M8 and M9 are untouched. See
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

A session runs `--workers` leads at once, three by default, clamped at 16. The
pool is also bounded by the replan cadence — it never runs past a planner
consultation — which is why the default equals `ReplanEvery`: a pool larger than
the cadence has workers that can never all be busy. The pool is per session and
multiplies with the daemon's session limit, so `serve` prints both numbers and
their product: four sessions of three workers is twelve concurrent leads leaving
the machine. Lead execution is ~91% of a session's wall clock, measured on the
pre-pool baseline, which is what the pool is aimed at.

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
| `internal/compute/connector` | Local data sources: intake, profiling, the read-only handle |
| `internal/compute/sqlguard` | The parse gate — one SELECT, allowlisted functions (§12.2) |
| `internal/compute/gate` | The aggregation gate — the only path from data to a model (§12.1) |
| `internal/compute/hypothesis` | Templates: the only SQL that reaches a connector (§12.3) |
| `internal/compute/stats` | Welch's t-test, effect size, and the verdict (§4) |
| `internal/compute/sandbox` | Container runtime detection, §3.6's flags, and running in one |
| `internal/compute/coderunner` | Model-authored analysis, and what may come back (§12.1) |
| `internal/dataset` | Dataset mode: schema, rows, cross-source merge, CSV/JSON (§13) |
| `internal/eval` | Mechanical scorecard, citation re-verification |
| `internal/config` | Config file and environment resolution |
| `cmd/mole` | CLI: `research`, `ask`, `serve`, `eval`, `connect`, `dataset`, `stats`, `trace`, `sessions`, `doctor`, `config`, `migrate`, `dev` |
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

**The pricing table is data, and a wrong rate is invisible.** Every ledger row
reconciles against every other, `mole eval` passes, and only the provider's invoice
disagrees — so DeepSeek's entries assert in dollars-per-million (the unit the price
list is published in) rather than in the nano-dollars the table stores. Three things
that are not obvious:

- **Four entries for two models.** The ledger prices what the API *returned*
  (`deepseek-chat` comes back as `deepseek-v4-flash`), and the pre-flight check that
  refuses `--usd` for an unpriced model reads what was *configured*. Register only
  one of the two names and half the mechanism silently does nothing.
- **`perMTok` is wrong for DeepSeek.** Its cache multipliers are Anthropic's
  contract — read at 0.1× input — and a DeepSeek cache hit is 2% of a miss. The
  helper would have overcharged cache reads fivefold.
- **Cache reads round up.** $0.0028/MTok is 2.8 nano-dollars per token and the rate
  is an `int64`. 3 over-bills by 7%; 2 would under-bill by 29%. Under-billing is the
  one direction this table may not fail in.

An alias is the provider's to repoint without telling anyone, so `deepseek-reasoner`
is registered at the dearer model's rates even though it currently resolves to the
cheaper one. An estimate that is too high costs a briefly under-used budget; one
that is too low lets work start that cannot be paid for.

**Reasoning models get their own token allowance.** qwen3, gemma4 and the o-series
emit a chain of thought against the *same* output allowance as the answer, so
`MaxTokens: 4096` can buy 4096 tokens of thinking and an empty message — measured
on qwen3:4b through Ollama, where a request for `{"ok":true}` spent its entire
budget reasoning and returned `content: ""` with `finish_reason: "length"`. Callers
then saw "no JSON object in the reply" and blamed the prompt. Ollama ignored every
documented way to switch reasoning off (`think:false`, `/no_think`,
`chat_template_kwargs.enable_thinking`), so mole budgets for it instead: `MaxTokens`
is the *answer* allowance and the provider adds room for the chain on top, learned
per model and capped. The first empty response jumps straight to 4,096 rather than
stepping — the same prompt cost 249 reasoning tokens in one measured run and over
2,128 in the next, so small steps just buy wasted calls. Every attempt's usage is
summed into the response, because the tokens were spent whether or not an answer
arrived. The chain itself is carried but never concatenated into the text: §11.5
would otherwise let a model cite its own reasoning as a source. A live test against
a real reasoning model is in `internal/llm` and skips unless one is reachable.

**Reservations are predicted from this install's own history.** The estimator
holds a rolling p75 of settled cost per `(actor type, depth)`, and it used to
start cold on every session — so what a hundred previous leads actually cost was
thrown away at the start of the next one, and §8.4's "improves with use" meant
"improves within one run, then forgets". It warms from the last 200 finished
leads, across sessions, joined to `leads` for the actor type and depth that
produced them. Nothing is guessed: leads that never reached `done` are not read,
because a lead that failed halfway is a sample of a failure. Bounded at 200 for a
second reason — an install's oldest leads were priced by whatever model was
configured then, and a warm start reaching back a year would size today's
reservations from last year's tier. A cold start is not an error; an install with
no history keeps the conservative seeds.

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
| M6 | AcademicActor | **done**, claim extraction unverified on a capable model |
| M7 | MCP daemon + stdio shim | **done** |
| M8 | LocalComputeActor (connector → sqlguard → aggregation gate → actor) | **done** + reviewed; the model's half now verified live on a local model |
| M9 | Dataset mode | **done** + reviewed; extraction unverified on a capable model |

---

## Known gaps

Stated plainly rather than left to be discovered:

- **M8's hypothesis planning is verified against a capable model.** Was blocked on
  credit. DeepSeek (`deepseek-v4-flash`) planned a comparison over 400 rows, chose
  `group_comparison` with the right column roles, and wrote eight claims that all
  passed §11.5 and all reconciled — for **$0.0006**. The scorecard reads clean:
  claim integrity 100%, verification coverage 100%, exfil regression 0% of 2
  envelopes, no overshoot. What that run also found is below, and it was a hole in
  the design rather than in the model.
- **The channel out of the sandbox is bounded, not zero.** Only names the plan
  declared come back, and only as finite numbers, so a script cannot return rows
  or labels. A determined model could still encode a value in the digits of a
  declared number. Stated because calling it zero would be the kind of claim the
  package exists to avoid making.
- ~~**Dataset extraction is unverified against a capable model.**~~ **Closed.** One
  DeepSeek run over "the largest UK supermarket chains and their annual revenue":
  27 extractions from 3 sources became 9 merged rows, 8 corroborated by more than
  one source, for $0.0339. Real revenue figures, correctly scaled, each traceable to
  a quote. It found three defects, all fixed and all described under "What live runs
  found" below — including the one that matters most for a fuzzy merge: the
  constructed ground truth scored 1.000/1.000 while real data came back 27%
  duplicated, because no fixture contained a country qualifier.
- **Schema inference is not built.** §13 says "user-defined or inferred schema",
  and only the first half exists. A dataset session with no schema is refused
  rather than inferred, deliberately: inference costs a model call, and a session
  that silently invented its own columns would produce a table nobody asked for and
  charge for it. It also could not be verified here for the reason above.
- **Parquet is not readable.** SQLite cannot read it and no decoder is written,
  so a Parquet export has to be converted before `mole connect` will take it.
  Named because "point mole at my data folder" quietly skipping half a folder is
  worse than refusing it — the count of ignored entries is reported.
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
- ~~**M6's claim extraction is unverified against a capable model.**~~ **Closed.**
  DeepSeek, one live run: 14 claims from PubMed and Crossref, all quote-verified, all
  reconciled, $0.0030. §11.5.2's grounding pass ran for the first time — 5 claims
  re-read, 4 confirmed, 1 flagged unsupported — and the synthesized report *used*
  that, writing "that finding is flagged as failing a source re-read, so it should
  not be relied upon". Grounding rate and citation accuracy are both measured
  numbers now rather than blocked entries. The original text is kept below for the
  record.
- **(Historical)** **M6's claim extraction was unverified against a capable model.** The academic
  path is confirmed working end to end — it queries both providers, deduplicates
  by DOI, builds openable citation URLs, and reconciles its ledger — but every
  live run produced zero claims. Isolated with a direct probe against a real
  arXiv abstract: the model proposed nothing at all (`proposed=0`), rather than
  proposing claims whose quotes failed §11.5. The only reachable model is a 3B
  local one that cannot do structured mining, and the Anthropic key has no
  credit. So the pipeline is verified and the extraction quality is not, and
  that distinction is the honest state of it.
- **Contradiction detection is measured now, and the number is not flattering.**
  The Verifier's precision had been checked once against a 37-pair set containing
  no true contradictions, so recall was `0/0`. That is fixed:
  `testdata/corpus/contradictions.json` was recorded end to end against DeepSeek
  (10/10 questions, ~400 claims, ~$0.18, cassettes on disk), producing 1,865 judged
  pairs — 115 `contradicts`, 146 `duplicate_of`, 1,604 `unrelated`. 159 of them were
  labelled by hand: every `contradicts` pair, complete rather than sampled, plus 44
  mechanically-shortlisted candidate misses. 149 got a label; 10 were skipped as
  genuinely ambiguous.

  | | value |
  |---|---|
  | adjudicator accuracy | **63%** (94 of 149) |
  | `contradicts` precision | **51%** |
  | `duplicate_of` precision | 64% |
  | `neither` precision | 100% |

  **Half the contradiction edges in a mole graph are wrong.** 50 of the 51 errors
  that change the graph are the same mistake: calling `contradicts` on a pair that
  is merely related. The judge almost never invents the opposite error — when it
  says `neither` it is right every time in this set.

  Recall is deliberately not quoted as a headline. The labelled set was drawn from
  the judge's own positives plus a numeric shortlist, so a contradiction neither
  found nor shortlisted is invisible to it; any recall computed here is an upper
  bound on a biased sample.

- **A confirm pass trades the error away, and it is a dial rather than a fix.**
  Re-judging the same pairs and keeping only confirmed edges (both arms recorded in
  `testdata/pairs/batch-size-arms.json` before the labels existed, so neither could
  be tuned to them):

  | | edges kept | precision | recall\* | F1 |
  |---|---|---|---|---|
  | graph as shipped | 105 | 51% | 98% | 68% |
  | confirmed by a second judgement | 53 | **70%** | 67% | 69% |
  | confirmed by three | 20 | **80%** | 29% | 43% |

  \*on the biased sample described above.

  F1 barely moves, so this is a choice about which error costs more, not a free
  win. For a tool whose output annotates "sources disagree", a false contradiction
  misleads a reader actively while a missed one is a silence — which argues for
  precision. The default is unchanged pending that decision.

  **The reproducibility numbers did not predict this.** Judged one pair per call,
  verdicts reproduce the stored graph only 35% of the time against 54% at batch 8,
  which looked like an argument for `--batch 1`. Against the labels, batch 1 is
  *worse* on both accuracy (54% vs 68%) and precision (64% vs 70%). Pre-registering
  both arms before labelling is the only reason that hypothesis was tested rather
  than shipped.

  The full §14.2 corpus is still deliberately not being built. Claim-precision
  labelling is ~2400 human judgements; if that number is ever needed, sample 200 and
  report an error bar.
- **A fatal error costs up to one batch, not one lead.** `Fatal` means the
  failure repeats — a bad key fails on every lead — so serially the next lead
  never starts and exactly one is charged. With a pool its siblings are already
  in flight. The batch is cancelled as soon as any worker reports a fatal, and
  the loop stops before leasing more, so the blast radius is bounded at one
  batch; it is not bounded at one lead, and `--workers 1` is the only way to get
  that back.
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
  schema, pricing table, ledger reconciliation, config permissions, both provider
  credentials — including whether the search provider returns page content, which
  decides whether §17.1's gate has a denominator — the sandbox, and the MCP socket
  are all checked for real. The socket check is a *requirement* rather than a note
  now that M7 has landed: an exposed socket directory or a world-connectable socket
  fails the command, because anything that can connect can spend the user's budget
  and read every claim they have collected. It calls the same `checkPrivateDir` the
  daemon enforces rather than a second hand-written check beside it. `doctor` also
  reads `.mcp.json` (and `~/.claude.json`) for a mole entry holding something
  key-shaped: the shim passes no credentials at all, so a key there is both useless
  and committed. The contact email stays a *note* — an install with no academic
  provider in use is not broken for want of an address.

---

## Licence

Apache-2.0. See `LICENSE`, and `NOTICE` for the third-party attributions that
travel with the binary.

Apache rather than MIT for the explicit patent grant, and rather than a
source-available licence (BUSL, ELv2, n8n's SUL) for a reason worth stating,
since the opposite choice is fashionable: for a tool that runs on the user's own
machine and spends the user's own API budget, the scarce resource is adoption,
not protection. A restrictive licence defends against a hosted competitor that
cannot exist until the project has users, and costs the distribution — package
managers, corporate legal review, the MCP ecosystem — that produces them.

It also sits badly with what mole is. §12's boundary is that data never leaves
the machine; a hosted mole inverts that, so the hosted product would be selling
convenience rather than the thing that makes this distinctive. Defending it with
a licence would be defending the weaker half.

Contributions are taken under a CLA, and `CONTRIBUTING.md` says exactly what it
is for rather than leaving it to be inferred: every *released* version stays
Apache-2.0 permanently, and the grant exists so future versions could respond if
somebody packages this as a service. Every dependency is permissive — 17 MIT,
16 BSD-3, 1 BSD-2, 4 Apache-2.0, and nothing copyleft.

---

## Before a public release

Deliberately deferred, tracked here rather than in a scratch file:

- **Comment pass.** The code is commented at the density of something being
  reasoned about in the open — every non-obvious decision carries its argument.
  Some of that is scaffolding for the build rather than for a reader, and should
  be cut once the shape has stopped moving.
- **Separate the dev commands from the product.** `mole dev`, `corpus`, `pairs`
  and parts of `eval` exist to build and check this thing, not to use it. They
  should be behind a build tag or a hidden group before the CLI is presented as
  a stable surface — an accidental `mole dev seed` against a real database is a
  bad first impression. `cmd/fixorphans` is a one-off repair for a mistake that
  can no longer happen and should simply go.
- **Finish the CLA.** The licence is settled — Apache-2.0, with `LICENSE`,
  `NOTICE` and `CONTRIBUTING.md` in place. `CLA.md` is a draft outline for a
  lawyer, explicitly not legal text, and it must be replaced with reviewed text
  before the repository goes public. The question inside it that is cheap now and
  expensive later: whether contributions are granted to an individual or to a
  company that does not exist yet.

---

## Reading less: the escalation ladder

The open question behind M6, and eventually behind every actor: **how few tokens
can answer the question?** Sending a whole document to a model is the expensive
default, and cost per correct claim is §14.3's headline metric.

The ladder, as M6 implements it for papers:

| Tier | Source | Cost |
|---|---|---|
| 0 | title + abstract | free — arXiv and PubMed return it in the search response, no fetch at all |
| 1 | full text, **ranked**, top sections only | one fetch, through the existing extractor |
| 2 | PDF | not built, and never fetched — so no `unsupported_type` arises from it |

Two decisions worth stating, because the obvious versions of both are wrong.

**Ranking, not searching.** The instinct is to ask a model which part of a
document holds the answer, then binary-search the rest. Binary search does not
apply: not finding the answer in one chunk says nothing about which other chunk
holds it, so there is no invariant to halve on and it degrades to a linear scan
at one model call per probe. What works is retrieval — rank every chunk against
the sub-question and read the top few. `verifier.LexicalRetriever` already does
exactly this for `research.ask`, at zero model calls, and papers have named
sections for it to rank.

**Escalate on a mechanical signal.** Asking the miner "did that answer it?" is
nearly free but is a model grading its own sufficiency. The gate is whether the
abstract's claims mention the sub-question's distinctive terms — a length and
stopword filter, not IDF, and no model opinion at all. A gate that cannot be
talked into spending money is worth more than a better-informed one.

Choosing a cheaper or stronger model per task is deliberately **not** in M6.
It cannot be tuned without the eval corpus, so any tuning now is guesswork
wearing the costume of optimization. Escalation is measurable today, in tokens
per claim, which `mole trace` already reports.

### How much is actually behind a PDF?

Tier 2 stays unbuilt until that is a number rather than an intuition — the same
decision-gate pattern §17.1 uses for the headless browser. It does not require
waiting for `unsupported_type` to accumulate: the metadata APIs answer it
directly. Unpaywall reports the PDF and landing-page locations separately, PMC
says whether a PMID has full text, and arXiv HTML availability is a `HEAD`. No
document is downloaded and no model is called.

Three buckets, not two, because only the middle one is a PDF extractor's job:

| | A PDF parser helps? |
|---|---|
| open access with HTML/XML | no — tier 1 already reads it |
| open access, PDF only | **yes, and only here** |
| not open access | no — there is no legal copy to parse |

The sample is `testdata/corpus/contradictions.json`, whose ten questions span
epidemiology, demography, energy, ML benchmarks, nutrition and economics — a
single-domain sample would answer this question wrong in a predictable
direction.

```
mole dev academic-coverage testdata/corpus/contradictions.json --per-question 6
```

**Measured, 2026-08-10** — 97 papers, 0 search failures, 3 DOIs Unpaywall could
not resolve (counted, not dropped). Raw rows in `testdata/coverage/`.

The same sweep sized Unpaywall's own contribution, which is smaller than §10.2
suggests: 46 of the 97 papers carried no DOI at all, and of the 28 unreadable
ones that did, Unpaywall placed none — so resolution is worth roughly 5%. The
binding constraint on academic coverage is missing identifiers, not unresolved
ones.

| | html | pdf_only | closed |
|---|---|---|---|
| **all** | 38 (39%) | 48 (49%) | 11 (11%) |
| pre-2024 | 10 | **42 (75%)** | 4 |
| 2024+ | 28 (68%) | **6 (15%)** | 7 |
| arXiv | 19 | 41 (68%) | 0 |
| PubMed | 19 | 7 (19%) | 11 |

The era split is the answer, and reporting one blended number would have been
misleading. For research published since arXiv's HTML rollout, 68% is readable
today and only 15% would need a PDF extractor. For the back catalogue it is 75%.
PDF-only is also overwhelmingly an arXiv phenomenon (68%) rather than a
biomedical one (19%), where the barrier is closed access instead.

So tier 1 is built first and tier 2 stays unbuilt: HTML covers most of the
current literature mole is actually asked about, and a PDF extractor's payoff is
real but bounded, concentrated in historical papers. The number to watch is whether real
sessions cite older work than this corpus does — which means re-running this
command on a broader corpus, not waiting for `unsupported_type` to accumulate:
the academic actor never fetches a PDF, so no live run can produce that outcome.
  

---

## Local data: the privacy boundary (M8)

Rev 1 of the sketch said "data never leaves the local machine; only aggregates
reach the LLM." That was a comment, not a mechanism. §12 makes it one, and M8
builds it in the order the sketch insists on — connector, then `sqlguard`, then
the aggregation gate, then the actor, and **not** the actor before both gates
exist.

### How a model analyses data it never sees

It doesn't. It chooses what to ask and reads what comes back; the computation is
deterministic SQL.

```
schema  →  a hypothesis template          (the model picks; §12.3 forbids it authoring SQL)
        →  read-only query
        →  AggregateEnvelope              n, quantiles, moments, TopK, test results
        →  the model writes the claim     "weekly seasonality in requests_per_hour,
                                           p<0.01, stable across 3 holdout windows"
```

The model sees column names, a test name, a p-value and an n. Never a row. This
is also more honest than letting a model read rows: a model shown five hundred
rows will describe a trend that isn't there, and here the trend is computed
before it is phrased — the same division of labour §11.3 already uses for
confidence.

### Choices behind this milestone

**SQLite, not DuckDB.** §10.2 picks DuckDB because it "embeds in the binary" —
but its Go driver bundles a C++ library and needs cgo, and mole builds
`CGO_ENABLED=0` precisely so it stays one static binary. The pure-Go SQLite
driver is already a dependency. The cost is real and worth stating: no Parquet,
and no built-in quantile or regression aggregates.

**A container runtime is never required to run mole.** The sandbox exists for
`CodeRunner`, which executes model-authored Python against real data. Without
podman or docker present, SQL analysis is unaffected and only code analysis is
unavailable — `doctor` reports which, the same way it reports a missing contact
email. mole itself is always a plain binary. See "The sandbox" below.

### `mole connect`

```
mole connect add sales ./exports/sales.csv     # one file
mole connect add exports ./exports             # a folder — one table per file
mole connect add warehouse ./warehouse.db      # attached in place, never copied
mole connect schema exports
mole connect list
mole connect remove exports [--purge]
```

A folder is read **one level deep**. `.csv`, `.tsv`, `.jsonl` and `.ndjson`
become one table each; anything else is ignored and the count is reported.
Subdirectories are not followed — a research tool that walks into folders nobody
meant to expose is the wrong shape for a privacy boundary.

Registration is the only moment mole reads a row. What it keeps is a profile —
type, null rate, distinct count, range — which is what §12.3's templates are
planned against. Columns holding prose or personal identifiers are flagged and
their values are never read at all, not even the min and max:

```
exports.jan — 3 row(s)  ← jan.csv
  COLUMN     TYPE       NULLS  DISTINCT  RANGE
  region     text       0      2         north … south
  units      integer    0      3         4 … 10
  revenue    real       0      3         220 … 1050
  closed_at  timestamp  0      3         2024-01-05T00:00:00Z … 2024-01-22T00:00:00Z
  rep_note   text       0      2         free text — excluded from top-values (§12.1)
```

### What `mode=ro` buys that `query_only` does not

§12.2 asks for least privilege first. A local file has no roles to grant, so the
equivalent is the handle: every query runs on a connection opened `mode=ro` with
`query_only(1)`, and no writable connection to connector data is ever opened
after import.

The two flags are not equals, and the test that established it runs statements
rather than inspecting the DSN. **`PRAGMA query_only = 0` succeeds** on the
handle — the SQL-layer flag can be switched off by the very statements it exists
to constrain, so alone it stops an accident and not an attempt. `mode=ro` refuses
at the VFS layer, cannot be reached from SQL, and is what actually holds: a write
still fails after that PRAGMA.

Two things follow. `sqlguard` must reject `PRAGMA` outright rather than treating
it as a harmless read-only verb. And any future engine whose read-only mode is
merely a session setting needs a different control here, because a settable flag
is not least privilege.

### The parse gate, and why half of it reads tokens

`sqlguard` is §12.2's second defence: one statement, and it must be a `SELECT`.
It checks two things by two different means, and the split was forced by what
the parser actually does rather than chosen for tidiness.

**Shape, from the parse tree.** Exactly one statement, and its type must be
`*sql.SelectStatement`. An allowlist of one, not a list of banned types — a
statement type added by a future parser version would otherwise be permitted by
default, and a gate that fails open on a dependency upgrade is not a gate. This
catches DDL, DML, `EXPLAIN`, `PRAGMA`, and `WITH … DELETE` (which parses as a
delete, so the CTE buys nothing). `ATTACH` and `VACUUM` are keywords the parser
has no statement type for, so they fail to parse at all.

It uses `ParseStatements`, and the plural matters. `ParseStatement` returns the
leading statement of `SELECT 1; DROP TABLE sales` **with no error** — a guard
built on it would inspect the `SELECT`, approve, and hand the whole string
including the `DROP` to the driver. One identifier apart in the API, and a test
pins it.

**Vocabulary, from the token stream.** Every identifier immediately followed by
`(` must be on a function allowlist. This is not done over the AST because
`sql.Walk` **does not descend into CTE bodies or subquery expressions** —
measured. For

```sql
WITH m AS (SELECT readfile('/etc/passwd') FROM s) SELECT COUNT(*) FROM m
```

a Walk sees `COUNT` and the reference to `m`, and never sees `readfile` at all.
An AST-based function check would have holes in precisely the places an escape
hatch would be put. The token stream has no gaps — the scanner emits every
token — so a rule expressed over tokens is complete by construction even though
it understands no grammar.

Adjacency is measured in tokens, not in source text, and that caught a hole in
an earlier version of this guard: the scanner emits a comment as its own token,
so `readfile/* nothing to see */('/etc/passwd')` put a `COMMENT` between the
name and its parenthesis, and a check tracking the raw previous token decided it
was not a call and permitted it.

The allowlist is short and adding to it is one reviewable line. Denying the
escape hatches by name instead would mean every function SQLite gains — and
every extension a future build links in — is permitted until someone remembers
to deny it. Absent on purpose: `load_extension`, `readfile`, `writefile`,
`edit`, `fts3_tokenizer`, `hex`, `quote`, `randomblob`, `zeroblob`. Anything
named `sqlite_*` or `pragma_*` is refused as a call *and* as a plain table
reference — SQLite reserves that prefix, so no user table can collide with the
rule.

### The aggregation gate

§12.1's rule is that nothing crosses to a model except an `AggregateEnvelope`.
That is enforced by a type rather than by discipline: **the package exposes no
function that returns rows.** `Aggregate` reads the result set, computes
statistics, and returns the statistics — the rows exist only inside that call,
and there is no API through which to obtain one. A `Query` returning rows plus a
`Summarize` turning them into an envelope would enforce nothing, since the
property would hold only while every caller remembered the second call, which is
the situation §12.1 exists to end.

**It refuses rather than truncates.** A result past `maxRawRows` is an error, not
a summary of its first five thousand rows — a summary of an arbitrary prefix of
an unordered result describes nothing while reading like a description of the
whole.

Two refusals are about SQLite specifically, and neither is obvious from the
sketch:

- **A bare column beside an aggregate.** SQLite permits `SELECT rep_note,
  COUNT(*) FROM tickets` and answers `rep_note` from an arbitrary row. That is
  one row of real data wearing an aggregate's clothes, and it passes any check
  that only counts result rows. The same permissiveness applies inside a group,
  so every non-aggregated result column must appear in `GROUP BY` — by position,
  by expression, or by alias.
- **A grouped query must select `COUNT(*)`.** Without it there is no *k* to
  compare against the floor. `SUM(spend) = 5` is not five records, so
  `SELECT email, SUM(spend) … GROUP BY email` would otherwise cross with one
  bucket per person and nothing to suppress it on.

A windowed call is not an aggregate regardless of its name: `COUNT(*) OVER ()`
returns one row per input row, so a statement whose only aggregate is windowed
is the raw result set with a count stapled to each row.

**The k-anonymity floor** folds every bucket covering fewer than five records
into a single `other` bucket, which names no key — that is what makes reporting
its count safe. The number folded is reported as `Suppressed`, because a
distribution missing its tail reads as a complete one otherwise.

**Free text is decided twice.** The connector flags it at ingest; the gate
re-derives it from the result, because a result column can be an expression no
profile ever described — `MIN(rep_note) AS lo` is a column called `lo` that
holds somebody's note. It is the *same rule*, exported from the connector rather
than reimplemented, since two copies would drift and the one that drifted would
be the one deciding whether prose reaches a model. A free-text column carries no
range, contributes no buckets, and grouping on one produces no buckets at all.

### The exfil regression, and the two leaks it found

§14.3 asks for an assertion that no row-level data crosses the aggregation gate.
It is written as a **property over generated query shapes** rather than a list
of examples — 296 statements built from the cross product of somewhere to group
and something to select, against a fixture whose sensitive values carry a
canary. Each envelope must contain no canary and no bucket describing fewer
than `KFloor` records. 264 of the 296 produce an envelope, so the property is
not holding vacuously.

It found two leaks the hand-written tests had missed, and both were real:

**Column ranges outlived their buckets.** For

```sql
SELECT code, COUNT(*) FROM records GROUP BY 1
```

every record is its own bucket, so all twenty were suppressed and no bucket
crossed — and `ColumnStats.Range` still reported `Zq7Kx00 … Zq7Kx19`, two
individual records. The k-anonymity floor protects *buckets*; nothing protected
the column summary. The fix restructured the accumulator into two phases,
because which rows may be described cannot be known until every row has been
read: **counts** are now computed over the whole result (a count discloses no
value), and **ranges and moments** only over the rows of buckets that survived.

**A text extremum is a record, not a statistic.** `MIN(code)` over a group
returns one specific record's code, selected by an ordering rather than
summarized — and it crossed even for groups well above the floor. Text columns
now carry a range only when they are a grouping key. Numbers are different: the
extremum of a numeric column is the summary statistic §12.1 asks for by name.

Alongside the property test the gate now **self-checks at runtime**: the
accumulator still holds every value it read, so before an envelope is returned
it is compared against the data it came from, and one carrying a value it was
not entitled to is withheld rather than reported. The permitted set is derived
from the rows the gate was allowed to describe, **not** from the envelope — an
earlier version read it out of the envelope's own bucket keys and ranges, which
meant a leaked value authorised itself. Both positive controls failed, which is
what positive controls are for.

### The actor: what a model decides, and what it cannot touch

```
profile  →  the model picks a template and columns   §12.3 — it cannot author SQL
         →  hypothesis.Render turns that into SQL     identifiers come from the profile
         →  sqlguard, then the aggregation gate       §12.2, §12.1
         →  the model mines claims from the envelope  §11.5 quote check, unchanged
```

The middle two steps are deterministic. What the model influences is which
question gets asked; what it never touches is the data, the statement, or
whether the answer may cross.

**Why lookup and not escaping.** §12.3 says web-derived content "can influence
template and column choice; it cannot author SQL". The model returns a template
name and column names, and those names are used as **lookup keys against the
connector's profile** — never spliced into a statement. A column called
`region" ); DROP TABLE tickets; --` does not match a column, so the plan is
refused. Escaping would make a hostile name safe to interpolate; lookup makes it
impossible to interpolate anything that is not already a column of a registered
table. That is a stronger claim and a simpler one, and it is what the injection
tests exercise.

**A local claim is cited and quoted like any other.** Its source is
`connector:<name>#<query hash>` — §4's "connector name + query hash" — and its
quote must appear verbatim in the rendered envelope. So §11.5 applies unchanged:
the envelope is the document, the numbers are the text, and a model that writes
"revenue fell 40%" without those words in front of it has the claim dropped
exactly as a fabricated web quote would be.

**A local-only session no longer needs a web search provider.** It used to:
the web actor was built unconditionally and refused without a Brave or Tavily
key, so analysing a CSV required an account with a search company — the precise
opposite of what §12 is for. Search is now required only when `web` is among
`--actors`.

```
mole connect add sales ./exports/sales.csv
mole research "how do the regions compare on revenue" --actors local_compute
```

### Statistical validity: what stops a model calling two means a finding

§4's actor table says a local claim is verified on **n, effect size,
significance** — where a web claim is checked for credibility and a paper for
venue signal. The difference is that a page *asserts* and a query *measures*, so
there is something to test.

A model handed `north: mean 100` and `south: mean 40` will describe a trend.
That is not a prompting problem and no instruction fixes it — the fix is for the
significance to arrive **as evidence, alongside the means**:

```
Statistical tests:
  the mean in "north" is higher than in "south" by 60 (means 100 and 40; n = 40 and 40);
  statistically significant (Welch t = 42.43, p = <0.001), effect size 9.49 (large),
  95% CI 57.23 to 62.77
```

and, when it is not:

```
  …; UNDERPOWERED — fewer than 20 records in a group, so this difference is not
  evidence either way
```

Because that sentence is in the passage claims are mined from, §11.5 applies to
it: a claim about the difference has to quote it or be dropped. And any claim
mined from an envelope whose comparison was *not* significant has its
`AssertionStrength` capped — it may still be true, but it must not enter the
graph asserting as much as a measured result, since §11.3 derives confidence
from what each source claims for itself.

**It runs on aggregates, so it fits behind the gate.** A count, Σx and Σx² are
sufficient statistics for a mean, a variance and a two-sample test, and all
three are aggregates §12.1 already permits — which is why the group-comparison
template selects the two sums. No sample is held and no row is read.

Welch's t-test rather than Student's, because equal variances is an assumption
nothing here can check. `MinGroupN = 20` is a judgement and is written down as
one: it exists so that p = 0.03 from six records against five is reported as
*underpowered* rather than as a finding — which is exactly what §4's row is
there to prevent. "Underpowered" is its own verdict rather than folded into "not
significant", because the two lead to opposite next actions.

The t distribution is implemented rather than imported — forty lines against a
statistics library in a binary that is one static file on purpose. It is checked
against published critical values at df = 2, 10, 20, 48 and ∞, and against the
closed form `1 − |t|/√(t²+2)` at df = 2. The incomplete beta's two evaluation
paths are asserted to agree, because that identity is what both of them rest on.

### Stability across holdout windows

§4's row asks for n, effect size, significance **and** stability. The first three
were computed and enforced from M8; the fourth was a known gap needing "three more
queries per hypothesis and a splitting rule nobody has chosen". It needs one more
query, and the rule is `rowid % 3`.

Why re-test at all: a significant result on one sample is one draw, and the failure
that misses is the one this tool is most exposed to — a difference driven by a
subset (one month, one region's rows, one batch of imports) reported as a property
of the data. No amount of care with α catches that; disjoint subsets do.

- **The rule is direction, not significance.** A third of the rows has a third of
  the power, so requiring each window to clear α would mark almost every real
  effect unstable — a statement about sample size dressed as one about the data. The
  count of individually-significant windows is reported beside it.
- **`rowid % 3` is deterministic and interleaved.** Deterministic so a replayed
  session gets the same evidence rather than a coin toss. Interleaved rather than
  sliced, because contiguous thirds of a file exported in date order would turn the
  check into a comparison of three time periods — which fails for a seasonal
  measure that is perfectly stable.
- **The per-window tests are not Holm-corrected against each other.** They are the
  same hypothesis re-examined on disjoint data, not three new hypotheses. (Contrast
  the pairwise comparison above, where correction is exactly right.)
- **"Could not be checked" never looks like "stable".** A third of a group often
  falls under the k-anonymity floor — the privacy guarantee holding, not a fault —
  and the clause says so explicitly.

The clause lands in the sentence a claim must quote, for the same reason the Holm
correction does:

```
the mean in "south" is lower than in "north" by 20 (means 80 and 100; n = 90 and 90);
statistically significant (p = <0.001), Welch t = -6.47, effect size -0.96 (large),
95% CI -26.14 to -13.86; the difference does NOT hold across holdout windows — it
points the same way in only 1 of 3, so it may be driven by a subset of the records
rather than being a property of the data
```

The holdout statement is audited like any other crossing: it reads the same data
and its figures reach the same model, so a trail that recorded one and not the
other would understate what left the machine.

### Comparing more than two groups

The gate compared the two largest buckets and nothing else, with the reason
written into the code: k(k−1)/2 tests against the same alpha manufacture
significance out of noise. That is right about the danger and wrong about the
remedy — ten groups at α = 0.05 give a 90% chance of at least one spurious
"significant", which is why multiple-comparison corrections exist. Declining to
look at eight groups out of ten is not the only alternative, and a user with five
regions was told about two of them.

Every pair is tested now, and every p is **Holm–Bonferroni adjusted** before any
verdict is derived from it. Holm rather than plain Bonferroni because it is
uniformly more powerful at the same family-wise error rate — it dominates
Bonferroni rather than approximating it — and rather than Benjamini–Hochberg
because BH controls the false-discovery *rate*, which is right for screening a
hundred hypotheses and wrong when a research tool is about to state a difference
as a finding.

Three details that are the point rather than the implementation:

- **One comparison is the identity case of the same code.** There is no separate
  two-group rule, and no version of the verdict that only the single-comparison
  path takes.
- **The correction is in the sentence a claim must quote**, not only in the JSON:
  `…, unadjusted p = 0.037 over 15 pairwise comparisons`. §11.5 is the only lever
  that stops a model writing "significant" over a p-value it should not have
  believed, and a correction present only in a field it never reads is no lever at
  all.
- **Six groups is the cap** (fifteen comparisons), past which Holm's threshold for
  the smallest p is α/15 and a genuine moderate effect stops being detectable. The
  largest are compared and the envelope says how many were left out — the honest
  version of the old behaviour rather than a silent one.

A test pins the property the old restriction existed for: a result that is
significant on its own stops being significant as one of fifteen.

### What live runs found

Five sessions against real data (400 rows, four regions): three local, over
ollama, and two against DeepSeek once credit existed. Between them they found
three bugs that every test in the repository had missed.

**A verbatim quote can drop every qualification in the sentence it came from.**
This is the worst of the three, and it invalidated a claim made in four separate
doc comments. The passage said:

```
the mean in "west" is higher than in "south" by 63.67 (means 141.97 and 78.30;
n = 100 and 100); statistically significant (p = <0.001), Welch t = 24.49, effect
size 3.46 (large), 95% CI 58.54 to 68.80, unadjusted p = <0.001 over 6 pairwise
comparisons; the difference points the same way in all 3 holdout windows
```

and DeepSeek quoted it exactly as far as `(p = <0.001)`, then stopped. §11.5 passed,
because a prefix *is* a verbatim substring. The Holm correction, the effect size and
the holdout stability all vanished — from the claim, from the citation, and from the
synthesized report, which then said "a statistically significant difference" with
nothing qualifying it. Every comment in `internal/compute/stats` arguing a
qualification is safe "because it is in the sentence a claim must quote" was resting
on an assumption nobody had checked: that a quote covers the whole sentence.

The fix widens a quote that lands inside a verdict line to the whole line, rather
than refusing the claim — refusing throws away a true claim over its packaging, and
the widened text is still verbatim, which is the property §11.5 exists to protect.
The claim's own text is left alone: that is the model's assertion, and rewriting it
would be mole putting words in its mouth. Verified by replaying the recorded
cassette of the run that found it, so the same model output now produces the
complete sentence.

**Local claims were never persisted.** `WebActor` and `AcademicActor` each write
their own claims; `LocalComputeActor` returned them in `Result` and wrote nothing,
and the executor only carries `Result.Claims` in memory for the planner's digest.
The report, `mole eval` and `mole ask` are all built from the *store*. So the run
printed "1 claim(s)" as it went past and then produced a report saying *"No
verifiable evidence was found"*, with the scorecard agreeing there were no claims.
Every local claim mole had ever mined was discarded after being paid for.

**A local claim's citation was scored as malformed.** `claim integrity` parsed
every source with `url.Parse` and required a host — and §4 defines two citation
shapes: a URL for web and academic claims, and `connector:<name>#<query hash>` for
a local one, because the data never left the machine. Five well-formed claims
scored 0% on the one metric whose entire purpose is to catch claims the pipeline
should have rejected.

All three are the same lesson the M8 review wrote down and this milestone had to
learn again: falsification tests the rules you wrote. None of them is a rule anyone
wrote down — they are what happens between two components that were each tested
alone, and no amount of testing either component alone would have found them.

What the runs *did* confirm, having been unverifiable before: a model choosing a
template and columns that render, the aggregation gate answering, §4's holdout
statement running against real data, the audit trail recording four crossings with
no refusals, and §14.3's exfil number reading `0 of 4 envelope(s) withheld` — the
first time that metric has been measured on a session rather than named as blocked.

### The audit trail: a table, not a log line

§12.1 asks that "every crossing is logged, so a user can audit exactly what left
their machine". M8 emitted a structured log line, which meets the letter of that
and not the use: logs rotate, `Info` is off in some setups, and a line cannot be
queried per session. The question a user actually has — *what did mole send about
my sales data* — is a query against a table.

```
mole crossings <session-id>            # one row per crossing
mole crossings <session-id> --queries  # each statement, and the reason for each refusal
```

```
WHEN                  CONNECTOR  OUTCOME   ROWS  BUCKETS  SUPPRESSED  WITHHELD COLS  HASH
2026-08-11T10:02:04Z  sales      crossed     40        2           1              1  447a678ffad5900a
2026-08-11T10:02:05Z  sales      refused      0        0           0              0  9c1e0b77a2f4d310

2 crossing(s): 1 aggregate(s) crossed, 1 refused, 0 withheld.
```

Three properties, each deliberate:

- **Every outcome, not only the successes.** A trail of what crossed would let a
  reader conclude that the questions mole answered are all it tried, and the
  refusals are the more interesting half — they are the gate doing the thing the
  user is trusting it to do. Attempts refused *before* a statement was even
  rendered are in there too, with the plan rather than a fictional query.
- **`withheld` is its own outcome**, not a kind of refusal. A refusal is the
  design working; a withholding means the exfil check — a backstop for structural
  rules that refuse first — caught a rule upstream having broken. The command says
  so in those words and asks for a bug report.
- **No value from the data.** Counts, a hash, one of mole's own reason strings, and
  the statement. The statement is safe to keep for a reason specific to §12.3: a
  model cannot author SQL, so a query is a template filled with identifiers the
  profile already published. An audit trail that is another copy of the thing the
  user was worried about would be worse than none.

The table is what makes §14.3's exfil number *measured* rather than blocked.
`mole eval` reports the share of envelopes that had to be withheld — zero, and a
hard regression at anything else — beside the gate's refusal rate and how many
buckets fell below the k-anonymity floor. It had been listed as blocked twice
over, once with the reason "needs the aggregation gate (M8)" long after M8
landed, which is what a list of excuses decays into if nothing ever leaves it.

### The sandbox: detected, measured, and never required

§3.6 decides the technology before `CodeRunner` is written. `mole doctor`
reports what it found:

```
✓ sandbox            docker 29.6.2, rootful, seccomp available, cgroup v2
✓                      no network, read-only rootfs, all capabilities dropped,
                       uid 65534, 1 cpu, 512MB, 64 pids, 30s wallclock
```

and when there is nothing:

```
! sandbox            no container runtime found (podman: not on PATH; docker: not on PATH)
!                      local code analysis unavailable; install podman or docker to enable it
!                      local SQL analysis is unaffected (§12.2)
```

**It is informational, and a test asserts that.** Running `doctor` with and
without a runtime on `PATH` must produce the same verdict — anything else would
tell every CI job and install script that a missing runtime breaks the install.
The second line is the point of the check: "not found" says something is missing
without saying whether it matters, and usually it does not.

**A binary on PATH is not a runtime.** A Docker install with a stopped daemon
has the binary and can run nothing, so detection asks the runtime about itself
and reports what it says. Present-but-unusable is a third state with its own
message, because "install a runtime" and "your runtime cannot filter syscalls"
are different problems. Seccomp is required — without a syscall filter a
container is a namespace trick around code somebody else wrote. cgroup v1 is a
warning, not a disqualifier: memory and pid limits still apply.

**The flags are verified, not described.** A `doctor` line claiming "netns
disabled, seccomp default" while the runner forgot `--network=none` would be a
check reporting a property nothing enforces, so the summary is rendered from the
same flag list `CodeRunner` will pass — and a test runs a real container to
confirm each one holds:

| observed | from |
|---|---|
| `uid=65534(nobody)` | `--user=65534:65534` |
| `Network unreachable` | `--network=none` |
| `Read-only file system` | `--read-only` |
| `/tmp` writable, `/tmp/e: Permission denied` | `--tmpfs=…,noexec` |
| `CapEff: 0000000000000000` | `--cap-drop=ALL` |

That test skips without a runtime and never pulls an image — a suite that
downloads 150MB on a cold cache is a suite people switch off.

**And the interpreter itself is verified now**, not only the container around it.
Every other container test runs a POSIX shell script, because the mechanism —
mount, stdin, output cap, wallclock, exit code — is not interpreter-specific, and
`python:3.13-slim` could not be pulled when the package was written. It can be
now, so `DefaultCommand()` (`python3 -`) runs for real against
`python:3.13-slim`: a script that opens the mounted database with Python's own
`sqlite3` module and gets its declared metrics back; a script that tries
`UPDATE … SET spend = 0` and cannot (the file is deliberately world-writable, so
the read-only mount is the only thing left standing); `socket.getaddrinfo` and
`urllib.request` both failing, which is `--network=none` observed by an
interpreter that can actually try it, rather than a flag read back from a list;
and a `raise` reported as a failed run with the traceback intact rather than as an
empty result. The undeclared-key channel is checked from Python too — it
serialises a row value as easily as a shell does, and the gate stops it.

These skip where no python image is present locally (`docker pull
python:3.13-slim`, or `MOLE_PYTHON_TEST_IMAGE`), for the same reason as above.

**Both runtimes are verified now, and podman found a bug in the tests rather than
in mole.** `Order()` prefers podman, so installing it made podman the detected
runtime — rootless, seccomp available, cgroup v2 — and every container test above
passes under it, including `--user=65534:65534` inside a user namespace and the
read-only mount holding against a deliberately world-writable database file.

What did not pass was the *helper*: podman lists images fully qualified
(`docker.io/library/python:3.13-slim`) where docker lists them short, and the
helper matched a bare `python:` prefix against the whole reference. So every Python
test **skipped silently** on a podman machine while the suite reported green. It
matches the repository component now. The lesson is the one this project keeps
relearning in a new costume: a test that skips is not a test that passes, and a
helper written against one runtime's output format encodes that runtime.

### `CodeRunner`: the boundary moves inside the container

§12.1 says where this is needed: *"Where analysis genuinely needs row-level data
(regression, seasonality decomposition), the computation happens inside the
sandbox and only its output envelope crosses the gate."*

So the script gets the connector's database **in full, read-only** — and that is
not a concession. Inside the sandbox there is no model to protect the data from:
no network, no writable filesystem, no capabilities, uid 65534. Extracting a
subset first would put a second copy of the user's data on the host, written by
mole, for nothing.

**The channel out is the whole design.** The script's stdout is untrusted — a
model wrote it, and a model reading a prompt-injected page is a threat §3.2
already assumes. So output is not parsed as "whatever the script wanted to say".
It is matched against names the plan **declared before the script ran**, and
values that are finite numbers:

```json
"code": {
  "script":  "…",
  "metrics": ["weekly_amplitude"],
  "tests":   ["seasonality"]
}
```

Declare `weekly_amplitude` and print `{"ada@example.org": 1}` and nothing comes
back — because the key is not on the list, not because something inspected it
for personal data. That is §12.3's choice again: the model declares, mole looks
up.

It is bounded, not zero. A determined model could encode a value in the digits of
a declared number. What it stops is the realistic case — a script that prints its
input, or an output shape nobody constrained.

**The verdict is not the script's to decide.** It supplies n, the statistic and
p; significance is derived with the same thresholds as the SQL path, so the two
routes cannot disagree about what counts as evidence. A claim from an
underpowered sandbox finding is capped exactly as one from an underpowered
`GROUP BY` is — otherwise the code path becomes the way around §4's check.

**A code claim cites the script, not a query.** `connector:<name>#code:<hash>` —
the script is what produced the figures; a query hash would name a statement that
only suggested what to look at. And it quotes its evidence like every other
claim: §11.5 applies unchanged.

That last part produced the one behavioural bug of the slice. The renderer first
emitted `amplitude = 12.5000` — nineteen characters, under §11.5's 24-character
minimum — so **every claim about a single metric was silently dropped by the
quote check**, which looked exactly like a sandbox returning nothing. Metrics are
now rendered as sentences.

Verified against a real container: the mounted database is readable and not
writable, a script printing half a megabyte is refused rather than parsed, a
script that never finishes is killed at the wallclock limit, and stderr reaches
the model so the next attempt is not the same attempt.

### What the M8 review found

Three parallel reviews — security, correctness, code quality — over roughly
10,000 lines. Every finding below was reproduced before it was fixed and is now
held by a regression test naming the shape that found it. The list is longer than
any previous milestone's, on the milestone built fastest.

**The central claim was falsified twice.** M8's premise is that rows never reach
a model. Two paths did:

- The **schema prompt**, rendered on every local run *before any query exists*,
  carried `MIN`/`MAX` for every column that was not prose — with no k-anonymity
  floor anywhere on that path. A probe registered a six-column CSV and the prompt
  contained a real name, a real SSN, a real phone number and a real date of
  birth. Two causes: `IsFreeText` returned false on the **type** check before the
  personal-identifier name list was read, so a column named `ssn` was never
  considered; and a range was recorded for any non-prose column. A range now
  requires `rows/distinct >= RangeFloor`, the same idea the gate applies.
- The **coderunner's refusal list** echoed the script's own undeclared *key* text
  into the passage handed to the miner, so a script could return anything by
  putting it in a key. A probe got 12KB of records out — past a package comment
  claiming that emitting `{"ada@example.org": 1}` "gets nothing out".

**Two ways to fabricate statistical significance.** Both reachable from the
ordinary template:

- `COUNT(*)` was used as *n* while `SUM` skips NULL. Two groups whose every
  non-null value was 10, one with half its measure missing, reported *"means 10
  and 5 … significant, p<0.001, effect size 1.41 (large)"* beside a bucket line
  printing `mean 10` for the group the test called 5. Blank CSV cells become
  NULL, so this was the default state of a real export.
- `Σx² − (Σx)²/n` cancels. At a mean of 1e7 the variance came out **ten times too
  small** and the test reported `p = 6.2e-10` for data whose true p is 0.171. The
  template now centres the measure — variance is shift-invariant — and the
  fallback guard was itself rebuilt after a measured sweep showed the first
  version accepted variances up to 18% wrong.

**Two k-anonymity bypasses.** `NULL` was encoded as `""`, so a NULL group and an
empty-string group shared a bucket: three records each under a floor of five
merged into six and *crossed*. And the floor was never applied to an ungrouped
aggregate, so the overview template over a one-row table published that row five
times.

**One crash.** Only a bare `*` was refused, so `SELECT t.*, COUNT(*)` passed
classification with a two-element shape against four result columns and panicked
with the whole result set in memory.

**One silent data loss.** `safeIdent` accepted only lower case, so an ordinary
CamelCase database registered with no error and a profile of **one table with one
column** — the model then planned over a schema that was not the user's data.

**Three claims the code did not keep.** §12.1's "every crossing is logged"
emitted nothing (written at `Info`, actor logger at `Warn`); `Usable = Seccomp`
where seccomp was neither passed nor verified, now checked by reading
`/proc/self/status` inside a real container; and a sandbox-only run printed "every
figure above came through the aggregation gate" for evidence whose own code says
it never touches it.

**Eight duplications**, two already divergent: the verdict rule (one required both
groups to clear the floor, its copy checked one), the verdict sentence, three
number formatters at two precisions — one of which rendered a rate column of
0.0001–0.003 as `0.00`, so §11.5 permitted only a wrong number as a citation.

Also fixed: JSONL integers corrupted through float64, a BOM readable in CSV but
not JSONL, `inf` inferring as a number, a failed re-import destroying a working
connector, `p` never range-checked so the script controlled both verdict inputs,
cancellation reported as a timeout, a cancelled caller waiting 40 seconds, an
unenforced input budget, `Suppressed` conflating the privacy floor with a
presentation limit, and the exfil check's allow-list derivation — the heart of the
check — covered by no test because every fixture value sat under the length floor.

**Why the milestone's own falsification missed all of it.** Every HIGH finding
came from a *data* shape, and the falsification pass tested the *rules*.
Twenty-two mutations on the statistics slice and not one used a NULL, an uppercase
identifier, a large magnitude, or a qualified star. Reverting a mechanism proves a
test can see that mechanism break; it says nothing about inputs the test never
supplies.

---

## Dataset mode (M9)

§13's third output mode: `user-defined schema → per-lead row extraction →
cross-source merge/dedup by fuzzy key → CSV/JSON`. §15 warns that "schema
inference plus cross-source fuzzy merge is the hardest quality problem in this
document", and that warning shaped every decision below.

```
mole research "revenue of the largest UK grocers" \
  --usd 2.00 --mode dataset \
  --schema 'company:text!,revenue:number=annual revenue in GBP,founded:date'

mole dataset s_06FZ... --format csv --provenance > grocers.csv
mole dataset s_06FZ... --format json            > grocers.json
```

A `!` marks a **key** field. It is required, and the refusal says why: without one
there is nothing to merge on, and a dataset that cannot be merged is a list of
quotes wearing a table's clothes.

### A row is a claim with columns

Rows are extracted in the **same model call** that would have mined claims — not a
second pass — so dataset mode costs the same per chunk as report mode. They go
through §11.5 unchanged: a row whose quote is not verbatim in the page is dropped,
and the rejection is counted. That standard does not relax because the output has a
header line; a CSV is *more* likely to be believed without checking, since nobody
reads a spreadsheet sceptically.

Three further refusals: a row with no key field identifies nothing, a value the
field's type cannot hold is dropped rather than carried (a number column holding
"roughly £1.2m" merges against nothing and renders as something somebody will
sort), and a field outside the schema is ignored — the reply is model output, so a
key nobody asked for would put text the model chose into a file header.

Values are normalised at **extraction**, not at merge time: "1,200,000" against
"1200000" is a disagreement only if nothing looked at the declared type. The forms
pages actually write are accepted — `$1.2m`, `1.5bn`, `12k`, `(4500)` for a
negative — because dropping them loses the fact rather than the noise.

### The merge, and the numbers behind it

The measure went through three versions, each rejected by measurement rather than
review:

| attempt | precision | why it failed |
|---|---|---|
| Jaro-Winkler + overlap ÷ smaller set | 0.583 | "Acme" ⊂ "Acme Bakery Holdings" scored 1.0 |
| the same ÷ union | 0.583 | Jaro-Winkler scored the same pairs 0.88 on its own |
| **token-level, JW inside a token** | **1.000** | — |

String similarity turned out to be the wrong primitive: Jaro-Winkler's prefix boost
is built for short personal names, and on company names the distinguishing word
comes second — `deutschebank` vs `deutschetelekom` scores 0.89. What separates
them is which *tokens* they share, with Jaro-Winkler used only *inside* a token to
absorb a typo:

```
acme            / acme bakery       0.50   not a match
deutsche bank   / deutsche telekom  0.33   not a match
deutsche bank   / deutsche bnak     1.00   a match, typo absorbed
british airways / airways british   1.00   a match, order absorbed
```

At the default threshold: **precision 1.000, recall 1.000**, 16 rows from 31
extractions against 16 known entities. The threshold is a *gap* rather than a knife
edge — every true pair scores ≥0.67 and every false one ≤0.50 — and the test prints
the whole curve, so 0.60 is calibrated rather than chosen.

**Complete linkage, not transitive closure.** A row joins a cluster only if it
matches every member, or "Acme Foods", "Acme Foods Europe" and "Acme Foods Europe
West" collapse into one row because the middle matches both ends.

**Conflicts are preserved, never resolved.** §11 renders contradiction edges
explicitly "rather than silently resolved by whichever claim the model liked", and
two sources giving one company two revenues is that problem with a column header.
Agreement is preferred to arrival order and the outlier is kept. A key field's
alternatives are *spellings*, not disagreements — the merge grouped those rows
because it judged the keys to name one entity, so reporting the difference as a
conflict would contradict its own decision.

**The merge runs on read.** `mole dataset` re-derives from the stored rows, so a
later fix to the matching rules improves every dataset already collected rather
than only the next one. It costs no model call — the merge is arithmetic.

### The two formats are not equivalent

JSON is complete: every cell, every disagreeing value, every source and quote. CSV
holds one value per cell, so it is lossy — and rather than pretend otherwise it
carries a `sources` count and a `contested` column naming the fields the sources
could not agree on. A format that quietly dropped disagreements would be the most
convenient output and the least honest one.

The summary goes to **stderr**, so `> out.csv` gets only data: what qualifies a
dataset is the part a redirect would otherwise discard.

### What `mole eval` reports, and what it cannot

Four numbers need no labelled data and are the ones that qualify a result — "40
rows" invites confidence that "40 rows, 31 from a single source, 6 contested" does
not:

```
dataset row integrity     100%   40 of 40 rows carry a source and a verbatim quote
dataset merge collapse     34%   61 extractions became 40 rows
dataset corroboration      22%   9 of 40 rows have more than one source
dataset disagreement       15%   6 of 40 rows have a field the sources disagree about
```

Row integrity is the one that is a **hard regression**: extraction already refuses
a row without a quote, so a stored one means something bypassed §11.5.

Merge accuracy is reported as *measured elsewhere* — on constructed ground truth,
because that needs no labelling — the same treatment §14.3's exfil assertion gets.
A per-session accuracy number would need labelled answers, exactly as §14.2's
corpus does.
