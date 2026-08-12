<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="banner-dark.png">
    <img src="banner.png" alt="Mole — a deep research agent in Go, exposed over MCP" width="820">
  </picture>
</p>

<p align="center">
  <em>A deep-research agent with an enforced budget, verified quotes, and a privacy
  boundary for local data.</em>
</p>

Ask a question. mole decomposes it, searches, reads sources, extracts claims,
checks each claim against the text it came from, looks for contradictions between
them, and writes an answer with citations. Every model call is reserved against a
budget before it happens and settled after, so the ceiling you set is the ceiling
it hits.

It runs as a single static binary on your machine, uses your own API keys, and
speaks MCP so a coding agent can drive it.

<p align="center">
  <img src="demo.svg" alt="mole researching a question: planning, 39 claims, two contradictions found, $0.0149 spent" width="900">
</p>

<p align="center">
  <sub>A real run, trimmed for length — five sections of the answer and three of its
  five sources are cut. Note what it does with a disagreement: it reports both sides
  rather than picking one. $0.0149 is DeepSeek pricing; the same run on a frontier
  model costs more.</sub>
</p>

---

## Why mole

Three things mole does that a chat interface with web search does not.

**The budget is enforced, not estimated.** Every call is reserved before it is
made and settled after, against a ledger with non-negative constraints in the
database schema itself. `--usd 0.50` means the run stops at fifty cents. Measured
overshoot across the test corpus is 0%.

**Every claim carries a quote, checked against the source.** A claim whose quote
does not appear verbatim in the page it was mined from is discarded at extraction,
before it can reach an answer. Claims that survive can be re-read against their
source afterwards, and one that turns out not to be supported is marked as such in
the report rather than quietly dropped.

**Your local data stays local.** Point mole at a CSV or a folder and it will
analyse it without the contents leaving your machine: the model chooses a
hypothesis template and column names, mole renders and runs the SQL, and only
aggregates — counts, means, test results, buckets covering at least five records —
are allowed back. `mole crossings` shows you exactly what left.

---

## Install

Requires Go 1.25+. Pre-built binaries are not published yet.

```bash
go install github.com/lajosdeme/mole/cmd/mole@latest
go install github.com/lajosdeme/mole/cmd/mole-mcp@latest   # optional, for MCP clients
```

Builds `CGO_ENABLED=0` into a single static binary with no runtime dependencies.
The database is SQLite, created on first use under your XDG data directory.

### Configure

You need a search provider and a model provider. Keys live in
`~/.config/mole/config.json`, mode 0600 — never in environment variables that
leak into process listings, and never in `.mcp.json`.

```bash
mole config set search.provider tavily          # or: brave
mole config set search.tavily-key tvly-...

mole config set llm.provider anthropic          # or: openai-compatible
mole config set llm.api-key sk-...
mole config set llm.model claude-sonnet-5
mole config set llm.cheap-model claude-haiku-4-5

mole doctor                                     # verify everything above
```

Any OpenAI-compatible endpoint works — DeepSeek, Ollama, llama.cpp, vLLM, a proxy:

```bash
mole config set llm.provider openai-compatible
mole config set llm.base-url https://api.deepseek.com/v1
mole config set llm.model deepseek-chat
```

A model served from `localhost` is priced at zero and still counted in tokens, so
`--tokens` bounds a self-hosted run that costs no money at all.

---

## Usage

### Research a question

```bash
mole research "how much electricity does the bitcoin network use?" --usd 0.50
mole research "..." --tokens 200000            # token budget instead of dollars
mole research "..." --max-sources 8 --max-depth 3
mole research "..." --json                      # machine-readable result
```

Budget is required, and the two units are mutually exclusive. Only dollar mode can
price a search call; only token mode can bound a model whose rates mole does not
know.

### Ask a follow-up

```bash
mole ask <session-id> "what did the Cambridge estimate say?"
```

Answers from the claims that session already collected. No new searching, no new
spending beyond the one call to phrase the answer.

### Build a dataset instead of prose

