package languages

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestOdooCSVRecovery(t *testing.T) {
	const file = "demo/ir.model.access.csv"
	// Patterns from l10n_{fr,hu,in,pk,tr} and spreadsheet_dashboard: extra
	// populated/empty cells, short rows, and a bare quote in an unquoted ID.
	src := "id,name,model_id:id,group_id:id\r\n" +
		"extra,Extra,model_demo,base.group_user,unmapped.model\r\n" +
		"trailing,Trailing,model_demo,base.group_user,\r\n" +
		"short,Short,model_demo\r\n" +
		"bare\",\"Bare\",model_demo,base.group_user\r\n" +
		"\r\n,,,\r\n" +
		"multi,\"Quoted,\r\nname with \"\"quotes\"\"\",model_demo,base.group_user\r\n" +
		"last,Last,model_demo,base.group_user\r\n"
	r, err := (&OdooDataExtractor{CSV: true}).Extract(file, []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	nodes := map[string]*graph.Node{}
	for _, n := range r.Nodes {
		if n.Kind != graph.KindFile {
			nodes[n.Name] = n
		}
	}
	if len(nodes) != 6 {
		t.Fatalf("lost records or indexed empty row: %v", nodes)
	}
	for _, tc := range []struct {
		name string
		line int
		key  string
		want any
	}{
		{"extra", 2, "odoo_csv_extra", []string{"unmapped.model"}},
		{"trailing", 3, "odoo_csv_extra", []string{""}},
		{"short", 4, "odoo_csv_missing", []string{"group_id:id"}},
		{"bare\"", 5, "odoo_csv_diagnostics", []string{"nonstandard_quotes"}},
		{"multi", 8, "odoo_spec", map[string]string{"id": "multi", "name": "Quoted,\nname with \"quotes\"", "model_id:id": "model_demo", "group_id:id": "base.group_user"}},
		{"last", 10, "odoo_csv_diagnostics", nil},
	} {
		n := nodes[tc.name]
		if n == nil || n.StartLine != tc.line {
			t.Fatalf("%s: wrong identity or line: %+v", tc.name, n)
		}
		if !reflect.DeepEqual(n.Meta[tc.key], tc.want) {
			t.Errorf("%s %s: got %#v, want %#v", tc.name, tc.key, n.Meta[tc.key], tc.want)
		}
	}
	short := nodes["short"]
	if _, exists := short.Meta["odoo_spec"].(map[string]string)["group_id:id"]; exists {
		t.Fatal("missing cell presented as explicitly empty")
	}
	for _, name := range []string{"extra", "trailing", "short"} {
		if !reflect.DeepEqual(nodes[name].Meta["odoo_csv_diagnostics"], []string{"column_count_mismatch"}) {
			t.Fatalf("row shape not diagnosed: %+v", nodes[name].Meta)
		}
		if nodes[name].Meta["odoo_csv_row"] == nil || nodes[name].Meta["odoo_csv_columns"] == nil {
			t.Fatal("irregular row not preserved")
		}
	}
	for _, n := range nodes {
		var refs []struct {
			Name, Relation string
			Line           int
		}
		b, _ := json.Marshal(n.Meta["odoo_refs"])
		if err := json.Unmarshal(b, &refs); err != nil {
			t.Fatal(err)
		}
		foundModel := false
		for _, ref := range refs {
			if ref.Name == "unmapped.model" || (n == short && ref.Relation == "group_id:id") {
				t.Fatalf("invented reference: %+v", ref)
			}
			if ref.Name == "model_demo" {
				foundModel = true
				if ref.Line != n.StartLine {
					t.Fatal("reference line drift")
				}
			}
		}
		if !foundModel {
			t.Fatalf("mapped model reference lost for %s", n.Name)
		}
	}
}

func TestOdooCSVPositionalMapping(t *testing.T) {
	// The Hungarian bank row omits an interior phone cell. The parser cannot
	// know that: keep positions and mark only the undeclared tail as missing.
	r, err := (&OdooDataExtractor{CSV: true}).Extract("res.bank.csv", []byte("id,email,phone,active\nbank,bank@example.test,False\n"))
	if err != nil {
		t.Fatal(err)
	}
	n := r.Nodes[1]
	want := map[string]string{"id": "bank", "email": "bank@example.test", "phone": "False"}
	if !reflect.DeepEqual(n.Meta["odoo_spec"], want) || !reflect.DeepEqual(n.Meta["odoo_csv_missing"], []string{"active"}) {
		t.Fatalf("guessed absent field: %+v", n.Meta)
	}
}

func TestOdooCSVQuoteRecoveryWithShortRow(t *testing.T) {
	r, err := (&OdooDataExtractor{CSV: true}).Extract("x.model.csv", []byte("id,name,other\nrow\",Name\nlast,Last,ok\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Nodes) != 3 || r.Nodes[2].Name != "last" || !reflect.DeepEqual(r.Nodes[1].Meta["odoo_csv_diagnostics"], []string{"column_count_mismatch", "nonstandard_quotes"}) {
		t.Fatalf("combined recovery lost a record: %+v", r.Nodes)
	}
}

// Optional validation against the six actual Odoo files that failed daemon
// indexing. This only reads source; it never imports Odoo or changes its repo.
func TestOdooCSVRealCorpus(t *testing.T) {
	root := os.Getenv("GORTEX_ODOO_CSV_ROOT")
	if root == "" {
		t.Skip("set GORTEX_ODOO_CSV_ROOT to the Odoo source checkout")
	}
	for _, tc := range []struct{ path, diagnostic string }{
		{"addons/l10n_fr/data/account.group.template.csv", "column_count_mismatch"},
		{"addons/l10n_hu/data/res.bank.csv", "column_count_mismatch"},
		{"addons/l10n_in/data/l10n_in.port.code.csv", "column_count_mismatch"},
		{"addons/l10n_pk/data/account.account.template.csv", "column_count_mismatch"},
		{"addons/l10n_tr/data/account.group.template.csv", "column_count_mismatch"},
		{"addons/spreadsheet_dashboard/security/ir.model.access.csv", "nonstandard_quotes"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(root, tc.path))
			if err != nil {
				t.Fatal(err)
			}
			r, err := (&OdooDataExtractor{CSV: true}).Extract(tc.path, data)
			if err != nil {
				t.Fatal(err)
			}
			if len(r.Nodes) < 3 {
				t.Fatal("records not retained")
			}
			diagnosed := 0
			for _, n := range r.Nodes {
				b, _ := json.Marshal(n.Meta["odoo_csv_diagnostics"])
				if strings.Contains(string(b), tc.diagnostic) {
					diagnosed++
				}
			}
			if diagnosed == 0 {
				t.Fatal("expected source irregularity not diagnosed")
			}
			t.Logf("%d records indexed, %d rows with %s", len(r.Nodes)-1, diagnosed, tc.diagnostic)
		})
	}
}
