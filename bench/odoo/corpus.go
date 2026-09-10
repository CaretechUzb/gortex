package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/resolver"
)

type referenceCounts struct {
	Total   int `json:"total"`
	Bound   int `json:"bound"`
	Unbound int `json:"unbound"`
}
type corpusReport struct {
	Roots              []string                    `json:"roots"`
	SourceSHA256       string                      `json:"source_sha256"`
	Files              int                         `json:"files"`
	Bytes              int64                       `json:"bytes"`
	SkippedDirectories int                         `json:"skipped_directories"`
	Symlinks           int                         `json:"symlinks"`
	OversizedFiles     int                         `json:"oversized_files"`
	Truncated          bool                        `json:"truncated"`
	Errors             int                         `json:"errors"`
	ErrorSamples       []string                    `json:"error_samples,omitempty"`
	CSVRecoveryRows    int                         `json:"csv_recovery_rows"`
	CSVRecoverySamples []string                    `json:"csv_recovery_samples,omitempty"`
	Nodes              int                         `json:"nodes"`
	OdooNodes          int                         `json:"odoo_nodes"`
	Kinds              map[string]int              `json:"declaration_kinds"`
	Relations          map[string]int              `json:"relations"`
	References         map[string]*referenceCounts `json:"raw_references_by_relation"`
	UnboundSamples     []string                    `json:"unbound_samples,omitempty"`
	InvariantErrors    []string                    `json:"invariant_errors,omitempty"`
	HTTPProviders      int                         `json:"http_providers"`
	ExtractionMS       float64                     `json:"extraction_ms"`
	SynthesisMS        float64                     `json:"synthesis_ms"`
}

