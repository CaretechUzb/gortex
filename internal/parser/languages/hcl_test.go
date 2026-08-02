package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestHCLExtractor_Blocks(t *testing.T) {
	src := []byte(`resource "aws_instance" "web" {
  ami           = "ami-12345"
  instance_type = "t2.micro"
}

variable "region" {
  default = "us-east-1"
}

output "instance_id" {
  value = aws_instance.web.id
}
`)
	e := NewHCLExtractor()
	result, err := e.Extract("main.tf", src)
	require.NoError(t, err)

	types := nodesOfKind(result.Nodes, graph.KindType)
	assert.GreaterOrEqual(t, len(types), 3, "should extract resource, variable, and output blocks")

	// Check that block names include labels.
	names := make(map[string]bool)
	for _, n := range types {
		names[n.Name] = true
	}
	assert.True(t, names["resource.aws_instance.web"], "should have resource.aws_instance.web")
	assert.True(t, names["variable.region"], "should have variable.region")
}

func TestHCLExtractor_ModuleAndData(t *testing.T) {
	src := []byte(`module "vpc" {
  source = "./modules/vpc"
}

data "aws_ami" "ubuntu" {
  most_recent = true
}
`)
	e := NewHCLExtractor()
	result, err := e.Extract("infra.tf", src)
	require.NoError(t, err)

	types := nodesOfKind(result.Nodes, graph.KindType)
	assert.GreaterOrEqual(t, len(types), 2, "should extract module and data blocks")
}

func TestHCLExtractor_References(t *testing.T) {
	src := []byte(`variable "region" {
  default = "us-east-1"
}

locals {
  name = "web-${var.region}"
}

data "aws_ami" "ubuntu" {
  most_recent = true
}

module "vpc" {
  source = "./modules/vpc"
}

resource "aws_instance" "web" {
  ami           = data.aws_ami.ubuntu.id
  instance_type = var.instance_type
  subnet_id     = module.vpc.subnet_ids[0]
  tags          = { Name = local.name }
}

output "ip" {
  value = aws_instance.web.private_ip
}
`)
	e := NewHCLExtractor()
	result, err := e.Extract("main.tf", src)
	require.NoError(t, err)

	// tf_address rides on each block's Meta.
	addrByName := map[string]string{}
	for _, n := range result.Nodes {
		if a, _ := n.Meta["tf_address"].(string); a != "" {
			addrByName[n.Name] = a
		}
	}
	assert.Equal(t, "aws_instance.web", addrByName["resource.aws_instance.web"])
	assert.Equal(t, "var.region", addrByName["variable.region"])
	assert.Equal(t, "data.aws_ami.ubuntu", addrByName["data.aws_ami.ubuntu"])
	assert.Equal(t, "module.vpc", addrByName["module.vpc"])

	// Each local declaration is its own addressable KindConstant node.
	var localName *graph.Node
	for _, n := range result.Nodes {
		if n.Kind == graph.KindConstant && n.Name == "local.name" {
			localName = n
		}
	}
	require.NotNil(t, localName, "locals { name = ... } should yield a local.name node")
	assert.Equal(t, "hcl::.::local.name", localName.ID)

	// Reference edges link the resource to everything it traverses.
	refs := map[string]bool{}
	for _, ed := range result.Edges {
		if ed.Kind == graph.EdgeReferences {
			refs[ed.From+" -> "+ed.To] = true
		}
	}
	web := "hcl::.::aws_instance.web"
	assert.True(t, refs[web+" -> hcl::.::data.aws_ami.ubuntu"], "web -> data.aws_ami.ubuntu")
	assert.True(t, refs[web+" -> hcl::.::var.instance_type"], "web -> var.instance_type")
	assert.True(t, refs[web+" -> hcl::.::module.vpc"], "web -> module.vpc")
	assert.True(t, refs[web+" -> hcl::.::local.name"], "web -> local.name")
	// The local's own value references the variable it interpolates.
	assert.True(t, refs["hcl::.::local.name -> hcl::.::var.region"], "local.name -> var.region")
	// The output references the resource.
	assert.True(t, refs["hcl::.::output.ip -> hcl::.::aws_instance.web"], "output.ip -> aws_instance.web")
	// Built-in scopes (count/each/self/path/terraform) never become refs.
	for k := range refs {
		assert.NotContains(t, k, "::count.")
		assert.NotContains(t, k, "::self.")
	}
}

