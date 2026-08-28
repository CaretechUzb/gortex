# Query quality of the copy-installed worktree (`test_w@docker-env`)

Measured 2026-08-28 against the live store: `test_w@docker-env`, installed by
`CopyRepoSubgraph` rather than indexed, versus its source checkout `local` and
versus `local@aurora-redesign` — a *normally derived* worktree, which is the
fidelity reference and the control for anything that looks like a copy artifact.

**Original headline: the graph was faithful, the paths were not.** Every id-keyed
dimension was at exact parity. Every *path*-keyed column still pointed at the
source checkout, so the worktree could not serve a single source read. A second,
independent gap: the copy carried no inbound cross-repo edges.

The parity check that cleared this path in the first place was **cardinality
only** — nodes, edges, `references` into `odoo/` and `addons/`, FTS rows. All of
those were and are correct. None of them reads a path column. That is why it
shipped, and it is the lesson worth keeping: counting rows cannot see a value
that is wrong in every row.

Both gaps are now fixed (`d4ad2206`, and the inbound commit that follows it).

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

Node-id set diff, prefix-normalised: **one** entry, the global contract node.
No sibling-checkout leakage in either direction.

## 2. Was broken: the path dimension — FIXED and verified live

`CopyRepoSubgraph` rewrote node **ids** (both `<prefix>/…` and `<prefix>::…`
grammars) and never the columns carrying a *path*. All of them read `local/…`:

| column | stale rows, before | after the fix |
|---|---:|---:|
| `nodes.file_path` | 120,951 (100%) | 0 of 121,328 |
| `nodes.file_dir` | 120,951 (100%) — *derived*, see below | 0 — it follows `file_path` |
| `edges.file_path` | 893,174 | 0 of 899,776 |
| `files.file_path` | 9,616 | 0 of 9,616 |
| `content_fts.file_path` | 41 | 0 of 41 |

`file_dir` is **not** a fifth column needing its own rewrite: it is a VIRTUAL
generated column over `file_path` (`fileDirColumnDDL`), excluded from the
projection by `realColumns` and recomputed by SQLite. It read stale only
because its source did. Worth knowing anyway, because a repository-root file
gives it the bare prefix with no separator — the one value an anchor of
`"<prefix>/"` would miss, had it needed anchoring.

The convention is settled by the derived worktree: `local@aurora-redesign` has
140,434 nodes and 1,053,393 edges whose `file_path` carries **its own** prefix.

**Every source read failed.** `absolute_file_path` is built by stripping the
node's own repo prefix from `file_path` and joining to the repository root; the
strip found `test_w@docker-env/` against a value starting `local/`, stripped
nothing, and appended the stale segment whole:

```
before: open .../src/local.worktrees/test_w/local/his/models/common/...: no such file or directory
after:  606 bytes, byte-identical to the same symbol read from `local`
```

Everything downstream of a file read was affected: `read`,
`get_editing_context`, `smart_context` bodies, `edit_symbol`, `review`, snippets.

### Retraction: search ranking was never degraded

This document previously recorded, as a falsifiable prediction, that the
worktree returned 69 ranked results where `local` returned 75 because failed
file reads dropped six of them, and that the path fix should restore 75.

**The prediction was wrong and the measurement that follows kills it.** After
the fix the counts are unchanged, and the cause is the response payload, not the
data:

| checkout | prefix length | `total` | `max_returned` |
|---|---:|---:|---:|
| `local` | 5 | 504 | 75 |
| `test_w@docker-env` (copied) | 17 | 504 | 69 |
| `local@aurora-redesign` (**derived**) | 21 | 504 | 65 |

`max_returned` is a token budget, and every id and path in the payload carries
the prefix. A longer prefix buys fewer results. The *derived* worktree, which
was never copied, sits below the copy — so this is a property of the repo's
name, not of how its graph was installed.

The direct check: three queries small enough not to truncate return
**byte-identical** result sets from both checkouts (sha1 of the sorted,
prefix-stripped id list): `VisitQueueServiceStatus` 2/2, `AdmissionExamination`
8/8, `cyrillic_latin_translator` 131 total → 40/40 identical even under
truncation. The four symbols that appeared "missing" all exist under `test_w`
as both a node and a `symbol_fts` row.

