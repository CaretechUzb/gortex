# Gortex evaluation methodology

This directory documents the agent-graded self-eval methodology
gortex uses to measure its own real-world quality. The headline:
we evaluate gortex with the agents that actually use it (Claude
Sonnet 4.6, GPT 5.4, Copilot CLI), on three real codebases, across
five task categories, with a documented judge prompt and an
explicit "report negative deltas" requirement.

This is methodology only — no published numbers live here. Numbers
land in [`BENCHMARK.md`](../../BENCHMARK.md) once we run the
methodology against a tagged build.

## Contents

- [`methodology.md`](methodology.md) — the protocol: agents, tasks,
  classifiers, bias checks, negative-delta requirement.
- [`judge-prompt.md`](judge-prompt.md) — the exact judge prompt
  template (reproducibility: change the prompt → bump the rev).
- [`task-set.md`](task-set.md) — the 15 seed tasks (3 per category)
  with canonical answers.
- [`run.md`](run.md) — operational recipe: how to invoke the
  harness, where outputs land, how to publish results.

## Why this exists

A retrieval / code-intelligence engine can ship excellent
substrate (graph, MCP tools, USD savings) and still produce
agents that prefer Read/Grep when given the choice. The eval
methodology answers "with our tools available, does the model
actually use them, and does it produce better answers than it
would without?" That's the real test — not benchmark NDCG@10
on synthetic queries.

The methodology has three properties competitors typically lack:

1. **Multi-agent**: same task set scored against ≥3 distinct
   agent / model combinations so a result isn't a quirk of one
   provider.
2. **Bias-of-prompt check**: every task runs with both the
   default agent prompt AND a deliberately worse prompt (the
   "ablation prompt"); a methodology that only looks good on the
   tuned prompt is honest-flagged.
3. **Negative-delta requirement**: per-task scoring uses an
   (a)/(b)/(c) classifier that distinguishes "gortex helped",
   "no measurable difference", "gortex hurt". The published
   summary MUST cite both ends — hiding the negatives gets the
   methodology disqualified.

The substrate is already shipped (`gortex eval` substrate +
`eval/` Python harness); this directory makes it reproducible
end-to-end without an oral tradition.
