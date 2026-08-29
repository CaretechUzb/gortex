package languages

import (
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

func odooCSV(t *testing.T, filePath, src string) *parser.ExtractionResult {
	t.Helper()
	res, err := NewOdooCSVExtractor().Extract(filePath, []byte(src))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	return res
}

func TestIsOdooCSV(t *testing.T) {
	const path = "addons/l10n_lu/data/account.account.template.csv"
	cases := []struct {
		name, path, src string
		want            bool
	}{
		{"id header", path, "id,code,name\nlu_421611,421611,Debtors\n", true},
		{"quoted id header", path, "\"id\",\"code\"\n\"a\",\"1\"\n", true},
		{"BOM before id", path, "\xef\xbb\xbfid,code\na,1\n", true},
		{"single column", path, "id\na\n", true},
		{"no id column", path, "code,name\n421611,Debtors\n", false},
		{"id not first", path, "code,id\n1,a\n", false},
		{"id as a prefix only", path, "identifier,name\nx,y\n", false},
		{"not named after a model", "addons/sale/data/export.csv", "id,code\na,1\n", false},
		{"bare path, no model name", "reports/export.csv", "id,code\na,1\n", false},
		{"empty", path, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsOdooCSV(c.path, []byte(c.src)); got != c.want {
				t.Errorf("IsOdooCSV = %v, want %v", got, c.want)
			}
		})
	}
}

// An ordinary spreadsheet that happens to live in an addon must degrade to
// just the file node rather than minting records from its rows.
func TestOdooCSV_NonOdooYieldsOnlyFileNode(t *testing.T) {
	res := odooCSV(t, "addons/sale/data/sale.order.csv", "date,revenue\n2024-01-01,10\n")
	if len(res.Nodes) != 1 || res.Nodes[0].Kind != graph.KindFile {
		t.Fatalf("expected only a file node, got %d nodes", len(res.Nodes))
	}
	if len(res.Edges) != 0 {
		t.Errorf("expected no edges, got %d", len(res.Edges))
	}
}

// Every row's `id` is an external ID exactly as authoritative as a
// `<record id=>`, so it must mint the same node identity the XML side
// references — otherwise `ref="lu_2020_account_421611"` never binds.
func TestOdooCSV_RowMintsRecordNode(t *testing.T) {
	res := odooCSV(t, "addons/l10n_lu/data/account.account.template.csv",
		"id,code,name\nlu_2020_account_421611,421611,Debtors\nlu_2020_account_461411,461411,Creditors\n")

	n := odooNode(res, "odoo::record::l10n_lu.lu_2020_account_421611")
	if n == nil {
		t.Fatal("expected a record node for the first row")
	}
	if n.Kind != graph.KindResource {
		t.Errorf("kind = %v, want %v", n.Kind, graph.KindResource)
	}
	if got := n.Meta["odoo_xml_id"]; got != "l10n_lu.lu_2020_account_421611" {
		t.Errorf("odoo_xml_id = %v", got)
	}
	if got := n.Meta["odoo_model"]; got != "account.account.template" {
		t.Errorf("odoo_model = %v, want account.account.template", got)
	}
	if got := n.Meta["odoo_module"]; got != "l10n_lu" {
		t.Errorf("odoo_module = %v, want l10n_lu", got)
	}
	// Line 1 is the header, so the first row is line 2.
	if n.StartLine != 2 {
		t.Errorf("StartLine = %d, want 2", n.StartLine)
	}
	if odooNode(res, "odoo::record::l10n_lu.lu_2020_account_461411") == nil {
		t.Error("expected a record node for the second row")
	}
	if odooHasEdge(res, graph.EdgeDefines, "odoo::record::l10n_lu.lu_2020_account_421611") == nil {
		t.Error("expected the file to define the record")
	}
}

// The model a data file feeds is only in its name, and Odoo lets one model
// be loaded from several `-suffix` variants in the same module.
func TestOdooCSVModel(t *testing.T) {
	cases := map[string]string{
		"addons/l10n_lu/data/account.account.template.csv":        "account.account.template",
		"addons/l10n_es/data/account.account.template-common.csv": "account.account.template",
		"addons/sale/security/ir.model.access.csv":                "ir.model.access",
		"odoo/addons/base/data/res.country.state.csv":             "res.country.state",
		// Hand-titled exports are not model names; a wrong model is
		// worse than none, because it mints a reference that can only
		// ever miss.
		"addons/x/data/Name_products.csv": "",
		"addons/x/data/products.csv":      "",
	}
	for path, want := range cases {
		if got := odooCSVModel(path); got != want {
			t.Errorf("odooCSVModel(%q) = %q, want %q", path, got, want)
		}
	}
}

