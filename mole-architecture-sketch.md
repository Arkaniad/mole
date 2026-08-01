# Mole — Deep Research Agent, Architecture Sketch (rev 2)

> **Naming.** Binary and daemon are `mole`; the MCP shim is `mole-mcp`; the MCP server key
> is `mole`. MCP *tool* names keep the `research.*` verb namespace (`research.report`,
> `research.cancel`) because MCP already prefixes tools with the server name — a caller
> sees `mole:research.report`, where the server identifies the product and the tool
> identifies the function. Renaming the tools to `mole.*` would produce `mole:mole.report`.

> **Revision note (rev 2).** Previous revision dropped in-repo LinkedIn/YouTube/Reddit
> scrapers and added an `academic/` package on stable free APIs. This revision fixes the
> structural problems found in review — the ones where the design *asserted* a property
> as discipline rather than *enforcing* it as a mechanism:
>
> - Cost was claimed linear but the planner loop made it quadratic (§9).
> - Budget was charged after the fact, so a worker pool overshoots the ceiling (§8).
> - Report generation had no reserved budget and could be starved by research (§8).
> - The recheck cycle guard didn't bind, because follow-up leads made fresh claims (§11).
> - The dedup cache skipped leads instead of returning cached results, causing livelock (§9).
> - "Claim graph" was a flat table with no edges — contradictions had nowhere to live (§7, §11).
> - The Verifier could not check *grounding*, only self-consistency (§11).
> - Local-data privacy and MCP credential isolation were conventions, not enforced boundaries (§3, §12).
> - SSRF, prompt injection, LLM-authored SQL against a live DSN, and daemon authn were unaddressed (§3, §12).
>
> Security model is now §3, up front, because several downstream designs depend on it.
> An eval harness is now a first-class milestone (§14) rather than absent.

A Go-based Planner–Executor–Verifier research agent with budget as a first-class primitive.
Supports web research, academic-literature research, and local-compute analysis through a
common Actor interface, exposes itself to coding agents (Claude Code, Cursor, OpenCode,
CodeWhale) via MCP, and supports either dollar-budget or token-budget accounting.

---

## 1. Core concepts

| Concept | What it is |
|---|---|
| **Session** | One research task: prompt + budget + mode (report/dataset/chain/ask) |
| **Lead** | A thing worth investigating — starts as a query or hypothesis, may spawn sub-leads |
| **Claim** | An atomic extracted fact, tied to a source, an evidence quote, and a tool call |
| **Claim edge** | A typed relation between claims: supports / contradicts / duplicate-of / supersedes |
| **Actor** | Executes a lead — `WebActor`, `AcademicActor`, or `LocalComputeActor` |
| **Ledger** | Append-only cost log. The single source of truth for spend |
| **Reservation** | A hold placed on budget *before* dispatch, settled to actual cost after |

---

## 2. High-level flow

```
User prompt + budget (USD or tokens)
        │
        ▼  escrow reserved for output+final-verify (§8)
   ┌─────────┐   compacted planner state   ┌──────────┐
   │ Planner │◄────────────────────────────│ Verifier │
   └────┬────┘   (never raw content)       └────▲─────┘
        │ dispatches leads                      │ checks claims + edges
        │  ▲ reserve budget before dispatch     │ may spend a budgeted
        ▼  │ settle after                       │ re-fetch for grounding
   ┌───────┴───────┐  fresh context each run    │
   │ Executor pool │────────────────────────────┘
   │  (N workers)  │  leased leads, crash-recoverable
   └──────┬────────┘
          │ each lead routed to an Actor
          ▼
   ┌───────────────┬─────────────────────┬──────────────────────┐
   │   WebActor    │   AcademicActor     │  LocalComputeActor   │
   │search→fetch→  │ search/resolve →    │ query→aggregation    │
   │extract→chunk→ │ chunk→summarize     │ gate→analyze→summ.   │
   │  summarize    │                     │                      │
   └───────┬───────┴──────────┬──────────┴───────────┬──────────┘
           │                  │                      │
           ▼                  ▼                      ▼
              Claim store — claims + typed edges, append-only
                              │
                              ▼
             Output generator (report / dataset / chain step / ask)
```

**Context discipline.** Planner and Verifier never see raw page content, raw paper text, or
raw data rows. They see a *compacted planner state* and the claim graph. Raw content lives
inside a single executor run and dies with it.

Two corrections to how rev 1 stated this:

1. Discipline alone does **not** make cost linear. Passing the full summary set to every
   replan makes planner input grow with lead count. §9 fixes this with a bounded rolling
   digest and batch replanning.
2. A Verifier that never sees source text can only check consistency, not grounding —
   which is the failure mode that actually matters. §11 fixes this with evidence quote
   spans captured at extraction time, plus a budgeted, verifier-initiated re-fetch. The
   context discipline is preserved: the quote is a bounded span, not the page.

---

## 3. Trust and data boundaries

This section is load-bearing. Several claims elsewhere in the doc are only true because of
what is specified here.

### 3.1 Threat model, stated plainly

| Boundary | Untrusted input | What it can reach if unguarded |
|---|---|---|
| Fetched web content | Attacker-controlled text | Summarizer prompt → claims → planner → next lead |
| Search results / LLM output | URLs | Daemon's network position: loopback, RFC1918, cloud metadata |
| LLM-generated SQL | Model output | The user's real database, with write access |
| LLM-generated analysis code | Model output | The user's filesystem and network |
| Daemon socket | Any local process | Every registered connector, with its credentials |
| Result sets | The user's private rows | A hosted LLM API's logs |

### 3.2 Content is data, never instruction

All fetched content, all extracted text, all summaries, and all claim text are **untrusted
data** in every downstream prompt. Concretely:

- Untrusted text is passed in a structurally delimited block with an explicit
  provenance label, never string-concatenated into an instruction.
- The Planner is instructed that summaries describe *what a source said*, not *what to do*.
- **Cross-actor constraint:** a `local_compute` lead may not be free-text authored from
  web- or academic-derived content. Local leads are constructed from a fixed set of
  hypothesis templates parameterized over the *known connector schema*. Web content can
  suggest *which* template and *which* column; it cannot author the query. This closes the
  path "attacker web page → planner → SQL against your database."

Prompt injection is not fully solvable. The goal is to bound the blast radius: injected
text can produce a bad claim (which the Verifier and the claim graph exist to catch), but
cannot produce a tool call the architecture doesn't already permit.

### 3.3 Egress control on the fetcher

`Fetcher` is the daemon's network position, and it fetches URLs chosen by a search engine
and an LLM.

- Scheme allowlist: `http`, `https` only.
- Port allowlist: 80, 443 (configurable).
- Resolve the hostname, then check the **resolved IPs** against a deny list: loopback,
  link-local (`169.254.0.0/16`, incl. cloud metadata), RFC1918, CGNAT, ULA, `::1`, and
  any user-configured internal CIDRs. Dial the checked IP directly to close the DNS
  rebinding window between check and connect.
- **Re-run the full check on every redirect hop.** A public URL that 302s to
  `http://169.254.169.254/` is the standard bypass.
- Response size cap and read timeout on every fetch.

### 3.4 Politeness and legal posture

Rev 1 dropped the site-specific scrapers for legal reasons but kept a headless-browser
fetcher that can reach the same sites. The posture only actually changes with mechanism:

- `robots.txt` respected by default (`--ignore-robots` exists, is off, and logs loudly).
- Per-domain denylist shipped with the binary, covering the sites whose ToS make automated
  access clearly disallowed.
- Identifying `User-Agent` with a project URL and contact.
- Per-domain rate limiting (§10.3) is a politeness control as much as an operational one.

### 3.5 Daemon authentication

The named-connector design (§5) keeps credentials out of the MCP tool call. That moves the
trust boundary to the daemon socket — so the socket has to actually be a boundary.

- **Default transport: unix domain socket**, mode `0600`, owned by the invoking user.
  Peer credentials checked via `SO_PEERCRED` where available.
- **HTTP/SSE mode (remote/team use)** requires: bound to an explicit interface, a bearer
  token generated at `serve` time and never defaulted, `Origin` header validation
  (a browser can reach `127.0.0.1`; DNS rebinding makes "localhost only" insufficient),
  and TLS when not on loopback.
