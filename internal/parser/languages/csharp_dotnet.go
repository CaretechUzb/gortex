package languages

import (
	"regexp"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// csharpTypeArgPat matches one C# type argument including a closed
// generic — Foo, App.IFoo, IRepository<User>, IRepository<Dictionary<K, V>>.
// One nesting depth (RE2 has no recursion); shared by every registration regex.
const csharpTypeArgPat = `[\w.]+(?:<[^<>]*(?:<[^<>]*>[^<>]*)*>)?`

// diRegistrationRe matches a .NET dependency-injection registration —
// services.AddScoped<IFoo, Foo>(), AddSingleton<IBar>(),
// AddTransient<…>(), AddHostedService<…>().
var diRegistrationRe = regexp.MustCompile(
	`\.Add(Scoped|Singleton|Transient|HostedService)\s*<\s*(` + csharpTypeArgPat + `)\s*(?:,\s*(` + csharpTypeArgPat + `)\s*)?>`)

// comInteropRe matches a COM / native-interop marker attribute.
var comInteropRe = regexp.MustCompile(
	`\[\s*(?:ComImport|ComVisible|Guid|DllImport|InterfaceType|ClassInterface|ComSourceInterfaces)\b`)

// autofacGenericRe matches an Autofac fluent generic registration —
// builder.RegisterType<Foo>().SingleInstance().As<IFoo>(). group1 =
// implementation, group2 = interface. The chain matcher skips lifetime
// calls without consuming .As<>() itself (`<>`, not `()`, after the name).
var autofacGenericRe = regexp.MustCompile(
	`RegisterType\s*<\s*(` + csharpTypeArgPat + `)\s*>\s*\(\s*\)(?:\s*\.\w+\s*\([^)]*\))*\s*\.As\s*<\s*(` + csharpTypeArgPat + `)\s*>`)

// diFactoryRe matches an MS.DI factory lambda that news up the impl —
// services.AddScoped<IMailer>(sp => new SmtpMailer(...)). group2 =
// interface, group3 = implementation. Delegating factories name no
// concrete type and are intentionally not matched.
var diFactoryRe = regexp.MustCompile(
	`\.Add(Scoped|Singleton|Transient)\s*<\s*(` + csharpTypeArgPat + `)\s*>\s*\(\s*\w+\s*=>\s*new\s+(` + csharpTypeArgPat + `)\b`)

// autofacFactoryRe matches an Autofac factory lambda —
// builder.Register(c => new PdfRenderer(...)).As<IRenderer>(). group1 =
// implementation, group2 = interface. `[^;]*?` bridges ctor arguments
// and chained lifetime calls without escaping the statement.
var autofacFactoryRe = regexp.MustCompile(
	`\.Register\s*\(\s*\w+\s*=>\s*new\s+(` + csharpTypeArgPat + `)\b[^;]*?\.As\s*<\s*(` + csharpTypeArgPat + `)\s*>`)

// autofacAsImplementedRe matches builder.RegisterType<Foo>()…
// .AsImplementedInterfaces(), tolerating intervening fluent calls,
// generic ones included. group1 = implementation. The interface set is
// not in the call text — the extractor emits a marker EdgeProvides the
// resolver joins with the impl's implements edges once resolved.
var autofacAsImplementedRe = regexp.MustCompile(
	`RegisterType\s*<\s*(` + csharpTypeArgPat + `)\s*>\s*\(\s*\)(?:\s*\.\w+\s*(?:<[^<>]*(?:<[^<>]*>[^<>]*)*>)?\s*\([^)]*\))*?\s*\.AsImplementedInterfaces\s*\(`)

// autofacTypeofRe matches the non-generic Autofac form —
// RegisterType(typeof(Foo)).As(typeof(IFoo)). group1 = implementation,
// group2 = interface. Like the generic form, intervening fluent calls
// (.SingleInstance(), .InstancePerLifetimeScope(), …) are skipped.
var autofacTypeofRe = regexp.MustCompile(
	`RegisterType\s*\(\s*typeof\s*\(\s*([\w.]+)\s*\)\s*\)(?:\s*\.\w+\s*\([^)]*\))*\s*\.As\s*\(\s*typeof\s*\(\s*([\w.]+)\s*\)\s*\)`)

// detectDotNetSurfaces stamps the C# file node with the two .NET
// surfaces a tree-sitter symbol walk does not otherwise expose:
// dependency-injection registrations (Meta["di_registrations"]) and a
// COM / native-interop flag (Meta["com_interop"]). Both are core to
// understanding a .NET service's wiring and unmanaged boundary.
func detectDotNetSurfaces(src []byte, result *parser.ExtractionResult) {
	var file *graph.Node
	for _, n := range result.Nodes {
		if n.Kind == graph.KindFile {
			file = n
			break
		}
	}
	if file == nil {
		return
	}
	text := string(src)

	// Emit real interface→impl binding edges (the resolver-consumed
	// EdgeProvides shape), in addition to the file-metadata summary below.
	emitDotNetDIBindings(text, file, result)

	var regs []string
	seen := map[string]bool{}
	for _, m := range diRegistrationRe.FindAllStringSubmatch(text, -1) {
		entry := strings.ToLower(m[1]) + " " + m[2]
		if m[3] != "" {
			entry += " -> " + m[3]
		}
		if !seen[entry] {
			seen[entry] = true
			regs = append(regs, entry)
		}
	}
	hasCOM := comInteropRe.MatchString(text)
	if len(regs) == 0 && !hasCOM {
		return
	}
	if file.Meta == nil {
		file.Meta = map[string]any{}
	}
	if len(regs) > 0 {
		sort.Strings(regs)
		file.Meta["di_registrations"] = regs
	}
	if hasCOM {
		file.Meta["com_interop"] = true
	}
}

// emitDotNetDIBindings turns explicit .NET DI registrations into
// EdgeProvides interface→implementation bindings — the shape the
// resolver's provides-index consumes to land interface-typed call sites
// onto the concrete impl. This complements detectDotNetSurfaces, which
// only records registrations as file metadata (and therefore never
// reaches find_implementations / the autowiring resolver).
//
// Covered forms (the common cases):
//   - Microsoft.Extensions.DI two-arg generic:
//     services.AddScoped<IFoo, Foo>()   (Singleton / Transient too)
//   - Autofac generic:   builder.RegisterType<Foo>().As<IFoo>()
//   - Autofac typeof:     RegisterType(typeof(Foo)).As(typeof(IFoo))
//   - closed-generic type args on either side (As<IRepository<User>>),
//     bound on the de-genericized name
//   - factory lambdas whose body news up the impl:
//     AddScoped<IFoo>(sp => new Foo(...)),
//     Register(c => new Foo(...)).As<IFoo>()
//
// Known gaps (no binding emitted — documented, not silent):
//   - single-type self-registration: AddScoped<Foo>() / RegisterType<Foo>()
//   - AsImplementedInterfaces(): the interface set is not in the call
//     text; resolving it needs joining with the type's own
//     EdgeImplements edges (a resolver-side enhancement)
//   - factory lambdas that delegate instead of newing up the impl:
//     Register(c => c.Resolve<Foo>()), factory-method calls
//   - multiple .As<>(): only the first interface is captured
//   - assembly scanning (RegisterAssemblyTypes): the registered set is a
//     runtime property of the assembly, not the call text
func emitDotNetDIBindings(text string, file *graph.Node, result *parser.ExtractionResult) {
	lineAt := func(off int) int { return 1 + strings.Count(text[:off], "\n") }
	// Closed-generic registrations (As<IRepository<User>>) bind on the
	// de-genericized name — type nodes index generic-erased, so the
	// type-argument list would keep the edge unresolvable forever.
	deGeneric := func(s string) string {
		if i := strings.IndexByte(s, '<'); i >= 0 {
			return s[:i]
		}
		return s
	}

	// Microsoft.Extensions.DI: group2 = interface, group3 = impl.
	// Index layout per match: [full.s, full.e, g1.s, g1.e, g2.s, g2.e, g3.s, g3.e].
	for _, ix := range diRegistrationRe.FindAllStringSubmatchIndex(text, -1) {
		if len(ix) < 8 || ix[6] < 0 || ix[7] < 0 {
			continue // single type arg: self/interface-only registration — skip
		}
		iface := deGeneric(text[ix[4]:ix[5]])
		impl := deGeneric(text[ix[6]:ix[7]])
		emitDIProvides(result, file.ID, file.FilePath, lineAt(ix[0]), iface, impl, "msdi")
	}

	// Autofac generic: group1 = impl, group2 = interface.
	for _, ix := range autofacGenericRe.FindAllStringSubmatchIndex(text, -1) {
		impl := deGeneric(text[ix[2]:ix[3]])
		iface := deGeneric(text[ix[4]:ix[5]])
		emitDIProvides(result, file.ID, file.FilePath, lineAt(ix[0]), iface, impl, "autofac")
	}

	// Autofac typeof: group1 = impl, group2 = interface.
	for _, ix := range autofacTypeofRe.FindAllStringSubmatchIndex(text, -1) {
		impl := deGeneric(text[ix[2]:ix[3]])
		iface := deGeneric(text[ix[4]:ix[5]])
		emitDIProvides(result, file.ID, file.FilePath, lineAt(ix[0]), iface, impl, "autofac")
	}

	// MS.DI factory lambda: group2 = interface, group3 = impl.
	for _, ix := range diFactoryRe.FindAllStringSubmatchIndex(text, -1) {
		iface := deGeneric(text[ix[4]:ix[5]])
		impl := deGeneric(text[ix[6]:ix[7]])
		emitDIProvides(result, file.ID, file.FilePath, lineAt(ix[0]), iface, impl, "msdi")
	}

	// Autofac factory lambda: group1 = impl, group2 = interface.
	for _, ix := range autofacFactoryRe.FindAllStringSubmatchIndex(text, -1) {
		impl := deGeneric(text[ix[2]:ix[3]])
		iface := deGeneric(text[ix[4]:ix[5]])
		emitDIProvides(result, file.ID, file.FilePath, lineAt(ix[0]), iface, impl, "autofac")
	}

	// Autofac AsImplementedInterfaces: group1 = impl. A marker the
	// provides-index ignores; ResolveCSharpDIImplementedInterfaces
	// expands it into per-interface useClass bindings post-resolution.
	for _, ix := range autofacAsImplementedRe.FindAllStringSubmatchIndex(text, -1) {
		implShort := diShortName(deGeneric(text[ix[2]:ix[3]]))
		if implShort == "" {
			continue
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From:     file.ID,
			To:       graph.UnresolvedMarker + implShort,
			Kind:     graph.EdgeProvides,
			FilePath: file.FilePath,
			Line:     lineAt(ix[0]),
			Origin:   graph.OriginASTInferred,
			Meta: map[string]any{
				"binding":   "asImplementedInterfaces",
				"di":        "autofac",
				"impl_fqcn": diNormalizeFQCN(deGeneric(text[ix[2]:ix[3]])),
			},
		})
	}
}
