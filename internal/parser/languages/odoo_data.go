package languages

import (
	"bytes"
	"encoding/csv"
	"encoding/xml"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// OdooDataExtractor indexes declarative data without evaluating Python or XML.
// Every record model is accepted, so security rules, jobs, mail templates,
// reports, website records and third-party addon configuration share one layer.
type OdooDataExtractor struct{ CSV bool }

func (e *OdooDataExtractor) Language() string {
	if e.CSV {
		return "odoo_csv"
	}
	return "odoo_xml"
}
func (e *OdooDataExtractor) Extensions() []string { return nil }

// CSVFileExtractor supplies the extension-level admission. Content detection
// promotes Odoo model data to odoo_csv; unrelated CSV stays a plain file.
type CSVFileExtractor struct{}

func (*CSVFileExtractor) Language() string     { return "csv" }
func (*CSVFileExtractor) Extensions() []string { return []string{".csv"} }
func (*CSVFileExtractor) Extract(file string, _ []byte) (*parser.ExtractionResult, error) {
	return &parser.ExtractionResult{Nodes: []*graph.Node{{ID: file, Name: path.Base(file), Kind: graph.KindFile, FilePath: file, Language: "csv"}}}, nil
}

var odooEvalRef = regexp.MustCompile(`\bref\s*\(\s*['"]([^'"]+)['"]\s*\)`)
var odooActionRef = regexp.MustCompile(`%\(([^)]+)\)[ds]`)

func (e *OdooDataExtractor) Extract(file string, src []byte) (*parser.ExtractionResult, error) {
	r := &parser.ExtractionResult{}
	r.Nodes = append(r.Nodes, &graph.Node{ID: file, Name: path.Base(file), Kind: graph.KindFile, FilePath: file, Language: e.Language()})
	if e.CSV {
		return e.extractCSV(r, file, src)
	}
	d := xml.NewDecoder(bytes.NewReader(src))
	type frame struct {
		tag   string
		attrs map[string]string
		owner *graph.Node
		text  strings.Builder
		line  int
	}
	var stack []*frame
	for {
		line, _ := d.InputPos()
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("Odoo XML %s: %w", file, err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			f := &frame{tag: t.Name.Local, attrs: map[string]string{}, line: line}
			if len(stack) > 0 {
				f.owner = stack[len(stack)-1].owner
			}
			for _, a := range t.Attr {
				f.attrs[a.Name.Local] = a.Value
			}
			a := f.attrs
			identity := a["id"]
			if identity == "" {
				identity = a["t-name"]
			}
			kind := ""
			switch f.tag {
			case "record":
				kind = a["model"]
			case "template":
				kind = "ir.ui.view"
			case "menuitem":
				kind = "ir.ui.menu"
			case "act_window":
				kind = "ir.actions.act_window"
			case "report":
				kind = "ir.actions.report"
			}
			if a["t-name"] != "" || a["t-inherit"] != "" || a["t-extend"] != "" {
				kind = "qweb"
			}
			if f.tag == "function" || f.tag == "delete" {
				kind = "operation:" + f.tag
			}
			if kind != "" {
				if identity == "" {
					identity = fmt.Sprintf("%s@%d", f.tag, line)
				}
				f.owner = odooNode(r, file, identity, graph.KindResource, line, e.Language())
				f.owner.Meta["odoo_kind"], f.owner.Meta["odoo_record_model"], f.owner.Meta["odoo_attributes"] = kind, kind, a
				if a["id"] != "" && f.tag != "delete" {
					f.owner.Meta["odoo_xmlid"] = a["id"]
				}
				if f.tag == "delete" {
					odooRef(f.owner, "xmlid", a["id"], "delete", line)
				}
				if a["t-name"] != "" {
					f.owner.Meta["odoo_template"] = a["t-name"]
				}
				odooRef(f.owner, "model", kind, "record_model", line)
				if len(stack) > 0 {
					for _, ancestor := range stack {
						if v := ancestor.attrs["noupdate"]; v != "" {
							f.owner.Meta["odoo_noupdate"] = v
						}
					}
				}
			}
			if f.owner != nil {
				n := f.owner
				for _, key := range []string{"ref", "inherit_id", "parent", "action", "groups"} {
					for _, ref := range strings.Split(a[key], ",") {
						ref = strings.TrimPrefix(strings.TrimSpace(ref), "!")
						relation := key
						if key == "ref" && a["name"] == "inherit_id" {
							relation = "inherit_id"
						}
						odooRef(n, "xmlid", ref, relation, line)
					}
				}
				for _, key := range []string{"t-inherit", "t-extend", "t-call"} {
					odooRef(n, "template", a[key], key, line)
				}
				for key, value := range a {
					for _, m := range odooEvalRef.FindAllStringSubmatch(value, -1) {
						odooRef(n, "xmlid", m[1], key, line)
					}
					for _, m := range odooActionRef.FindAllStringSubmatch(value, -1) {
						odooRef(n, "xmlid", m[1], "action", line)
					}
				}
				if f.tag == "xpath" {
					appendOdooSpec(n, "odoo_xpath", a)
				}
				if f.tag == "field" || f.tag == "button" || f.tag == "filter" {
					appendOdooSpec(n, "odoo_elements", a)
				}
				var fieldPath []string
				for i, ancestor := range stack {
					if ancestor.owner == n && ancestor.tag == "field" && ancestor.attrs["name"] != "arch" && i > 0 && stack[i-1].tag != "record" {
						fieldPath = append(fieldPath, ancestor.attrs["name"])
					}
				}
				if f.tag == "field" && a["name"] != "" && len(stack) > 0 && stack[len(stack)-1].tag != "record" {
					appendOdooSpec(n, "odoo_view_fields", map[string]string{"name": strings.Join(append(fieldPath, a["name"]), "."), "line": fmt.Sprint(line)})
				}
				if f.tag == "button" && a["type"] == "object" {
					appendOdooSpec(n, "odoo_buttons", map[string]string{"name": strings.Join(append(fieldPath, a["name"]), "."), "line": fmt.Sprint(line)})
				}
				for _, key := range []string{"model", "res_model"} {
					if f.tag != "record" {
						odooRef(n, "model", a[key], key, line)
					}
				}
				if f.tag == "function" {
					odooRef(n, "method", a["model"]+"."+a["name"], "function", line)
				}
			}
			stack = append(stack, f)
		case xml.CharData:
			if len(stack) > 0 {
				stack[len(stack)-1].text.Write(t)
			}
		case xml.EndElement:
			if len(stack) == 0 {
				continue
			}
			f := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if f.owner == nil {
				continue
			}
			f.owner.EndLine = line
			text := strings.TrimSpace(f.text.String())
			if f.tag == "field" && len(stack) > 0 && stack[len(stack)-1].tag == "record" {
				name := f.attrs["name"]
				appendOdooSpec(f.owner, "odoo_values", map[string]string{"name": name, "value": text, "eval": f.attrs["eval"], "ref": f.attrs["ref"]})
				switch name {
				case "model", "res_model":
					f.owner.Meta["odoo_view_model"] = text
					odooRef(f.owner, "model", text, name, f.line)
				case "report_name", "report_file":
					odooRef(f.owner, "template", text, name, f.line)
				case "tag":
					odooRef(f.owner, "registry", "actions/"+text, "client_action", f.line)
				}
			}
		}
	}
	return r, nil
}

func appendOdooSpec(n *graph.Node, key string, value map[string]string) {
	rows, _ := n.Meta[key].([]map[string]string)
	n.Meta[key] = append(rows, value)
}

func (e *OdooDataExtractor) extractCSV(r *parser.ExtractionResult, file string, src []byte) (*parser.ExtractionResult, error) {
	reader := csv.NewReader(bytes.NewReader(src))
	reader.FieldsPerRecord = -1
	header, err := reader.Read()
	if err != nil {
		return nil, err
	}
	if len(header) > 0 {
		header[0] = strings.TrimPrefix(header[0], "\ufeff")
	}
	// Odoo's CSV reader accepts bare quotes and variable-width records. Keep
	// header parsing strict: recovering a damaged schema could mislabel every
	// value in the file. Data-row irregularities are retained as diagnostics.
	reader.LazyQuotes = true
	model := strings.TrimSuffix(path.Base(file), ".csv")
	for {
		start := reader.InputOffset()
		row, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("Odoo CSV %s: %w", file, err)
		}
		// The Odoo loader ignores rows containing only empty cells. Whitespace
		// is still a value, matching Python's any(row) rather than TrimSpace.
		nonempty := false
		for _, value := range row {
			if value != "" {
				nonempty = true
				break
			}
		}
		if !nonempty {
			continue
		}
		line, _ := reader.FieldPos(0)
		values := map[string]string{}
		for i, key := range header[:min(len(header), len(row))] {
			values[key] = row[i]
		}
		var diagnostics []string
		if len(row) != len(header) {
			diagnostics = append(diagnostics, "column_count_mismatch")
		}
		// Re-read only this logical record strictly to flag quote recovery.
		// InputOffset includes quoted newlines, so physical lines are never
		// split into invented records. LazyQuotes preserves literal bare quotes.
		raw := src[start:reader.InputOffset()]
		if bytes.ContainsRune(raw, '"') {
			strict := csv.NewReader(bytes.NewReader(raw))
			strict.FieldsPerRecord = -1
			if _, err := strict.Read(); err != nil {
				diagnostics = append(diagnostics, "nonstandard_quotes")
			}
		}
		id := values["id"]
		if id == "" {
			id = fmt.Sprintf("%s@%d", model, line)
		}
		n := odooNode(r, file, id, graph.KindResource, line, e.Language())
		n.Meta["odoo_kind"], n.Meta["odoo_record_model"], n.Meta["odoo_spec"] = model, model, values
		if len(diagnostics) > 0 {
			n.Meta["odoo_csv_diagnostics"] = diagnostics
			n.Meta["odoo_csv_row"] = row
			n.Meta["odoo_csv_columns"] = map[string]int{"expected": len(header), "actual": len(row)}
		}
		if len(row) > len(header) {
			// Extra values have no declared field name; retain them without
			// guessing field names or manufacturing external-ID references.
			n.Meta["odoo_csv_extra"] = row[len(header):]
		} else if len(row) < len(header) {
			// Preserve positional mapping; never guess where an omitted cell
			// belonged or conflate an absent column with an explicitly empty one.
			n.Meta["odoo_csv_missing"] = header[len(row):]
		}
		if values["id"] != "" {
			n.Meta["odoo_xmlid"] = values["id"]
		}
		odooRef(n, "model", model, "record_model", line)
		for _, key := range header {
			if strings.HasSuffix(key, ":id") || strings.HasSuffix(key, "/id") {
				for _, ref := range strings.Split(values[key], ",") {
					odooRef(n, "xmlid", strings.TrimSpace(ref), key, line)
				}
			}
		}
	}
	return r, nil
}