- No secrets in `.mcp.json` (§5.2).

### 3.6 Sandbox

`CodeRunner` executes LLM-authored code against real local data. **Default: OCI container,
no network namespace, read-only rootfs, tmpfs scratch, dropped capabilities, seccomp
default profile, non-root uid, and hard CPU/memory/wallclock/pid limits.** gVisor is the
upgrade path when the threat model warrants a syscall-level boundary; Firecracker if a
full VM is required. The technology is decided *before* `CodeRunner` is written, not after.

Critically: **the sandbox is not the control for the SQL path.** See §12.2.

---

## 4. Actor design

All three share an orchestration shape — take a lead, run it, return summary + claims +
cost — but they are different kinds of evidence.

| | WebActor | AcademicActor | LocalComputeActor |
|---|---|---|---|
| Lead is | a search query | a research question or DOI | a templated hypothesis over a known schema |
| Executes by | search → fetch → extract → chunk → summarize | provider query / DOI resolve → chunk → summarize | read-only query → aggregation gate → analyze → summarize |
| Claim source | URL | DOI / paper ID | connector name + query hash |
| Egress | guarded HTTP (§3.3); headless only per §17.1 | plain HTTP, official APIs | none — no network in sandbox |
| Verifier checks | credibility, contradiction, grounding | venue/citation signal, retraction status, grounding | statistical validity: n, effect size, significance, holdout stability |
| Privacy | public web | public metadata/abstracts | rows never reach the LLM — enforced by §12.1 |

```go
type Actor interface {
    Run(ctx context.Context, lead Lead) (ActorResult, error)
}

type ActorResult struct {
    Summary string
    Claims  []Claim
    Cost    Cost
}
```

### 4.1 Chunking is the actor's job

A 300-page PDF or a large HTML document does not fit a context window, and rev 1 had no
answer for it. Each actor runs a bounded map-reduce inside its own run:

- Extracted text is split into chunks with a token budget per chunk.
- Each chunk is summarized and mined for claims independently (cheap model tier).
- Chunk summaries are reduced to one document summary (stronger tier).
- The whole map-reduce runs against a **sub-budget** derived from the lead's reservation
  (§8.2). If the document is too large for the sub-budget, the actor summarizes the
  highest-scoring chunks and records `Truncated: true` on the result rather than silently
  dropping content or blowing the reservation.

Claims always carry the quote span from the *chunk* they came from (§7), so grounding
checks work even for a document that was never held in one context.

---

## 5. MCP integration

Distributed as a **local daemon plus a disposable stdio shim**. Not hosted by the project.
Research sessions run past an hour; a bare stdio subprocess dies when the coding agent's
session closes.

- `mole serve` — persistent local daemon, holds session/claim/budget state.
- `mole-mcp` — thin stdio MCP shim, talks to the daemon over the unix socket.
  Disposable; the daemon and its sessions outlive it.

### 5.1 Tool surface

```
research.report(prompt, budget)                  → session_id
research.dataset(schema, prompt, budget)         → session_id
research.analyze_local(connector, prompt, budget)→ session_id
research.ask(session_id, question)               → answer + claims   # §13
research.status(session_id)                      → {status, spent, remaining, findings_so_far}
research.result(session_id)                      → {report_md | dataset_json, claims[], edges[]}
research.cancel(session_id)                      → ok                # NEW — rev 1 had no stop
research.sessions.list()                         → [{id, status, spent, prompt}]  # NEW
research.connectors.list()                       → [{name, kind, schema}]
```

`research.cancel` and `research.sessions.list` are not conveniences. Without them a caller
that kicks off a runaway session has no way to stop it and no way to find it again after
its own context is compacted.

`connector` is a **name**, registered ahead of time on the daemon host
(`mole connect add mydb --dsn ...`). Credentials never traverse the MCP call and
are never visible to the calling agent. This holds only in combination with §3.5.

### 5.2 Registration — no secrets in the repo

Rev 1 showed API keys inline in a `.mcp.json` described as checked into a repo. That is
committed credentials. Keys live daemon-side:

```bash
mole config set anthropic-key    # prompts, stores in OS keyring or 0600 file
mole config set search-key
```

```json
// .mcp.json — safe to commit; contains no secrets
{
  "mcpServers": {
    "mole": {
      "command": "mole-mcp",
      "args": ["--socket", "${XDG_RUNTIME_DIR}/mole.sock"]
    }
  }
}
```

Env-var indirection (`"ANTHROPIC_API_KEY": "${ANTHROPIC_API_KEY}"`) is supported for users
who prefer it, but the daemon-side store is the default because it also covers the HTTP
and cron paths where no MCP env exists.

### 5.3 What agent callers get back

Return the claim list (`text, source, quote, published_at, confidence, edges`) alongside
any rendered prose. A coding agent asking "what's the current best practice for X" wants
sourced, dated claims it can weigh, not a paragraph. Async by design: kick off → poll
`research.status` → `research.result`, same shape as a long CI job.

### 5.4 Remote option

The same daemon speaks HTTP/SSE per the MCP remote-server spec, with a Docker guide, for
sessions that must survive a closing laptop or for a shared team instance. Auth
requirements in §3.5 are mandatory in this mode, not optional. Still self-hosted — the
project runs no shared infrastructure.

---

## 6. Repo structure

```
mole/
├── cmd/
│   ├── server/main.go          # HTTP/SSE entrypoint (remote mode)
│   ├── mcp/main.go             # stdio MCP shim → unix socket
│   └── agent/main.go           # `mole serve` + CLI (connect, config, trace)
├── internal/
│   ├── orchestrator/
│   │   ├── planner.go          # rolling digest, batch replan (§9)
│   │   ├── executor.go         # lead leases, worker pool, reserve→run→settle
│   │   ├── verifier.go         # edge inference, grounding checks, lineage cycle guard
│   │   └── recovery.go         # orphaned-lease sweep on boot (§9.4)
│   ├── budget/
│   │   ├── ledger.go           # append-only cost log; Spent is a materialized sum
│   │   ├── reservation.go      # Reserve / Settle / Release (§8.2)
│   │   └── escrow.go           # output + final-verify reserve (§8.3)
│   ├── actors/
│   │   ├── actor.go            # Actor interface
│   │   ├── chunk.go            # shared map-reduce summarization under a sub-budget
│   │   ├── web.go
│   │   ├── academic.go
│   │   └── local.go
│   ├── tools/
│   │   ├── search/             # Provider iface + Brave/Tavily/SerpAPI
│   │   ├── fetch/              # Fetcher iface + guarded HTTP (headless gated on §17.1)
│   │   │   ├── guard.go        # SSRF: scheme/port/IP checks, per-hop redirect (§3.3)
│   │   │   └── robots.go       # robots.txt cache, domain denylist (§3.4)
│   │   ├── extract/            # HTML→text (readability), PDF
│   │   ├── academic/           # arXiv/PubMed/SemanticScholar/OpenAlex/Unpaywall
│   │   └── limiter/            # per-provider + per-domain token buckets (§10.3)
│   ├── compute/
│   │   ├── connector/          # postgres.go, duckdb.go, parquet.go, csv.go
│   │   ├── registry.go         # named connectors; credentials never leave this layer
│   │   ├── sqlguard/           # SELECT-only parse gate, read-only txn, limits (§12.2)
│   │   ├── aggregate/          # aggregation gate — the privacy boundary (§12.1)
│   │   └── sandbox/            # CodeRunner; container/gVisor (§3.6)
│   ├── store/
│   │   ├── store.go            # Store interface — one backend today (§7.1)
│   │   ├── sqlite/             # WAL, single-writer pool, busy_timeout
│   │   └── cache.go            # result-returning dedup cache (§9.3)
│   ├── llm/
│   │   ├── provider.go         # Provider interface — Anthropic + OpenAI-compatible
│   │   └── anthropic/, openai/
│   ├── record/                 # cassette record/replay for search/fetch/LLM (§14.1)
│   ├── mcpserver/handlers.go
│   ├── eval/                   # harness, question sets, scorers (§14)
│   └── output/
│       ├── report.go
│       ├── dataset.go
│       ├── chain.go
│       └── ask.go
├── api/openapi.yaml
└── migrations/
```

---

## 7. Data model

