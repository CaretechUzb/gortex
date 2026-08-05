package main

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

func emitInitDryRunIntake(cmd *cobra.Command, root string) error {
	cfg, err := config.Load("")
	if err != nil {
		cfg = &config.Config{}
	}

	// The intake walk only classifies files against the admission gates —
	// it parses nothing and writes nothing — so it runs off the config and
	// the extension registry, with no graph store behind it.
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)

	manifest, err := indexer.DryRunIntake(context.Background(), root, cfg.Index, reg, zap.NewNop())
	if err != nil {
		return err
	}
	manifest.RepoRef = gitRepoRef(root)

	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(manifest)
}

func gitRepoRef(root string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	headBytes, err := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return "unknown"
	}
	head := strings.TrimSpace(string(headBytes))
	if head == "" {
		head = "unknown"
	}

	statusBytes, err := exec.CommandContext(ctx, "git", "-C", root, "status", "--porcelain").Output()
	if err != nil {
		return head
	}
	if strings.TrimSpace(string(statusBytes)) != "" {
		return head + "+dirty"
	}
	return head
}
