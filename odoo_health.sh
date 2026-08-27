#!/bin/sh
# Odoo indexing health check against a live Gortex store.
#
#   usage: ./odoo_health.sh [path/to/store.sqlite]
#
# Reads the store read-only; safe to run against a live daemon. The unit
# and E2E suites prove each Odoo layer in isolation:
#
#   go test -race -run Odoo ./internal/parser/languages/ ./internal/contracts/ ./internal/resolver/
#   go test -race -run TestOdooEndToEnd ./pkg/gortex/
#
# This script answers the question those cannot: how well the pipeline
# binds on a REAL corpus. Section 4 is the regression watch — the two
# defect classes fixed by odoo_implicit_xmlid.go and the legacy-name half
# of odooResolveJSModule. Both should sit near zero.
#
# Read section 4b before drawing any conclusion from 4: a repo indexed by
# an older binary keeps its stale bindings until it is re-indexed, so a
# mixed-vintage store shows a large canary count that says nothing about
# the current resolver. Compare per repo, not in aggregate.
DB="${1:-$HOME/.gortex/store/store.sqlite}"
LOG="${GORTEX_DAEMON_LOG:-$HOME/.gortex/cache/daemon.log}"
q() { sqlite3 -readonly "$DB" "$@"; }

# --- Gate: has framework synthesis run since the last index? ---------------
# Sections 2-4 read edges the `framework_synthesis` global pass produces.
# That pass runs in the daemon's end_batch phase, which comes AFTER semantic
# enrichment — and enrichment budgets an hour and a half PER REPO. The daemon
# reports "ready - queryable" as soon as resolution finishes, hours before
# synthesis actually happens. Reading the rates in that window shows 0% bound
# and looks exactly like a broken resolver. It is not; it is a pass that has
# not started. Refuse to print numbers rather than print misleading ones.
last_index=$(q "select max(coalesce(nullif(indexed_at,''),0)) from repo_index_state;" 2>/dev/null \
  || q "select * from repo_index_state;" 2>/dev/null | awk -F'|' '{if($4>m)m=$4}END{print m+0}')
# Two log lines mark the pass and only one of them is emitted on a given
# path: the batch driver announces 'global pass: framework dispatch
# synthesis' before running, while the indexer reports 'framework dispatch
# calls synthesized' after. Matching only the announcement made a completed
# pass read as never-run and printed a false alarm over correct numbers, so
# take the later of the two.
last_synth=$(grep -E 'global pass: framework dispatch synthesis|framework dispatch calls synthesized' "$LOG" 2>/dev/null \
  | tail -1 | sed 's/.*"ts":\([0-9]*\).*/\1/')
if [ -z "$last_synth" ] || [ "${last_synth:-0}" -lt "${last_index:-0}" ] 2>/dev/null; then
  echo "!! framework_synthesis has NOT run since the last index."
  echo "!! Sections 2-4 below are meaningless until it does — every Odoo pass"
  echo "!! will read 0% bound. It runs in end_batch, queued behind enrichment;"
  echo "!! watch for 'global pass: framework dispatch synthesis' in:"
  echo "!!   $LOG"
  echo
fi


# The implicit-external-ID test, shared by 4 and 4b. Matches an xmlid whose
# local part carries an ORM-minted prefix, qualified or bare.
IMPLICIT_TEST="to_id like 'unresolved::odoo::xmlid::%'
       and (substr(to_id,26) like 'model~_%' escape '~' or substr(to_id,26) like 'field~_%' escape '~'
            or substr(to_id,26) like 'module~_%' escape '~'
            or to_id like '%.model~_%' escape '~' or to_id like '%.field~_%' escape '~' or to_id like '%.module~_%' escape '~')"
# A legacy odoo.define name is dotted; a modern specifier is @-scoped.
LEGACY_TEST="to_id like 'unresolved::odoo::jsmodule::%'
       and substr(to_id,29) not like '@%' and instr(substr(to_id,29),'.')>0"

echo "== 1. EXTRACT — did each Odoo extractor fire? (all must be non-zero) =="
q "select 'odoo_xml file nodes',        count(*) from nodes where language='odoo_xml' and kind='file'
   union all select 'odoo::record:: nodes',   count(*) from nodes where instr(id,'odoo::record::')>0
   union all select 'odoo::template:: nodes', count(*) from nodes where instr(id,'odoo::template::')>0
   union all select 'python classes w/ _name',count(*) from nodes where kind='type'   and meta is not null and instr(cast(meta as text),'odoo_model')>0
   union all select 'manifest module nodes',  count(*) from nodes where kind='module' and instr(id,'module::odoo:')>0
   union all select 'js files w/ odoo.define',count(*) from nodes where kind='file'   and meta is not null and instr(cast(meta as text),'odoo_js_legacy_name')>0;"