Stringly-typed status/mode fields from rev 1 are now enums with DB check constraints.
A `Store` interface hides the backend. There is exactly one, and §7.1 explains why.

### 7.1 SQLite is the only backend

Rev 2 said "SQLite **and** Postgres behind a `Store` interface". Postgres is dropped.
Nothing in this design's workload needs it, and carrying a second backend costs a
`cgo`-free build, a dependency, a second dialect of every query, and a second set of
migrations to keep honest.

**The workload is small and the shape favours SQLite.** One tool call is one row.
A generous session is ~100 calls over tens of minutes. Fifty concurrent sessions on a
shared team instance is on the order of **3 writes per second** — three orders of
magnitude inside what SQLite does on a laptop SSD. Reads are trivial. The database
grows by a few MB per session, against a 281 TB file limit.

**Write serialization is a feature here, not a limit.** The single most important
property of §8's ledger is that the availability check and the hold happen atomically.
SQLite's one-writer model gives that for free; Postgres would have us reintroduce it
with explicit row locks.

**Multi-process access is the real wrinkle, and WAL covers it.** The daemon holds the
writer while `mole trace` runs in another terminal. Under WAL, readers see the last
committed snapshot and never block on an open write transaction — so the CLI's read
commands must genuinely not write. That is a live constraint on the implementation,
not a theoretical one: an early version migrated the schema on every open, which made
every `trace` contend for the writer. Read commands now open read-only and report
`run: mole migrate` if the schema is behind.

**What is genuinely lost.** Two things, both currently hypothetical:

| Capability | Cost of not having it |
|---|---|
| Several daemons sharing one state store | §5 and §18.7 describe *one* daemon serving many users, not a horizontally scaled fleet. Not needed. |
| Network-attached state for a container deployment | A mounted volume solves it. Only bites if someone insists on an ephemeral filesystem. |

**What would reverse this**, stated concretely so it is a decision and not a drift:

1. A deployment genuinely needs **more than one daemon process** against shared state.
2. An organization already runs Postgres and will not accept a second persistence
   story — an operational argument, not a technical one, and a fair one.
3. Cross-session semantic search over the claim graph outgrows brute force. Within a
   session (§11.2) it never will: a few hundred claims is a linear scan in
   microseconds. `ask` over *years* of sessions is where a real vector index earns
   its place, and `pgvector` would be the obvious answer. That is well past M10.

The `Store` / `Tx` / `Queries` split stays, because it already exists and it is the
seam any of the above would use. What is not built is a second implementation behind it.

```go
type BudgetUnit string
const (
    BudgetUSD    BudgetUnit = "usd"
    BudgetTokens BudgetUnit = "tokens"
)

type SessionStatus string
const (
    StatusRunning    SessionStatus = "running"
    StatusDone       SessionStatus = "done"
    StatusExhausted  SessionStatus = "budget_exhausted"
    StatusCancelled  SessionStatus = "cancelled"
    StatusFailed     SessionStatus = "failed"
)

type Session struct {
    ID         string
    Prompt     string
    Mode       Mode          // report | dataset | chain | ask
    ActorTypes []ActorType   // web | academic | local_compute
    BudgetUnit BudgetUnit
    Budget     int64         // micro-dollars or tokens, per BudgetUnit (§8.2)
    // Spent is NOT authoritative state. It is a materialized sum of the ledger,
    // written in the same transaction as each cost row (§8.1). A daemon crash
    // can never desynchronize it, because it can always be recomputed.
    Spent          int64
    EscrowReserved int64     // held back for output + final verify (§8.3)
    MaxToolCalls   int       // hard ceiling independent of budget unit (§8.5)
    ToolCallCount  int
    Status         SessionStatus
    CreatedAt      time.Time
}

type Lead struct {
    ID        string
    SessionID string
    ActorType ActorType
    Query     string
    ParentID  *string
    Depth     int          // planner-tree depth, capped
    Priority  int
    Status    LeadStatus   // queued | leased | running | done | failed | skipped_cached

    // Crash recovery (§9.4): a worker takes a lease, heartbeats it, and a boot-time
    // sweep requeues leads whose lease expired. Without this a daemon restart
    // strands leads in `running` forever.
    LeaseOwner   *string
    LeaseExpires *time.Time
}

type Claim struct {
    ID        string
    SessionID string
    LeadID    string
    Text      string

    // Provenance
    Source     string   // URL, DOI, or "connector:<name>#<query_hash>"
    ToolCallID string
    Quote      string   // NEW: verbatim evidence span from the chunk the claim came from.
                        // Bounded (~500 chars). This is what makes grounding checks
                        // possible without the Verifier ever reading the full source.
    QuoteOffset int     // byte offset into the extracted text, for re-verification

    // Temporal — required to answer "what is *current* best practice" (§5.3)
    PublishedAt *time.Time // source publication date, if extractable
    RetrievedAt time.Time  // when we fetched it

    // Verification lineage. Replaces rev 1's RecheckCount, which did not bind:
    // a follow-up lead produced a *new* claim with count 0, so the cap never fired.
    RootClaimID string // the original claim this verification chain descends from
    VerifyDepth int    // depth in that chain; capped at maxVerifyDepth (§11.4)

    // Derived, not model-reported (§11.3). Recomputed from graph structure.
    Confidence float64
    Grounded   *bool // nil = unchecked, true/false = verifier grounding result
}

// NEW. Rev 1 called this a "claim graph" but modeled a flat table, so contradictions
// had nowhere to live and the same fact from five sources was five unrelated rows.
type ClaimEdge struct {
    ID        string
    SessionID string
    FromID    string
    ToID      string
    Kind      EdgeKind // supports | contradicts | duplicate_of | supersedes | refines
    Weight    float64
    CreatedBy string   // verifier | clustering | planner
    Rationale string   // short, for the trace viewer
}

type ToolCall struct {
    ID        string
    SessionID string
    LeadID    *string
    Role      Role     // planner | executor | verifier | output
    Type      CallType // search | fetch | llm | local_query | academic_query | sandbox
    Input     string   // redacted/hashed for local queries
    Cost      Cost
    Err       string
    Timestamp time.Time
}
```

Citations still fall out for free — the report generator joins
`Claim → ToolCall → Source`, and now also renders the `Quote` as the supporting excerpt.

---

## 8. Budget: ledger, reservations, escrow

Rev 1 charged *after* the actor ran and mutated `Session.Spent` in memory. Both are wrong
once there is a worker pool or a crash.

### 8.1 The ledger is the source of truth

Every tool call writes a cost row **immediately, never batched, always in both units**,
tagged with the `Role` that spent it. `Session.Spent` is a materialized sum updated in the
same transaction as the cost row. Switching a session's accounting unit is a display and
gating choice, not a data migration. "How much went to verification vs. execution?" is a
`GROUP BY role`.

### 8.2 Reserve → run → settle

Money is an integer, never a float. Budget adherence is a tested metric (§14.3: max
overshoot must be ~0), and `float64` dollars accumulate error across the thousands of
reserve/settle operations a long session performs — the one place where a rounding
artefact silently becomes a budget breach. Costs are micro-dollars; *rates* are
nano-dollars per token, so `$5/MTok` is exactly `5000` and a cache read is exactly
`500`, both integers.

```go
const MicrosPerUSD = 1_000_000

type Cost struct {
    USDMicros        int64  // integer money — see above
    InputTokens      int64
    OutputTokens     int64
    CacheReadTokens  int64  // ~10% of input price — this design caches heavily,
    CacheWriteTokens int64  // so a flat `Tokens int64` misprices sessions badly
}

type Reservation struct {
    ID        string
    SessionID string
    Amount    int64 // in the session's BudgetUnit
    ExpiresAt time.Time
}

// Reserve atomically checks (Budget - Spent - Held - Escrow) >= amount and, if so,
// records a hold. Returns ErrInsufficientBudget otherwise. This is the hard ceiling.
func (l *Ledger) Reserve(ctx, sessionID string, amount int64) (*Reservation, error)

// Settle writes the actual cost rows and releases the hold, in one transaction.
func (l *Ledger) Settle(ctx, r *Reservation, actual []ToolCall) error

// Release drops the hold without charging (used when a lead is cancelled pre-dispatch).
func (l *Ledger) Release(ctx, r *Reservation) error
```

