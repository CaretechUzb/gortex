# Cross-repo API contracts

Gortex detects API contracts across repos and matches providers to consumers:

```bash
# After indexing, contracts are auto-detected
gortex track .

# Via MCP tools
contracts                        # list all detected contracts (default action)
contracts {action: "check"}      # find mismatches and orphans
```

| Contract type | Detection | Provider | Consumer |
|--------------|-----------|----------|----------|
| **HTTP routes** | Framework annotations (gin, Express, FastAPI, Spring, etc.) | Route handler | HTTP client calls (fetch, http.Get) |
| **gRPC** | Proto service definitions | Service RPC | Client stub calls |
| **GraphQL** | Schema type/field definitions | Schema | Query/mutation strings |
| **Message topics** | Pub/sub patterns across Kafka, RabbitMQ, NATS, and Redis (`KindTopic` nodes, `produces_topic` / `consumes_topic` edges); dynamic topic names suppressed | Publish calls | Subscribe calls |
| **WebSocket** | Event emit/listen patterns | `emit()` | `on()` |
| **Env vars** | `os.Getenv`, `process.env`, `.env` files, Terraform `aws_lambda_function` `environment.variables` | `Setenv` / `.env` / Terraform `environment.variables` key | `Getenv` / `process.env` |
| **OpenAPI** | Swagger/OpenAPI spec files | Spec paths | (linked to HTTP routes) |
| **Temporal workflows** | Go SDK `worker.RegisterActivity(WithOptions)` / `RegisterActivities` / Java `@ActivityInterface` / `@WorkflowInterface` annotations | Activity / workflow function (carries `temporal_role` Meta) | `workflow.ExecuteActivity` / `ExecuteChildWorkflow` / `client.ExecuteWorkflow` / handler & signal/query calls |

Contracts are normalized to canonical IDs (e.g., `http::GET::/api/users/{id}`) and matched across repos to detect orphan providers/consumers and mismatches.

## Temporal edge taxonomy

The Go and Java extractors tag Temporal call sites with a `via` Meta value on the `EdgeCalls` edge (plus `temporal_kind` and `temporal_name`); `ResolveTemporalCalls` rewrites the resolvable ones (`temporal.stub` / `temporal.start`) to the registered handler / workflow node. Because they are ordinary `EdgeCalls`, `find_usages` / `get_callers` / `explain_change_impact` traverse them with no temporal-specific code.

| `via` | Direction | Emitted from | `temporal_kind` | Resolved? |
|-------|-----------|--------------|-----------------|-----------|
| `temporal.register` | provider tag | `worker.RegisterActivity(WithOptions)` / `RegisterWorkflow(WithOptions)` / `RegisterActivities` | `activity` / `workflow` | indexed, not rewritten |
| `temporal.stub` | workflow → activity / child-workflow | `workflow.ExecuteActivity` / `ExecuteLocalActivity` / `ExecuteChildWorkflow` | `activity` / `workflow` | yes → registered handler |
| `temporal.start` | service → workflow | `client.ExecuteWorkflow` / `SignalWithStartWorkflow` | `workflow` | yes → registered workflow |
| `temporal.handler` | workflow exposes | `workflow.SetQueryHandler` / `GetSignalChannel` / `SetUpdateHandler` (+`WithOptions`) | `query` / `signal` / `update` | provider edge |
| `temporal.signal-send` | sender → running workflow | `workflow.SignalExternalWorkflow` / `client.SignalWorkflow` | `signal` | consumer edge |
| `temporal.query-call` | caller → running workflow | `client.QueryWorkflow` | `query` | consumer edge |

Extra Meta on these edges: `temporal_registered_name` (the `RegisterOptions{Name}` override that is the actual dispatch key), `temporal_register_plural` (a `RegisterActivities(&Struct{})` registration whose exported methods are each promoted), and `temporal_name_origin=env_default` (a dispatch name resolved from an env-var-with-literal-default, landed at the speculative tier). Node roles are stamped as `temporal_role` (`activity` / `workflow` / `activity_interface` / `workflow_interface` / `signal` / `query` / `update`). Aliased `import wf "go.temporal.io/sdk/workflow"` receivers are canonicalised before detection.
