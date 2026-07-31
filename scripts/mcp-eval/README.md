# mcp-eval: an A/B harness for Jaeger's MCP tools and Skills

Proof of concept for LFX 2026 Term 3, *Benchmarking the AI Assistant's MCP Tools
and Skills* ([jaegertracing/jaeger#9135](https://github.com/jaegertracing/jaeger/issues/9135),
[cncf/mentoring#2027](https://github.com/cncf/mentoring/issues/2027)).

**This is a demonstration artifact, not a merge candidate.** It is here to show a
working method, and to be argued with.

```bash
go run ./scripts/mcp-eval             # run the matrix, print the report
go run ./scripts/mcp-eval -scenarios  # describe the suite and its ground truth
go test ./scripts/mcp-eval/           # 21 tests, 0 lint issues
```

## The hypothesis under test

Jaeger's assistant already asserts a strategy. `mcptools/INSTRUCTIONS.md` tells
the model:

> These tools support progressive disclosure to manage context density. […]
> prefer starting with broad discovery (`get_services` or `search_traces`) or
> structural overviews (`get_trace_topology`) before requesting verbose OTLP
> details for specific spans.

That is a falsifiable claim about agent behaviour, and nothing currently measures
it. "Context density" is the same quantity #9135 calls **context bloat**. This
harness exists to turn that assertion into a number.

## What it measures

The three trajectory metrics from #9135, plus a correctness pair:

| Metric | Definition here |
|---|---|
| Call error rate | fraction of tool calls the layer rejected (unavailable tool, malformed arguments) |
| Steps to evidence | number of calls after which every ground-truth evidence span had become **usable**, with attribute-level detail where the answer depends on it |
| Context bytes | total payload the agent had to read |
| Correct | did it name the ground-truth root cause |
| Justified | had it actually assembled the evidence |

`Correct` and `Justified` are separate on purpose. An agent can name the right
span without having seen why, and a benchmark that scores only the final answer
cannot tell that apart from reasoning. `TestCorrectButUnjustifiedIsDistinguished`
pins the distinction.

Evidence requires *summary* level, not a bare span ID. An enumeration tool that
returns only IDs tells the agent a span exists and nothing it could reason from,
so counting that as evidence would credit an arm for information it never
received. `TestBareEnumerationIsNotEvidence` pins that too.

## The experiment

Full factorial, both axes varied together because #9135's premise is that they
interact:

- **Tool shape** — `analytical` (`get_trace_topology`, `get_trace_errors`,
  `get_span_details`) vs `granular` (`get_services`, `get_trace_spans`,
  `get_span_details`).
- **Skill narrative** — `procedural` (follow the shipped `SKILL.md` steps
  literally) vs `goal-oriented` (skip any step whose data is already in hand).

`get_trace_spans` does not exist in Jaeger. It is the proposed granular variant of
`get_trace_topology`: same structure, no summary. Producing such variants is
itself a deliverable of #9135.

## The scenarios

Two deterministic, **trace-solvable** faults, derived from the skills Jaeger ships
in `mcptools/skills/`, so the suite measures the assistant that exists:

| Scenario | Skill | Spans | Root cause |
|---|---|---|---|
| `n-plus-one` | `detect-n-plus-one` | 27 | the handler issuing 24 sequential near-identical queries |
| `error-cascade` | `error-root-cause` | 6 | the deepest error span, three levels below the reported failure |

*Trace-solvable* is the load-bearing property and the hardest part of building the
suite: the root cause must follow from span data alone — structure, timing, status
and attributes — with no application logs or source access. Every scenario records
its own `Solvability` argument, printed by `-scenarios`, so a reviewer can
**disagree** with the claim. That is deliberate: an unauditable benchmark is an
anecdote.

`error-cascade` includes both gotchas from the skill it exercises: a timed-out
parent that masks the real cause, and a cancelled child carrying no error status,
so a naive "deepest error span" heuristic cannot win by accident.

## Results

```
## Scenario: n-plus-one
| tools      | skill narrative | calls | steps to evidence | context bytes |
|------------|-----------------|------:|------------------:|--------------:|
| analytical | goal-oriented   |     1 |                 1 |         5,071 |
| analytical | procedural      |     3 |                 1 |        14,486 |
| granular   | goal-oriented   |     3 |                 2 |        12,090 |
| granular   | procedural      |     5 |                 2 |        21,505 |

## Scenario: error-cascade
| tools      | skill narrative | calls | steps to evidence | context bytes |
|------------|-----------------|------:|------------------:|--------------:|
| analytical | goal-oriented   |     2 |                 2 |         2,857 |
| analytical | procedural      |     3 |                 2 |         3,518 |
| granular   | goal-oriented   |     2 |                 2 |         2,563 |
| granular   | procedural      |     3 |                 2 |         3,224 |
```

All eight cells are correct and justified, so the differences are pure efficiency.

**1. The guidance holds on wide traces, and the effect is large.** On the 27-span
N+1 trace the analytical arm reaches evidence in 1 call instead of 2 and reads
**5.0 KB instead of 12.1 KB**, a 58% reduction. `TestAnalyticalToolsReduceCostOnWideTraces`
asserts this so a regression in tool shape fails the build.

**2. The guidance does not hold everywhere.** On the 6-span error-cascade trace
the analytical arm costs **294 bytes more** and saves no calls. `get_trace_errors`
returns full `SpanDetail` for every error span, so on a small trace where most
spans have errored it ships nearly the whole trace verbosely. Progressive
disclosure is a property of trace *shape*, not a universal rule — which is exactly
the kind of claim only measurement can produce.

**3. "Granular vs analytical" is two axes, not one.** `get_trace_errors` is
*analytical in selection* (server-side filtering to error spans) but *granular in
payload* (full attributes and events). `get_trace_topology` is analytical in both.
Finding 2 is entirely explained by that split, and it suggests a concrete design
question for the real project: should `get_trace_errors` take a verbosity
parameter, the way `get_trace_topology` already takes `depth`?

**4. Procedural narratives are expensive when tools already answer the question.**
Following `detect-n-plus-one` step 4 literally ("inspect repeated siblings with
`get_span_details`") triples context on the N+1 trace — 14.5 KB against 5.1 KB —
without changing the answer, because the structural overview had already
established the pattern.

## What this proves, and what it does not

The agent here is **scripted, not an LLM**. That is a deliberate choice, and it
bounds the claims.

An evaluation harness is a measuring instrument, and an instrument has to be
calibrated against a known signal before it is trusted with a noisy one. With a
deterministic agent every number above is reproducible — `TestMatrixIsDeterministic`
runs the matrix six times and requires byte-identical results — so a change in a
metric can only come from a change in the tool or skill variant. That property is
what makes the harness usable as a regression gate in CI.

So this establishes that **the measurement pipeline is sound and the tool
variants differ measurably**. It does **not** establish how a real model behaves:
an LLM may not follow the procedure it is given, may issue malformed arguments
(the call-error-rate metric exists for exactly that and is 0% throughout here,
which is itself a sign the scripted agent cannot exercise it), and may be swayed
by prompt wording in ways no script reproduces.

Substituting a real model means implementing the same `Agent` interface over an
MCP client and a chat-completion loop. Nothing else changes. Under the mentorship
that swap is where Opik or Langfuse enters: they store and compare trajectories
across runs, which is what you need once results stop being deterministic and
statistical validity (run counts, temperature, seeds, variance) starts to matter.

Other limitations worth stating plainly:

- **Bytes are a proxy for tokens.** Deterministic and tokenizer-independent, but
  it under-counts JSON structural overhead, which most tokenizers charge for.
  Real trajectory work should measure tokens for the model under test.
- **Two scenarios is not a benchmark**, it is an existence proof. #9135 asks for
  5–10.
- **The tool implementations are mirrored, not real.** The response shapes are
  restated from `mcptools/internal/types` because that package is not importable
  from here; a production harness would drive the real MCP server over stdio.
- **Overfitting is unaddressed.** Tuning skills against a handful of scenarios
  optimises for the benchmark. Held-out scenarios are the obvious mitigation and
  are not implemented here.

## Layout

| File | Role |
|---|---|
| `scenario.go` | `Scenario`, `GroundTruth`, and the two fixtures with their solvability arguments |
| `trace.go` | fixture span model and tree helpers |
| `toolset.go` | the tools, their response shapes, and the two variants |
| `agent.go` | the two skill narratives; reads only what tools return |
| `eval.go` | trajectory recording, the metrics, the matrix runner |
| `report.go` | Markdown rendering |
| `harness_test.go` | 21 tests, including the fixture and determinism guards |

The agent never inspects a fixture directly — only tool payloads. If it could
peek, the call counts would be fiction.