Why this and not `Charge` after the fact: with N workers and post-hoc charging, the
session can overshoot the ceiling by up to N times the cost of a lead. With reservations,
the overshoot is bounded by *estimate error on a single lead*, and can be capped by
refusing to settle above `k × reserved` (the actor is aborted at the sub-budget line
instead — §4.1).

Rev 1's `Remaining() > floorCost(leads.Peek())` followed by `leads.Pop()` was also a
TOCTOU bug — the peeked lead is not necessarily the popped one. `Reserve` is taken
against the lead actually dequeued.

### 8.3 Escrow for output

At session start, hold back `escrowFraction` (default 15%, floor of one report-generation
estimate) for `output.Generate` and the final verification pass. Rev 1's loop could exit
`budget_exhausted` and *then* call `output.Generate` — one LLM pass over the entire claim
graph, plausibly the most expensive single call in the session, with nothing left to pay
for it. Research spend draws only against `Budget - Escrow`.

### 8.4 Cost estimation

`floorCost` in rev 1 was undefined and, strictly, unknowable before running. Replace with a
calibrated estimator: per `(actor_type, lead_depth)`, use a rolling p75 of settled costs
from the ledger, seeded with conservative constants on a cold start. The estimator is
observable and improves with use; the reservation mechanism means a bad estimate costs
accuracy, not a budget breach.

### 8.5 Token mode's free-fetch hole

Rev 1: in token mode, "search/fetch/academic calls don't consume budget." That makes an
unbounded fetch loop free. Every session therefore carries **unit-independent ceilings**:
`MaxToolCalls`, `MaxWallClock`, and `MaxLeads`. Hitting any of them ends the session as
cleanly as budget exhaustion, with the escrow intact so a report still gets written.

Token mode remains the natural default for MCP callers (§5) — a coding agent already
reasons in tokens.

---

## 9. Planner–Executor–Verifier loop

### 9.1 Bounded planner state

Rev 1 called `planner.Replan(sessionID, store.Summaries(sessionID))` after *every* lead,
passing *all* summaries. Summaries accumulate, so planner input grows with lead count and
total planner cost is quadratic in leads — exactly backwards from the linear-cost claim,
and worst precisely when research goes deep.

Replacement:

- The planner keeps a **rolling digest**: a fixed-token-budget structured state
  (open questions, answered questions, dead ends, coverage gaps, per-question claim
  counts). It is updated incrementally from new claims and re-compacted when it exceeds
  its budget. Planner input size is O(1) in lead count.
- **Batch replan**: replan every `k` completed leads or when a branch of the lead tree
  closes, not after every single lead. Cuts planner calls by roughly `k`.
- Lead-tree depth is capped (`Lead.Depth`), independent of budget.

### 9.2 The loop

```go
func RunSession(ctx context.Context, s *Session) error {
    ledger.ReserveEscrow(s)                       // §8.3 — before anything else
    queue.PushAll(planner.InitialLeads(s.Prompt, s.ActorTypes))

    var wg sync.WaitGroup
    for i := 0; i < s.Workers; i++ {
        wg.Add(1)
        go func() {
            defer wg.Done()
            for {
                if ctx.Err() != nil || s.HitCeiling() { return }   // §8.5

                lead, lease, ok := queue.LeaseNext(ctx, s.ID)      // atomic dequeue+lease
                if !ok { return }                                  // queue drained

                // Cache returns the prior result — it does NOT skip the lead (§9.3)
                if prior, hit := cache.Lookup(lead); hit {
                    store.LinkClaims(lead, prior.Claims)           // zero cost
                    queue.Complete(lease)
                    continue
                }

                est := estimator.For(lead)                          // §8.4
                res, err := ledger.Reserve(ctx, s.ID, est)
                if errors.Is(err, ErrInsufficientBudget) {
                    queue.Requeue(lease)                            // may fit later? no —
                    return                                          // budget only shrinks
                }

                result, runErr := actors.For(lead.ActorType).
                    Run(withSubBudget(ctx, res), lead)               // §4.1

                if runErr != nil {
                    // Explicit policy (§9.5). Partial cost is ALWAYS settled — the
                    // money was spent whether or not the call succeeded.
                    ledger.Settle(ctx, res, partialCosts(runErr))
                    policy.Handle(s, lead, lease, runErr)
                    continue
                }

                ledger.Settle(ctx, res, result.Costs)
                store.SaveClaims(result.Claims)
                cache.Record(lead, result)
                queue.Complete(lease)

                // Verifier runs against the WHOLE store, not just this batch (§11.1)
                for _, fu := range verifier.Assess(ctx, s.ID, result.Claims) {
                    if fu.VerifyDepth < maxVerifyDepth {             // §11.4
                        queue.PushPriority(fu)
                    }
                }

                if planner.ShouldReplan(s.ID) {                      // §9.1 — batched
                    queue.PushAll(planner.Replan(s.ID, planner.Digest(s.ID)))
                }
            }
        }()
    }
    wg.Wait()

    verifier.FinalPass(ctx, s.ID)     // paid from escrow
    return output.Generate(ctx, s)    // paid from escrow
}
```

### 9.3 The cache returns results; it never skips

Rev 1 did `if cache.SeenRecently(lead) { continue }`. Two problems: the planner asked for
something and got nothing back, so the next replan re-spawns an equivalent lead —
livelock; and lead-level keying misses the common case where two *different* queries
surface the *same* URL.

Fixed:

- Cache is keyed at the **artifact** level — normalized URL (scheme/host lowercased,
  tracking params stripped, fragment dropped), DOI, and query hash — not at the lead level.
- A hit returns the stored `ActorResult`. Claims are re-linked to the new lead with cost
  zero, so the planner sees the finding it asked for.
- Nested: the WebActor checks the URL cache after search and before fetch, so two distinct
  queries converging on one page pay for one fetch.

### 9.4 Crash recovery

Leads are leased, not just marked `running`. Workers heartbeat their lease. On daemon boot,
`recovery.Sweep` requeues every lead with an expired lease and releases every stale
reservation. Rev 1 would strand leads in `running` across a restart with no way out.

### 9.5 Error policy — explicit

`actor.Run` fails routinely. Classify:

| Class | Examples | Action |
|---|---|---|
| Transient | 429, 5xx, timeout, connection reset | Retry with jittered exponential backoff, capped at 3; then Degraded |
| Degraded | Dead link, extraction failure, empty result | Mark lead `failed`, record a `no_evidence` note for the planner digest, continue |
| Fatal | Auth failure, connector unreachable, sandbox unavailable, budget ceiling | Abort session, status `failed`, generate a partial report from escrow |

Partial cost is settled in every case.

---

## 10. Tool layer

```go
type SearchProvider interface {
    Search(ctx context.Context, query string) ([]SearchResult, Cost, error)
}

type Fetcher interface {
    Fetch(ctx context.Context, url string) (RawContent, Cost, error) // guarded per §3.3
}

type Extractor interface {
    Extract(raw RawContent) (CleanText, Metadata, error) // Metadata carries PublishedAt
}

type AcademicProvider interface {
    Search(ctx context.Context, query string) ([]PaperResult, Cost, error)
    Resolve(ctx context.Context, doi string) (PaperResult, error)
}

type DataConnector interface {
    Query(ctx context.Context, q string, limits QueryLimits) (ResultSet, error) // §12.2
    Schema(ctx context.Context) (Schema, error)
}

type CodeRunner interface {
    Run(ctx context.Context, code string, in AggregateEnvelope) (AnalysisOutput, Cost, error)
}
```

### 10.1 LLM provider is an interface

Rev 1 specified "a thin wrapper over the Anthropic API" while justifying token-mode
budgeting by pointing at self-hosted models — a contradiction. `llm.Provider` is an
interface with an Anthropic implementation and an OpenAI-compatible one (which covers
vLLM, Ollama, llama.cpp, LiteLLM, and most hosted alternatives). Pricing tables are config,
not code, so the cost model works for a self-hosted `$0` model too.

Model tiering is explicit: cheap tier for chunk mining and extraction, strong tier for
planning, verification, and report synthesis.

### 10.2 Defaults

- Search: Brave or Tavily.
- Fetch: `net/http` + `go-readability`. This generic path is deliberately the only
  web-fetch mechanism — no first-party site adapters. Whether a headless browser is ever
  added is an open question with a written decision gate (§17.1); it is not assumed.
