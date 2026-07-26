package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// editDaemonTool is the daemon-tool relay seam. It is indirected through a
// package var so tests can stub the daemon call (asserting the lowered
// tool + args) without a running daemon.
var editDaemonTool = requireDaemonTool

// Shared persistent flags for the edit group.
var (
	editIndex  string
	editFormat string
)

var editCmd = &cobra.Command{
	Use:   "edit",
	Short: "Edit safely & verify — the Layer-2 mutation verbs over the daemon",
	Long: `The edit verb group exposes the daemon's safe-edit surface as CLI verbs:
read editing context, verify a signature change, plan a multi-file edit,
speculatively preview / simulate an edit, apply an edit by string or symbol ID,
rename across files, check team guards, find the tests to run, run the full
change-contract pipeline, and safely delete a symbol.

Every subcommand is a thin shell over one MCP tool on the daemon that owns the
repo. The mutating verbs (apply / symbol / batch / safe-delete) write to your
working tree; preview / simulate / verify / plan / contract are read-only.

Requires a running daemon that tracks the repo.`,
	// A daemon-required / flag-validation error from a subcommand is
	// self-explanatory; don't bury it under the full usage dump.
	SilenceUsage: true,
}

func init() {
	editCmd.PersistentFlags().StringVar(&editIndex, "index", ".", "repository path the daemon must track")
	editCmd.PersistentFlags().StringVar(&editIndex, "repo", ".", "alias for --index")
	editCmd.PersistentFlags().StringVar(&editFormat, "format", "text", "output format: text|json")

	editCmd.AddCommand(editContextCmd)
	editCmd.AddCommand(editVerifyCmd)
	editCmd.AddCommand(editPlanCmd)
	editCmd.AddCommand(editPreviewCmd)
	editCmd.AddCommand(editSimulateCmd)
	editCmd.AddCommand(editBatchCmd)
	editCmd.AddCommand(editApplyCmd)
	editCmd.AddCommand(editSymbolCmd)
	editCmd.AddCommand(editRenameCmd)
	editCmd.AddCommand(editGuardsCmd)
	editCmd.AddCommand(editTestsCmd)
	editCmd.AddCommand(editContractCmd)
	editCmd.AddCommand(editSafeDeleteCmd)

	rootCmd.AddCommand(editCmd)
}

// runEditTool calls the daemon tool and renders the result. With --format json
// (or as the fallback for any shape) it pretty-prints the JSON; otherwise it
// runs the supplied text renderer. A nil renderer falls through to JSON.
func runEditTool(cmd *cobra.Command, tool string, args map[string]any, text func(*cobra.Command, json.RawMessage) error) error {
	raw, err := editDaemonTool(editIndex, tool, args)
	if err != nil {
		return err
	}
	if editFormat == "json" || text == nil {
		return emitDaemonJSON(cmd, raw)
	}
	return text(cmd, raw)
}

// editChanges holds the repeatable --change sugar.
var editChanges []string

// --- edit context ----------------------------------------------------------

var (
	editContextDetail   string
	editContextCompress bool
)

var editContextCmd = &cobra.Command{
	Use:   "context <file>",
	Short: "Show the editing context for a file before changing it",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		toolArgs := map[string]any{"path": args[0]}
		if editContextDetail != "" {
			toolArgs["detail"] = editContextDetail
		}
		if editContextCompress {
			toolArgs["compress_bodies"] = true
		}
		return runEditTool(cmd, "get_editing_context", toolArgs, nil)
	},
}

// --- edit verify -----------------------------------------------------------

var (
	editVerifyChangesInline string
	editVerifyChangesFile   string
	editVerifyCompact       bool
)

var editVerifyCmd = &cobra.Command{
	Use:   "verify",
	Short: "Verify proposed signature changes against callers & implementors",
	Long: `Checks every caller and interface implementor for contract violations
before a refactor. Build the changes array with repeatable --change sugar:

  gortex edit verify --change 'pkg/foo.go::Bar=func(x int) error'

Each --change is 'symbol_id=new_signature' (split on the first =). Alternatively
pass the raw JSON array via --changes '<json>', --changes-file <path>, or
--changes - (stdin).`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		changes, err := buildVerifyChanges(cmd, editChanges, editVerifyChangesInline, editVerifyChangesFile)
		if err != nil {
			return err
		}
		toolArgs := map[string]any{"changes": string(changes)}
		if editVerifyCompact {
			toolArgs["compact"] = true
		}
		return runEditTool(cmd, "verify_change", toolArgs, nil)
	},
}