func TestHCLExtractor_VariableAndOutputEmitConfigKeys(t *testing.T) {
	src := []byte(`variable "chunk_size" {
  default = 500
}

output "queue_url" {
  value = aws_sqs_queue.jobs.url
}
`)
	e := NewHCLExtractor()
	result, err := e.Extract("main.tf", src)
	require.NoError(t, err)

	nodes := nodesByID(result.Nodes)
	for _, tc := range []struct{ name, from, source string }{
		{"chunk_size", "hcl::.::var.chunk_size", "tf_variable"},
		{"queue_url", "hcl::.::output.queue_url", "tf_output"},
	} {
		id := "cfg::env::" + tc.name
		require.Contains(t, nodes, id, "missing config_key for %q", tc.name)
		assert.Equal(t, graph.KindConfigKey, nodes[id].Kind)
		assert.Equal(t, tc.name, nodes[id].Name)
		assert.Equal(t, "terraform", nodes[id].Meta["origin"])
		assert.Equal(t, tc.source, nodes[id].Meta["source"])
		assert.True(t, hasEdgeBetween(result.Edges, graph.EdgeUsesEnv, tc.from, id),
			"missing EdgeUsesEnv %s -> %s", tc.from, id)
	}
}

func TestHCLExtractor_LambdaEnvironmentVariables(t *testing.T) {
	src := []byte(`resource "aws_lambda_function" "processor" {
  function_name = "processor"

  environment {
    variables = {
      MAX_BATCH_SIZE = "100"
      "CHUNK_SIZE"   = "500"
    }
  }
}
`)
	e := NewHCLExtractor()
	result, err := e.Extract("lambda.tf", src)
	require.NoError(t, err)

	nodes := nodesByID(result.Nodes)
	resID := "hcl::.::aws_lambda_function.processor"
	require.Contains(t, nodes, resID, "resource block node should still be emitted")

	// Every declared key gets its own node — bare and quoted alike.
	for _, key := range []string{"MAX_BATCH_SIZE", "CHUNK_SIZE"} {
		id := "cfg::env::" + key
		require.Contains(t, nodes, id, "missing config_key for %q", key)
		assert.Equal(t, graph.KindConfigKey, nodes[id].Kind)
		assert.Equal(t, "env", nodes[id].Meta["source"])
		assert.Equal(t, "terraform", nodes[id].Meta["origin"])
		assert.True(t, hasEdgeBetween(result.Edges, graph.EdgeUsesEnv, resID, id),
			"missing EdgeUsesEnv %s -> %s", resID, id)
	}
}

func TestHCLExtractor_KubernetesProviderDataKeys(t *testing.T) {
	src := []byte(`resource "kubernetes_config_map" "app" {
  metadata {
    name = "app"
  }
  data = {
    LOG_LEVEL   = "debug"
    FEATURE_X   = "on"
  }
}

resource "kubernetes_secret" "app" {
  data = {
    API_TOKEN = "shhh"
  }
}
`)
	e := NewHCLExtractor()
	result, err := e.Extract("k8s.tf", src)
	require.NoError(t, err)

	nodes := nodesByID(result.Nodes)
	cmID := "hcl::.::kubernetes_config_map.app"
	secID := "hcl::.::kubernetes_secret.app"

	for _, tc := range []struct{ key, from, source string }{
		{"LOG_LEVEL", cmID, "k8s_cm"},
		{"FEATURE_X", cmID, "k8s_cm"},
		{"API_TOKEN", secID, "k8s_secret"},
	} {
		id := "cfg::env::" + tc.key
		require.Contains(t, nodes, id, "missing config_key for %q", tc.key)
		assert.Equal(t, graph.KindConfigKey, nodes[id].Kind)
		assert.Equal(t, tc.source, nodes[id].Meta["source"])
		assert.Equal(t, "terraform", nodes[id].Meta["origin"])
		assert.True(t, hasEdgeBetween(result.Edges, graph.EdgeUsesEnv, tc.from, id),
			"missing EdgeUsesEnv %s -> %s", tc.from, id)
	}
}