- PDF: a real extraction library, plus an OCR fallback flag. Non-trivial; budget for it.
- Academic: arXiv + PubMed/PMC as keyless defaults; Semantic Scholar and OpenAlex for
  citation-graph queries; **Unpaywall for DOI → legal open-access copy**, the single
  highest-leverage call in the package. No ScienceDirect/Elsevier — full text needs an
  institutional entitlement the API doesn't grant.
- Local: DuckDB as the default connector (Parquet/CSV/Postgres-scanner natively, embeds in
  the binary); sandboxed Python/pandas `CodeRunner` for statistics DuckDB SQL can't express.

### 10.3 Rate limiting is mandatory, not optional

A worker pool will get the user banned in week one otherwise. Every provider goes through
`tools/limiter` with a per-provider token bucket and the provider's required identification:

| Provider | Constraint |
|---|---|
| NCBI E-utilities | 3 req/s without key, 10 with; `tool` + `email` params required |
| Unpaywall | `email` query param required |
| arXiv | Documented request delay; single connection recommended |
| Semantic Scholar / OpenAlex | Keyless tiers are rate-limited; polite pool wants a contact |
| Web domains | Per-domain bucket + `Crawl-delay` from robots.txt |

Contact email is a required config value before any academic provider is enabled. Ship it
as a startup check, not a README line.

### 10.4 Fetch failure taxonomy

Every fetch that does not yield usable text is classified and logged with its cause. This
is cheap to add in M1 and is the only thing that turns "do we need a headless browser?"
from an argument into a number (§17).

```go
type FetchOutcome string
const (
    FetchOK          FetchOutcome = "ok"
    FetchJSRequired  FetchOutcome = "js_required"   // extractor yielded < N chars, but the
                                                    // document is script-heavy and has an
                                                    // app-root element — the SPA signature
    FetchStructured  FetchOutcome = "structured_only" // readability failed BUT __NEXT_DATA__ /
                                                    // JSON-LD / OpenGraph carried the content
    FetchConsentWall FetchOutcome = "consent_wall"
    FetchBotBlock    FetchOutcome = "bot_block"     // 403/429 with a challenge signature
    FetchPaywall     FetchOutcome = "paywall"
    FetchNotFound    FetchOutcome = "not_found"     // 404/410
    FetchTimeout     FetchOutcome = "timeout"
    FetchRobotsDeny  FetchOutcome = "robots_denied"
    FetchGuardDeny   FetchOutcome = "guard_denied"  // SSRF/denylist — a security event, not a
                                                    // capability gap; must not be counted as
                                                    // evidence for building headless
    FetchExtractFail FetchOutcome = "extract_failed"
)
```

Recorded per fetch with the domain, so the output is a per-cause rate *and* a ranked
domain list per cause. `FetchGuardDeny` and `FetchRobotsDeny` are deliberately separated —
those are the system working, and folding them into a generic failure rate would inflate
the apparent case for a browser.

**Try the cheap paths before the expensive one.** In failure-rate terms these are three
distinct mitigations, and the taxonomy tells you which one to reach for:

1. **Use content the search provider already returned.** Tavily returns extracted page
   text and Brave has a summarizer endpoint; for many results no fetch is needed at all.
   Check this first — it reduces cost and latency independent of the browser question.
2. **Structured-data extraction.** `__NEXT_DATA__`, JSON-LD, OpenGraph, and embedded
   hydration payloads carry the full content on a large share of sites that *look* like
   they need JS. Roughly a day of work; `structured_only` measures exactly how much it buys.
3. **A hosted render API.** Buys the capability as a metered per-call cost that fits the
   existing ledger, instead of a permanent binary-size and memory cost.

---

## 11. Claim graph and verification

### 11.1 The Verifier sees the store, not the batch

Rev 1 called `verifier.NeedsRecheck(result.Claims)` on freshly produced claims only —
which cannot detect a contradiction with a claim produced by a different lead. The
Verifier operates over the session's whole claim set (via the graph, not raw text), and
runs incrementally: each new claim is clustered against existing claims and edges are
inferred within the cluster.

### 11.2 Clustering and edges

1. **Candidate retrieval** — embed each claim, retrieve near-duplicates and topically
   adjacent claims from the session.
2. **Edge inference** — a cheap LLM pass over each candidate pair emits
   `supports | contradicts | duplicate_of | supersedes | refines | unrelated`.
   `supersedes` uses `PublishedAt`, which is why §7 requires it: a 2019 claim contradicted
   by a 2025 claim on the same question is usually staleness, not disagreement.
3. **Duplicate collapse** — `duplicate_of` clusters render as one claim with N sources,
   which is the corroboration signal §11.3 needs.

### 11.3 Confidence is derived, not asked for

Rev 1's `Confidence float64` was set by the Verifier with no defined semantics. LLM
self-reported confidence numbers are not calibrated and mostly encode fluency. Compute it
instead from graph structure:

```
confidence = f(
    independent corroborating sources (distinct domains/publishers, not distinct URLs),
    source class weight (peer-reviewed / primary / secondary / anonymous),
    presence and weight of `contradicts` edges,
    grounding result (§11.5),
    recency vs. the question's volatility
)
```

Deterministic, explainable in the trace viewer, and testable against §14's eval set.

### 11.4 Cycle guard that actually binds

`RecheckCount` on a row could not work: `FollowUpLead` produced a *new* claim starting at
zero. Lineage instead — every claim carries `RootClaimID` and `VerifyDepth`, both inherited
and incremented by follow-up leads. The cap is on chain depth
(`maxVerifyDepth`, default 3), plus a per-root budget cap so one stubborn claim cannot
consume the session.

### 11.5 Grounding: the check that actually matters

Consistency checking cannot catch a hallucinated-but-self-consistent claim, which is the
dominant failure mode. Two mechanisms, both preserving the "no raw content upstream" rule:

1. **Quote span at extraction time** (§7). The extracting actor must emit a verbatim span
   from the source supporting each claim. A claim whose quote does not appear verbatim in
   the extracted text is rejected at the actor boundary — cheap, deterministic, and it
   catches fabricated citations before they enter the store.
2. **Budgeted re-fetch.** For high-stakes claims — load-bearing for the conclusion,
   contradicted, or low-corroboration — the Verifier may spend from the escrow to re-fetch
   one source and check the quote in context. Bounded per session. This is the only path
   by which source text re-enters the pipeline, and it enters a *verifier* context that is
   discarded immediately, never the planner digest.

---

## 12. LocalComputeActor: the privacy boundary

Rev 1 stated "data never leaves the local machine; only aggregates/summaries reach the
LLM." That was a comment, not a mechanism: `CodeRunner.Run(ctx, code, data ResultSet)`
took rows as input, and an LLM — a hosted API — summarized the result.

### 12.1 The aggregation gate

A gate sits between the connector and anything that can reach an LLM. Nothing crosses it
except an `AggregateEnvelope`:

```go
type AggregateEnvelope struct {
    Query        string          // the SQL that was run
    QueryHash    string
    RowCount     int64           // count only — never the rows
    Columns      []ColumnStats   // type, null rate, distinct count, min/max, quantiles, moments
    TopK         []Bucket        // only for buckets with count >= kAnonymityFloor
    TestResults  []StatTest      // statistic, p-value, effect size, CI, n
    Truncated    bool
}
```

Enforcement:

- A result set that is not an aggregate (no `GROUP BY`/aggregate function, or row count
  above `maxRawRows`) **cannot** be passed to an LLM. The gate rejects it.
- `TopK` buckets below the k-anonymity floor (default 5) are collapsed into `other`.
  Otherwise `GROUP BY email` trivially exfiltrates rows one aggregate at a time.
- Free-text columns are excluded from `TopK` by default — a "top values" list over a
  notes field is just the rows.
- `CodeRunner` receives an `AggregateEnvelope`, not a `ResultSet`. Where analysis genuinely
  needs row-level data (regression, seasonality decomposition), the computation happens
  **inside** the sandbox and only its output envelope crosses the gate.
- Every crossing is logged, so a user can audit exactly what left their machine.

The claim `"weekly seasonality detected in requests_per_hour, p<0.01, stable across 3
holdout windows"` is now a property the architecture enforces, not one it hopes for.