// buildVerifyChanges builds verify_change's changes array (JSON). When one or
// more --change pairs are given they take precedence and are lowered to
// {symbol_id, new_signature} elements; otherwise the raw JSON array is resolved
// from --changes / --changes-file / stdin.
func buildVerifyChanges(cmd *cobra.Command, pairs []string, inline, file string) (json.RawMessage, error) {
	if len(pairs) > 0 {
		type change struct {
			SymbolID     string `json:"symbol_id"`
			NewSignature string `json:"new_signature"`
		}
		arr := make([]change, 0, len(pairs))
		for _, p := range pairs {
			eq := strings.Index(p, "=")
			if eq <= 0 {
				return nil, fmt.Errorf("--change %q: expected 'symbol_id=new_signature'", p)
			}
			arr = append(arr, change{SymbolID: p[:eq], NewSignature: p[eq+1:]})
		}
		return json.Marshal(arr)
	}
	raw, err := readStructuredArg(inline, file, cmd.InOrStdin())
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, fmt.Errorf("no changes given — pass --change 'id=sig' (repeatable) or --changes / --changes-file / -")
	}
	return raw, nil
}

// --- edit plan -------------------------------------------------------------

var (
	editPlanIDs   string
	editPlanDepth int
)

var editPlanCmd = &cobra.Command{
	Use:   "plan",
	Short: "Dependency-ordered file/symbol edit plan for a set of symbols",
	RunE: func(cmd *cobra.Command, _ []string) error {
		if editPlanIDs == "" {
			return fmt.Errorf("--ids is required (comma-separated symbol IDs)")
		}
		return runEditTool(cmd, "get_edit_plan",
			map[string]any{"ids": editPlanIDs, "depth": editPlanDepth}, nil)
	},
}

// --- edit preview ----------------------------------------------------------

var (
	editPreviewWEInline    string
	editPreviewWEFile      string
	editPreviewNoDiag      bool
	editPreviewInheritOver bool
)

var editPreviewCmd = &cobra.Command{
	Use:   "preview",
	Short: "Speculatively apply one WorkspaceEdit and report the impact (disk untouched)",
	RunE: func(cmd *cobra.Command, _ []string) error {
		we, err := readStructuredArg(editPreviewWEInline, editPreviewWEFile, cmd.InOrStdin())
		if err != nil {
			return err
		}
		if we == nil {
			return fmt.Errorf("a WorkspaceEdit is required — pass --workspace-edit '<json>', --workspace-edit-file <path>, or --workspace-edit -")
		}
		toolArgs := map[string]any{"workspace_edit": string(we)}
		// diagnostics defaults to true; --no-diagnostics turns it off.
		if editPreviewNoDiag {
			toolArgs["diagnostics"] = false
		}
		if editPreviewInheritOver {
			toolArgs["inherit_overlay"] = true
		}
		return runEditTool(cmd, "preview_edit", toolArgs, nil)
	},
}

// --- edit simulate ---------------------------------------------------------

var (
	editSimulateStepsInline string
	editSimulateStepsFile   string
	editSimulateKeep        bool
	editSimulateNoStop      bool
	editSimulateInheritOver bool
)

var editSimulateCmd = &cobra.Command{
	Use:   "simulate",
	Short: "Apply an ordered chain of WorkspaceEdits to a shadow view and report per-step impact",
	RunE: func(cmd *cobra.Command, _ []string) error {
		steps, err := readStructuredArg(editSimulateStepsInline, editSimulateStepsFile, cmd.InOrStdin())
		if err != nil {
			return err
		}
		if steps == nil {
			return fmt.Errorf("a steps array is required — pass --steps '<json>', --steps-file <path>, or --steps -")
		}
		toolArgs := map[string]any{"steps": string(steps)}
		if editSimulateKeep {
			toolArgs["keep"] = true
		}
		// stop_on_error defaults to true; --no-stop-on-error turns it off.
		if editSimulateNoStop {
			toolArgs["stop_on_error"] = false
		}
		if editSimulateInheritOver {
			toolArgs["inherit_overlay"] = true
		}
		return runEditTool(cmd, "simulate_chain", toolArgs, nil)
	},
}