## 3. Was incomplete: inbound cross-repo edges — FIXED

A global pass owns an edge by its **source** node, so a copy keyed on the source
prefix carries nothing that other repositories mint *into* the checkout.

| checkout | from `odoo` | from `addons` | total inbound |
|---|---:|---:|---:|
| `local` (derived) | 110,865 | 74,149 | **185,023** |
| `local@aurora-redesign` (derived) | 103,777 | 65,208 | **169,157** |
| `test_w` (copied, before) | 0 | 0 | **0** |

Inbound kinds into `local`: `references` 180,458 · `cross_repo_extends` 2,149 ·
`extends` 2,149 · `overrides` 163 · `composes` 95.

Outbound questions were already complete — go-to-definition, "what does this
reference", Odoo model/xmlid/JS binding, impact *from* the worktree. Reverse
questions under-reported badly:

| `find_usages` neighbourhood (nodes / edges) | `local` | `test_w` before |
|---|---:|---:|
| `…/web_view_leaflet_map/models/ir_ui_view.py::IrUiView` | 1,946 / 2,000 | 1,992 / 2,000 |
| `…/his/models/common/localtion_inherit_models.py::State` | 1,789 / 1,824 | **40 / 39** |
| `…/dms_attachments_viewer/models/dms_file.py::DmsFile` | 1,580 / 2,000 | **1 / 0** |

**That these belong in a copy was measured, not assumed.** Across the two derived
checkouts of one repository, 14,067 `odoo` symbols bind into `local` and 13,960
into `local@aurora-redesign` — **13,954 into both**. The binder fans out to every
checkout, so a derive that had seen the destination would mint these same edges;
the copy anticipates a derive rather than inventing state one would contradict.
Had the binder picked a single checkout, copying inbound would have produced a
graph the next derive silently halved.

Two constraints the fix respects. Only `to_id` moves: `from_id` and `file_path`
name the *source* repository's symbol and file. And a sibling checkout may never
be a source — two checkouts of one repository bound to each other is exactly the
contamination checkout groups exist to prevent. Probed before writing the filter:
`aurora → local` = 0 and `local → aurora` = 0, so no such edge exists today; the
filter is what keeps that true rather than lucky. `PurgeRepo` already deletes by
both `from_id` and `to_id`, so untracking the copy removes them again.

## 4. Minor — sidecars the copy still skips

| table | `local` | `test_w` | effect |
|---|---:|---:|---|
| `ref_facts` | 66 | 0 | resolution provenance (`analyze kind=resolution_outcomes`); only `gortex` (39,971) and `local` (66) are populated at all |
| `clone_corpus_state` | 1 | 0 | clone corpus rebuilds on demand; `clone_shingles` is 0 for both |
| `vectors` | 0 | 0 | no semantic index anywhere in this workspace — no relative loss |

## 5. Verdict

| surface | before | after |
|---|---|---|
| symbol search / FTS recall | full | full |
| graph structure, outbound binding to `odoo` / `addons` | full | full |
| Odoo model / xmlid / JS binding, CSV vocabulary | full | full |
| go-to-definition, impact *from* the worktree | full | full |
| ranked search results | *thought degraded — was not* | full |
| "who uses X", impact *into* the worktree | **empty** | full |
| source read, editing context, snippets, edit, review | **broken** | full |

## 6. Post-fix verification, on the live store

Both fixes were verified by untracking `test_w`, rebuilding, restarting the
daemon, and tracking it again — twice, once per commit.

| check | result |
|---|---|
| install time | **188 s** (path fix) / **149 s** (inbound), goal 300 s |
| stale path columns | 0 of 121,328 nodes · 0 of 899,776 edges · 0 of 9,616 files · 0 of 41 content rows |
| `get_symbol_source` on a copied symbol | 606 bytes, byte-identical to `local` |
| inbound from `odoo` | 110,865 → `local`, **110,865** → `test_w` |
| inbound from `addons` | 74,149 → `local`, **74,149** → `test_w` |
| inbound edges keep their owner's `file_path` | 110,865 carry `odoo/…`, 0 rewritten |
| cross-checkout invariant, 6 probes both directions | **0** everywhere |
| `find_usages` `…::State` | 1,789 / 1,824 — was 40 / 39, now equal to `local` |
| `find_usages` `…::DmsFile` | 1,580 / 2,000 — was 1 / 0, now equal to `local` |
| `find_usages` `…::IrUiView` | 1,946 / 2,000 — equal to `local` |
| `odoo_health.sh` §2b | `addons` 99.95% / 99.96% **PASS** · `odoo` 99.92% / 99.95% **PASS** |
| `odoo_health.sh` §4b canaries for `test_w` | implicit 14, legacy_js 13 — identical to `local` |
| CSV declaration vocabulary | 20,025 — identical to `local` |