// A `:id` column is an external-ID reference — a many2one written as a
// name rather than a database key — so the file consumes the same
// vocabulary it declares.
func TestOdooCSV_ReferenceColumnsBecomeEdges(t *testing.T) {
	res := odooCSV(t, "addons/sale/security/ir.model.access.csv",
		"id,name,model_id:id,group_id:id,perm_read\n"+
			"access_sale_order,sale.order,model_sale_order,base.group_user,1\n")

	from := "odoo::record::sale.access_sale_order"
	for _, want := range []string{"sale.model_sale_order", "base.group_user"} {
		if odooHasEdge(res, graph.EdgeReferences, odooXMLIDPlaceholder+want) == nil {
			t.Errorf("expected a reference to %q", want)
		}
	}
	for _, e := range res.Edges {
		if e != nil && e.To == odooXMLIDPlaceholder+"base.group_user" && e.From != from {
			t.Errorf("reference hangs off %q, want %q", e.From, from)
		}
	}
}

// A to-many column holds a comma-separated list. Treating the cell as one
// name mints a reference no record can ever answer.
func TestOdooCSV_ToManyReferenceCellIsSplit(t *testing.T) {
	res := odooCSV(t, "addons/l10n_do/data/account.account.template.csv",
		"id,code,tag_ids/id\nacc_1,1,\"account_tag_6,account_tag_52\"\n")

	for _, want := range []string{"l10n_do.account_tag_6", "l10n_do.account_tag_52"} {
		if odooHasEdge(res, graph.EdgeReferences, odooXMLIDPlaceholder+want) == nil {
			t.Errorf("expected a reference to %q", want)
		}
	}
	if odooHasEdge(res, graph.EdgeReferences, odooXMLIDPlaceholder+"l10n_do.account_tag_6,account_tag_52") != nil {
		t.Error("the whole cell must not be referenced as one external ID")
	}
}

// A cross-module reference is already qualified and must not pick up the
// referencing module's prefix.
func TestOdooCSV_QualifiedIDsAreLeftAlone(t *testing.T) {
	res := odooCSV(t, "addons/l10n_lu/data/account.account.template.csv",
		"id,parent_id:id\nl10n_generic.base_acc,account.other_acc\n")

	if odooNode(res, "odoo::record::l10n_generic.base_acc") == nil {
		t.Error("an already-qualified row id must keep its own module")
	}
	if odooHasEdge(res, graph.EdgeReferences, odooXMLIDPlaceholder+"account.other_acc") == nil {
		t.Error("an already-qualified reference must keep its own module")
	}
}

// Odoo's own data files carry ragged and occasionally malformed rows.
// Losing a whole chart of accounts to one bad line would be far worse
// than losing the line.
func TestOdooCSV_RaggedRowsDoNotAbortTheFile(t *testing.T) {
	res := odooCSV(t, "addons/sale/data/res.country.state.csv",
		"id,code,name,extra\nstate_a,A,Alpha\nstate_b,B,Beta,x\n\nstate_c,C,Gamma\n")

	for _, want := range []string{"state_a", "state_b", "state_c"} {
		if odooNode(res, "odoo::record::sale."+want) == nil {
			t.Errorf("expected %q to survive a ragged neighbour", want)
		}
	}
}

// A dataset that merely shares the extension must not become a node per
// row; the cap is what keeps a `.csv` claim from bloating the graph.
func TestOdooCSV_OversizeFileIsFileNodeOnly(t *testing.T) {
	src := "id,v\n" + strings.Repeat("a,1\n", odooCSVMaxBytes/4+16)
	res := odooCSV(t, "addons/sale/data/res.country.state.csv", src)
	if len(res.Nodes) != 1 {
		t.Errorf("expected only a file node, got %d nodes", len(res.Nodes))
	}
}