func corpusCandidate(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".py", ".xml", ".csv", ".js", ".ts", ".tsx", ".jsx":
		return true
	}
	return strings.Contains(filepath.ToSlash(path), "/static/")
}
func excludedDirectory(name string) bool {
	return strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".worktrees") || name == "node_modules" || name == "__pycache__" || name == "venv" || name == "env"
}
func auditCorpus(roots []string, maxFiles int, maxBytes int64) (corpusReport, error) {
	r := corpusReport{Kinds: map[string]int{}, Relations: map[string]int{}, References: map[string]*referenceCounts{}}
	// Reject overlap rather than indexing the same declarations twice. Resolve
	// root symlinks once; never follow symlinks discovered below these roots.
	for _, root := range roots {
		abs, err := filepath.Abs(root)
		if err != nil {
			return r, err
		}
		abs, err = filepath.EvalSymlinks(abs)
		if err != nil {
			return r, err
		}
		stat, err := os.Stat(abs)
		if err != nil {
			return r, err
		}
		if !stat.IsDir() {
			return r, fmt.Errorf("corpus root is not a directory: %s", root)
		}
		for _, prior := range r.Roots {
			if abs == prior || strings.HasPrefix(abs, prior+string(filepath.Separator)) || strings.HasPrefix(prior, abs+string(filepath.Separator)) {
				return r, fmt.Errorf("overlapping corpus roots: %s and %s", prior, abs)
			}
		}
		r.Roots = append(r.Roots, abs)
	}
	p := newPipeline()
	digest := sha256.New()
	start := time.Now()
	recordError := func(path string, err error) {
		r.Errors++
		if len(r.ErrorSamples) < 50 {
			r.ErrorSamples = append(r.ErrorSamples, path+": "+err.Error())
		}
	}
	for i, root := range r.Roots {
		repo := fmt.Sprintf("corpus%d", i)
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				recordError(path, walkErr)
				return nil
			}
			if entry.Type()&os.ModeSymlink != 0 {
				r.Symlinks++
				return nil
			}
			if entry.IsDir() {
				if path != root && excludedDirectory(entry.Name()) {
					r.SkippedDirectories++
					return filepath.SkipDir
				}
				return nil
			}
			if !entry.Type().IsRegular() || !corpusCandidate(path) {
				return nil
			}
			if r.Files >= maxFiles {
				r.Truncated = true
				return filepath.SkipAll
			}
			r.Files++
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			id := repo + "/" + filepath.Base(root) + "/" + filepath.ToSlash(relative)
			info, err := entry.Info()
			if err != nil {
				recordError(id, err)
				return nil
			}
			if info.Size() > maxBytes {
				r.OversizedFiles++
				if len(r.ErrorSamples) < 50 {
					r.ErrorSamples = append(r.ErrorSamples, id+": exceeds max-file-bytes")
				}
				return nil
			}
			f, err := os.Open(path)
			if err != nil {
				recordError(id, err)
				return nil
			}
			data, readErr := io.ReadAll(io.LimitReader(f, maxBytes+1))
			closeErr := f.Close()
			if readErr != nil {
				recordError(id, readErr)
				return nil
			}
			if closeErr != nil {
				recordError(id, closeErr)
				return nil
			}
			if int64(len(data)) > maxBytes {
				r.OversizedFiles++
				return nil
			}
			r.Bytes += int64(len(data))
			// Length framing avoids concatenation ambiguities in corpus fingerprints.
			fmt.Fprintf(digest, "%d:%s:%d:", len(id), id, len(data))
			digest.Write(data)
			if err := p.extract(sourceFile{Path: id, Source: string(data), Repo: repo, Workspace: "corpus"}); err != nil {
				recordError(id, err)
			}
			return nil
		})
		if err != nil {
			return r, err
		}
		if r.Truncated {
			break
		}
	}
	r.ExtractionMS = float64(time.Since(start).Microseconds()) / 1000
	r.SourceSHA256 = hex.EncodeToString(digest.Sum(nil))
	start = time.Now()
	p.synthesize()
	r.SynthesisMS = float64(time.Since(start).Microseconds()) / 1000
	r.InvariantErrors = invariants(p)
	nodes := p.graph.AllNodes()
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	r.Nodes = len(nodes)
	for _, n := range nodes {
		if n.Meta["framework"] != "odoo" {
			continue
		}
		r.OdooNodes++
		if diagnostics, ok := n.Meta["odoo_csv_diagnostics"].([]any); ok && len(diagnostics) > 0 {
			r.CSVRecoveryRows++
			if len(r.CSVRecoverySamples) < 50 {
				r.CSVRecoverySamples = append(r.CSVRecoverySamples, fmt.Sprintf("%s:%d %s", n.ID, n.StartLine, canonical(diagnostics)))
			}
		}
		if kind, ok := n.Meta["odoo_kind"].(string); ok {
			r.Kinds[kind]++
		}
		bound := map[string]bool{}
		for _, e := range p.graph.GetOutEdges(n.ID) {
			if e.Meta[resolver.MetaSynthesizedBy] != resolver.SynthOdoo {
				continue
			}
			relation, _ := e.Meta["odoo_relation"].(string)
			key, _ := e.Meta["odoo_key"].(string)
			r.Relations[relation]++
			bound[fmt.Sprintf("%s\x00%s\x00%d", relation, key, e.Line)] = true
		}
		var refs []struct {
			Name     string `json:"name"`
			Relation string `json:"relation"`
			Line     int    `json:"line"`
		}
		raw, _ := json.Marshal(n.Meta["odoo_refs"])
		_ = json.Unmarshal(raw, &refs)
		for _, ref := range refs {
			count := r.References[ref.Relation]
			if count == nil {
				count = &referenceCounts{}
				r.References[ref.Relation] = count
			}
			count.Total++
			if bound[fmt.Sprintf("%s\x00%s\x00%d", ref.Relation, ref.Name, ref.Line)] {
				count.Bound++
			} else {
				count.Unbound++
				if len(r.UnboundSamples) < 50 {
					r.UnboundSamples = append(r.UnboundSamples, fmt.Sprintf("%s:%d %s %s", n.ID, ref.Line, ref.Relation, ref.Name))
				}
			}
		}
	}
	for _, c := range p.contracts {
		if c.Meta["framework"] == "odoo" {
			r.HTTPProviders++
		}
	}
	// A typo or an empty/non-Odoo source tree must not report a successful audit.
	if r.OdooNodes == 0 {
		recordError("corpus", fmt.Errorf("no Odoo nodes extracted"))
	}
	return r, nil
}
func renderCorpus(w io.Writer, r corpusReport) {
	fmt.Fprintf(w, "Corpus (unlabeled): %d files, %d Odoo nodes, %d HTTP contracts; %d errors, %d oversized, truncated=%t\n", r.Files, r.OdooNodes, r.HTTPProviders, r.Errors, r.OversizedFiles, r.Truncated)
	fmt.Fprintf(w, "Corpus timings: extraction %.1f ms, synthesis %.1f ms; source SHA256 %s\n", r.ExtractionMS, r.SynthesisMS, r.SourceSHA256)
	fmt.Fprintf(w, "CSV rows recovered with diagnostics: %d (source irregularities retained; not extraction failures)\n", r.CSVRecoveryRows)
	fmt.Fprintln(w, "Raw reference availability (not precision or recall; excludes derived view references):")
	keys := make([]string, 0, len(r.References))
	for k := range r.References {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		c := r.References[k]
		fmt.Fprintf(w, "  %-24s bound=%d unbound=%d total=%d\n", k, c.Bound, c.Unbound, c.Total)
	}
	for _, e := range r.ErrorSamples {
		fmt.Fprintln(w, "CORPUS:", e)
	}
}