### 12.2 SQL safety — the sandbox is not the control here

`DataConnector.Query` runs LLM-authored SQL against the user's real DSN. §3.6's sandbox
covers `CodeRunner` and does nothing for this path. A generated `DROP TABLE` would execute.

Defense in depth, all four required:

1. **Least privilege** — connector registration requires a read-only role and warns loudly
   if the DSN's user has write grants.
2. **Parse gate** (`compute/sqlguard`) — parse the statement; permit a single `SELECT`
   or `WITH … SELECT`; reject DDL, DML, multiple statements, `COPY`, and any dialect
   escape hatch to the filesystem or shell.
3. **Read-only transaction** — `SET TRANSACTION READ ONLY` (Postgres) / equivalent per
   engine, with `statement_timeout` and an enforced `LIMIT`/row cap.
4. **Egress-free** — DuckDB runs with its network extensions disabled; the sandbox has no
   network namespace.

### 12.3 Lead construction is constrained

Per §3.2, local leads are instantiated from hypothesis templates over the connector's
known schema. Web-derived content can influence template and column choice; it cannot
author SQL.

---

## 13. Output modes

- **Report** — claim graph → LLM synthesis with `[n]` citations mapped to
  `ToolCall.Source`, plus the supporting `Quote` and `PublishedAt` per citation.
  Contradiction edges are rendered explicitly ("sources disagree: …") rather than silently
  resolved by whichever claim the model liked. Paid from escrow.
- **Dataset** — user-defined or inferred schema → per-lead row extraction → cross-source
  merge/dedup by fuzzy key → CSV/JSON. For bulk structured extraction at scale, point users
  at an official API or a paid extraction service rather than shipping a maintained scraper.
- **Chain** — ordered `Session`s; step N's claim graph seeds step N+1's planner digest.
  Budget split across steps with explicit semantics: unspent budget rolls forward by
  default (`--no-rollover` to disable), and a failed step halts the chain unless marked
  `optional`.
- **Ask** — listed in rev 1's `Session.Mode` but never defined. It is a cheap
  retrieval-only query against a *completed* session's claim graph: retrieve relevant
  claims + edges, answer with citations, spend no research budget beyond one LLM call.
  This is what makes the daemon's persistence worth something — a coding agent whose own
  context was compacted can re-interrogate a finished session instead of redoing it.

---

## 14. Evaluation harness

Rev 1 had none. For a project whose entire differentiator is *process quality* — enforced
budget, real verification, a durable claim graph — all three properties are measurable, and
without measurement the Verifier is untunable.

### 14.1 Record/replay

Every outbound call (search, fetch, LLM, academic) goes through `internal/record`, which
records request/response cassettes on first run and replays thereafter. Consequences:
orchestrator tests are deterministic, run in CI, and cost nothing. Build this before the
first actor, not after — retrofitting means auditing every call site.

### 14.2 Question sets

30–50 questions with known answers, spanning: settled facts, questions with a genuine
source disagreement, questions where the correct answer changed recently (staleness),
questions with a plausible-but-wrong widely-repeated answer, and a local-compute set over
a synthetic dataset with known ground-truth structure.

### 14.3 Metrics

| Metric | Measures |
|---|---|
| Claim precision | Fraction of claims that are true |
| Grounding rate | Fraction whose `Quote` actually supports the claim (§11.5) |
| Citation accuracy | Fraction where the cited source actually contains the quote |
| Contradiction recall | Fraction of planted disagreements the Verifier flags |
| Staleness detection | Fraction of superseded facts correctly ordered by `supersedes` |
| **Cost per correct claim** | The headline number. Optimizes the actual product |
| Budget adherence | Max observed overshoot past ceiling. Must be ~0 |
| Exfil regression | Assert no row-level data crosses the aggregation gate (§12.1) |
| Fetch outcome mix | Rate per `FetchOutcome` (§10.4), plus top domains per cause. Decides §17's headless question |

Every milestone from M3 on reports these numbers. Regressions block merge.

The fetch outcome mix is reported from M1 rather than M3 — it is a capability measurement,
not a quality one, and it is only useful if it accumulates across the whole build.

---

## 15. Build order

Reordered on one principle: **build the ledger and the measurement before the
intelligence.** You cannot enforce a budget you can't reconcile or tune a verifier you
can't score. Durations assume one experienced Go developer and are deliberately less
optimistic than rev 1 — dataset mode in particular was a 2-week milestone for what is
actually a research problem.

**M0 — Foundations (2 weeks).** `Store` interface; SQLite+WAL with a single-writer pool
(the only backend — §7.1). Migrations. Append-only ledger with materialized `Spent`.
Reserve/Settle/Release + escrow. `Cost` with cache-token breakdown, in integer
micro-dollars rather than `float64` (§8.2). `internal/record` cassette layer. Structured
logging, one trace span per lead, `mole trace <session>`.

**M1 — WebActor, single worker, end to end (2 weeks).** search → fetch → extract → chunk →
summarize → claims. SSRF guard, robots.txt, per-domain limiter, identifying UA — in the
fetcher from the first commit, because retrofitting means auditing every call site. Claims
carry `Quote`, `RetrievedAt`, `PublishedAt`. Verbatim-quote rejection at the actor boundary.
**Fetch failure classification (§10.4)** — every non-OK fetch records a typed
`FetchOutcome` plus its domain. Half a day of work here, and it is the input to the M10
headless-browser decision (§17); added later it means re-running the eval corpus to get
the data. Deliverable: cited answers, USD budget, hard ceiling proven by test.

**M2 — Eval harness (1 week).** §14. Everything after this is scored. Includes
`mole stats --fetch` — cross-session `FetchOutcome` aggregation (§18.6), which §17.1's gate
requires and which per-session `trace` does not provide.

**M3 — Planner loop (2 weeks).** Rolling digest, batch replan, artifact-level
result-returning cache, explicit error policy, unit-independent ceilings. Report mode.
First scored run.

**M4 — Claim graph + Verifier (3 weeks).** Edges, clustering, derived confidence, grounding
checks, budgeted re-fetch, lineage cycle guard. The differentiator; expect it to overrun.

**M5 — Executor pool (1 week).** Turn on N workers. M0's reservations and M3's leases make
this configuration, not a rewrite. Add the boot-time recovery sweep.

**M6 — AcademicActor (1.5 weeks).** arXiv + PubMed defaults, Unpaywall resolve. The API
surface is easy; per-provider rate limiting and required contact identification are the
real work.

**M7 — MCP daemon + shim (2 weeks).** Unix socket 0600, daemon-side secrets, cancel and
sessions.list, token-mode budget, async status/result.

**M8 — LocalComputeActor (3 weeks).** Sandbox technology chosen and built *first*. Then
`sqlguard`, then the aggregation gate, then the actor. Statistical-validity verifier checks.
Do not start the actor before the two gates exist.

**M9 — Dataset mode (3 weeks).** Own milestone, own eval set. Schema inference plus
cross-source fuzzy merge is the hardest quality problem in this document.

**M10 — Later.** Chains, `ask` mode polish, remote/Docker deployment, OCR, and the
headless-browser fetch path *only if* the §17.1 gate says so — the M2 data may close that
item permanently.

Roughly 20 weeks to M9 for one developer. M1–M4 (about 8 weeks) is the point at which the
thing is genuinely differentiated and worth showing to people.

---

## 16. Where this differs from a naive "loop until done" agent

- **Budget is enforced, not estimated.** Reserved before dispatch, settled after, escrowed
  for output, with unit-independent ceilings. Max overshoot is a tested metric.
- **Cost is actually bounded as research deepens** — a bounded planner digest and batch
  replanning, rather than re-reading a growing summary set on every step.
- **Claims are grounded, not just consistent.** A verbatim quote span is required at
  extraction and checkable afterwards. Fabricated citations are rejected at the boundary.
- **There is a real graph.** Contradiction, duplication, and supersession are modeled
  edges, so disagreement and staleness are reportable instead of silently resolved.
- **Local data has an enforced boundary.** An aggregation gate with k-anonymity, a
  SELECT-only parse gate, a read-only transaction, and an egress-free sandbox — not a
  convention that rows shouldn't be sent.
