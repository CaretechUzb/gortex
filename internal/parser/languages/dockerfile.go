package languages

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/dockerfile"
)

// DockerfileExtractor extracts Dockerfile files into graph nodes and
// edges. The extractor produces a graph layer for container builds:
//
//   - FROM creates a KindImage stage (per `AS` alias, defaulting to
//     the file path when no alias is declared) and a KindImage node
//     for the parent base image. EdgeDependsOn links the stage to its
//     base. EdgeImports is kept alongside for back-compat with consumers
//     that walk import edges.
//   - ENV emits both a KindVariable (legacy surface) and a KindConfigKey
//     `cfg::env::<NAME>` matching the convention used by os.Getenv /
//     os.environ extractors. EdgeUsesEnv from the stage Image to the
//     ConfigKey wires the cross-ref between infra-side declaration and
//     code-side consumption.
//   - ARG emits a KindVariable + KindConfigKey scoped as a build arg
//     (Meta["scope"]="build_arg").
//   - EXPOSE emits EdgeExposes from the stage Image to a synthetic
//     port endpoint `port::<proto>::<n>`.
//   - RUN/CMD/ENTRYPOINT/COPY/VOLUME/USER/WORKDIR are kept as
//     instruction nodes (RUN as KindFunction, others as KindVariable).
type DockerfileExtractor struct {
	lang *sitter.Language
}

func NewDockerfileExtractor() *DockerfileExtractor {
	return &DockerfileExtractor{lang: dockerfile.GetLanguage()}
}

func (e *DockerfileExtractor) Language() string { return "dockerfile" }

// Extensions covers the three naming conventions in the wild: the
// `.dockerfile` suffix, the bare `Dockerfile` / `Containerfile`
// basename, and the per-purpose `Dockerfile.<suffix>` form
// (Dockerfile.perf, Dockerfile.ci, Containerfile.dev). The stem entries
// only match a suffix that is not itself a registered extension, so
// `Dockerfile.md` stays markdown.
func (e *DockerfileExtractor) Extensions() []string {
	return []string{
		".dockerfile",
		"Dockerfile", "Containerfile",
		"Dockerfile.*", "Containerfile.*",
	}
}

// dockerfileState carries the per-extraction "current stage" cursor.
// Each FROM begins a new stage; ENV/ARG/EXPOSE bind to whichever stage
// is currently in scope. The very first instruction in the file may be
// ARG (for global build args) before any FROM — those are bound to the
// file node directly.
type dockerfileState struct {
	stageID    string
	stageIndex int
	hasStage   bool
}

func (e *DockerfileExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	tree, err := parser.ParseFile(src, e.lang)
	if err != nil {
		return nil, err
	}
	defer tree.Close()

	root := tree.RootNode()
	result := &parser.ExtractionResult{}

	fileNode := &graph.Node{
		ID: filePath, Kind: graph.KindFile, Name: filePath,
		FilePath: filePath, StartLine: 1, EndLine: int(root.EndPoint().Row) + 1,
		Language: "dockerfile",
	}
	result.Nodes = append(result.Nodes, fileNode)

	st := &dockerfileState{}
	e.walk(root, src, filePath, fileNode.ID, result, st)

	return result, nil
}

func (e *DockerfileExtractor) walk(node *sitter.Node, src []byte, filePath, fileID string, result *parser.ExtractionResult, st *dockerfileState) {
	if node == nil {
		return
	}

	nodeType := node.Type()

	switch nodeType {
	case "from_instruction":
		e.extractFrom(node, src, filePath, fileID, result, st)
	case "env_instruction":
		e.extractEnvArg(node, src, filePath, fileID, result, "ENV", st)
	case "arg_instruction":
		e.extractEnvArg(node, src, filePath, fileID, result, "ARG", st)
	case "expose_instruction":
		e.extractExpose(node, src, filePath, fileID, result, st)
	case "run_instruction", "cmd_instruction", "entrypoint_instruction", "copy_instruction":
		e.extractInstruction(node, src, filePath, fileID, result, nodeType)
	}

	for i, _nc := 0, int(node.ChildCount()); i < _nc; i++ {
		child := node.Child(i)
		if child != nil {
			e.walk(child, src, filePath, fileID, result, st)
		}
	}
}