// --- edit batch ------------------------------------------------------------

var (
	editBatchInline  string
	editBatchFile    string
	editBatchDryRun  bool
	editBatchCompact bool
)

var editBatchCmd = &cobra.Command{
	Use:   "batch",
	Short: "Apply multiple edits atomically in dependency order",
	RunE: func(cmd *cobra.Command, _ []string) error {
		edits, err := readStructuredArg(editBatchInline, editBatchFile, cmd.InOrStdin())
		if err != nil {
			return err
		}
		if edits == nil {
			return fmt.Errorf("an edits array is required — pass --edits '<json>', --edits-file <path>, or --edits -")
		}
		toolArgs := map[string]any{"edits": string(edits)}
		if editBatchDryRun {
			toolArgs["dry_run"] = true
		}
		if editBatchCompact {
			toolArgs["compact"] = true
		}
		return runEditTool(cmd, "batch_edit", toolArgs, nil)
	},
}

// --- edit apply ------------------------------------------------------------

var (
	editApplyOld        string
	editApplyNew        string
	editApplyReplaceAll bool
	editApplyDryRun     bool
	editApplyAllowParse bool
	editApplyExpected   int
)

var editApplyCmd = &cobra.Command{
	Use:   "apply <file>",
	Short: "Edit a file by exact string replacement (edit_file)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if !cmd.Flags().Changed("old") || !cmd.Flags().Changed("new") {
			return fmt.Errorf("--old and --new are required")
		}
		toolArgs := map[string]any{
			"path":       args[0],
			"old_string": editApplyOld,
			"new_string": editApplyNew,
		}
		if editApplyReplaceAll {
			toolArgs["replace_all"] = true
		}
		if editApplyDryRun {
			toolArgs["dry_run"] = true
		}
		if editApplyAllowParse {
			toolArgs["allow_parse_errors"] = true
		}
		if editApplyExpected > 0 {
			toolArgs["expected_occurrences"] = editApplyExpected
		}
		return runEditTool(cmd, "edit_file", toolArgs, nil)
	},
}

// --- edit symbol -----------------------------------------------------------

var (
	editSymbolOld    string
	editSymbolNew    string
	editSymbolDryRun bool
)

var editSymbolCmd = &cobra.Command{
	Use:   "symbol <id>",
	Short: "Edit a symbol's source by ID (edit_symbol)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if !cmd.Flags().Changed("old") || !cmd.Flags().Changed("new") {
			return fmt.Errorf("--old and --new are required")
		}
		toolArgs := map[string]any{
			"id":         args[0],
			"old_source": editSymbolOld,
			"new_source": editSymbolNew,
		}
		if editSymbolDryRun {
			toolArgs["dry_run"] = true
		}
		return runEditTool(cmd, "edit_symbol", toolArgs, nil)
	},
}

// --- edit rename -----------------------------------------------------------

var editRenameTo string

var editRenameCmd = &cobra.Command{
	Use:   "rename <id>",
	Short: "Plan a coordinated cross-file rename (plan-only, never writes)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if editRenameTo == "" {
			return fmt.Errorf("--to is required (the new name)")
		}
		return runEditTool(cmd, "rename_symbol",
			map[string]any{"id": args[0], "new_name": editRenameTo}, nil)
	},
}

// --- edit guards -----------------------------------------------------------

var (
	editGuardsIDs     string
	editGuardsCompact bool
)

var editGuardsCmd = &cobra.Command{
	Use:   "guards",
	Short: "Evaluate team guard rules against a set of changed symbols",
	RunE: func(cmd *cobra.Command, _ []string) error {
		if editGuardsIDs == "" {
			return fmt.Errorf("--ids is required (comma-separated symbol IDs)")
		}
		toolArgs := map[string]any{"ids": editGuardsIDs}
		if editGuardsCompact {
			toolArgs["compact"] = true
		}
		return runEditTool(cmd, "check_guards", toolArgs, nil)
	},
}

