package contracts

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// lambdaResourceNode mirrors the KindType node the HCL language
// extractor emits for a resource block, which is what the Terraform
// contract's SymbolID anchors to.
func lambdaResourceNode(filePath, address string) *graph.Node {
	return &graph.Node{
		ID:       "hcl::infra::" + address,
		Kind:     graph.KindType,
		Name:     "resource." + address,
		FilePath: filePath,
		Language: "hcl",
		Meta:     map[string]any{"block_type": "resource", "tf_address": address},
	}
}

func TestTerraformExtractor_LambdaEnvironmentVariables(t *testing.T) {
	ext := &TerraformExtractor{}
	src := []byte(`resource "aws_lambda_function" "processor" {
  function_name = "processor"

  environment {
    variables = {
      MAX_BATCH_SIZE = "100"
      "QUEUE_URL"    = aws_sqs_queue.jobs.url
    }
  }
}
`)
	nodes := []*graph.Node{lambdaResourceNode("infra/lambda.tf", "aws_lambda_function.processor")}

	got := ext.Extract("infra/lambda.tf", src, nodes, nil)
	if len(got) != 2 {
		t.Fatalf("expected 2 provider contracts, got %d: %+v", len(got), got)
	}
	assertContract(t, got[0], "env::MAX_BATCH_SIZE", ContractEnv, RoleProvider)
	assertContract(t, got[1], "env::QUEUE_URL", ContractEnv, RoleProvider)

	first := got[0]
	if first.Confidence != 0.9 {
		t.Errorf("expected confidence 0.9 (attribute-inferred), got %v", first.Confidence)
	}
	if first.Line != 6 {
		t.Errorf("expected line 6, got %d", first.Line)
	}
	// A regression to findEnclosingSymbol would return "" here, since
	// HCL blocks are KindType rather than KindFunction/KindMethod.
	if first.SymbolID != "hcl::infra::aws_lambda_function.processor" {
		t.Errorf("expected SymbolID anchored to the Lambda resource node, got %q", first.SymbolID)
	}
	if first.Meta["source"] != "terraform" {
		t.Errorf("expected Meta[source]=terraform, got %v", first.Meta["source"])
	}
	if first.Meta["var"] != "MAX_BATCH_SIZE" {
		t.Errorf("expected Meta[var]=MAX_BATCH_SIZE, got %v", first.Meta["var"])
	}
	// The indexer stamps these centrally; extractors must leave them unset.
	if first.RepoPrefix != "" || first.WorkspaceID != "" || first.ProjectID != "" {
		t.Errorf("expected repo/workspace/project unset by the extractor, got %+v", first)
	}
}

func TestTerraformExtractor_PairsWithGetenvSameRepo(t *testing.T) {
	tf := &TerraformExtractor{}
	tfSrc := []byte(`resource "aws_lambda_function" "processor" {
  environment {
    variables = {
      MAX_BATCH_SIZE = "100"
    }
  }
}
`)
	goSrc := []byte(`package worker

func batchSize() string {
	return os.Getenv("MAX_BATCH_SIZE")
}
`)

	reg := NewRegistry()
	for _, c := range tf.Extract("infra/lambda.tf", tfSrc, nil, nil) {
		c.RepoPrefix = "svc"
		reg.Add(c)
	}
	for _, c := range (&EnvVarExtractor{}).Extract("worker/batch.go", goSrc, nil, nil) {
		c.RepoPrefix = "svc"
		reg.Add(c)
	}

	result := Match(reg)
	if len(result.Matched) != 1 {
		t.Fatalf("expected the Terraform provider to pair with os.Getenv; got %d matches, %d orphan providers, %d orphan consumers",
			len(result.Matched), len(result.OrphanProviders), len(result.OrphanConsumers))
	}
	m := result.Matched[0]
	if m.ContractID != "env::MAX_BATCH_SIZE" {
		t.Errorf("expected env::MAX_BATCH_SIZE, got %q", m.ContractID)
	}
	if m.Provider.FilePath != "infra/lambda.tf" || m.Consumer.FilePath != "worker/batch.go" {
		t.Errorf("unexpected pair: provider=%s consumer=%s", m.Provider.FilePath, m.Consumer.FilePath)
	}
	if m.CrossRepo {
		t.Errorf("expected CrossRepo=false within one repo, got %+v", m)
	}
}

func TestTerraformExtractor_PairsWithGetenvCrossRepo(t *testing.T) {
	tf := &TerraformExtractor{}
	tfSrc := []byte(`resource "aws_lambda_function" "processor" {
  environment {
    variables = {
      MAX_BATCH_SIZE = "100"
    }
  }
}
`)
	goSrc := []byte(`package worker

func batchSize() string {
	return os.Getenv("MAX_BATCH_SIZE")
}
`)

	// Repo A declares the value in Terraform; repo B reads it. Both sit
	// in one workspace, which is the boundary Match() pairs inside.
	reg := NewRegistry()
	for _, c := range tf.Extract("infra/lambda.tf", tfSrc, nil, nil) {
		c.RepoPrefix = "acme-infra"
		c.WorkspaceID = "acme"
		c.ProjectID = "acme"
		reg.Add(c)
	}
	for _, c := range (&EnvVarExtractor{}).Extract("worker/batch.go", goSrc, nil, nil) {
		c.RepoPrefix = "acme-worker"
		c.WorkspaceID = "acme"
		c.ProjectID = "acme"
		reg.Add(c)
	}

	result := Match(reg)
	if len(result.Matched) != 1 {
		t.Fatalf("expected 1 cross-repo match; got %d (%s)", len(result.Matched), summarisePairs(result))
	}
	m := result.Matched[0]
	if !m.CrossRepo {
		t.Fatalf("expected CrossRepo=true across acme-infra → acme-worker; got %+v", m)
	}
	if m.Provider.RepoPrefix != "acme-infra" || m.Consumer.RepoPrefix != "acme-worker" {
		t.Errorf("unexpected pair: provider=%s consumer=%s", m.Provider.RepoPrefix, m.Consumer.RepoPrefix)
	}
}