// extractFrom parses a `FROM <image>[:tag][@digest] [AS <stage>]` line.
// It emits a KindImage node for the parent image, a KindImage node for
// the local stage (using the AS alias when present, otherwise an
// implicit name based on the stage index), and an EdgeDependsOn edge
// from the stage to its base. EdgeImports is preserved so consumers
// that walk import edges keep working.
func (e *DockerfileExtractor) extractFrom(node *sitter.Node, src []byte, filePath, fileID string, result *parser.ExtractionResult, st *dockerfileState) {
	line := int(node.StartPoint().Row) + 1
	endLine := int(node.EndPoint().Row) + 1

	imageName := ""
	for i, _nc := 0, int(node.ChildCount()); i < _nc; i++ {
		child := node.Child(i)
		if child == nil {
			continue
		}
		if child.Type() == "image_spec" && imageName == "" {
			imageName = strings.TrimSpace(child.Content(src))
		}
	}
	rawText := strings.TrimSpace(node.Content(src))
	imageName, alias := parseFromText(rawText, imageName)

	if imageName == "" {
		return
	}

	stageName := alias
	if stageName == "" {
		stageName = fmt.Sprintf("stage-%d", st.stageIndex)
	}
	stageID := imageStageID(filePath, stageName)
	stageMeta := map[string]any{
		"role":  "stage",
		"index": st.stageIndex,
	}
	if alias != "" {
		stageMeta["alias"] = alias
	} else {
		stageMeta["is_default_stage"] = true
	}
	stageMeta["base_image"] = imageName
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: stageID, Kind: graph.KindImage, Name: stageName,
		FilePath: filePath, StartLine: line, EndLine: endLine,
		Language: "dockerfile",
		Meta:     stageMeta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: stageID, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: line,
	})
	st.stageID = stageID
	st.stageIndex++
	st.hasStage = true

	baseID := imageNodeID(imageName)
	imageRef, tag := splitImageRef(imageName)
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: baseID, Kind: graph.KindImage, Name: imageName,
		FilePath: filePath, StartLine: line, EndLine: endLine,
		Language: "dockerfile",
		Meta: map[string]any{
			"role": "base",
			"ref":  imageRef,
			"tag":  tag,
		},
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: stageID, To: baseID, Kind: graph.EdgeDependsOn,
		FilePath: filePath, Line: line,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From:     fileID,
		To:       "unresolved::import::" + imageName,
		Kind:     graph.EdgeImports,
		FilePath: filePath,
		Line:     line,
	})

	priorStageID := imageStageID(filePath, imageName)
	if priorStageID != stageID {
		result.Edges = append(result.Edges, &graph.Edge{
			From: stageID, To: priorStageID, Kind: graph.EdgeDependsOn,
			FilePath: filePath, Line: line,
			Meta: map[string]any{"link": "stage_chain"},
		})
	}
}

func (e *DockerfileExtractor) extractEnvArg(node *sitter.Node, src []byte, filePath, fileID string, result *parser.ExtractionResult, prefix string, st *dockerfileState) {
	for i, _nc := 0, int(node.ChildCount()); i < _nc; i++ {
		child := node.Child(i)
		if child == nil {
			continue
		}
		ct := child.Type()
		if ct == "env_pair" {
			nameNode := e.findChildOfType(child, "unquoted_string")
			if nameNode == nil {
				nameNode = e.findChildOfType(child, "env_key")
			}
			if nameNode != nil {
				varName := nameNode.Content(src)
				e.addVariable(varName, prefix, node, filePath, fileID, result, st)
			}
		} else if ct == "unquoted_string" && i == 1 {
			varName := child.Content(src)
			if idx := strings.Index(varName, "="); idx > 0 {
				varName = varName[:idx]
			}
			e.addVariable(varName, prefix, node, filePath, fileID, result, st)
		}
	}
}

