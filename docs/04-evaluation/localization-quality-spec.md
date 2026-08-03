# Localization quality specification

Status: implementation specification. This document controls localization-quality work until it is superseded by a reviewed revision.

## Objective

Given a natural-language coding task, return the production files and declared symbols that a developer should inspect or change. The system must solve this generically from syntax, graph structure, query intent, and evidence provenance. It must not recognize benchmark cases.

The quality targets are:

- at least 90% file recall and 75% declared-symbol recall for every supported language with a statistically meaningful sample;
- no material regression in languages already above a target;
- every emitted symbol claim resolves to a declaration in the emitted file;
- bounded, deterministic localization with no unbounded graph walk or model-dependent repair step.

A scorer result is a measurement, not an input to production behavior. Once a task set influences implementation choices, it is a development/regression set. Claims of generalization require a fresh holdout.

## Non-goals

- Memorizing repository names, issue identifiers, prompt phrases, file paths, or gold answers.
- Adding per-project routing or language-and-repository special cases.
- Treating a filename mention, prose occurrence, or line-local label as a declared-symbol hit.
- Replacing precise syntax extraction with a universal text scanner.
- Maximizing a single benchmark at the expense of latency, determinism, or other task classes.

## Anti-overfitting contract

Production code and tests introduced for localization work must satisfy all of these rules:

1. No benchmark task identifiers, titles, repositories, paths, transcripts, or expected answers.
2. No lookup table or conditional keyed by project identity, corpus identity, or a combination of language and project identity.
3. Test fixtures are minimal synthetic programs that exercise a language construct or graph topology. Their identifiers describe the construct, not a benchmark case.
4. Heuristics are expressed in generic features: node kind, declaration span, owner, edge kind, path role, query term, provenance channel, or graph distance.
5. Language-specific behavior is allowed only in parsing and syntax-aware resolution, where the language grammar requires it.
6. Every ranking rule needs an ablation or focused test demonstrating the generic invariant it implements.
7. Development-set metrics, paired canary metrics, and fresh-holdout metrics are reported separately.

A review that finds a forbidden artifact blocks the change even if metrics improve.

## Failure model

Localization is a pipeline. Each stage has a distinct responsibility and should be measured separately.

| Stage | Responsibility | Typical failure |
| --- | --- | --- |
| Parse | Extract declarations, spans, and syntax-local references | declaration absent or wrong span |
| Resolve | Assign canonical owners and connect references to declarations | method attached to a local label, wrong overload, unresolved member |
| Retrieve | Find candidates from lexical, literal, structural, and semantic signals | correct file or symbol never enters the candidate set |
| Expand | Follow bounded relationships to likely implementation owners | wrapper, configuration, test, or caller is returned without its implementation |
| Rank | Order and diversify valid candidates | correct evidence is present but falls below the visible budget |
| Preserve | Merge evidence across initial, refinement, exact-read, and recovery passes | earlier correct evidence disappears after a later pass |
| Terminalize | Decide whether evidence is sufficient and render it | unresolved state is presented as a confident answer |
| Validate | Ensure final file/symbol tuples refer to real declarations | prose or path text is mistaken for a symbol claim |

Metrics must distinguish candidate recall from visible recall and final-claim recall. Otherwise retrieval failures and presentation failures are conflated.

## Parser and resolver policy

### Tree-sitter

Tree-sitter is the default extractor for declarations, lexical scopes, receivers, signatures, and source spans. Query files and AST walks should cover ordinary language syntax, including declarations nested in language-defined containers.

Use tree-sitter when a fact is represented structurally in the grammar. A ranking heuristic must never compensate for a declaration that the parser can extract correctly.

### Manual parsing

A manual scanner is acceptable only as a narrow fallback when at least one of the following is true:

- the supported tree-sitter grammar omits the construct;
- error recovery removes the structural node but balanced delimiters still define a safe declaration boundary;
- a preprocessor or macro layer hides structure from the grammar;
- a language-specific external block has a small, documented syntax surface.

Manual scanners must be bounded, delimiter-aware, comment/string-aware where relevant, and covered by malformed-input tests. They emit the same declaration representation as tree-sitter and are deduplicated by canonical identity. They must not become a second general-purpose parser.

### Resolution

The resolver owns canonical symbol identity. A canonical declaration is the tuple:

`(repository, normalized file, declaration kind, canonical owner chain, name, declaration start)`

Display names may differ, but candidate merging and final validation use canonical identity. Local labels such as `name@line` are navigation aids, not final symbol identities; they must resolve to the enclosing declared function, method, type, or field before emission.

Member resolution should use receiver/type information first, then owner scope, arity/signature evidence, imports, and only then name fallback. Ambiguous fallback results remain multiple candidates and cannot become an enforceable terminal answer.

## Retrieval and expansion policy

Initial retrieval combines independent channels rather than relying on one query formulation:

- declaration-name and qualified-name search;
- exact literals and configuration keys mentioned in the task;
- file/path/module concepts;
- structural graph matches;
- semantic retrieval when available.

Every candidate records its channel and supporting provenance. A later pass may add evidence or rank, but must not silently erase an earlier valid candidate.

After initial retrieval, perform at most one bounded implementation-owner expansion from candidates with a non-implementation role, including tests/examples, configuration/registration sites, fields/constants, interfaces/traits, adapters, callers, and generic forwarding wrappers. Eligible edges are typed graph relations such as calls, implements, overrides, references, aliases/re-exports, and member ownership. Expansion is subject to a fixed node/edge budget, same-repository preference, declaration validation, and cycle detection.

Expansion is evidence, not certainty. A unique complete wrapper-to-callee route can be strong proof; incomplete or ambiguous routes remain ranked alternatives.

## Ranking and visibility

Ranking uses generic, inspectable features:

- task-term agreement with declared and qualified names;
- exact literal or path evidence;
- declaration kind and production/test/generated role;
- relationship type and graph distance;
- direct versus expanded provenance;
- ambiguity and resolution confidence;
- independent-channel support.

Production declarations receive a prior for implementation questions, but test, example, configuration, and generated candidates are not discarded. They can be the task subject and can provide routes to production implementations.

The visible result budget reserves diversity across evidence roles and source files before filling remaining slots by score. This prevents several near-duplicate candidates from hiding a lower-ranked implementation candidate supported by a different channel.

## Evidence invariants

1. **Declaration backed:** every candidate has a resolvable declaration identity and file. Non-declaration evidence may support a candidate but cannot itself be emitted as a symbol.
2. **Tuple consistent:** a file/symbol claim is valid only when that declaration belongs to that file.
3. **Monotonic preservation:** for one task generation, accepted evidence is unioned by canonical identity across initial, refinement, exact-read, and recovery passes. It can be removed only when validation proves it stale or invalid, and that reason is recorded.
4. **Provenance preserving:** merging unions channels, routes, scores, and proof flags rather than replacing an earlier row with a weaker later row.
5. **Bounded expansion:** graph expansion has explicit hop and candidate caps, deterministic ordering, and cycle prevention.
6. **Honest terminality:** `answer_ready` requires visible declaration-backed evidence that satisfies the evidence policy. A depleted recovery budget is not proof.
7. **No silent unresolved finalization:** an unresolved or ambiguous state remains advisory and reports uncertainty; it is never upgraded merely because the allowed call was consumed.

## First implementation slice

The first slice is intentionally cross-language and addresses pipeline loss before adding parser breadth:

1. Preserve and union declaration-backed evidence across terminal-state recovery and exact/refinement reads.
2. Keep bounded implementation routes visible alongside their originating candidates.
3. Validate emitted file/symbol tuples against indexed declarations and normalize navigation-only labels to their declared owner.
4. Add role-diverse visible-result selection so a production implementation is not hidden by several candidates of one role.
5. Add one concrete parser/resolver correction only where a generic syntax construct is demonstrably misrepresented; do not add corpus-derived aliases.

Further parser work is prioritized from extraction-gap measurements after this slice, not from aggregate symbol misses alone.

## Verification

### Unit and property tests

- Synthetic snippets for declaration ownership, nested scopes, receivers, extensions/implementations, overloads, generics, macros or recovery syntax as applicable.
- State-machine tests proving evidence monotonicity across every permitted transition.
- Tuple-validation tests proving that prose, paths, signatures, and wrong-file symbols cannot become declaration claims.
- Expansion tests with wrapper, test-to-production, configuration-to-handler, interface-to-implementation, and ambiguous/cyclic graphs.
- Determinism tests under shuffled insertion order.
- Budget tests proving fixed upper bounds.

### Evaluation ladder

1. Run parser and resolver unit tests.
2. Run localization contract, digest, terminal-state, retrieval, and ranking tests.
3. Replay frozen transcripts and candidate artifacts offline; do not rebill model calls.
4. Run a small paired canary on a single pinned binary and scorer revision.
5. Run the full development benchmark only after the canary passes.
6. Evaluate a fresh, sealed holdout before making a generalization claim.

Every evaluation artifact records the task-set checksum, transcript prefix/count and checksum, scorer revision, corpus revision, Gortex revision/build identity, exclusions, and completion marker.

## Acceptance gates

A change is ready to merge when:

- the anti-overfitting contract passes review;
- all new generic tests and affected existing suites pass;
- candidate recall, visible recall, and final-claim recall are reported separately;
- no supported language shows a material regression on the frozen development set;
- latency and result-size budgets remain within their documented limits;
- any claimed cross-corpus improvement is confirmed on a fresh holdout.

The 90%/75% targets are release objectives, not permission to weaken these gates.