```bash
mole research "largest UK supermarket chains and their revenue" \
  --mode dataset \
  --schema 'company:text!,revenue:number=annual revenue in GBP,employees:number' \
  --usd 0.50

mole dataset <session-id> --format csv > chains.csv
mole dataset <session-id> --format json          # every value every source gave
```

`!` marks the field that identifies a row. Rows are merged across sources by fuzzy
key, so `Aldi` and `Aldi UK` become one row with two sources. CSV holds one value
per cell and says so — it carries a source count and a `contested` column naming
the fields the sources disagree about. JSON carries every disagreeing value with
the sources behind each.

### Analyse local data

```bash
mole connect add sales ./exports/sales.csv       # one file
mole connect add exports ./exports               # or a whole folder
mole research "how does spend differ between regions?" \
  --actors local_compute --usd 0.30

mole crossings <session-id>                      # what left the machine
```

CSV, TSV, JSON and JSONL are supported; Parquet is not. The model never sees a
row and never writes SQL — it picks a template and column names, and mole renders
the statement.

### Serve MCP clients

```bash
mole serve
```

Listens on a unix socket, mode 0600, in a private directory, and refuses
connections from any other user. Point a client at the shim:

```json
{
  "mcpServers": {
    "mole": { "command": "mole-mcp" }
  }
}
```

No credentials in that file — the shim forwards to the daemon, which holds them.

### Inspect a run

```bash
mole sessions                # recent sessions and what they cost
mole trace <session-id>      # per-call cost and timing breakdown
mole stats --fetch           # why fetches failed, across sessions
```

---

## How it works

```
question
   ↓  planner            decompose into sub-questions, replan as evidence arrives
   ↓  executor           one lead at a time per worker, reserved and settled
   ↓  actor              search → fetch → extract → mine claims
   ↓                     every claim quote-checked against its source
   ↓  verifier           pair up related claims, adjudicate, build the claim graph
   ↓                     re-read a sample of claims against their sources
   ↓  output             synthesise from claims that survived, with citations
answer
```

Three actor types feed the same graph. **web** searches and reads pages.
**academic** queries Crossref, OpenAlex, arXiv and PubMed, deduplicates by DOI and
prefers open-access full text. **local_compute** runs deterministic SQL over data
you registered and never lets a row reach the model.

The design document — every decision, what was measured, and what is still
unproven — is in [docs/DESIGN.md](docs/DESIGN.md).

---

## Honest numbers

mole grades its own runs. `mole eval <session-id>` prints a scorecard, and any
metric it cannot compute says so instead of quietly reading zero.

| | |
|---|---|
| budget overshoot | **0%** — no run has exceeded its ceiling |
| claim integrity | **100%** — every stored claim carries a source and a verbatim quote |
| citation accuracy | **100%** — every quote found in the source it cites |
| grounding rate | **80%** — of claims re-read against their source, confirmed |
| contradiction precision | **70%** with the confirm pass, 51% without |
| merge precision / recall | **1.000 / 1.000** on constructed ground truth |

Contradiction detection is the weakest link, and mole is built to treat it as one.
A single model judgement calls two claims contradictory correctly about half the
time — so an edge is only written when a second judgement agrees, which raises
precision to 70% and keeps roughly half as many edges. `--no-confirm-edges` if you
would rather have the recall.

Three things are not measured: claim precision against labelled answers,
contradiction recall, and staleness detection. All three need a bigger labelled
corpus than exists today, so they are reported as unmeasured rather than estimated
from something easier to count.

---

## Contributing

Bug reports and issues are welcome. Code contributions go through a CLA — see
[CONTRIBUTING.md](CONTRIBUTING.md), which explains what it is for and what it
cannot do.

The one practice this project asks for that most do not: **falsify your own fix**.
After a change, revert the mechanism and confirm the test fails. A test that passes
with the fix removed proves nothing, and several of this project's own tests have
been caught doing exactly that.

```bash
gofmt -l .      # must print nothing
go build ./...
go test ./...   # must be clean, and no new skips
```

---

## Licence

Apache-2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
