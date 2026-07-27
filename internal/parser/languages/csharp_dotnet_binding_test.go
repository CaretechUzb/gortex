package languages

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// providesEdges returns the EdgeProvides bindings in a result, keyed by
// the provides_for (interface short name) → impl_fqcn pair, with the di
// source, so tests can assert interface→impl wiring independent of edge
// order or the unresolved:: target encoding.
type provided struct {
	iface  string
	impl   string
	source string
}

func collectProvides(result *parser.ExtractionResult) []provided {
	var out []provided
	for _, e := range result.Edges {
		if e.Kind != graph.EdgeProvides {
			continue
		}
		pf, _ := e.Meta["provides_for"].(string)
		impl, _ := e.Meta["impl_fqcn"].(string)
		src, _ := e.Meta["di"].(string)
		out = append(out, provided{iface: pf, impl: impl, source: src})
	}
	return out
}

func TestEmitDotNetDIBindings_MSDI_TwoArg(t *testing.T) {
	result := &parser.ExtractionResult{
		Nodes: []*graph.Node{{ID: "Startup.cs", Kind: graph.KindFile, FilePath: "Startup.cs"}},
	}
	src := []byte(`public void ConfigureServices(IServiceCollection services) {
    services.AddScoped<IUserRepo, UserRepo>();
    services.AddTransient<IFoo, Foo>();
}`)
	detectDotNetSurfaces(src, result)

	got := collectProvides(result)
	require.Contains(t, got, provided{iface: "IUserRepo", impl: "UserRepo", source: "msdi"})
	require.Contains(t, got, provided{iface: "IFoo", impl: "Foo", source: "msdi"})
}

func TestEmitDotNetDIBindings_MSDI_SingleArg_NoBinding(t *testing.T) {
	result := &parser.ExtractionResult{
		Nodes: []*graph.Node{{ID: "Startup.cs", Kind: graph.KindFile, FilePath: "Startup.cs"}},
	}
	// Self / interface-only registration carries no iface->impl pair.
	src := []byte(`services.AddSingleton<ILogger>();
services.AddHostedService<Worker>();`)
	detectDotNetSurfaces(src, result)

	require.Empty(t, collectProvides(result))
}

func TestEmitDotNetDIBindings_MSDI_SingletonAndFullyQualified(t *testing.T) {
	result := &parser.ExtractionResult{
		Nodes: []*graph.Node{{ID: "Startup.cs", Kind: graph.KindFile, FilePath: "Startup.cs"}},
	}
	src := []byte(`services.AddSingleton<IClock, SystemClock>();
services.AddScoped<App.Abstractions.IRepo, App.Data.Repo>();`)
	detectDotNetSurfaces(src, result)

	got := collectProvides(result)
	require.Contains(t, got, provided{iface: "IClock", impl: "SystemClock", source: "msdi"})
	// provides_for (the match key) is short-named even from a fully-qualified
	// interface; impl_fqcn retains the qualified spelling.
	require.Contains(t, got, provided{iface: "IRepo", impl: "App.Data.Repo", source: "msdi"})
}

func TestEmitDotNetDIBindings_Autofac_Generic(t *testing.T) {
	result := &parser.ExtractionResult{
		Nodes: []*graph.Node{{ID: "Module.cs", Kind: graph.KindFile, FilePath: "Module.cs"}},
	}
	src := []byte(`builder.RegisterType<EmailSender>().As<IEmailSender>();`)
	detectDotNetSurfaces(src, result)

	require.Contains(t, collectProvides(result),
		provided{iface: "IEmailSender", impl: "EmailSender", source: "autofac"})
}

func TestEmitDotNetDIBindings_Autofac_FluentChainBeforeAs(t *testing.T) {
	result := &parser.ExtractionResult{
		Nodes: []*graph.Node{{ID: "Module.cs", Kind: graph.KindFile, FilePath: "Module.cs"}},
	}
	// Lifetime / registration calls chained before .As<>() are idiomatic Autofac.
	src := []byte(`builder.RegisterType<EmailSender>().SingleInstance().As<IEmailSender>();
builder.RegisterType(typeof(SmsSender)).InstancePerLifetimeScope().As(typeof(ISmsSender));`)
	detectDotNetSurfaces(src, result)

	got := collectProvides(result)
	require.Contains(t, got, provided{iface: "IEmailSender", impl: "EmailSender", source: "autofac"})
	require.Contains(t, got, provided{iface: "ISmsSender", impl: "SmsSender", source: "autofac"})
}

func TestEmitDotNetDIBindings_Autofac_Typeof(t *testing.T) {
	result := &parser.ExtractionResult{
		Nodes: []*graph.Node{{ID: "Module.cs", Kind: graph.KindFile, FilePath: "Module.cs"}},
	}
	src := []byte(`builder.RegisterType(typeof(SmsSender)).As(typeof(ISmsSender));`)
	detectDotNetSurfaces(src, result)

	require.Contains(t, collectProvides(result),
		provided{iface: "ISmsSender", impl: "SmsSender", source: "autofac"})
}

