package parser

import (
	"bytes"
	"encoding/csv"
	"encoding/xml"
	"path/filepath"
	"strings"
)

func hasOdooXMLMarkers(src []byte) bool {
	d := xml.NewDecoder(bytes.NewReader(src))
	for {
		t, err := d.Token()
		if err != nil {
			return false
		}
		if start, ok := t.(xml.StartElement); ok {
			if start.Name.Space != "" {
				return false
			}
			switch start.Name.Local {
			case "odoo", "openerp":
				return true
			case "templates":
				return bytes.Contains(src, []byte("t-name=")) || bytes.Contains(src, []byte("t-inherit=")) || bytes.Contains(src, []byte("t-extend="))
			}
			return false
		}
	}
}

func hasOdooCSVMarkers(file string, src []byte) bool {
	base := strings.TrimSuffix(filepath.Base(file), ".csv")
	// Odoo's import data convention names the file after a dotted model.
	// Require the external-ID column as well; ordinary CSV stays generic.
	if !strings.Contains(base, ".") {
		return false
	}
	r := csv.NewReader(bytes.NewReader(src))
	header, err := r.Read()
	if err != nil {
		return false
	}
	for _, key := range header {
		if strings.TrimPrefix(key, "\ufeff") == "id" {
			return true
		}
	}
	return false
}
