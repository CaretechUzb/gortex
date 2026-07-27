package languages

import (
	"bytes"
	"encoding/xml"
	"io"
	"path"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// Dependency-injection container binding extraction.
//
// Frameworks like Symfony and Spring wire a constructor/field that is
// typed as an *interface* to a *concrete class* at runtime via an
// external config file (services.yaml, applicationContext.xml). The
// PHP/Java source alone only shows the interface type, so a call site
// typed as the interface looks like it has no implementation in the
// graph. These extractors read the container config and emit the
// interface→implementation binding so the cross-file resolver can land
// the autowiring.
//
// The binding is modelled as an EdgeProvides carrying
// binding:"useClass" + provides_for:<interface> and pointing at
// `unresolved::<impl>`. Every Meta key and binding form in this contract
// is enumerated as a graph.MetaDI* / graph.DIBinding* constant, and both
// the emitters here and the consumers (resolver.buildProvidesForIndex,
// indexer's DI post-pass) compile against that one spelling. That is
// the exact shape NestJS useClass and
// Laravel `$this->app->bind` already produce, so the existing
// resolver.buildProvidesForIndex pass — which rewrites abstract-typed
// call sites onto the concrete impl — picks these up with no resolver
// changes. Each edge is additionally stamped Meta["di"] = "symfony" /
// "spring" so DI-sourced bindings are queryable as such, plus the full
// FQCNs (interface_fqcn / impl_fqcn) for fidelity, while the
// unresolved:: target uses the short class name (the last `\` or `.`
// segment) because PHP/Java type nodes are keyed by short name and the
// resolver reduces targets to a bare identifier before lookup.

// diShortName returns the last `\`- or `.`-separated segment of a
// fully-qualified class name, with a leading `@` (Symfony service
// reference) and surrounding whitespace stripped. Returns "" for empty
// input. `App\Foo\BarImpl` → `BarImpl`; `com.app.BarImpl` → `BarImpl`.
func diShortName(fqcn string) string {
	s := strings.TrimSpace(fqcn)
	s = strings.TrimPrefix(s, "@")
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if i := strings.LastIndexAny(s, "\\."); i >= 0 && i+1 < len(s) {
		return s[i+1:]
	}
	return s
}

// looksLikeClassFQCN reports whether s looks like a PHP/Java class
// reference rather than a primitive value, parameter reference, or
// container expression. Symfony service ids are usually FQCNs but can
// also be lowercase aliases; we accept any non-empty token that is not
// obviously a non-class value (parameter `%foo%`, env `%env(...)%`,
// yaml tag expression) so aliases still bind, while filtering noise.
func looksLikeClassFQCN(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	if strings.HasPrefix(s, "%") || strings.HasPrefix(s, "!") {
		return false // parameter / env / yaml tag expression
	}
	return true
}

// ---- Symfony services.yaml ---------------------------------------------

// isSymfonyServicesYAML reports whether a parsed root mapping looks like
// a Symfony container config: it has a top-level `services:` mapping.
// Kept cheap — a single top-level key probe.
func isSymfonyServicesYAML(root *yaml.Node) bool {
	if root == nil || root.Kind != yaml.MappingNode {
		return false
	}
	svc := mappingGet(root, "services")
	if svc == nil || svc.Kind != yaml.MappingNode {
		return false
	}
	// Distinguish a Symfony services file from a docker-compose file —
	// both have a top-level `services:`. Symfony service IDs are PHP FQCNs
	// (contain a backslash) or the special `_defaults` / `_instanceof`
	// keys; docker-compose service names are plain identifiers. Require at
	// least one Symfony-shaped key so a compose file falls through to the
	// generic YAML extractor.
	for i := 0; i+1 < len(svc.Content); i += 2 {
		key := strings.TrimSpace(svc.Content[i].Value)
		if strings.Contains(key, `\`) || key == "_defaults" || key == "_instanceof" {
			return true
		}
	}
	return false
}

// extractSymfonyServicesYAML scans a YAML stream for a Symfony
// `services:` block and emits the DI interface→implementation bindings
// it can derive.
//
// Forms covered (under `services:`):
//
//	# explicit class
//	App\SomeInterface:
//	    class: App\ConcreteImpl
//
//	# alias to another service (value is `@id`)
//	App\SomeInterface: '@App\ConcreteImpl'
//	App\SomeInterface:
//	    alias: '@App\ConcreteImpl'
//	    # (Symfony also accepts a bare `alias: App\ConcreteImpl`)
//
//	# self-registration (no binding, just a concrete service)
//	App\ConcreteImpl: ~
//	App\ConcreteImpl:
//	    class: App\ConcreteImpl   # redundant but legal
//
// Returns true when the file was recognised as a Symfony services file
// (so the caller can skip the generic YAML key walk). A file with a
// `services:` block but no derivable bindings still returns true.
func extractSymfonyServicesYAML(filePath, fileID string, src []byte, result *parser.ExtractionResult) bool {
	dec := yaml.NewDecoder(bytes.NewReader(src))
	recognised := false
	for {
		var doc yaml.Node
		if err := dec.Decode(&doc); err != nil {
			break // EOF or malformed — keep what was decoded
		}
		root := documentMapping(&doc)
		if !isSymfonyServicesYAML(root) {
			continue
		}
		recognised = true
		services := mappingGet(root, "services")
		emitSymfonyServiceBindings(filePath, fileID, services, result)
	}
	return recognised
}

// emitSymfonyServiceBindings walks the entries of a `services:` mapping
// and emits one EdgeProvides per interface→impl binding it derives.
func emitSymfonyServiceBindings(filePath, fileID string, services *yaml.Node, result *parser.ExtractionResult) {
	if services == nil || services.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(services.Content); i += 2 {
		keyNode := services.Content[i]
		valNode := services.Content[i+1]
		if keyNode == nil {
			continue
		}
		serviceID := strings.TrimSpace(keyNode.Value)
		if serviceID == "" {
			continue
		}
		// Container defaults / globals are not bindings.
		switch serviceID {
		case "_defaults", "_instanceof":
			continue
		}
		line := keyNode.Line
		if line <= 0 {
			line = 1
		}

		impl := symfonyBindingTarget(valNode)
		if impl == "" || !looksLikeClassFQCN(impl) {
			// `App\ConcreteImpl: ~` self-registration, or a value we
			// can't interpret as a class binding — nothing to wire.
			continue
		}
		if !looksLikeClassFQCN(serviceID) {
			continue
		}
		// Self-binding (`App\Impl: { class: App\Impl }`) is not an
		// interface→impl relationship; skip it.
		if serviceID == impl {
			continue
		}
		emitDIProvides(result, fileID, filePath, line, serviceID, impl, "symfony")
	}
}

// symfonyBindingTarget extracts the concrete implementation FQCN that a
// single `services:` entry binds its key to. Handles:
//   - scalar value `'@App\Impl'` (alias) and bare `App\Impl`;
//   - mapping value with `class: App\Impl`;
//   - mapping value with `alias: '@App\Impl'` / `alias: App\Impl`.
//
// Returns "" when the entry declares no concrete target (e.g. `~`).
func symfonyBindingTarget(val *yaml.Node) string {
	if val == nil {
		return ""
	}
	switch val.Kind {
	case yaml.ScalarNode:
		// `App\Iface: '@App\Impl'` or `App\Iface: App\Impl`. A bare `~`
		// / null decodes to a scalar with Tag "!!null" — skip it.
		if val.Tag == "!!null" {
			return ""
		}
		return strings.TrimSpace(val.Value)
	case yaml.MappingNode:
		if c := scalarOf(mappingGet(val, "class")); c != "" {
			return strings.TrimSpace(c)
		}
		if a := scalarOf(mappingGet(val, "alias")); a != "" {
			return strings.TrimSpace(a)
		}
	}
	return ""
}

// ---- Spring applicationContext.xml -------------------------------------

// SpringContextExtractor indexes a Spring XML application context
// (`<beans>` root, the classic non-annotation DI config). It surfaces
// the interface→implementation bindings the container declares so a
// field/constructor typed as an interface resolves to the concrete bean
// class.
//
// Two derivations (see emitSpringBeanBindings):
//   - Direct: `<bean id="com.app.SomeInterface" class="com.app.Impl"/>`
//     where the id is a class-shaped FQCN distinct from the class — the
//     id is treated as the interface (the Symfony alias analogue).
//   - Ref-based: a `<bean>` whose `<constructor-arg ref=…>` / `<property
//     ref=…>` names another bean whose id is a class-shaped FQCN — that
//     referenced bean's id→class is the wired interface→impl binding.
//
// `<beans>` uses the plain `.xml` extension shared with every other XML
// document, so the extractor is content-gated on IsSpringBeansXML; a
// misrouted plain-XML file yields just the file node.
type SpringContextExtractor struct{}

// NewSpringContextExtractor constructs a SpringContextExtractor.
func NewSpringContextExtractor() *SpringContextExtractor { return &SpringContextExtractor{} }

func (e *SpringContextExtractor) Language() string     { return "spring" }
func (e *SpringContextExtractor) Extensions() []string { return []string{".xml"} }

// IsSpringBeansXML reports whether src is a Spring XML application
// context — recognised by a `<beans` root element (optionally namespaced)
// plus a `<bean` child, or the Spring beans DTD/schema reference. Only
// the document head is scanned, so the probe is cheap on large files.
func IsSpringBeansXML(src []byte) bool {
	head := src
	const headCap = 8 * 1024
	if len(head) > headCap {
		head = head[:headCap]
	}
	lower := bytes.ToLower(head)
	if bytes.Contains(lower, []byte("springframework.org/schema/beans")) ||
		bytes.Contains(lower, []byte("springframework.org/dtd/spring-beans")) {
		return true
	}
	// Root-element form: a `<beans` start tag (namespaced `<b:beans`
	// counts via the local-name match in the decoder). Require a `<bean`
	// child marker to avoid matching unrelated `<beans>`-named roots.
	if bytes.Contains(lower, []byte("<beans")) && bytes.Contains(lower, []byte("<bean")) {
		return true
	}
	return false
}

// springBean is a decoded `<bean>` element. Only the fields the binding
// derivation needs are kept.
type springBean struct {
	id       string
	class    string
	line     int
	ctorRefs []string // <constructor-arg ref="..">
	propRefs []string // <property ref="..">
}

// Extract parses a Spring XML context. A document that is not a Spring
// beans file yields only the file node. A malformed file yields whatever
// was decoded before the error — never a hard failure.
func (e *SpringContextExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	result := &parser.ExtractionResult{}
	fileNode := &graph.Node{
		ID:       filePath,
		Kind:     graph.KindFile,
		Name:     path.Base(filePath),
		FilePath: filePath,
		Language: "spring",
	}
	result.Nodes = append(result.Nodes, fileNode)

	if !IsSpringBeansXML(src) {
		return result, nil
	}

	beans := parseSpringBeans(src)
	emitSpringBeanBindings(filePath, fileNode.ID, beans, result)
	return result, nil
}

// parseSpringBeans decodes the `<bean>` elements of a Spring context,
// capturing id/class plus the refs declared by nested `<constructor-arg
// ref>` / `<property ref>`. Namespace-agnostic on element/attr local
// names.
func parseSpringBeans(src []byte) []springBean {
	dec := xml.NewDecoder(bytes.NewReader(src))
	dec.Strict = false

	var beans []springBean
	var cur *springBean

	flush := func() {
		if cur != nil {
			beans = append(beans, *cur)
			cur = nil
		}
	}

	for {
		tok, err := dec.Token()
		if err == io.EOF || err != nil {
			break // EOF or malformed — keep what was decoded
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch strings.ToLower(t.Name.Local) {
			case "bean":
				flush()
				b := springBean{
					id:    xmlAttr(t, "id"),
					class: xmlAttr(t, "class"),
				}
				if b.id == "" {
					b.id = xmlAttr(t, "name")
				}
				b.line = 1 + bytes.Count(src[:clampOffset(dec.InputOffset(), len(src))], []byte{'\n'})
				cur = &b
			case "constructor-arg":
				if cur != nil {
					if ref := xmlAttr(t, "ref"); ref != "" {
						cur.ctorRefs = append(cur.ctorRefs, ref)
					}
				}
			case "property":
				if cur != nil {
					if ref := xmlAttr(t, "ref"); ref != "" {
						cur.propRefs = append(cur.propRefs, ref)
					}
				}
			}
		case xml.EndElement:
			if strings.ToLower(t.Name.Local) == "bean" {
				flush()
			}
		}
	}
	flush()
	return beans
}

// emitSpringBeanBindings turns decoded beans into DI binding edges.
//
// Two derivations:
//  1. Direct: a `<bean id="com.app.SomeInterface" class="com.app.Impl">`
//     where the id is a class-shaped FQCN distinct from the class — the
//     id is treated as the interface (the Symfony alias analogue).
//  2. Ref-based: a `<bean>` whose `<constructor-arg ref>` / `<property
//     ref>` names another bean whose id is a class-shaped FQCN — the
//     referenced bean's id is the wired interface, the referenced bean's
//     class is the concrete impl. Deduped against derivation 1.
func emitSpringBeanBindings(filePath, fileID string, beans []springBean, result *parser.ExtractionResult) {
	byID := make(map[string]springBean, len(beans))
	for _, b := range beans {
		if b.id != "" {
			byID[b.id] = b
		}
	}

	emitted := make(map[string]struct{}) // dedupe by "iface->impl"
	bind := func(iface, impl string, line int) {
		if iface == "" || impl == "" || iface == impl {
			return
		}
		if !looksLikeClassFQCN(iface) || !looksLikeClassFQCN(impl) {
			return
		}
		key := iface + "->" + impl
		if _, dup := emitted[key]; dup {
			return
		}
		emitted[key] = struct{}{}
		if line <= 0 {
			line = 1
		}
		emitDIProvides(result, fileID, filePath, line, iface, impl, "spring")
	}

	for _, b := range beans {
		// Derivation 1: id names an interface, class is the impl.
		if b.id != "" && b.class != "" && b.id != b.class {
			bind(b.id, b.class, b.line)
		}
		// Derivation 2: a ref points at a bean whose id is an
		// interface FQCN — bind that bean's id→class.
		for _, ref := range append(append([]string{}, b.ctorRefs...), b.propRefs...) {
			target, ok := byID[ref]
			if !ok || target.class == "" {
				continue
			}
			if target.id != target.class {
				bind(target.id, target.class, target.line)
			}
		}
	}
}

// ---- shared emission ---------------------------------------------------

// emitDIProvides appends one EdgeProvides modelling an interface→impl
// container binding, shaped so resolver.buildProvidesForIndex consumes
// it. From is the config file node (the only real node in a config
// file); the resolver rewrites To onto the concrete class once the
// PHP/Java class is indexed.
func emitDIProvides(result *parser.ExtractionResult, fileID, filePath string, line int, ifaceFQCN, implFQCN, source string) {
	implShort := diShortName(implFQCN)
	if implShort == "" {
		return
	}
	ifaceShort := diShortName(ifaceFQCN)
	result.Edges = append(result.Edges, &graph.Edge{
		From:     fileID,
		To:       graph.UnresolvedMarker + implShort,
		Kind:     graph.EdgeProvides,
		FilePath: filePath,
		Line:     line,
		Origin:   graph.OriginASTInferred,
		Meta: map[string]any{
			graph.MetaDIBinding:       graph.DIBindingUseClass,
			graph.MetaDIProvidesFor:   ifaceShort,
			graph.MetaDISource:        source,
			graph.MetaDIInterfaceFQCN: diNormalizeFQCN(ifaceFQCN),
			graph.MetaDIImplFQCN:      diNormalizeFQCN(implFQCN),
		},
	})
}

// diNormalizeFQCN trims whitespace and a leading `@` service-reference
// marker from an FQCN for storage in Meta.
func diNormalizeFQCN(fqcn string) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(fqcn), "@"))
}

// xmlAttr returns the value of the attribute with the given local name
// (namespace-agnostic), trimmed. Returns "" when absent.
func xmlAttr(se xml.StartElement, local string) string {
	for _, a := range se.Attr {
		if a.Name.Local == local {
			return strings.TrimSpace(a.Value)
		}
	}
	return ""
}