- **Quality is measured.** Cost per correct claim, grounding rate, contradiction recall,
  on a fixed question set with replayed cassettes.
- **It outlives the caller.** A daemon plus a persisted claim graph means a finished
  session survives a compacted context or a closed terminal, and `ask` can re-interrogate
  it for the price of one LLM call.

The differentiator is not fetch or compute capability — every target caller already ships
`WebSearch`/`WebFetch` and can run local analysis. It is the process wrapped around them.

---

## 17. Open questions still to resolve

Genuinely open, as distinct from the resolved items above. Each one names the milestone
that resolves it and the evidence that decides it — an open question with no decision
procedure is just a worry.

| Question | Decided at | Decided by |
|---|---|---|
| Claim atomicity | M4 | Written extraction spec + eval cases |
| Independence of corroboration | M4 | Corroboration-inflation rate on the eval set |
| Embedding provider | M4 | Per-claim clustering cost vs. dependency weight |
| Escrow fraction | after M3 | Observed output cost as a share of real sessions |
| Headless browser | after M2, built at M10 if at all | `FetchOutcome` mix (§10.4, §14.3) |
| Session top-up after exhaustion | M3 | Whether partial reports are actually useful in practice |
| Chain input format | M10 | Falls out of the first two real chains anyone writes |
| Cross-session fetch stats | M2 | Required by §17.1 — not optional, just unspecified |

- **Claim atomicity.** What counts as one claim is under-specified and directly determines
  whether clustering and corroboration counting work — too coarse and nothing clusters, too
  fine and every sentence fragments into five rows that all "corroborate" each other.
  Needs a written extraction spec plus eval cases at M4.
- **Independence of corroboration.** Five outlets syndicating one wire story are one
  source, but §11.3 counts them as five. Domain-level dedup is a weak proxy;
  content-similarity dedup across sources is better and costs more. Measure the inflation
  rate on the eval set before choosing.
- **Embedding provider for clustering.** Local keeps clustering free and offline but adds
  a model dependency to a Go binary whose selling point is that it has none; hosted is
  simpler but adds per-claim cost to a hot path.
- **Escrow fraction.** 15% is a guess. Calibrate from settled ledgers after M3 — the data
  is already there, since output cost is tagged `Role: output`.
- **Session top-up after exhaustion.** A session that hits `budget_exhausted` writes a
  partial report from escrow and stops. There is no way to add budget and continue. The
  ledger supports it trivially — a top-up is one more row and a raised ceiling — but the
  session state machine has no `exhausted → running` transition, and resuming means the
  planner digest and lead queue must have survived, which they do. The open part is not
  feasibility, it is whether users want it: if partial reports turn out to be good enough,
  top-up is a confusing second way to spend money. Decide from M3 usage, not now.
- **Chain input format.** §13 specifies chain *semantics* (budget rollover on by default,
  a failed step halts unless marked `optional`) but not how a user expresses one. Presumably
  a YAML file of steps with per-step prompt, mode, actor types, and budget share. Left
  unspecified deliberately — the format should fall out of the first two real chains
  someone writes, not be designed before there is a single one.
- **Cross-session fetch statistics.** §17.1's gate needs `FetchOutcome` aggregated across
  every session ever run; `mole trace` reports one session. That is a different query and
  a different command (`mole stats --fetch`, §18.6). This is not a design question — it is
  a required piece of M2 that the milestone list currently omits. Fold it into M2.

### 17.1 Headless browser — the decision procedure

Deferred to M10, and possibly to never. This is the one open question where the cost of
guessing wrong is structural rather than a tuning constant, so it gets a written gate.

**What deferral costs.** Sites that genuinely require JS execution are unreachable. That
is the entire downside.

**What building it costs, in weight order:**

1. **The distribution story.** "Single Go binary, DuckDB embedded, no separate service" is
   a stated selling point. Chromium is 150–300MB bundled, or an external dependency to
   detect and version-check. Largest single regression to how Mole installs.
2. **The executor pool.** 100–500MB resident per browser instance, multiplied by workers.
   Needs a browser pool with lifecycle management, zombie reaping, and a crash policy.
3. **A second egress boundary.** §3.3's guard works by resolving, checking, and dialling
   the checked IP from Go. A browser issues its own subresource requests — images, XHR,
   fonts, iframes — that never traverse that hook. Enforcing the same policy requires a
   filtering proxy or a network namespace with egress rules. **Headless is therefore not
   a drop-in `Fetcher` implementation; it is a differently-shaped security boundary.**
   This is the cost most likely to be underestimated.
4. **The legal posture from §3.4.** Driving a real browser invites bot detection, and the
   countermeasures are stealth plugins and fingerprint spoofing. That is detection evasion
   — exactly what dropping the site scrapers was meant to avoid. Building headless without
   an explicit written policy on this quietly reverses that decision.
5. **Latency.** 2–10s per page against ~200ms. On a budgeted agent that is wall-clock the
   user pays for.

**The gate.** After M2 has run the eval corpus, read the `FetchOutcome` mix (§10.4):

- Take `js_required` **only** — not the aggregate failure rate. `robots_denied` and
  `guard_denied` are the system working correctly; `paywall` and `bot_block` are not fixed
  by a browser; `structured_only` is fixed by a day of parser work instead.
- **`js_required` under ~5%** — delete the headless item from M10. Record the number in
  the repo so the question stays closed and doesn't get relitigated on intuition.
- **~5–15%** — build structured-data extraction and re-measure. Expect most of it to move
  to `structured_only`. Reconsider only on what remains.
- **Above ~15%** — the capability is worth buying, but reach for the hosted render API
  first: it costs a metered per-call charge that the ledger already models, and it carries
  none of costs 1–4 above. Ship an in-process browser only if a concrete requirement rules
  the render API out.

In all four branches, the domain list per cause is as useful as the rate — a failure rate
concentrated in three domains is a denylist entry or a targeted adapter, not an
architecture change.

---

## 18. Usage surface — what the finished thing looks like

The sections above specify mechanism. This one specifies what a user actually types, because
several of the guarantees in §3 and §12 are only real if the CLI enforces them at the point
of use rather than documenting them. Output samples are illustrative.

Three surfaces over one daemon: **CLI** (human), **MCP** (coding agent), **HTTP/SSE**
(team). They are equal citizens — the CLI is not a debug tool bolted onto the MCP path.

### 18.1 Install and initialize

```bash
brew install mole      # or: go install github.com/<org>/mole/cmd/mole@latest
mole init              # interactive
```

`mole init` writes `~/.config/mole/config.toml`, creates the state DB, and collects:

| Value | Why it is required at init, not first use |
|---|---|
| LLM provider + key | §10.1 |
| Search provider + key | §10.2 |
| **Contact email** | Unpaywall and NCBI require it; OpenAlex polite pool wants it (§10.3) |
| Default budget unit + ceiling | So a bare `mole research` cannot run away (§8.5) |

Keys go to the OS keyring, falling back to a `0600` file. They are never written into
`.mcp.json` (§5.2).

```bash
mole doctor
```

```
✓ config           ~/.config/mole/config.toml
✓ state db         ~/.local/share/mole/mole.db (sqlite, WAL)
✓ llm provider     anthropic — key in keyring
✓ search provider  brave — key in keyring
✓ contact email    set                      [required for academic providers, §10.3]
✓ socket           $XDG_RUNTIME_DIR/mole.sock (0600, uid 1000)
✓ sandbox          podman 5.2 — netns disabled, seccomp default   [required for M8]
! connectors       none registered
! egress denylist  18 days old — run `mole update-denylist`
```

`doctor` is where the several "ship it as a startup check, not a README line" requirements
scattered through §3 and §10.3 actually live.

### 18.2 Run the daemon

```bash
mole serve                            # foreground, unix socket
systemctl --user enable --now mole    # shipped unit file
```

Unix socket, `0600`, peer credentials checked (§3.5). Nothing binds TCP unless asked (§18.5).

The daemon holds the database's single writer for its lifetime. CLI read commands
(`sessions`, `trace`, `doctor`, `stats`) open read-only and are therefore never blocked
by it, and never block it — see §7.1. They refuse rather than migrate if the schema is
behind, because migrating from a second process while the daemon runs is exactly the
contention the split exists to avoid.

### 18.3 CLI research session

