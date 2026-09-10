package resolver

import (
	"encoding/json"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

const SynthOdoo = "odoo"

type odooReference struct {
	Domain   string `json:"domain"`
	Name     string `json:"name"`
	Relation string `json:"relation"`
	Line     int    `json:"line"`
}

type odooDeclarations struct {
	nodes       []*graph.Node
	modules     []*graph.Node
	keys        map[string][]*graph.Node
	roots       map[string]*graph.Node
	moduleCache map[string]string
	moduleNames map[string]bool
}

func odooString(n *graph.Node, key string) string { s, _ := n.Meta[key].(string); return s }
func odooDecode(value any, to any) {
	if b, err := json.Marshal(value); err == nil {
		_ = json.Unmarshal(b, to)
	}
}
func (d *odooDeclarations) module(n *graph.Node) string {
	key := n.RepoPrefix + ":" + n.FilePath
	if module, ok := d.moduleCache[key]; ok {
		return module
	}
	for dir := path.Dir(n.FilePath); ; dir = path.Dir(dir) {
		if m := d.roots[n.RepoPrefix+":"+dir]; m != nil {
			module := odooString(m, "odoo_module")
			d.moduleCache[key] = module
			return module
		}
		if dir == "." || dir == "/" {
			break
		}
	}
	d.moduleCache[key] = ""
	return ""
}
func (d *odooDeclarations) qualified(n *graph.Node, name string) string {
	if strings.Contains(name, ".") {
		return name
	}
	if module := d.module(n); module != "" {
		return module + "." + name
	}
	return name
}
func (d *odooDeclarations) add(domain, name string, n *graph.Node) {
	if name == "" {
		return
	}
	key := domain + ":" + name
	for _, old := range d.keys[key] {
		if old.ID == n.ID {
			return
		}
	}
	d.keys[key] = append(d.keys[key], n)
}
func (d *odooDeclarations) targets(from *graph.Node, domain, name string) []*graph.Node {
	return d.resolveTargets(from, domain, name, map[string]bool{})
}
func (d *odooDeclarations) resolveTargets(from *graph.Node, domain, name string, seen map[string]bool) []*graph.Node {
	key := domain + ":" + name
	if seen[key] {
		return nil
	}
	seen[key] = true
	if domain == "xmlid" {
		name = d.qualified(from, name)
	}
	if domain == "file" && len(d.keys[domain+":"+name]) == 0 && from.RepoPrefix != "" && !strings.HasPrefix(name, from.RepoPrefix+"/") {
		name = from.RepoPrefix + "/" + name
	}
	var out []*graph.Node
	for _, n := range d.keys[domain+":"+name] {
		if from.WorkspaceID != n.WorkspaceID {
			continue
		}
		if domain == "file" && n.RepoPrefix != from.RepoPrefix {
			continue
		}
		// An addon present in the source repo shadows a same-named addon
		// elsewhere. Never bridge two physical copies of the same addon.
		shadow := false
		module := d.module(n)
		if n.RepoPrefix != from.RepoPrefix && module != "" {
			shadow = d.moduleNames[from.RepoPrefix+":"+module]
		}
		if !shadow {
			out = append(out, n)
		}
	}
	if len(out) == 0 && (domain == "field" || domain == "method") {
		if dot := strings.LastIndex(name, "."); dot > 0 {
			model, member := name[:dot], name[dot+1:]
			for _, base := range d.targets(from, "model", model) {
				var refs []odooReference
				odooDecode(base.Meta["odoo_refs"], &refs)
				for _, ref := range refs {
					if ref.Relation == "inherit" || (domain == "field" && ref.Relation == "delegates") {
						out = append(out, d.resolveTargets(from, domain, ref.Name+"."+member, seen)...)
					}
				}
			}
		}
	}
	return out
}

func (d *odooDeclarations) viewModel(n *graph.Node, seen map[string]bool) string {
	if seen[n.ID] {
		return ""
	}
	seen[n.ID] = true
	if model := odooString(n, "odoo_view_model"); model != "" {
		return model
	}
	var refs []odooReference
	odooDecode(n.Meta["odoo_refs"], &refs)
	for _, ref := range refs {
		if ref.Relation == "inherit_id" {
			for _, parent := range d.targets(n, "xmlid", ref.Name) {
				if model := d.viewModel(parent, seen); model != "" {
					return model
				}
			}
		}
	}
	return ""
}

func ResolveOdoo(g graph.Store) int { return ResolveOdooScoped(g, nil) }

// ResolveOdooScoped reconciles an entire affected workspace. Odoo extensions
// can change an unchanged caller's candidate set, so changed-source-only scans
// cannot safely rebuild these relationships. Reads use language predicates;
// adjacency is batched and only Odoo-owned relationships are reconciled.
func ResolveOdooScoped(g graph.Store, scope map[string]bool) int {
	d := &odooDeclarations{keys: map[string][]*graph.Node{}, roots: map[string]*graph.Node{}, moduleCache: map[string]string{}, moduleNames: map[string]bool{}}
	var files []*graph.Node
	for _, language := range []string{"python", "odoo_xml", "odoo_csv", "javascript", "typescript"} {
		for _, n := range g.GetNodesByLanguage(language) {
			if n.Kind == graph.KindFile {
				files = append(files, n)
			}
			if odooString(n, "framework") != "odoo" {
				continue
			}
			d.nodes = append(d.nodes, n)
			if odooString(n, "odoo_kind") == "module" {
				d.modules = append(d.modules, n)
			}
		}
	}
	if len(d.nodes) == 0 {
		return 0
	}
	// Manifests reference CSS, images and other non-code files as well.
	files = files[:0]
	for n := range g.NodesByKind(graph.KindFile) {
		files = append(files, n)
	}
	for _, m := range d.modules {
		d.roots[m.RepoPrefix+":"+path.Dir(m.FilePath)] = m
		d.moduleNames[m.RepoPrefix+":"+odooString(m, "odoo_module")] = true
	}
	sort.Slice(d.nodes, func(i, j int) bool { return d.nodes[i].ID < d.nodes[j].ID })
	workspaces := map[string]bool{}
	for _, n := range d.nodes {
		if scope == nil || scope[n.RepoPrefix] {
			workspaces[n.WorkspaceID] = true
		}
	}
	for _, n := range files {
		d.add("file", n.FilePath, n)
	}
	for _, n := range d.nodes {
		kind := odooString(n, "odoo_kind")
		model := odooString(n, "odoo_model")
		switch kind {
		case "module":
			d.add("module", odooString(n, "odoo_module"), n)
		case "model":
			if model != "" {
				d.add("model", model, n)
				d.add("model_base", model, n)
			}
			var inherits []string
			odooDecode(n.Meta["odoo_inherit"], &inherits)
			if model == "" && len(inherits) > 0 {
				d.add("model", inherits[0], n)
			}
			if model != "" {
				d.add("xmlid", d.qualified(n, "model_"+strings.ReplaceAll(model, ".", "_")), n)
			}
		case "field":
			d.add("field", model+"."+odooString(n, "odoo_field"), n)
			d.add("xmlid", d.qualified(n, "field_"+strings.ReplaceAll(model, ".", "_")+"__"+odooString(n, "odoo_field")), n)
		case "method":
			d.add("method", model+"."+odooString(n, "odoo_method"), n)
		}
		if id := odooString(n, "odoo_xmlid"); id != "" {
			d.add("xmlid", d.qualified(n, id), n)
			if kind == "ir.ui.view" {
				d.add("template", d.qualified(n, id), n)
			}
		}
		d.add("template", odooString(n, "odoo_template"), n)
		d.add("registry", odooString(n, "odoo_registry"), n)
		d.add("asset", odooString(n, "odoo_asset"), n)
	}
	// Module hooks target ordinary module-level functions; only manifest
	// directories are searched, so identical hook names stay addon-local.
	for _, n := range g.GetNodesByLanguage("python") {
		if n.Kind == graph.KindFunction {
			d.add("function", n.Name, n)
		}
	}
	var sources []*graph.Node
	var ids []string
	for _, n := range d.nodes {
		if workspaces[n.WorkspaceID] {
			sources = append(sources, n)
			ids = append(ids, n.ID)
		}
	}
	old := g.GetOutEdgesByNodeIDs(ids)
	var writes []*graph.Edge
	wanted := map[graph.EdgeIdentity]bool{}
	add := func(from, to *graph.Node, ref odooReference) {
		if from.ID == to.ID {
			return
		}
		kind := graph.EdgeReferences
		switch ref.Relation {
		case "inherit", "inherit_id", "t-inherit", "t-extend":
			kind = graph.EdgeExtends
		case "delegates":
			kind = graph.EdgeComposes
		case "depends_on":
			kind = graph.EdgeDependsOn
		case "compute", "inverse", "search", "object", "function", "rpc":
			kind = graph.EdgeCalls
		}
		e := &graph.Edge{From: from.ID, To: to.ID, Kind: kind, FilePath: from.FilePath, Line: ref.Line, Confidence: ConfidenceTyped, Origin: graph.OriginASTInferred, Meta: map[string]any{"odoo_relation": ref.Relation, "odoo_key": ref.Name}}
		StampSynthesizedTyped(e, SynthOdoo)
		identity := graph.EdgeIdentity{From: e.From, To: e.To, Kind: e.Kind, FilePath: e.FilePath, Line: e.Line}
		if !wanted[identity] {
			wanted[identity] = true
			writes = append(writes, e)
		}
	}
	for _, n := range sources {
		var refs []odooReference
		odooDecode(n.Meta["odoo_refs"], &refs)
		viewModel := d.viewModel(n, map[string]bool{})
		if viewModel != "" {
			for _, spec := range []struct{ key, domain, relation string }{{"odoo_view_fields", "field_path", "view_field"}, {"odoo_buttons", "method_path", "object"}} {
				var rows []map[string]string
				odooDecode(n.Meta[spec.key], &rows)
				for _, row := range rows {
					line, _ := strconv.Atoi(row["line"])
					refs = append(refs, odooReference{spec.domain, viewModel + "/" + row["name"], spec.relation, line})
				}
			}
		}
		for _, ref := range refs {
			var targets []*graph.Node
			switch ref.Domain {
			case "field_path", "method_path":
				parts := strings.SplitN(ref.Name, "/", 2)
				if len(parts) != 2 {
					continue
				}
				models := []string{parts[0]}
				members := strings.Split(parts[1], ".")
				for i, field := range members {
					var next []string
					domain := "field"
					if ref.Domain == "method_path" && i == len(members)-1 {
						domain = "method"
					}
					for _, model := range models {
						for _, f := range d.targets(n, domain, model+"."+field) {
							if ref.Domain == "field_path" || domain == "method" {
								targets = append(targets, f)
							}
							if related := odooString(f, "odoo_comodel"); related != "" {
								next = append(next, related)
							}
						}
					}
					models = next
				}
			case "asset_file":
				for _, f := range files {
					if f.RepoPrefix != n.RepoPrefix {
						continue
					}
					module := d.module(f)
					if module == "" {
						continue
					}
					var relative string
					for _, m := range d.modules {
						if m.RepoPrefix == f.RepoPrefix && odooString(m, "odoo_module") == module {
							relative = module + "/" + strings.TrimPrefix(f.FilePath, path.Dir(m.FilePath)+"/")
							break
						}
					}
					if odooAssetMatch(ref.Name, relative) {
						targets = append(targets, f)
					}
				}
			default:
				targets = d.targets(n, ref.Domain, ref.Name)
			}
			for _, target := range targets {
				if ref.Domain == "function" && (target.RepoPrefix != n.RepoPrefix || d.module(target) != d.module(n)) {
					continue
				}
				add(n, target, ref)
			}
		}
	}
	// Remove before adding, outside any live store iterator. RemoveEdge's
	// endpoint-shaped API may remove several sites; the complete desired set
	// below restores every still-valid site in one batch.
	for _, edges := range old {
		for _, e := range edges {
			if e.Meta[MetaSynthesizedBy] != SynthOdoo {
				continue
			}
			identity := graph.EdgeIdentity{From: e.From, To: e.To, Kind: e.Kind, FilePath: e.FilePath, Line: e.Line}
			if !wanted[identity] {
				g.RemoveEdge(e.From, e.To, e.Kind)
			}
		}
	}
	g.AddBatch(nil, writes)
	return len(writes)
}

// Odoo assets use recursive ** globs. Memoization bounds matching to the
// product of pattern and path segment counts, including repeated ** entries.
func odooAssetMatch(pattern, name string) bool {
	p, n := strings.Split(pattern, "/"), strings.Split(name, "/")
	seen, result := map[[2]int]bool{}, map[[2]int]bool{}
	var match func(int, int) bool
	match = func(i, j int) bool {
		key := [2]int{i, j}
		if seen[key] {
			return result[key]
		}
		seen[key] = true
		ok := false
		if i == len(p) {
			ok = j == len(n)
		} else if p[i] == "**" {
			ok = match(i+1, j) || (j < len(n) && match(i, j+1))
		} else if j < len(n) {
			segment, err := path.Match(p[i], n[j])
			ok = err == nil && segment && match(i+1, j+1)
		}
		result[key] = ok
		return ok
	}
	return match(0, 0)
}