echo
echo "== 2. RESOLVE — binding rate per synthesizer pass =="
q "with b as (select
     (select count(*) from edges where is_unresolved=0 and meta is not null and instr(cast(meta as text),'odoo-xml')>0)   bx,
     (select count(*) from edges where is_unresolved=0 and meta is not null and instr(cast(meta as text),'odoo-js')>0)    bj,
     (select count(*) from edges where is_unresolved=0 and meta is not null and instr(cast(meta as text),'odoo-model')>0) bm,
     (select count(*) from edges where to_id like 'unresolved::odoo::xmlid::%'    or to_id like 'unresolved::odoo::method::%')   ux,
     (select count(*) from edges where to_id like 'unresolved::odoo::jsmodule::%' or to_id like 'unresolved::odoo::jsmethod::%'
                                    or to_id like 'unresolved::odoo::template::%') uj,
     (select count(*) from edges where to_id like 'unresolved::odoo::model::%')    um)
   select 'odoo-xml',   bx, ux, round(100.0*bx/(bx+ux),1)||'%' from b
   union all select 'odoo-js',    bj, uj, round(100.0*bj/(bj+uj),1)||'%' from b
   union all select 'odoo-model', bm, um, round(100.0*bm/(bm+um),1)||'%' from b;"

echo
echo "== 2b. PER-REPO overall Odoo bind rate — the shipping bar is 95% =="
q "with e as (select from_repo r,
     case when is_unresolved=0 and meta is not null
           and (instr(cast(meta as text),'odoo-xml')>0
             or instr(cast(meta as text),'odoo-js')>0
             or instr(cast(meta as text),'odoo-model')>0) then 1 else 0 end bound,
     case when to_id like 'unresolved::odoo::%' then 1 else 0 end unres,
     -- @odoo/* names the vendored OWL framework, which has no file in the
     -- graph; unresolvable by construction rather than a miss.
     case when to_id like 'unresolved::odoo::jsmodule::@odoo/%' then 1 else 0 end ext
   from edges where from_repo in ('odoo','addons','docker-env'))
   select r, sum(bound) bound, sum(unres) unres, sum(ext) external,
     round(100.0*sum(bound)/nullif(sum(bound)+sum(unres),0),2)||'%' raw,
     round(100.0*sum(bound)/nullif(sum(bound)+sum(unres)-sum(ext),0),2)||'%' net,
     case when 100.0*sum(bound)/nullif(sum(bound)+sum(unres)-sum(ext),0) >= 95 then 'PASS' else 'FAIL' end
   from e group by r order by r;"

echo
echo "== 2c. CSV data records — the Odoo declaration vocabulary indexed via .csv =="
q "select repo_prefix, count(*) from nodes
   where language='odoo_csv' and kind='resource' group by repo_prefix order by 2 desc;"
q "select 'csv file nodes', count(*) from nodes where language='odoo_csv' and kind='file';"

echo
echo "== 3. Unresolved placeholders by family =="
q "select substr(to_id,19,instr(substr(to_id,19),'::')-1) family, count(*) edges, count(distinct to_id) targets
   from edges where to_id like 'unresolved::odoo::%' group by family order by edges desc;"

echo
echo "== 4. REGRESSION CANARIES — must be ~0 in a repo indexed by a current binary =="
q "select 'implicit ORM external IDs (model_/field_/module_)', count(*) from edges where $IMPLICIT_TEST
   union all
   select 'legacy odoo.define specs (dotted, not @scoped)', count(*) from edges where $LEGACY_TEST;"

echo
echo "== 4b. Same two canaries per repo — a stale repo is a stale INDEX, not a bug =="
q "select from_repo, 'implicit', count(*) from edges where $IMPLICIT_TEST group by from_repo
   union all
   select from_repo, 'legacy_js', count(*) from edges where $LEGACY_TEST group by from_repo
   order by 3 desc;"

echo
echo "== 5. EXPECTED residue — not defects =="
q "select '@odoo/* framework imports (no file exists)', count(*) from edges
     where to_id like 'unresolved::odoo::jsmodule::@odoo/%';"

echo
echo "== 6. Top 15 still-unbound external IDs — eyeball for real misses =="
echo "   (data records living in CSV/SQL seed files are expected here)"
q "select substr(to_id,26) xmlid, count(*) n from edges
   where to_id like 'unresolved::odoo::xmlid::%' group by xmlid order by n desc limit 15;"