func (e *DockerfileExtractor) addVariable(varName, prefix string, node *sitter.Node, filePath, fileID string, result *parser.ExtractionResult, st *dockerfileState) {
	if varName == "" {
		return
	}
	line := int(node.StartPoint().Row) + 1
	endLine := int(node.EndPoint().Row) + 1
	id := filePath + "::" + prefix + "." + varName
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindVariable, Name: prefix + " " + varName,
		FilePath: filePath, StartLine: line, EndLine: endLine,
		Language: "dockerfile",
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: line,
	})

	keyID := configKeyEnvID(varName)
	scope := "runtime"
	if prefix == "ARG" {
		scope = "build_arg"
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: keyID, Kind: graph.KindConfigKey, Name: varName,
		FilePath: filePath, StartLine: line, EndLine: endLine,
		Language: "dockerfile",
		Meta: map[string]any{
			"source": "env",
			"scope":  scope,
			"origin": "dockerfile",
		},
	})
	usesFrom := st.stageID
	if !st.hasStage {
		usesFrom = "" // dropped below
	}
	if usesFrom != "" {
		result.Edges = append(result.Edges, &graph.Edge{
			From: usesFrom, To: keyID, Kind: graph.EdgeUsesEnv,
			FilePath: filePath, Line: line,
			Meta: map[string]any{"scope": scope},
		})
	} else {
		// Pre-FROM ARG (global build arg). Bind to the file node
		// so the relationship is still queryable.
		result.Edges = append(result.Edges, &graph.Edge{
			From: id, To: keyID, Kind: graph.EdgeUsesEnv,
			FilePath: filePath, Line: line,
			Meta: map[string]any{"scope": scope, "global_arg": true},
		})
	}
}

func (e *DockerfileExtractor) extractExpose(node *sitter.Node, src []byte, filePath, fileID string, result *parser.ExtractionResult, st *dockerfileState) {
	line := int(node.StartPoint().Row) + 1
	text := strings.TrimSpace(node.Content(src))
	upper := strings.ToUpper(text)
	if strings.HasPrefix(upper, "EXPOSE") {
		text = strings.TrimSpace(text[len("EXPOSE"):])
	}
	for _, tok := range strings.Fields(text) {
		port, proto := parsePortToken(tok)
		if port == 0 {
			continue
		}
		from := st.stageID
		if !st.hasStage {
			from = fileID
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From: from, To: portTargetID(proto, port),
			Kind:     graph.EdgeExposes,
			FilePath: filePath, Line: line,
			Meta: map[string]any{"proto": proto, "port": port},
		})
	}
}

func (e *DockerfileExtractor) extractInstruction(node *sitter.Node, src []byte, filePath, fileID string, result *parser.ExtractionResult, instrType string) {
	label := strings.TrimSuffix(instrType, "_instruction")
	label = strings.ToUpper(label)
	text := node.Content(src)
	if len(text) > 80 {
		text = text[:77] + "..."
	}
	startLine := int(node.StartPoint().Row) + 1
	endLine := int(node.EndPoint().Row) + 1
	if instrType == "run_instruction" {
		name := fmt.Sprintf("run-line-%d", startLine)
		id := filePath + "::" + name
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindFunction, Name: name,
			FilePath: filePath, StartLine: startLine, EndLine: endLine,
			Language: "dockerfile",
			Meta: map[string]any{
				"instruction": label,
				"command":     text,
			},
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileID, To: id, Kind: graph.EdgeDefines,
			FilePath: filePath, Line: startLine,
		})
		return
	}
	id := filePath + "::" + label + "::" + strings.ReplaceAll(text, "\n", " ")
	if len(id) > 200 {
		id = id[:200]
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindVariable, Name: label,
		FilePath: filePath, StartLine: startLine, EndLine: endLine,
		Language: "dockerfile",
		Meta: map[string]any{
			"instruction": label,
		},
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: startLine,
	})
}