```bash
mole research "What is the current consensus on tokenizer-free byte-level LLMs?" \
  --usd 3.00 --mode report --actors web,academic
```

Budget flags are explicit and mutually exclusive — `--usd 3.00` or `--tokens 50000`. There
is deliberately no bare `--budget N`: a bare number is ambiguous between the two units, and
§8 makes the unit semantically load-bearing.

```
session  s_01JQ8F3K   mode=report  budget=$3.00 (escrow $0.45 held, §8.3)

 planning ──────────────────────────────────────────
   8 initial leads   web:5  academic:3

 executing ─────────────────────────────────────────
 ✓ web       "byte-level transformer benchmark"    12 claims   $0.041
 ✓ academic  arXiv: MegaByte / MambaByte           19 claims   $0.038
 ⚠ web       techblog.example.com                  js_required — skipped (§10.4)
 ✓ academic  DOI 10.48550/… → Unpaywall OA copy     7 claims   $0.012
 ↻ replan    3 leads added, 2 gaps closed
 ✓ verify    2 contradictions, 1 supersession                  $0.089

 spent $1.84 / $3.00  ·  claims 61  ·  edges 94  ·  leads 19/21
```

Ctrl-C detaches; the session continues in the daemon. Reattach with
`mole status s_01JQ8F3K --follow`.

```bash
mole result s_01JQ8F3K                     # markdown to stdout
mole result s_01JQ8F3K --format json       # claims + edges
mole sessions                              # list
mole cancel s_01JQ8F3K                     # stop a runaway
```

Reports surface disagreement rather than resolving it silently (§13):

```markdown
MambaByte reports 1.31 BPB on PG-19 at 350M params [3].

> **Sources disagree.** [3] (2024-01, preprint) reports 1.31 BPB; [7] (2024-11,
> peer-reviewed) reports 1.44 under a corrected harness. [7] supersedes [3] on
> publication date and venue weight.
```

### 18.4 Interrogating a finished session

```bash
mole ask s_01JQ8F3K "Which of these beat a BPE baseline at equal FLOPs?"
```

Retrieval-only over the persisted claim graph — one LLM call, no research budget (§13).
This is the concrete payoff of the daemon plus a durable graph: the alternative to `ask` is
re-running the whole session.

```bash
mole trace s_01JQ8F3K
```

```
s_01JQ8F3K  $2.61 total
├─ planner   $0.34  (12%)   6 calls        ← batch replan, bounded digest (§9.1)
├─ executor  $1.61  (62%)   19 leads
│  ├─ web      $0.92   11 leads   38 claims
│  └─ academic $0.69    8 leads   23 claims
├─ verifier  $0.31  (12%)   grounding re-fetches: 2 ($0.04)
└─ output    $0.35  (13%)                   ← paid from escrow (§8.3)

fetch outcomes: ok 24 · js_required 2 · structured_only 3 · robots_denied 1 · 404 1
```

The role breakdown is a `GROUP BY role` over the ledger — the reason `ToolCall.Role` exists
(§8.1).

### 18.5 MCP — Claude Code and other agents

```json
// .mcp.json — committed; contains no secrets (§5.2)
{
  "mcpServers": {
    "mole": {
      "command": "mole-mcp",
      "args": ["--socket", "${XDG_RUNTIME_DIR}/mole.sock"]
    }
  }
}
```

`mole-mcp` is disposable. The editor spawns it, it forwards over the socket, it dies with
the editor session — while the research continues in the daemon. That asymmetry is the
whole reason for the daemon/shim split (§5).

The interaction pattern is async, like waiting on CI:

```
⏺ mole:research.report
  prompt: "GCRA vs token bucket for multi-tenant API rate limiting…"
  budget: { unit: "tokens", amount: 40000 }
  → { session_id: "s_01JQ9M2P", status: "running" }

[agent continues other work across several turns]

⏺ mole:research.status  { session_id: "s_01JQ9M2P" }
  → { status: "done", spent: 31240, remaining: 8760, claims: 34 }

⏺ mole:research.result  { session_id: "s_01JQ9M2P" }
  → { report_md: "…", claims: [ … ], edges: [ … ] }
```

Agent callers get claims, not only prose (§5.3):

```json
{
  "text": "GCRA requires O(1) state per key versus token bucket's two fields",
  "source": "https://…",
  "quote": "…a single timestamp, the theoretical arrival time, is sufficient…",
  "published_at": "2023-06-14",
  "confidence": 0.88,
  "edges": [{ "kind": "supports", "to": "c_0f21" }]
}
```

Token mode is the default on this surface (§8.5) — the caller already reasons in tokens.

Three tools exist specifically so an agent cannot get stuck: `research.cancel` (stop a
runaway), `research.sessions.list` (recover a session ID after the agent's own context was
compacted), and `research.ask` (re-interrogate for one call instead of redoing the work).

### 18.6 Local data analysis

Connectors are registered once, on the machine, so credentials never traverse an MCP call
(§5, §12.2):

```bash
mole connect add prod-ro --dsn "postgres://readonly@db.internal/app"
```

```
✓ connected
✓ role privileges: SELECT only — no INSERT/UPDATE/DELETE/DDL grants
✓ schema cached: 14 tables, 203 columns
  registered as 'prod-ro'
```

Least privilege is checked at registration, not assumed:

```
✗ REFUSED: role 'app_user' holds INSERT, UPDATE, DELETE on 9 tables.

  Mole runs LLM-authored SQL against this connection. The parse gate and
  read-only transaction (§12.2) are defense in depth, not a substitute for
  least privilege.

  Create a read-only role, or re-run with --i-accept-write-risk.
```

Then, from an agent:

```
⏺ mole:research.analyze_local
  connector: "prod-ro"        ← a name. never a DSN, never credentials
  prompt: "test for weekly seasonality in request latency"
  budget: { unit: "tokens", amount: 20000 }
```

What comes back is an aggregate, by construction (§12.1):

```
claim:  "Weekly seasonality present in api_requests.latency_ms
         (Ljung-Box p<0.001, period=168h, stable across 3 holdout windows)"
source: connector:prod-ro#q_8f21ab
```

The privacy claim is auditable rather than asserted:

```bash
mole audit s_01JQ9X4T
```

```
data crossings for session s_01JQ9X4T:
  q_8f21ab  SELECT date_trunc('hour',…) … GROUP BY 1  → envelope: 4 col stats, 0 rows
  q_c02e19  sandbox: statsmodels seasonal_decompose   → envelope: 3 test results, 0 rows
  raw rows crossing the gate: 0
```

Cross-session statistics, including the input to §17.1's gate:

```bash
mole stats --fetch --since 90d      # FetchOutcome mix + ranked domains per cause
mole stats --cost --by role
```

### 18.7 Team / remote

```bash
mole serve --http 0.0.0.0:7777 --tls-cert … --token-file /etc/mole/token
```

Bearer token with no default, `Origin` validated, TLS required off-loopback (§3.5). Docker
image and compose file shipped. Teammates point
`mole-mcp --remote https://mole.internal:7777 --token …` at it. Still self-hosted; the
project runs no shared infrastructure.

### 18.8 Session lifecycle, end to end

| Phase | What happens |
|---|---|
| Admission | Escrow held (§8.3); ceilings set — budget, `MaxToolCalls`, wallclock, `MaxLeads` (§8.5) |
| Planning | Initial leads from prompt + enabled actor types |
| Execution | Workers lease leads; per lead reserve → run → settle (§8.2). A cache hit returns the prior result at zero cost, never skips (§9.3) |
| Inside an actor | search → guarded fetch → extract → chunk → map-reduce summarize under a sub-budget → claims, each with a quote verified verbatim against the source (§4.1, §11.5) |
| Verification | New claims clustered against the whole store; edges inferred; grounding re-fetch for high-stakes claims, from escrow (§11) |
| Replanning | Batched, against a bounded rolling digest — not a growing summary set (§9.1) |
| Termination | Budget floor, any ceiling, queue drained, or `cancel` |
| Output | Final verify + report, from escrow. Session persists indefinitely for `ask` |

Kill the daemon mid-session and restart: the boot sweep requeues leads with expired leases
and releases stale reservations (§9.4). The session resumes rather than stranding — which
is the observable difference between "leases" and "a status column".