func TestHCLExtractor_ResourceOutsideAllowlistEmitsNoConfigKeys(t *testing.T) {
	src := []byte(`resource "aws_instance" "web" {
  ami = "ami-12345"

  tags = {
    Name = "web"
  }
}
`)
	e := NewHCLExtractor()
	result, err := e.Extract("main.tf", src)
	require.NoError(t, err)

	assert.Empty(t, nodesOfKind(result.Nodes, graph.KindConfigKey),
		"non-allowlisted resource types must not emit config keys")
	for _, ed := range result.Edges {
		assert.NotEqual(t, graph.EdgeUsesEnv, ed.Kind)
	}

	// The generic block node is unaffected.
	nodes := nodesByID(result.Nodes)
	require.Contains(t, nodes, "hcl::.::aws_instance.web")
	assert.Equal(t, graph.KindType, nodes["hcl::.::aws_instance.web"].Kind)
}

// Keys are only enumerable from a literal object. Anything computed —
// merge(), a for-expression, a reference to a local — must be skipped
// rather than guessed at, and a missing block must not panic.
func TestHCLExtractor_NonLiteralConfigMapsAreSkipped(t *testing.T) {
	src := []byte(`resource "aws_lambda_function" "merged" {
  environment {
    variables = merge(local.base_env, { SHOULD_NOT_APPEAR = "1" })
  }
}

resource "aws_lambda_function" "computed" {
  environment {
    variables = { for k, v in local.env_map : k => v }
  }
}

resource "aws_lambda_function" "indirect" {
  environment {
    variables = local.env_map
  }
}

resource "aws_lambda_function" "empty" {
  environment {
    variables = {}
  }
}

resource "aws_lambda_function" "bare" {
  function_name = "bare"
}

resource "kubernetes_config_map" "nodata" {
  metadata {
    name = "nodata"
  }
}
`)
	e := NewHCLExtractor()
	result, err := e.Extract("lambda.tf", src)
	require.NoError(t, err)

	assert.Empty(t, nodesOfKind(result.Nodes, graph.KindConfigKey),
		"non-literal, empty, and absent maps must yield no config keys")
}

// The config-key walker is additive: block nodes and the reference
// edges between blocks are unchanged, and a declared key name never
// leaks into the reference graph as a phantom block address.
func TestHCLExtractor_ConfigKeysDoNotDisturbReferences(t *testing.T) {
	src := []byte(`variable "log_level" {
  default = "info"
}

resource "aws_lambda_function" "processor" {
  role = aws_iam_role.lambda.arn

  environment {
    variables = {
      LOG_LEVEL = var.log_level
    }
  }
}
`)
	e := NewHCLExtractor()
	result, err := e.Extract("main.tf", src)
	require.NoError(t, err)

	lambda := "hcl::.::aws_lambda_function.processor"
	assert.True(t, hasEdgeBetween(result.Edges, graph.EdgeReferences, lambda,
		"hcl::.::aws_iam_role.lambda"))
	assert.True(t, hasEdgeBetween(result.Edges, graph.EdgeReferences, lambda,
		"hcl::.::var.log_level"), "value expressions inside the env block still resolve")

	for _, ed := range result.Edges {
		if ed.Kind == graph.EdgeReferences {
			assert.NotContains(t, ed.To, "LOG_LEVEL",
				"an env key name must not become a reference target")
		}
	}
}

func TestHCLExtractor_FileNode(t *testing.T) {
	src := []byte(`variable "name" {}
`)
	e := NewHCLExtractor()
	result, err := e.Extract("vars.tf", src)
	require.NoError(t, err)

	files := nodesOfKind(result.Nodes, graph.KindFile)
	assert.Equal(t, 1, len(files))
	assert.Equal(t, "vars.tf", files[0].Name)
}