// Terraform config sources whose keys aren't structurally guaranteed to
// be environment variables stay graph-level only. Minting providers from
// them would assert a binding this extractor never verified.
func TestTerraformExtractor_NoProviderForVariableOrConfigMapBlocks(t *testing.T) {
	ext := &TerraformExtractor{}
	src := []byte(`variable "chunk_size" {
  default = 500
}

output "queue_url" {
  value = aws_sqs_queue.jobs.url
}

resource "kubernetes_config_map" "settings" {
  data = {
    MAX_BATCH_SIZE = "100"
  }
}

resource "aws_lambda_function" "anchor" {
  function_name = "anchor"
}
`)
	if got := ext.Extract("infra/main.tf", src, nil, nil); len(got) != 0 {
		t.Fatalf("expected no contracts from variable/output/config-map blocks, got %+v", got)
	}
}

func TestTerraformExtractor_IgnoresResourcesOutsideAllowlist(t *testing.T) {
	ext := &TerraformExtractor{}
	src := []byte(`resource "aws_instance" "web" {
  environment {
    variables = {
      MAX_BATCH_SIZE = "100"
    }
  }
}
`)
	if got := ext.Extract("infra/ec2.tf", src, nil, nil); len(got) != 0 {
		t.Fatalf("expected no contracts from a non-allowlisted resource, got %+v", got)
	}
}

func TestTerraformExtractor_SkipsNonLiteralVariableMaps(t *testing.T) {
	cases := map[string]string{
		"for expression": `variables = { for k, v in var.settings : k => v }`,
		"merge call":     `variables = merge(local.base_env, { EXTRA = "1" })`,
		"local map ref":  `variables = local.lambda_env`,
		"interpolated key": `variables = {
        "PREFIX_${var.stage}" = "1"
      }`,
	}
	for name, attr := range cases {
		t.Run(name, func(t *testing.T) {
			src := []byte(`resource "aws_lambda_function" "processor" {
  environment {
    ` + attr + `
  }
}
`)
			got := (&TerraformExtractor{}).Extract("infra/lambda.tf", src, nil, nil)
			if len(got) != 0 {
				t.Fatalf("expected no contracts from a non-enumerable variables map, got %+v", got)
			}
		})
	}
}

// merge(local.base, { EXTRA = "1" }) is skipped wholesale above; this
// pins that a Lambda whose map is partly literal doesn't leak the inline
// object's keys while dropping the rest, which would report a partial
// env set as if it were complete.
func TestTerraformExtractor_MixedLiteralAndDynamicLambdas(t *testing.T) {
	src := []byte(`resource "aws_lambda_function" "dynamic" {
  environment {
    variables = merge(local.base_env, { EXTRA = "1" })
  }
}

resource "aws_lambda_function" "literal" {
  environment {
    variables = {
      MAX_BATCH_SIZE = "100"
    }
  }
}
`)
	got := (&TerraformExtractor{}).Extract("infra/lambda.tf", src, nil, nil)
	if len(got) != 1 {
		t.Fatalf("expected only the literal Lambda's key, got %+v", got)
	}
	assertContract(t, got[0], "env::MAX_BATCH_SIZE", ContractEnv, RoleProvider)
	if got[0].Meta["tf_address"] != "aws_lambda_function.literal" {
		t.Errorf("expected the contract attributed to the literal Lambda, got %v", got[0].Meta["tf_address"])
	}
}

func TestTerraformExtractor_SupportedLanguages(t *testing.T) {
	langs := (&TerraformExtractor{}).SupportedLanguages()
	if len(langs) != 1 || langs[0] != "hcl" {
		t.Fatalf("expected the extractor to claim hcl only, got %v", langs)
	}
}

func TestTerraformExtractor_ExtractWithTreeMatchesExtract(t *testing.T) {
	src := []byte(`resource "aws_lambda_function" "processor" {
  environment {
    variables = {
      MAX_BATCH_SIZE = "100"
    }
  }
}
`)
	ext := &TerraformExtractor{}
	tree := parseHCLTree(src)
	defer tree.Release()

	withTree := ext.ExtractWithTree("infra/lambda.tf", src, nil, nil, tree)
	plain := ext.Extract("infra/lambda.tf", src, nil, nil)
	if len(withTree) != 1 || len(plain) != 1 {
		t.Fatalf("expected 1 contract from both paths, got %d and %d", len(withTree), len(plain))
	}
	if withTree[0].ID != plain[0].ID || withTree[0].Line != plain[0].Line {
		t.Errorf("tree-aware and lazy-parse paths disagree: %+v vs %+v", withTree[0], plain[0])
	}
}