func (e *DockerfileExtractor) findChildOfType(node *sitter.Node, childType string) *sitter.Node {
	for i, _nc := 0, int(node.ChildCount()); i < _nc; i++ {
		child := node.Child(i)
		if child != nil && child.Type() == childType {
			return child
		}
	}
	return nil
}

// parseFromText extracts the image reference and optional AS alias
// from a FROM instruction's full text. Handles `FROM <image>[:tag]
// [AS <stage>]` and `FROM --platform=… <image> AS <stage>`. The
// astImage argument is the image_spec captured by the AST walker;
// when present we take it verbatim and only use the text to recover
// the AS alias.
func parseFromText(rawText, astImage string) (image, alias string) {
	tokens := strings.Fields(rawText)
	if len(tokens) == 0 {
		return astImage, ""
	}
	if strings.EqualFold(tokens[0], "FROM") {
		tokens = tokens[1:]
	}
	stripped := tokens[:0]
	for _, t := range tokens {
		if strings.HasPrefix(t, "--") {
			continue
		}
		stripped = append(stripped, t)
	}
	tokens = stripped

	if len(tokens) == 0 {
		return astImage, ""
	}
	image = tokens[0]
	if astImage != "" {
		image = astImage
	}
	for i := 1; i+1 < len(tokens); i++ {
		if strings.EqualFold(tokens[i], "AS") {
			alias = tokens[i+1]
			break
		}
	}
	return image, alias
}

// splitImageRef breaks `repo[:tag][@digest]` into ref and tag.
// Tag defaults to "latest" when omitted. Digests are appended back
// onto ref so consumers can detect immutability via meta.
func splitImageRef(name string) (ref, tag string) {
	digest := ""
	if at := strings.Index(name, "@"); at >= 0 {
		digest = name[at:]
		name = name[:at]
	}
	tag = "latest"
	ref = name
	if slash := strings.LastIndex(name, "/"); slash >= 0 {
		if colon := strings.LastIndex(name[slash:], ":"); colon >= 0 {
			tag = name[slash+colon+1:]
			ref = name[:slash+colon]
		}
	} else if colon := strings.LastIndex(name, ":"); colon >= 0 {
		tag = name[colon+1:]
		ref = name[:colon]
	}
	if digest != "" {
		ref = ref + digest
	}
	return ref, tag
}

func parsePortToken(tok string) (int, string) {
	proto := "tcp"
	if slash := strings.Index(tok, "/"); slash >= 0 {
		proto = strings.ToLower(strings.TrimSpace(tok[slash+1:]))
		tok = tok[:slash]
	}
	tok = strings.TrimSpace(tok)
	n, err := strconv.Atoi(tok)
	if err != nil || n <= 0 {
		return 0, proto
	}
	return n, proto
}

// imageNodeID and imageStageID centralise the ID conventions for
// KindImage nodes so K8s extractors and Dockerfile extractors share
// the same scheme — a Pod referencing `image: nginx:1.25` and a
// Dockerfile `FROM nginx:1.25` both link to the same KindImage node.
func imageNodeID(name string) string {
	ref, tag := splitImageRef(name)
	return "image::" + ref + ":" + tag
}

func imageStageID(filePath, stage string) string {
	return "image::stage::" + filePath + "::" + stage
}

// configKeyEnvID is the ID convention shared between the Dockerfile,
// K8s, and Go-side os.Getenv extractors. Keeping all sites at the
// same string ensures the cross-ref between container env
// declarations and code-side reads materialises through node
// identity instead of through a separate resolver pass.
func configKeyEnvID(name string) string {
	return "cfg::env::" + name
}

// portTargetID builds the synthetic port endpoint used as the target
// of EdgeExposes edges. We don't materialise port nodes — these IDs
// are dangling endpoints, mirroring how unresolved::import::* works
// for cross-language imports.
func portTargetID(proto string, port int) string {
	return fmt.Sprintf("port::%s::%d", proto, port)
}