// --- edit tests ------------------------------------------------------------

var (
	editTestsIDs     string
	editTestsDepth   int
	editTestsCompact bool
)

var editTestsCmd = &cobra.Command{
	Use:   "tests",
	Short: "Find the test files & functions that exercise changed symbols",
	RunE: func(cmd *cobra.Command, _ []string) error {
		if editTestsIDs == "" {
			return fmt.Errorf("--ids is required (comma-separated symbol IDs)")
		}
		toolArgs := map[string]any{"ids": editTestsIDs, "depth": editTestsDepth}
		if editTestsCompact {
			toolArgs["compact"] = true
		}
		return runEditTool(cmd, "get_test_targets", toolArgs, nil)
	},
}

// --- edit contract ---------------------------------------------------------

var (
	editContractSource    string
	editContractLens      string
	editContractRiskGate  bool
	editContractAck       bool
	editContractBase      string
	editContractWEInline  string
	editContractWEFile    string
	editContractSymbols   string
	editContractRanges    string
	editContractRangeFile string
	editContractPath      string
	editContractStartLine int
	editContractEndLine   int
)

var editContractCmd = &cobra.Command{
	Use:   "contract",
	Short: "Run a change through the full change-contract pipeline (one verdict)",
	Long: `Lowers a change source to a changed-symbol set, predicts its blast radius,
evaluates the guard / architecture rules, scores risk, classifies, and emits one
verdict {allow|warn|refuse}. Sources (--source): auto (default), edit, diff,
symbols, or ranges.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		toolArgs := map[string]any{}
		if editContractSource != "" {
			toolArgs["source"] = editContractSource
		}
		if editContractLens != "" {
			toolArgs["lens"] = editContractLens
		}
		if editContractRiskGate {
			toolArgs["risk_gate"] = true
		}
		if editContractAck {
			toolArgs["ack"] = true
		}
		if editContractBase != "" {
			toolArgs["base"] = editContractBase
		}
		if editContractSymbols != "" {
			toolArgs["symbols"] = editContractSymbols
		}
		if editContractPath != "" {
			toolArgs["path"] = editContractPath
		}
		if cmd.Flags().Changed("start-line") {
			toolArgs["start_line"] = editContractStartLine
		}
		if cmd.Flags().Changed("end-line") {
			toolArgs["end_line"] = editContractEndLine
		}
		// workspace_edit (source=edit) — JSON via inline / file / stdin.
		we, err := readStructuredArg(editContractWEInline, editContractWEFile, cmd.InOrStdin())
		if err != nil {
			return err
		}
		if we != nil {
			toolArgs["workspace_edit"] = string(we)
		}
		// ranges (source=ranges) — JSON via inline / file / stdin.
		ranges, err := readStructuredArg(editContractRanges, editContractRangeFile, cmd.InOrStdin())
		if err != nil {
			return err
		}
		if ranges != nil {
			toolArgs["ranges"] = string(ranges)
		}
		return runEditTool(cmd, "change_contract", toolArgs, nil)
	},
}

// --- edit safe-delete ------------------------------------------------------

var (
	editSafeDeleteApply     bool
	editSafeDeleteForce     bool
	editSafeDeleteCascade   string
	editSafeDeleteIntoTests bool
	editSafeDeletePropagate bool
)

var editSafeDeleteCmd = &cobra.Command{
	Use:   "safe-delete <id>",
	Short: "Delete a symbol with a graph-aware safety gate (dry run by default)",
	Long: `Computes referencing edges first and refuses the delete if any exist
(unless --force). Runs as a DRY RUN by default — pass --apply to commit. Use
--cascade preview|apply to also handle the transitive orphan closure, and
--propagate to patch surviving call sites instead of refusing.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		toolArgs := map[string]any{
			"id": args[0],
			// dry_run defaults to true; --apply flips it to a real delete.
			"dry_run": !editSafeDeleteApply,
		}
		if editSafeDeleteForce {
			toolArgs["force"] = true
		}
		if editSafeDeleteCascade != "" {
			toolArgs["cascade"] = editSafeDeleteCascade
		}
		if editSafeDeleteIntoTests {
			toolArgs["cascade_into_tests"] = true
		}
		if editSafeDeletePropagate {
			toolArgs["propagate"] = true
		}
		if !editSafeDeleteApply {
			fmt.Fprintln(cmd.ErrOrStderr(), "note: dry run — nothing is deleted. Re-run with --apply to commit.")
		} else {
			fmt.Fprintln(cmd.ErrOrStderr(), "note: --apply set — this deletes the symbol from disk.")
		}
		return runEditTool(cmd, "safe_delete_symbol", toolArgs, nil)
	},
}

