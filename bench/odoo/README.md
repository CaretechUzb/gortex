# Odoo quality and coverage benchmark

Run from the Gortex repository with its normal Go/CGO build dependencies:

```sh
go run ./bench/odoo -json /tmp/odoo-quality.json
go test ./bench/odoo -count=1
```

The benchmark extracts hand-authored Python, XML, CSV and JavaScript fixtures,
round-trips metadata through JSON, and runs the registered `odoo` framework
synthesizer in an isolated in-memory graph. It also extracts HTTP contracts.
It does not require an indexed repository, Odoo server, database, network,
or a running Gortex daemon. Source files are never executed.

## What the scores mean

Each labeled query defines a **complete expected set within its stated scope**:

- `node`: a metadata key on one subject, one file, or the entire fixture graph;
- `edge`: Odoo-synthesized targets for a relation and optional source/file;
- `contract`: all HTTP provider identities and handlers in the selected scope,
  including providers incorrectly labeled as another framework.

Precision is `TP / (TP + FP)`, recall is `TP / (TP + FN)`, and F1 is their
harmonic mean. Missing and unexpected values are printed and included in JSON.
Values are sets: repeated identical observations are not extra true positives.
Aggregate counts sum query results, so overlapping custom queries weight their
facts more than once. **Unqueried facts are unassessed, not false positives.**
Empty denominators use 1 (no error in that dimension). Counts and feature status
must accompany those ratios: an untested feature does not have demonstrated
accuracy. Correct negative cases count as checks, but cannot by themselves
make a feature positively covered.

The inventory distinguishes passing/failing labeled categories, negative-only
categories, untested static categories, and runtime-only categories. Feature
coverage is the number of static categories with at least one positive label
divided by the static inventory size. It measures this fixture inventory,
**not the percentage of all Odoo behavior supported**. Categories may contain
many untested variants. Runtime features are shown but excluded from the static
denominator. A failing category remains labeled; coverage and correctness are
separate measurements.

The built-in suite covers models, inheritance, delegated field lookup, fields,
callbacks, API dependencies, environment lookups, manifests, hooks, assets,
records, views, QWeb, ACLs/rules, actions/menus, reports, cron/mail data,
registries, services, component templates, ORM calls, patch specifications,
controllers, source admission, and workspace/addon isolation. It also checks
edge kinds/provenance and repeated-synthesis stability. Unassessed areas remain
visible, including direct field imports, API return models, legacy versions,
asset directives, cross-file patches, and incremental rebinding. Existing
resolver unit tests cover some areas that this benchmark has not labeled.

## Regression gates and reports

```sh
go run ./bench/odoo \
  -min-precision 1 -min-recall 1 -min-feature-coverage 0.8 \
  -json /tmp/odoo-quality.json
```

Precision and recall gates apply **per tested feature**, preventing a large
successful category from hiding failures in a smaller one. Both default to 1.
Feature coverage is informational by default (`-min-feature-coverage 0`).
Choose a coverage floor explicitly in CI; 0.8 is met by the initial suite.

Exit codes from a compiled binary are 0 for pass, 1 for quality/audit failure,
and 2 for invalid arguments, invalid suites, or operational errors. `go run`
wraps nonzero program exits; build a binary when CI needs the exact code:

```sh
go build -o /tmp/odoo-bench ./bench/odoo
/tmp/odoo-bench -json - > /tmp/odoo-quality.json
```

JSON contains a schema version, suite version/SHA256, configured thresholds,
per-query differences, per-feature and aggregate metrics, coverage, invariant
failures, and extraction/synthesis timings. Only timings should vary between
unchanged fixture runs. The suite hash fingerprints the labels and inputs;
record your Git commit alongside the report to identify the implementation.
Timings are diagnostic single runs, not statistically controlled performance
comparisons.

## Read-only corpus audit

Use repeated roots to resolve cross-repository dependencies in one workspace:

```sh
go run ./bench/odoo \
  -corpus /Users/commeta/projects/his/docker-env/src/local \
  -json /tmp/odoo-his-quality.json
```

A smaller smoke audit:

```sh
go run ./bench/odoo \
  -corpus /Users/commeta/projects/his/docker-env/src/local/his_dhp \
  -corpus /Users/commeta/projects/his/docker-env/src/local/his_dhp_lis \
  -json /tmp/odoo-dhp-quality.json
```

A root can be an addon, addon collection, or repository. Include core and other
addon repositories using additional `-corpus` arguments to make their targets
available. Overlapping roots are rejected. The scanner skips nested symlinks,
hidden directories, `*.worktrees`, `node_modules`, virtual environments, and
Python caches. It reads Python/XML/CSV/JS/TS files plus files under `static/`.
Root order is retained and assigns distinct repository prefixes. This simulates
separate repositories even if two roots belong to the same checkout; prefer
one root per checkout for faithful addon shadowing.

Defaults are 20,000 candidate files total and 2 MiB per file; change these with
`-max-files` and `-max-file-bytes`. Truncation, oversized files, extraction/read
errors, no Odoo nodes, and graph invariant failures cause a failing audit. The
JSON reports skipped directories/symlinks and error samples (up to 50). The
source SHA256 covers the identities and contents actually read, not skipped or
oversized files. Sources must remain unchanged during an audit for a coherent
snapshot; the tool does not lock another repository.

Corpus output includes declaration kinds, relationship counts, HTTP provider
counts, and raw-reference binding availability by relation. Recovered CSV rows
are counted in `csv_recovery_rows`, with up to 50 source/diagnostic samples in
`csv_recovery_samples`. These warnings preserve source irregularities without
failing an otherwise complete extraction. The labeled suite includes ragged-row
and bare-quote recovery, preserved values, and reference checks. A raw reference is
bound when an Odoo edge with its source, relation, key and line exists. Derived
view references are counted in edge totals but excluded from the raw-reference
denominator. Unbound examples are included in JSON (up to 50).

**Corpus availability is not precision or recall.** Unbound references can mean
missing dependencies, dynamic behavior, parser gaps, self-references, or edge
identity coalescing. They do not automatically fail the audit. A wrong binding
can also count as available. Only independently labeled queries establish
accuracy; do not use a large edge count as evidence of correctness.

## Extend the ground truth

Edit `builtinSuite` in `suite.go`, or provide `-suite path.json`. A custom suite
replaces the built-in suite entirely. Unknown JSON keys, duplicate identities,
unknown features/files and malformed queries are rejected. Minimal example:

```json
{
  "version": "example-v1",
  "features": [{"id": "models", "description": "Literal model names"}],
  "files": [{
    "path": "demo/models.py",
    "repo": "addons",
    "workspace": "example",
    "source": "from odoo import models\nclass Item(models.Model):\n    _name = 'demo.item'\n"
  }],
  "checks": [{
    "name": "model name",
    "feature": "models",
    "kind": "node",
    "subject": "demo/models.py::Item",
    "predicate": "odoo_model",
    "expected": ["demo/models.py::Item = \"demo.item\""]
  }]
}
```

Node observations are `subject = <canonical JSON value>`. Edge observations
are `source -> target`; select `predicate` with an `odoo_relation` value (empty
selects all Odoo relations). Contract observations are `handler -> contract ID [framework]`.
Omit `subject` to label all matching subjects in `file`; omit both for the full
graph. Use `expected: []` for a deliberately empty result. Write expected sets
from Odoo semantics and the documented metadata schema; never regenerate labels
from current output to make a regression disappear. Add negative and ambiguous
examples, not only one successful spelling per feature.

Tests deliberately remove an inheritance target and introduce an unexpected
view field to prove the scorer detects both missing and extra links. They also
check metric arithmetic, threshold exits, custom-suite validation, report
reproducibility, missing corpus dependencies, malformed XML, file limits,
oversized files, overlapping roots, and skipped paths.
