package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func findSpawn(edges []*graph.Edge, to string) *graph.Edge {
	for _, e := range edges {
		if e.Kind == graph.EdgeSpawns && e.To == to {
			return e
		}
	}
	return nil
}

// TestCSharpExtractor_HangfireSpawns: Hangfire registrations launch a
// method on a type named only by the generic argument — the lambda
// receiver is otherwise untyped, so without this the call binds (at
// best) by name heuristics. The extractor must emit a receiver-typed
// EdgeSpawns the resolver can land exactly.
func TestCSharpExtractor_HangfireSpawns(t *testing.T) {
	src := []byte(`public class JobSetup {
    public void Configure(string cron, TimeSpan delay, InvoiceDto dto) {
        RecurringJob.AddOrUpdate<SyncService>("sync users", x => x.SynchronizeAsync(), cron);
        BackgroundJob.Enqueue<Mailer>(m => m.SendAsync(1));
        BackgroundJob.Schedule<Cleaner>(c => c.Sweep(), delay);
        Hangfire.BackgroundJob.Enqueue(() => Process(dto));
    }

    public void Process(InvoiceDto dto) { }
}
`)
	result, err := NewCSharpExtractor().Extract("JobSetup.cs", src)
	require.NoError(t, err)

	// Generic forms: receiver-typed targets the resolver binds exactly.
	for _, want := range []struct{ to, recv string }{
		{"unresolved::*.SynchronizeAsync", "SyncService"},
		{"unresolved::*.SendAsync", "Mailer"},
		{"unresolved::*.Sweep", "Cleaner"},
	} {
		e := findSpawn(result.Edges, want.to)
		require.NotNil(t, e, "missing spawn edge %s", want.to)
		assert.Equal(t, "background", e.Meta["mode"])
		assert.Equal(t, want.recv, e.Meta["receiver_type"])
		assert.Equal(t, "JobSetup.cs::JobSetup.Configure", e.From)
	}

	// Inline lambda: bare target, no receiver hint needed (same-class
	// call; the ordinary EdgeCalls already exists alongside).
	inline := findSpawn(result.Edges, "unresolved::Process")
	require.NotNil(t, inline, "inline Enqueue(() => Process(...)) must spawn")
	assert.Equal(t, "background", inline.Meta["mode"])
	assert.Nil(t, inline.Meta["receiver_type"])
}

// TestCSharpExtractor_HangfireSpawns_InsideConfigLambda: registrations
// standardly live inside a configuration lambda
// (AddHangfireServer(opts => { … })) — the deferred-client detection
// must descend where the task/async walk does not.
func TestCSharpExtractor_HangfireSpawns_InsideConfigLambda(t *testing.T) {
	src := []byte(`public class Startup {
    public void Configure(IServiceCollection services, string cron) {
        services.AddHangfireServer(opts =>
        {
            RecurringJob.AddOrUpdate<SyncService>("sync", (x) => x.SynchronizeAsync(), cron);
            Task.Run(() => Helper());
        });
    }

    public void Helper() { }
}
`)
	result, err := NewCSharpExtractor().Extract("Startup.cs", src)
	require.NoError(t, err)

	e := findSpawn(result.Edges, "unresolved::*.SynchronizeAsync")
	require.NotNil(t, e, "registration inside a config lambda must still spawn")
	assert.Equal(t, "SyncService", e.Meta["receiver_type"])
	assert.Equal(t, "Startup.cs::Startup.Configure", e.From)

	// Task/async attribution semantics are unchanged: a Task.Run inside
	// a lambda still emits nothing.
	assert.Nil(t, findSpawn(result.Edges, "unresolved::Task.Run"),
		"lambda-nested Task.Run attribution must not change")
}

// TestCSharpExtractor_HangfireSpawns_TypeParamReceiver: a generic
// wrapper's Enqueue<T> names no indexed type — emitting a hint would
// steer the resolver onto an arbitrary same-named method. Emit nothing.
func TestCSharpExtractor_HangfireSpawns_TypeParamReceiver(t *testing.T) {
	src := []byte(`public class JobScheduler {
    public void EnqueueJob<T>() where T : IJob {
        BackgroundJob.Enqueue<T>(x => x.Run());
    }
    public void EnqueueEntity<TEntity>() where TEntity : IJob {
        BackgroundJob.Enqueue<TEntity>(x => x.Run());
    }
}
`)
	result, err := NewCSharpExtractor().Extract("Wrapper.cs", src)
	require.NoError(t, err)

	for _, e := range result.Edges {
		if e.Kind == graph.EdgeSpawns {
			assert.NotEqual(t, "background", e.Meta["mode"],
				"type-param receiver must not emit a background spawn: %s", e.To)
		}
	}
}