The 105.5 s first measurement ran on an otherwise idle daemon; both figures
above shared the machine with a full `gortex` re-index.

`internal/graph`, `internal/graph/store_sqlite` and `internal/graph/storetest`
are green under `-race`.

`internal/indexer` is not, and the first explanation for it was wrong. It fails
3–4 tests per full `-race` run with a **different set each time** — across five
runs: `TestGDScriptResolution_ReceiverGate`,
`TestGDScriptResolution_MethodReferencesAreUsages`,
`TestGDScriptResolution_AutoloadScriptAsCaller`,
`TestContracts_CrossFileHandlerStillResolvesResponseType`,
`TestInlineContractPassRecordsCompletionMarker`,
`TestNodeNextEmittedJSSpecifier_CallGraph`. Every one passes in isolation, and
none touches the copy path.

That first looked load-induced, since the early runs shared the machine with a
full re-index. It is not: HEAD fails on a quiet machine too. It is also **not
introduced by these commits** — the same failures reproduce at `527d3f3e`,
before either of them. `4b102487` (30 commits back) passed once, which is not
enough to place the origin; a single green run of a non-deterministic suite
proves nothing. Left as an open, pre-existing item.

---

## 7. Third defect, same shape: edges sourced at a synthetic node

Found 2026-08-28 by recycling the worktree end-to-end (remove → untrack →
recreate → track) and differencing the edge set **by kind** instead of by total.

`CopyRepoSubgraph` selected its outbound frontier with `WHERE from_repo = ?`.
`edges.from_repo` is a GENERATED column — `substr(from_id, 1, instr(from_id,'/')-1)`
when there is a `/`, else `''` — so it understands only the `<prefix>/` id
grammar. A synthetic id has no slash (`local::stdlib::re::compile` → `''`) or
carries one in the wrong place (`local::builtin::js::array/map/object::entries`
→ `local::builtin::js::array`). `from_repo = 'local'` matches neither, so every
edge **sourced at** a synthetic node was silently left behind.

| | `local` (derived) | `local@aurora-redesign` (derived) | `test_w` (copied, before) |
|---|---:|---:|---:|
| edges sourced at `<prefix>::` nodes | 245 | 254 | **0** |

All 245 were `member_of`, binding a stdlib symbol to its module
(`local::stdlib::re::compile → local::module::go:re`). The fix uses
`prefixKeyRanges(srcPrefix)` — the two half-open ranges the *inbound* copy
already used, which cover both grammars and exclude sibling checkouts because
`@` (0x40) sorts above both terminators `0` (0x30) and `;` (0x3B).

### Why three rounds of checking missed it

The defect moves **only the raw edge total**, by 245 in ~900,000. Nodes, path
columns, FTS rows, inbound edges, cross-checkout invariants and every per-repo
target count were at exact parity. I saw the 245 twice and wrote it off as drift
between a copy and a source that had since been re-derived. It is visible only
when the edge set is differenced **by kind**, or when the frontier is split by id
grammar — both now in the probe.

The unit fixture was blind for the matching reason: every edge in it was sourced
at a `<prefix>/` node, the one shape `from_repo` can see. Eight tests asserted on
ids, paths and inbound edges and none could reach this. The fixture now carries
an edge sourced at a synthetic node — and a sibling's synthetic node too, which
pins the key-range bound at both ends.

This is the same failure mode as §2 and §3, for the third time: **a check that
only looks where the bug is not.** §2 counted rows and never read a path; §3
counted outbound and never asked about inbound; §7 counted a total and never
split it by kind.

## 8. Full recycle on a physically recreated worktree