func TestEmitDotNetDIBindings_Autofac_GenericInterfaceArg(t *testing.T) {
	result := &parser.ExtractionResult{
		Nodes: []*graph.Node{{ID: "Module.cs", Kind: graph.KindFile, FilePath: "Module.cs"}},
	}
	// A closed-generic service interface: the binding must land on the
	// de-genericized name (IRepository), the key the type graph binds by.
	src := []byte(`builder.RegisterType<UserRepository>().As<IRepository<User>>();
builder.RegisterType<CacheRepository>()
    .SingleInstance()
    .As<IRepository<Dictionary<string, User>>>();`)
	detectDotNetSurfaces(src, result)

	got := collectProvides(result)
	require.Contains(t, got, provided{iface: "IRepository", impl: "UserRepository", source: "autofac"})
	require.Contains(t, got, provided{iface: "IRepository", impl: "CacheRepository", source: "autofac"})
}

func TestEmitDotNetDIBindings_MSDI_GenericTypeArgs(t *testing.T) {
	result := &parser.ExtractionResult{
		Nodes: []*graph.Node{{ID: "Startup.cs", Kind: graph.KindFile, FilePath: "Startup.cs"}},
	}
	src := []byte(`services.AddScoped<IRepository<User>, UserRepository>();
services.AddSingleton<IHandler<OrderMsg>, OrderHandler<OrderMsg>>();`)
	detectDotNetSurfaces(src, result)

	got := collectProvides(result)
	require.Contains(t, got, provided{iface: "IRepository", impl: "UserRepository", source: "msdi"})
	require.Contains(t, got, provided{iface: "IHandler", impl: "OrderHandler", source: "msdi"})
}

func TestEmitDotNetDIBindings_FactoryLambdas(t *testing.T) {
	result := &parser.ExtractionResult{
		Nodes: []*graph.Node{{ID: "Program.cs", Kind: graph.KindFile, FilePath: "Program.cs"}},
	}
	src := []byte(`services.AddScoped<IMailer>(sp => new SmtpMailer(sp.GetRequiredService<IConfig>()));
builder.Register(c => new PdfRenderer(c.Resolve<IFonts>()))
    .As<IRenderer>()
    .InstancePerLifetimeScope();`)
	detectDotNetSurfaces(src, result)

	got := collectProvides(result)
	require.Contains(t, got, provided{iface: "IMailer", impl: "SmtpMailer", source: "msdi"})
	require.Contains(t, got, provided{iface: "IRenderer", impl: "PdfRenderer", source: "autofac"})
}

func TestEmitDotNetDIBindings_FactoryLambda_DelegatingBodySkipped(t *testing.T) {
	result := &parser.ExtractionResult{
		Nodes: []*graph.Node{{ID: "Program.cs", Kind: graph.KindFile, FilePath: "Program.cs"}},
	}
	// A factory that resolves/delegates rather than newing up the impl
	// names no concrete type — nothing to bind.
	src := []byte(`builder.Register(c => c.Resolve<UserService>()).As<IUserService>();`)
	detectDotNetSurfaces(src, result)

	require.Empty(t, collectProvides(result))
}

func TestEmitDotNetDIBindings_AsImplementedInterfaces_Marker(t *testing.T) {
	result := &parser.ExtractionResult{
		Nodes: []*graph.Node{{ID: "Module.cs", Kind: graph.KindFile, FilePath: "Module.cs"}},
	}
	// The interface set is not in the call text — the extractor emits a
	// marker edge the resolver joins with the impl's implements edges.
	src := []byte(`builder.RegisterType<AmAuthenticationService>()
    .AsImplementedInterfaces()
    .InstancePerLifetimeScope();
builder.RegisterType<ClaimDataWriteService>()
    .As<IWriteService<ClaimData>>()
    .AsImplementedInterfaces()
    .InstancePerLifetimeScope();`)
	detectDotNetSurfaces(src, result)

	var markers []string
	for _, e := range result.Edges {
		if e.Kind != graph.EdgeProvides {
			continue
		}
		if b, _ := e.Meta["binding"].(string); b == "asImplementedInterfaces" {
			markers = append(markers, e.To)
		}
	}
	require.ElementsMatch(t, markers,
		[]string{"unresolved::AmAuthenticationService", "unresolved::ClaimDataWriteService"})
	// The explicit .As<> on the second registration still emits its own
	// concrete pair alongside the marker.
	require.Contains(t, collectProvides(result),
		provided{iface: "IWriteService", impl: "ClaimDataWriteService", source: "autofac"})
}
