# Query quality of the copy-installed worktree (`test_w@docker-env`)

Measured 2026-08-28 against the live store, `test_w@docker-env` (installed in 105.5 s
by `CopyRepoSubgraph`) versus its source checkout `local` (indexed + derived normally)
and `local@aurora-redesign` (a *normally derived* worktree — the fidelity reference).

**Headline: the graph is faithful, the paths are not.** Every id-keyed dimension is at
exact parity. Every *path*-keyed column still points at the source checkout, so the
worktree cannot serve source reads. A second, separate gap: the copy carries no
inbound cross-repo edges.

The earlier parity check that cleared this path was **cardinality-only** — it counted
nodes, edges, `references` into `odoo/` and `addons/`, and FTS rows. All of those are
still correct. None of them touches the path dimension, which is why this shipped.

---

## 1. At exact parity — no measurable loss

| dimension | `local` | `test_w` | |
|---|---:|---:|---|
| nodes | 121,329 | 121,328 | delta = global `http::GET::/form`, correctly not duplicated |
| edges owned by the checkout | 899,776 | 899,776 | exact |
| → internal | 428,128 | 428,128 | exact |
| → `addons/` | 134,514 | 134,514 | exact |
| → `odoo/` | 108,891 | 108,891 | exact |
| → global / other | 228,243 | 228,243 | exact |
| `symbol_fts` rows | 95,470 | 95,470 | exact |
| `content_fts` rows | 41 | 41 | exact |
| `files` | 9,616 | 9,616 | exact |
| `file_mtimes` | 9,629 | 9,627 | delta = 2 `.ruff_cache/.gitignore`, absent from the worktree — correct |
| `contract_state` / `enrichment_state` / `repo_index_state` | 1 / 5 / 1 | 1 / 5 / 1 | exact |
| Odoo CSV data records | 20,025 | 20,025 | exact |
| search corpus recall (`HisVisitQueue`) | total 504, pre-rank 312 | total 504, pre-rank 312 | exact |

Node-id set diff, prefix-normalised: **one** entry, the global contract node. No
sibling-checkout leakage in either direction.

## 2. Broken — the path dimension

`CopyRepoSubgraph` rewrites node **ids** (both `<prefix>/…` and `<prefix>::…` grammars)
but never the columns that carry a *path*. All four still read `local/…`:

| column | stale rows under `test_w` |
|---|---:|
| `nodes.file_path` | 120,951 (100%) |
| `nodes.file_dir` | 120,951 (100%) — of which **73 are the bare `local`**, no trailing slash |
| `edges.file_path` | 893,174 |
| `files.file_path` | 9,616 |

The convention is settled by the normally derived worktree: `local@aurora-redesign` has
140,434 nodes and 1,053,393 edges whose `file_path` carries **its own** prefix.

**Consequence — every source read fails:**

```
$ get_symbol_source test_w@docker-env/his/models/common/localtion_inherit_models.py::State
Error: could not read source: open
  .../src/local.worktrees/test_w/local/his/models/common/localtion_inherit_models.py:
  no such file or directory
```

`absolute_file_path` is built by stripping the node's own repo prefix from `file_path`
and joining to the repo root. The strip finds `test_w@docker-env/` and the value starts
with `local/`, so nothing is stripped and the stale segment is appended to the worktree
root. Everything downstream of a file read is affected: `read`, `get_editing_context`,
`smart_context` bodies, `edit_symbol`, `review`, snippets.

Symbol search retrieval is unharmed (identical corpus, identical pre-rank count) but the
returned set shrinks: **75 results from `local`, 69 from `test_w`**, the 6 missing ones
all body-dependent (test methods, a `_compute_`, a JS `attr@320`). *Falsifiable
prediction:* after the path fix the worktree must return exactly 75. If it does not,
there is a second defect — note the tier-drop counts were identical (135/53) in both
runs, so the loss happens after tiering, and the file-read explanation is unconfirmed.

Free-text columns were swept too: `search_signature` (3), `section_text` (4),
`search_doc` (1) contain the literal `local/` — all genuine source text (a hardcoded
`/Users/mack/…` path, a `src/local/` mention in docs). These must **not** be rewritten.
`qual_name`, `search_qual_name`, `signature`, `clone_sig`, `doc`, `external`,
`nodes.meta`, `edges.meta`, `edges.member_receiver_dir`: 0 stale rows.

## 3. Incomplete — inbound cross-repo edges

A global pass owns an edge by its **source** node, so the copy (keyed on source prefix)
carries nothing that other repos mint *into* the checkout.

| checkout | from `odoo` | from `addons` | total inbound |
|---|---:|---:|---:|
| `local` (derived) | 110,865 | 74,149 | **185,023** |
| `local@aurora-redesign` (derived) | 103,777 | 65,208 | **169,157** |
| `test_w` (copied) | 0 | 0 | **0** |

Inbound kinds into `local`: `references` 180,458 · `cross_repo_extends` 2,149 ·
`extends` 2,149 · `overrides` 163 · `composes` 95.

**What this costs a query.** Outbound questions are complete — go-to-definition, "what
does this reference", Odoo model/xmlid/JS binding, impact *from* the worktree. Reverse
questions under-report:

| `find_usages` (nodes / edges in the neighbourhood) | `local` | `test_w` |
|---|---:|---:|
| `…/web_view_leaflet_map/models/ir_ui_view.py::IrUiView` | 1,946 / 2,000 | 1,992 / 2,000 |
| `…/his/models/common/localtion_inherit_models.py::State` | 1,789 / 1,824 | **40 / 39** |
| `…/dms_attachments_viewer/models/dms_file.py::DmsFile` | 1,580 / 2,000 | **1 / 0** |

Sibling-invariant probe: `aurora → local` = 0 and `local → aurora` = 0. No checkout of
one repository ever sources an edge into another checkout of it, so an inbound copy is
safe provided it excludes every sibling prefix as a source.

## 4. Minor — sidecars the copy skips

| table | `local` | `test_w` | effect |
|---|---:|---:|---|
| `ref_facts` | 66 | 0 | resolution provenance (`analyze kind=resolution_outcomes`); only `gortex` (39,971) and `local` (66) are populated at all |
| `clone_corpus_state` | 1 | 0 | clone corpus rebuilds on demand; `clone_shingles` is 0 for both |
| `vectors` | 0 | 0 | no semantic index anywhere in this workspace — no relative loss |

## 5. Verdict

| surface | quality |
|---|---|
| symbol search / FTS recall | **full** |
| graph structure, outbound + cross-repo binding to `odoo` / `addons` | **full** |
| Odoo model / xmlid / JS binding, CSV vocabulary | **full** |
| go-to-definition, "what does X reference", impact *from* the worktree | **full** |
| ranked search results | degraded (69/75) |
| "who uses X" / impact *into* the worktree from `odoo`/`addons` | **empty** |
| source read, editing context, snippets, edit, review | **broken** |

The 105.5 s install stands, and so does the binding claim the goal asked for — but
"100% ready" does not, until §2 is fixed. §3 must be fixed before claiming parity with
a derived worktree.