The two earlier cycles untracked and re-tracked **without removing the worktree
directory**, so on-disk mtimes never moved. This cycle removed it with
`git worktree remove` and recreated it with `git worktree add` — 13,408 files,
every one freshly written. That exercises the hazard the original plan flagged
and deferred: *"`file_mtimes` must be re-stamped, since `git worktree add` writes
fresh mtimes on every file."*

| check | result |
|---|---|
| install time | **144 s** (goal 300) |
| `file_mtimes` provenance | restamped from the new checkout (1787892152–176), **not** carried from source (max 1787810672) |
| `gortex repos` freshness | `fresh` — no spurious re-index of 9,627 "modified" files fired |
| nodes | 121,328 = 121,328 |
| edges owned | **900,021 = 900,021** — exact, first time including the synthetic grammar |
| → file grammar / synthetic grammar | 899,776 / 245 on both sides |
| by-kind × by-grammar edge diff | **zero rows** |
| inbound from `odoo` / `addons` | 110,865 / 74,149 — exactly `local`'s |
| cross-checkout invariant, 6 probes | 0 |
| `find_usages` State / DmsFile / IrUiView | 1789·1824·44 / 1582·3154·32 / 8785·8873·4502 — all equal to `local`, file counts included |
| `get_symbol_source` on cross-repo-linked symbols | 25/25 byte-identical to `local` |
| odoo-side + addons-side untruncated probes | 30/30 where `test_w` equals `local` exactly |
| `odoo_health.sh` §2b | `addons` 99.95%/99.96% **PASS** · `odoo` 99.92%/99.95% **PASS** |
| §2c CSV vocabulary · §4b canaries | 20,025 · implicit 14 / legacy_js 13 — identical to `local` |

An earlier run of this same cycle took 340 s; that run shared the machine with a
full `-race` suite. Re-run on a quiet machine it is 144 s. The mtime hazard is
handled, not merely survived.

### Purge is symmetric — measured, not assumed

§3 asserted that "`PurgeRepo` already deletes by both `from_id` and `to_id`".
That was never actually measured. Untracking before removing the directory made
it cheap to check, and it holds: after the untrack, nodes, outbound edges,
**inbound edges**, `symbol_fts`, `files`, `file_mtimes` and both path columns all
read 0 for the prefix, while `local` was untouched (121,328 / 900,021 / 110,865 /
74,149). Had purge been source-keyed only, 185k edges would have been left
pointing at a dead prefix for the next copy to insert on top of.

### A second truncation trap, not a defect

From the odoo side, `get_dependencies` on `odoo/odoo::record::base.default_user`
returned `local` 24 and `aurora` 11 neighbours and **`test_w` 0** — which reads
exactly like the copy missing its inbound edges again. It is not. Results are
sorted by id ascending and the response truncated at 87 of 198 nodes; the cut
fell inside `local@aurora-redesign/…`, so `odoo` **and** `test_w@docker-env`
were both past it. In the store all three checkouts are symmetric: `local` 600
edges from that record, `test_w` 600, `aurora` 625.

`test_w@docker-env` sorts last among these prefixes, so it is systematically the
first thing dropped whenever a neighbourhood truncates. This is the same trap as
the retracted search-ranking claim in §2 — the second time a long prefix has
looked like missing data. **The discriminator is an untruncated control:** 30
probes whose neighbourhoods fit the budget all return `test_w` == `local`
exactly. Never call a long-prefixed repo's absence a defect without one.

### The last unexplained number in the probe: 6,602 bare edge paths

`edges.file_path` reads 6,602 rows per checkout that do **not** start with the
owning prefix. Both sides carry exactly 6,602, and cross-contamination is 0 in
each direction, so it is not a copy artifact — but "equal on both sides" is the
same reasoning that let §7 hide, so it was worth resolving rather than noting.

They are `his_website/…`, `his_dhp_lis/…`, `his/…` — the *bare repo-relative*
form, not another repository: there are 5,097 nodes under `local/his_website/`
and no repo named `his_website`. So this is the third place the two path
conventions coexist, alongside `files.file_path` (prefixed) and
`file_mtimes.file_path` (bare). Carrying them verbatim is correct on both counts:
a bare path is checkout-independent, and `pathExpr` anchors on `"<prefix>/"`, so
it leaves them alone rather than prefixing them.