func init() {
	// context
	editContextCmd.Flags().StringVar(&editContextDetail, "detail", "", "detail level: brief|full")
	editContextCmd.Flags().BoolVar(&editContextCompress, "compress", false, "compress function bodies to stubs (compress_bodies)")

	// verify
	editVerifyCmd.Flags().StringArrayVar(&editChanges, "change", nil, "a 'symbol_id=new_signature' change (repeatable); builds the changes array")
	editVerifyCmd.Flags().StringVar(&editVerifyChangesInline, "changes", "", "raw changes JSON array, or \"-\" to read from stdin")
	editVerifyCmd.Flags().StringVar(&editVerifyChangesFile, "changes-file", "", "read the changes JSON array from a file")
	editVerifyCmd.Flags().BoolVar(&editVerifyCompact, "compact", false, "one-line-per-violation text output")

	// plan
	editPlanCmd.Flags().StringVar(&editPlanIDs, "ids", "", "comma-separated symbol IDs to change")
	editPlanCmd.Flags().IntVar(&editPlanDepth, "depth", 3, "dependent traversal depth")

	// preview
	editPreviewCmd.Flags().StringVar(&editPreviewWEInline, "workspace-edit", "", "LSP WorkspaceEdit JSON, or \"-\" to read from stdin")
	editPreviewCmd.Flags().StringVar(&editPreviewWEFile, "workspace-edit-file", "", "read the WorkspaceEdit JSON from a file")
	editPreviewCmd.Flags().BoolVar(&editPreviewNoDiag, "no-diagnostics", false, "skip the LSP diagnostics round-trip (diagnostics default on)")
	editPreviewCmd.Flags().BoolVar(&editPreviewInheritOver, "inherit-overlay", false, "apply on top of the caller's current overlay")

	// simulate
	editSimulateCmd.Flags().StringVar(&editSimulateStepsInline, "steps", "", "JSON array of WorkspaceEdits, or \"-\" to read from stdin")
	editSimulateCmd.Flags().StringVar(&editSimulateStepsFile, "steps-file", "", "read the steps JSON array from a file")
	editSimulateCmd.Flags().BoolVar(&editSimulateKeep, "keep", false, "persist the final simulated state as an overlay session")
	editSimulateCmd.Flags().BoolVar(&editSimulateNoStop, "no-stop-on-error", false, "evaluate the whole chain even after an error (stop_on_error default on)")
	editSimulateCmd.Flags().BoolVar(&editSimulateInheritOver, "inherit-overlay", false, "start from the caller's current overlay")

	// batch
	editBatchCmd.Flags().StringVar(&editBatchInline, "edits", "", "JSON array of edit operations, or \"-\" to read from stdin")
	editBatchCmd.Flags().StringVar(&editBatchFile, "edits-file", "", "read the edits JSON array from a file")
	editBatchCmd.Flags().BoolVar(&editBatchDryRun, "dry-run", false, "return the dependency-ordered plan without applying")
	editBatchCmd.Flags().BoolVar(&editBatchCompact, "compact", false, "one-line-per-edit summary")

	// apply
	editApplyCmd.Flags().StringVar(&editApplyOld, "old", "", "exact text to replace (old_string)")
	editApplyCmd.Flags().StringVar(&editApplyNew, "new", "", "replacement text (new_string)")
	editApplyCmd.Flags().BoolVar(&editApplyReplaceAll, "replace-all", false, "replace every occurrence instead of requiring uniqueness")
	editApplyCmd.Flags().BoolVar(&editApplyDryRun, "dry-run", false, "report what would change without writing")
	editApplyCmd.Flags().BoolVar(&editApplyAllowParse, "allow-parse-errors", false, "bypass the pre-write parse gate")
	editApplyCmd.Flags().IntVar(&editApplyExpected, "expected", 0, "refuse unless old_string matches exactly N locations (0 disables)")

	// symbol
	editSymbolCmd.Flags().StringVar(&editSymbolOld, "old", "", "exact source fragment to replace (old_source)")
	editSymbolCmd.Flags().StringVar(&editSymbolNew, "new", "", "replacement source fragment (new_source)")
	editSymbolCmd.Flags().BoolVar(&editSymbolDryRun, "dry-run", false, "validate and preview the diff without writing")

	// rename
	editRenameCmd.Flags().StringVar(&editRenameTo, "to", "", "new name for the symbol (new_name)")

	// guards
	editGuardsCmd.Flags().StringVar(&editGuardsIDs, "ids", "", "comma-separated changed symbol IDs")
	editGuardsCmd.Flags().BoolVar(&editGuardsCompact, "compact", false, "one-line-per-rule text output")

	// tests
	editTestsCmd.Flags().StringVar(&editTestsIDs, "ids", "", "comma-separated changed symbol IDs")
	editTestsCmd.Flags().IntVar(&editTestsDepth, "depth", 3, "caller traversal depth")
	editTestsCmd.Flags().BoolVar(&editTestsCompact, "compact", false, "one-line-per-test-file text output")

	// contract
	editContractCmd.Flags().StringVar(&editContractSource, "source", "", "change source: auto|diff|edit|symbols|ranges")
	editContractCmd.Flags().StringVar(&editContractLens, "lens", "", "analysis lens (e.g. api)")
	editContractCmd.Flags().BoolVar(&editContractRiskGate, "risk-gate", false, "require an impact-review ack for load-bearing symbols")
	editContractCmd.Flags().BoolVar(&editContractAck, "ack", false, "record a risk-gate acknowledgement instead of emitting a verdict")
	editContractCmd.Flags().StringVar(&editContractBase, "base", "", "base ref for a compare-scope diff (implies source=diff scope=compare)")
	editContractCmd.Flags().StringVar(&editContractWEInline, "workspace-edit", "", "source=edit: LSP WorkspaceEdit JSON, or \"-\" for stdin")
	editContractCmd.Flags().StringVar(&editContractWEFile, "workspace-edit-file", "", "source=edit: read the WorkspaceEdit JSON from a file")
	editContractCmd.Flags().StringVar(&editContractSymbols, "symbols", "", "source=symbols: comma-separated symbol IDs")
	editContractCmd.Flags().StringVar(&editContractRanges, "ranges", "", "source=ranges: JSON array of {file,start_line,end_line}, or \"-\" for stdin")
	editContractCmd.Flags().StringVar(&editContractRangeFile, "ranges-file", "", "source=ranges: read the ranges JSON from a file")
	editContractCmd.Flags().StringVar(&editContractPath, "path", "", "source=ranges single-file form: the file whose range to lower")
	editContractCmd.Flags().IntVar(&editContractStartLine, "start-line", 0, "1-based start line for the single-file ranges form")
	editContractCmd.Flags().IntVar(&editContractEndLine, "end-line", 0, "1-based end line for the single-file ranges form")

	// safe-delete
	editSafeDeleteCmd.Flags().BoolVar(&editSafeDeleteApply, "apply", false, "commit the delete (default is a dry run)")
	editSafeDeleteCmd.Flags().BoolVar(&editSafeDeleteForce, "force", false, "bypass the referencing-edge safety check")
	editSafeDeleteCmd.Flags().StringVar(&editSafeDeleteCascade, "cascade", "", "orphan propagation mode: off|preview|apply")
	editSafeDeleteCmd.Flags().BoolVar(&editSafeDeleteIntoTests, "cascade-into-tests", false, "let test-only references be eligible for the cascade closure")
	editSafeDeleteCmd.Flags().BoolVar(&editSafeDeletePropagate, "propagate", false, "patch surviving call sites instead of refusing")
}