// TestCSharpExtractor_HangfireSpawns_SameMethodNameAcrossReceivers: job
// classes conventionally share method names (Run/ExecuteAsync) and
// registrations cluster in one method — every receiver must keep its
// own edge (the dedup key includes the receiver).
func TestCSharpExtractor_HangfireSpawns_SameMethodNameAcrossReceivers(t *testing.T) {
	src := []byte(`public class Setup {
    public void Configure() {
        BackgroundJob.Enqueue<Mailer>(m => m.ExecuteAsync());
        BackgroundJob.Enqueue<Cleaner>(c => c.ExecuteAsync());
    }
}
`)
	result, err := NewCSharpExtractor().Extract("Setup.cs", src)
	require.NoError(t, err)

	var recvs []string
	for _, e := range result.Edges {
		if e.Kind == graph.EdgeSpawns && e.To == "unresolved::*.ExecuteAsync" {
			r, _ := e.Meta["receiver_type"].(string)
			recvs = append(recvs, r)
		}
	}
	assert.ElementsMatch(t, []string{"Mailer", "Cleaner"}, recvs,
		"each receiver keeps its own spawn edge")
}

// TestCSharpExtractor_HangfireSpawns_TVShowIsAClass: the type-param
// check is an AST fact, not a naming guess — a real class that happens
// to spell T+uppercase keeps its receiver-typed edge.
func TestCSharpExtractor_HangfireSpawns_TVShowIsAClass(t *testing.T) {
	src := []byte(`public class Setup {
    public void Configure() {
        BackgroundJob.Enqueue<TVShow>(x => x.Refresh());
    }
}
`)
	result, err := NewCSharpExtractor().Extract("Setup.cs", src)
	require.NoError(t, err)

	e := findSpawn(result.Edges, "unresolved::*.Refresh")
	require.NotNil(t, e, "TVShow is a class, not a type parameter")
	assert.Equal(t, "TVShow", e.Meta["receiver_type"])
}

// TestCSharpExtractor_HangfireSpawns_NoWrongLambda: an invocationless
// job lambda must not let a later lambda (the cron callback) substitute,
// and a captured-receiver inline call carries no type evidence — both
// emit nothing.
func TestCSharpExtractor_HangfireSpawns_NoWrongLambda(t *testing.T) {
	src := []byte(`public class Setup {
    public void Configure(IJob job, string id) {
        RecurringJob.AddOrUpdate<Svc>(id, x => x.Prop, () => Cron.Daily());
        BackgroundJob.Enqueue(() => job.Run());
    }
}
`)
	result, err := NewCSharpExtractor().Extract("Setup.cs", src)
	require.NoError(t, err)

	for _, e := range result.Edges {
		if e.Kind == graph.EdgeSpawns {
			assert.NotEqual(t, "background", e.Meta["mode"],
				"neither the cron callback nor a captured-receiver call may spawn: %s", e.To)
		}
	}
}

// TestCSharpExtractor_HangfireSpawns_Negatives: same method names on
// other receivers, and non-registration Hangfire calls, emit nothing.
func TestCSharpExtractor_HangfireSpawns_Negatives(t *testing.T) {
	src := []byte(`public class NotJobs {
    public void Run(Cache cache) {
        cache.AddOrUpdate<Entry>("k", x => x.Refresh());
        RecurringJob.RemoveIfExists("sync users");
    }
}
`)
	result, err := NewCSharpExtractor().Extract("NotJobs.cs", src)
	require.NoError(t, err)

	for _, e := range result.Edges {
		if e.Kind == graph.EdgeSpawns {
			assert.NotEqual(t, "background", e.Meta["mode"],
				"non-Hangfire receiver must not emit a background spawn: %s", e.To)
		}
	}
}
