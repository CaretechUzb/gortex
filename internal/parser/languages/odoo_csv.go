package languages

import (
	"bytes"
	"encoding/csv"
	"io"
	"path"
	"regexp"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// OdooCSVExtractor indexes Odoo's CSV data files — the third declaration
// vocabulary of an addon, alongside Python models and XML records.
//
// Odoo loads a data file named after a model (`account.account.template.csv`,
// `ir.model.access.csv`) row by row, and the `id` column of every row is an
// external ID exactly as authoritative as a `<record id=>` in XML. Chart-of-
// accounts and access-rule data is written this way almost exclusively,
// because the rows are wide and repetitive and XML would be unreadable.
//
// Left unindexed, those rows are invisible while the XML that references
// them is not, so every `ref="lu_2020_account_421611"` resolves to nothing
// and the addon looks broken rather than merely CSV-shaped. Columns whose
// header ends in `:id` (or the legacy `/id`) are themselves external-ID
// references — a many2one written as a name rather than a database key — so
// the file both declares and consumes the same vocabulary.
//
// `.csv` is a generic extension, so like the Odoo XML extractor this one is
// gated on content: IsOdooCSV recognises the shape and Extract returns just
// the file node for anything else, leaving ordinary spreadsheets alone.
type OdooCSVExtractor struct{}

// NewOdooCSVExtractor constructs an OdooCSVExtractor.
func NewOdooCSVExtractor() *OdooCSVExtractor { return &OdooCSVExtractor{} }

func (e *OdooCSVExtractor) Language() string     { return "odoo_csv" }
func (e *OdooCSVExtractor) Extensions() []string { return []string{".csv"} }

// odooCSVMaxBytes caps what is parsed row by row. Odoo's own data files top
// out well under a megabyte; anything larger on a `.csv` extension is a
// dataset rather than an addon fixture, and minting a node per row of it
// would bloat the graph for no navigational gain.
const odooCSVMaxBytes = 4 << 20

// odooCSVModelRE matches the dotted lower-case shape of an Odoo model name,
// which is what a data file is named after.
var odooCSVModelRE = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z0-9_]+)+$`)

// IsOdooCSV reports whether src is an Odoo data CSV.
//
// Two conditions, both of them Odoo's own loading rules rather than
// heuristics. The file must be NAMED after a model, because that name is
// how Odoo decides which model to feed — a `.csv` that is not is simply
// never loaded as data. And its first header column must be `id`, which
// Odoo requires to name each row's external ID.
//
// Both are needed. `.csv` is a generic extension shared with every export
// and fixture in every language, and the module probe alone accepts any
// path at all: `odooModuleFromPath` falls back to the parent directory
// name, so a bare `reports/export.csv` would qualify as module "reports".
// Requiring the model name is what keeps a spreadsheet with an `id` column
// from becoming a node per row.
//
// Only the header line is scanned, so the probe stays cheap on large files.
func IsOdooCSV(filePath string, src []byte) bool {
	if odooModuleFromPath(filePath) == "" || odooCSVModel(filePath) == "" {
		return false
	}
	line := src
	if i := bytes.IndexAny(src, "\r\n"); i >= 0 {
		line = src[:i]
	}
	first := line
	if i := bytes.IndexByte(line, ','); i >= 0 {
		first = line[:i]
	}
	first = bytes.TrimPrefix(first, []byte("\xef\xbb\xbf")) // UTF-8 BOM
	return string(bytes.Trim(first, " \t\"'")) == "id"
}

// odooCSVModel derives the model a data file feeds from its base name.
//
// Odoo permits a `-suffix` variant so one model can be loaded from several
// files in the same module (`account.account.template-common.csv` beside
// `account.account.template-full.csv`), so the suffix is cut before the name
// is validated. A base name that is not a dotted model name — a hand-titled
// export, a test fixture — yields no model rather than a wrong one.
func odooCSVModel(filePath string) string {
	base := strings.TrimSuffix(path.Base(filePath), path.Ext(filePath))
	if i := strings.IndexByte(base, '-'); i >= 0 {
		base = base[:i]
	}
	if !odooCSVModelRE.MatchString(base) {
		return ""
	}
	return base
}

// Extract parses an Odoo data CSV. A file that is not one yields only the
// file node, so an ordinary spreadsheet degrades gracefully. A malformed
// row is skipped rather than failing the file — Odoo's own data files carry
// ragged rows, and losing the whole chart of accounts to one bad line would
// be far worse than losing the line.
func (e *OdooCSVExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	result := &parser.ExtractionResult{}
	fileNode := &graph.Node{
		ID:       filePath,
		Kind:     graph.KindFile,
		Name:     path.Base(filePath),
		FilePath: filePath,
		Language: "odoo_csv",
	}
	result.Nodes = append(result.Nodes, fileNode)

	if len(src) > odooCSVMaxBytes || !IsOdooCSV(filePath, src) {
		return result, nil
	}

	module := odooModuleFromPath(filePath)
	model := odooCSVModel(filePath)
	fileNode.Meta = map[string]any{"odoo_module": module, "odoo_model": model}

	r := csv.NewReader(bytes.NewReader(src))
	// Odoo data files are ragged by design: trailing optional columns are
	// simply absent on rows that do not set them.
	r.FieldsPerRecord = -1
	r.LazyQuotes = true

	header, err := r.Read()
	if err != nil {
		return result, nil
	}
	// refCols are the columns holding external-ID references rather than
	// scalar values; `:id` is the modern spelling, `/id` the legacy one.
	var refCols []int
	for i, h := range header {
		if h = strings.TrimSpace(h); strings.HasSuffix(h, ":id") || strings.HasSuffix(h, "/id") {
			refCols = append(refCols, i)
		}
	}

	seen := map[string]bool{}
	for {
		row, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			if _, ok := err.(*csv.ParseError); ok {
				continue // skip the bad row, keep the file
			}
			break
		}
		if len(row) == 0 || strings.TrimSpace(row[0]) == "" {
			continue
		}
		line, _ := r.FieldPos(0)
		nodeID := e.emitRow(result, fileNode, seen, filePath, module, model, strings.TrimSpace(row[0]), line)
		if nodeID == "" {
			continue
		}
		for _, i := range refCols {
			if i >= len(row) {
				continue
			}
			e.emitRefs(result, nodeID, filePath, module, row[i], line)
		}
	}
	return result, nil
}

// emitRow mints the record node for one CSV row and links the file to it.
func (e *OdooCSVExtractor) emitRow(result *parser.ExtractionResult, fileNode *graph.Node, seen map[string]bool, filePath, module, model, id string, line int) string {
	xmlID := odooQualifyXMLID(module, id)
	if xmlID == "" {
		return ""
	}
	nodeID := "odoo::record::" + xmlID
	if seen[nodeID] {
		return nodeID
	}
	seen[nodeID] = true
	meta := map[string]any{
		"odoo_xml_id":   xmlID,
		"resource_kind": "odoo_record",
		"odoo_module":   module,
	}
	if model != "" {
		meta["odoo_model"] = model
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: nodeID, Kind: graph.KindResource, Name: id, QualName: xmlID,
		FilePath: filePath, StartLine: line, EndLine: line,
		Language: "odoo_csv", Meta: meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileNode.ID, To: nodeID, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: line,
	})
	// The row configures a Python model, named by its `_name` string —
	// the same link `<record model=>` carries on the XML side.
	if model != "" {
		result.Edges = append(result.Edges, odooModelRef(nodeID, model, graph.EdgeReferences, filePath, line, "csv_model"))
	}
	return nodeID
}

// emitRefs links a row to the records a `:id` column names.
//
// A to-many column holds a comma-separated list rather than a single ID
// (`account_tag_6,account_tag_52`), so the cell is split before qualifying;
// treating it as one name would mint a reference no record can ever answer.
func (e *OdooCSVExtractor) emitRefs(result *parser.ExtractionResult, from, filePath, module, cell string, line int) {
	for _, ref := range strings.Split(cell, ",") {
		if ref = strings.TrimSpace(ref); ref == "" {
			continue
		}
		if xmlID := odooQualifyXMLID(module, ref); xmlID != "" {
			result.Edges = append(result.Edges, odooXMLIDRef(from, xmlID, graph.EdgeReferences, filePath, line, "csv_ref"))
		}
	}
}
