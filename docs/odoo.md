# Odoo indexing

Gortex recognizes Odoo declarations in Python, XML, CSV and JavaScript/TypeScript.
The `odoo` synthesizer connects their literal model names, external IDs and
registry keys after extraction. It runs in the normal full and incremental
framework pipeline. No Odoo server or database is required, and source files
are never executed.

## Enable and query

The default framework selection includes Odoo. If your repository sets an
explicit allow-list, add `odoo` to `index.framework_synthesizers`:

```yaml
index:
  framework_synthesizers:
    - odoo
```

Keep any other synthesizers your project needs in that list. Index the core,
community/enterprise addons and custom addon repositories in the same Gortex
workspace to connect their declarations. Separate workspaces are not joined.
After upgrading Gortex, reindex those repositories to extract the new facts;
rerunning synthesis alone cannot recover facts absent from an older index.

Python classes, fields and methods keep their normal symbol IDs. Declarative
nodes use `<file>::odoo:<name>`, carry `framework: odoo`, and remain owned by
their source file. Synthesized edges carry `synthesized_by: odoo`,
`provenance: framework`, and `odoo_relation` explaining the relationship.
Use ordinary symbol search, inheritance/usages queries, and
`analyze kind=synthesizers` to inspect them.

## Coverage

| Source | Indexed declarations and specifications | Relationships |
| --- | --- | --- |
| Python models | `_name`, `_inherit`, `_inherits`; model bases including Model, AbstractModel and TransientModel; class attributes such as `_table`, `_auto`, `_description`, `_order`, `_rec_name`, company checks and SQL constraints | Model extension/classical inheritance, delegated composition; inherited member lookup |
| ORM fields | Field type and complete constructor expression, including relation options, selection additions, defaults, domains, storage, translation, indexing, security/groups, tracking and deletion behavior | Comodels, compute/inverse/search callbacks, related-field chains |
| Methods | Existing Python symbols and decorator annotations; API decorator arguments | `depends`, `constrains`, `onchange` field dependencies; literal environment model and XML-ID lookups |
| Addon manifests | `__manifest__.py` and `__openerp__.py`; all dictionary values retained, including external dependencies, licensing, installation flags and asset directives | Addon dependencies, data/demo/QWeb files, module hooks, literal asset paths/globs |
| XML records | Arbitrary record models, IDs, attributes, field values/eval expressions and `noupdate`; includes actions, menus, reports, ACLs/rules, groups, cron/automation, mail, website and third-party records | External IDs, model bindings, menus/actions/groups, report templates, literal `ref(...)` expressions |
| Views | View model, `inherit_id`, XPath expression/position, field/button/filter attributes | View inheritance, field usage including relational subviews, object buttons to model methods |
| QWeb/OWL templates | Template IDs, `t-name`, `t-inherit`, `t-extend` | Template inheritance and `t-call`; JS component template references |
| CSV data | Dotted model filenames with an `id` header; all rows/columns, quoted multiline values, ACL permissions | `:id` and `/id` external-ID columns; implicit model/field IDs |
| JavaScript/TypeScript | Registry categories/entries, legacy `odoo.define` names, patch specifications, component templates | Registry add/get/useService, same-file registry implementation and patch targets, literal ORM service calls to Python methods |
| Controllers | Literal `http.route` paths/lists and complete options (`auth`, `type`, `methods`, `csrf`, etc.) | HTTP provider contracts bound to the handler, including converter arguments such as `model("res.partner")` |

Specifications are retained even when they cannot become an exact relationship.
For example a record rule's domain and a cron's code are indexed as expressions;
the index does not execute them or claim to know their runtime result.

CSV data rows accept unequal column counts and bare quotes. Header-to-value
mapping remains positional: missing trailing columns are omitted from
`odoo_spec`, and unnamed extra values stay in `odoo_csv_extra`. The extractor
does not guess where a missing interior cell belonged or strip literal quotes
from IDs. Irregular rows retain `odoo_csv_row`, expected/actual counts in
`odoo_csv_columns`, and `odoo_csv_diagnostics` (`column_count_mismatch` or
`nonstandard_quotes`). Missing trailing column names are in `odoo_csv_missing`.
Wholly empty rows are skipped; malformed headers still fail extraction.

## Resolution and limits

Module names come from manifest directories, not manifest display names.
Unqualified external IDs are resolved within the containing addon. A same-named
addon in the source repository shadows another copy in a different repository.
Model extensions can produce several candidate declarations; these edges do
not claim a particular installed-module order or Python runtime MRO.

Facts survive target deletion in source-node metadata. The pass reconciles
its owned edges in affected workspaces, allowing unchanged sources to rebind
when declarations are renamed, removed or reintroduced. It uses language
predicates and batched adjacency/writes; incremental reconciliation currently
revisits the affected workspace rather than only the changed file.

Static indexing cannot cover database-created/Studio models and views, computed
model names, dynamic registration, monkey-patching effects, executed domains,
server-action code, runtime asset ordering, installed-module state or access
rights evaluation. Dynamic expressions stay as specifications without invented
targets. Cross-file JS patch targets and arbitrary recordset/dataflow inference
are not resolved by this pass. It does not replace an Odoo runtime inspection.

## Validation

The [Odoo quality benchmark](../bench/odoo/README.md) measures precision and
recall against hand-labeled facts, reports feature-inventory coverage and gaps,
and optionally audits real source repositories without using the daemon:

```sh
go run ./bench/odoo -min-feature-coverage 0.8 -json /tmp/odoo-quality.json
```

Corpus binding availability is reported separately from labeled accuracy.

Focused extractor, resolver and HTTP-contract tests cover positive and negative
detection, inheritance, related/computed fields, view bindings, CSV/XML ownership,
workspace/addon boundaries and stale-link repair. An optional read-only smoke
test uses the HIS addon checkout:

```sh
GORTEX_ODOO_TEST_ROOT=/path/to/docker-env/src/local \
  go test ./internal/resolver -run TestOdooHISCorpus -count=1
```
