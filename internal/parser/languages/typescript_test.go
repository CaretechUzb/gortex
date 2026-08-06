package languages

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestTSExtractor_Function(t *testing.T) {
	src := []byte(`function greet(name: string): string {
  return "Hello " + name;
}
`)
	e := NewTypeScriptExtractor()
	result, err := e.Extract("app.ts", src)
	require.NoError(t, err)

	funcs := nodesOfKind(result.Nodes, graph.KindFunction)
	require.Len(t, funcs, 1)
	assert.Equal(t, "greet", funcs[0].Name)
}

func TestTSExtractor_ArrowFunction(t *testing.T) {
	src := []byte(`const handler = () => {
  console.log("hello");
};
`)
	e := NewTypeScriptExtractor()
	result, err := e.Extract("app.ts", src)
	require.NoError(t, err)

	funcs := nodesOfKind(result.Nodes, graph.KindFunction)
	require.Len(t, funcs, 1)
	assert.Equal(t, "handler", funcs[0].Name)
}

func TestTSExtractor_Class(t *testing.T) {
	src := []byte(`class UserService {
  getUser(id: string) {
    return {};
  }
}
`)
	e := NewTypeScriptExtractor()
	result, err := e.Extract("service.ts", src)
	require.NoError(t, err)

	types := nodesOfKind(result.Nodes, graph.KindType)
	require.Len(t, types, 1)
	assert.Equal(t, "UserService", types[0].Name)

	methods := nodesOfKind(result.Nodes, graph.KindMethod)
	require.Len(t, methods, 1)
	assert.Equal(t, "getUser", methods[0].Name)
}

func TestTSExtractor_Interface(t *testing.T) {
	src := []byte(`interface Config {
  port: number;
  host: string;
}
`)
	e := NewTypeScriptExtractor()
	result, err := e.Extract("types.ts", src)
	require.NoError(t, err)

	ifaces := nodesOfKind(result.Nodes, graph.KindInterface)
	require.Len(t, ifaces, 1)
	assert.Equal(t, "Config", ifaces[0].Name)
}

func TestTSExtractor_Variables(t *testing.T) {
	src := []byte(`const API_URL = "https://api.example.com";
let count = 0;
export const MAX_RETRIES = 3;
`)
	e := NewTypeScriptExtractor()
	result, err := e.Extract("config.ts", src)
	require.NoError(t, err)

	vars := nodesOfKind(result.Nodes, graph.KindVariable)
	assert.GreaterOrEqual(t, len(vars), 2)
}

func TestTSExtractor_Enum(t *testing.T) {
	src := []byte(`export enum KeybindingWeight {
    EditorCore = 0,
    EditorContrib = 100,
    WorkbenchContrib = 200,
    BuiltinExtension = 300,
    ExternalExtension = 400
}

enum Simple {
    A,
    B,
    C
}
`)
	e := NewTypeScriptExtractor()
	result, err := e.Extract("weights.ts", src)
	require.NoError(t, err)

	// Enums come through as KindType with Meta["kind"]="enum".
	enumNames := map[string]bool{}
	for _, n := range result.Nodes {
		if n.Kind == graph.KindType && n.Meta != nil && n.Meta["kind"] == "enum" {
			enumNames[n.Name] = true
		}
	}
	assert.Equal(t, map[string]bool{"KeybindingWeight": true, "Simple": true}, enumNames)

	// Members are KindVariable with Meta["kind"]="enum_member".
	memberCount := 0
	byReceiver := map[string]int{}
	for _, n := range result.Nodes {
		if n.Kind == graph.KindVariable && n.Meta != nil && n.Meta["kind"] == "enum_member" {
			memberCount++
			if recv, ok := n.Meta["receiver"].(string); ok {
				byReceiver[recv]++
			}
		}
	}
	assert.Equal(t, 8, memberCount) // 5 + 3
	assert.Equal(t, 5, byReceiver["KeybindingWeight"])
	assert.Equal(t, 3, byReceiver["Simple"])
}

func TestTSExtractor_ClassProperties(t *testing.T) {
	src := []byte(`class Server {
    public readonly port: number = 8080;
    private _connections: number = 0;
    protected logger: Logger;

    start() {}
}
`)
	e := NewTypeScriptExtractor()
	result, err := e.Extract("server.ts", src)
	require.NoError(t, err)

	props := map[string]bool{}
	for _, n := range result.Nodes {
		if n.Kind == graph.KindVariable && n.Meta != nil && n.Meta["kind"] == "class_property" {
			props[n.Name] = true
		}
	}
	assert.Equal(t, map[string]bool{"port": true, "_connections": true, "logger": true}, props)
}

func TestTSExtractor_InterfaceMethods(t *testing.T) {
	src := []byte(`interface Repository {
    findById(id: string): User;
    save(user: User): void;
}
`)
	e := NewTypeScriptExtractor()
	result, err := e.Extract("repo.ts", src)
	require.NoError(t, err)

	ifaces := nodesOfKind(result.Nodes, graph.KindInterface)
	require.Len(t, ifaces, 1)
	methods, ok := ifaces[0].Meta["methods"].([]string)
	require.True(t, ok)
	assert.Contains(t, methods, "findById")
	assert.Contains(t, methods, "save")
}

func TestTSExtractor_Imports(t *testing.T) {
	src := []byte(`import { Router } from 'express';
import axios from 'axios';
`)
	e := NewTypeScriptExtractor()
	result, err := e.Extract("app.ts", src)
	require.NoError(t, err)

	imports := edgesOfKind(result.Edges, graph.EdgeImports)
	// Two module-level dependency edges (express, axios) plus one per-binding
	// edge for the named import `{ Router }`. `import axios` is a default
	// import with no named bindings, so it adds only its module edge.
	require.Len(t, imports, 3)
	if importEdge(result, graph.EdgeImports, "unresolved::import::express::Router") == nil {
		t.Error("missing per-binding import edge for Router")
	}
}

func TestTSExtractor_TypeEnv_ExplicitType(t *testing.T) {
	src := []byte(`
class UserService {
  save() {}
}

function main() {
  const svc: UserService = new UserService();
  svc.save();
}
`)
	e := NewTypeScriptExtractor()
	result, err := e.Extract("app.ts", src)
	require.NoError(t, err)

	calls := edgesOfKind(result.Edges, graph.EdgeCalls)
	var saveCall *graph.Edge
	for _, c := range calls {
		if strings.HasSuffix(c.To, "save") {
			saveCall = c
			break
		}
	}
	require.NotNil(t, saveCall, "expected a call edge to save")
	require.NotNil(t, saveCall.Meta, "expected Meta on save call edge")
	assert.Equal(t, "UserService", saveCall.Meta["receiver_type"])
}

func TestTSExtractor_TypeEnv_NewExpression(t *testing.T) {
	src := []byte(`
class Client {
  connect() {}
}

function main() {
  const client = new Client();
  client.connect();
}
`)
	e := NewTypeScriptExtractor()
	result, err := e.Extract("app.ts", src)
	require.NoError(t, err)

	calls := edgesOfKind(result.Edges, graph.EdgeCalls)
	var connectCall *graph.Edge
	for _, c := range calls {
		if strings.HasSuffix(c.To, "connect") {
			connectCall = c
			break
		}
	}
	require.NotNil(t, connectCall)
	require.NotNil(t, connectCall.Meta)
	assert.Equal(t, "Client", connectCall.Meta["receiver_type"])
}

func TestTSExtractor_TypeEnv_Unknown(t *testing.T) {
	src := []byte(`
function getService() { return null; }

function main() {
  const svc = getService();
  svc.process();
}
`)
	e := NewTypeScriptExtractor()
	result, err := e.Extract("app.ts", src)
	require.NoError(t, err)

	calls := edgesOfKind(result.Edges, graph.EdgeCalls)
	var processCall *graph.Edge
	for _, c := range calls {
		if strings.HasSuffix(c.To, "process") {
			processCall = c
			break
		}
	}
	require.NotNil(t, processCall)
	assert.NotContains(t, processCall.Meta, "receiver_type", "unknown type should not produce a receiver_type hint")
}

func TestTSExtractor_TypeEnv_Chain(t *testing.T) {
	src := []byte(`
class Connection {
  query(): Result {
    return new Result();
  }
}

class Result {
  first(): User {
    return new User();
  }
}

class User {
  save() {}
}

function main() {
  const conn = new Connection();
  conn.query().first().save();
}
`)
	e := NewTypeScriptExtractor()
	result, err := e.Extract("app.ts", src)
	require.NoError(t, err)

	calls := edgesOfKind(result.Edges, graph.EdgeCalls)
	var saveCall *graph.Edge
	for _, c := range calls {
		if strings.HasSuffix(c.To, "save") {
			saveCall = c
			break
		}
	}
	require.NotNil(t, saveCall, "expected a call edge to save")
	require.NotNil(t, saveCall.Meta, "expected Meta on chained save call edge")
	assert.Equal(t, "User", saveCall.Meta["receiver_type"])
}

func TestTSExtractor_MethodReceiver(t *testing.T) {
	src := []byte(`
class Server {
  start() {}
  stop() {}
}
`)
	e := NewTypeScriptExtractor()
	result, err := e.Extract("server.ts", src)
	require.NoError(t, err)

	methods := nodesOfKind(result.Nodes, graph.KindMethod)
	require.Len(t, methods, 2)
	for _, m := range methods {
		assert.Equal(t, "Server", m.Meta["receiver"], "method %s should have receiver Server", m.Name)
	}
}

func TestTSExtractor_NestJsUseGuardsDispatch(t *testing.T) {
	// @UseGuards on a controller method should emit a synthetic call edge
	// from the handler to the guard's canActivate method. This is the one
	// DI shape that has no explicit call site anywhere in source — the
	// framework dispatches to the guard based on decorator metadata.
	src := []byte(`
import { Controller, Post, UseGuards } from '@nestjs/common';
import { AuthGuard } from './auth.guard';

@Controller('x')
export class XController {
  @Post()
  @UseGuards(AuthGuard)
  async handle(): Promise<void> {}
}
`)
	e := NewTypeScriptExtractor()
	result, err := e.Extract("x.controller.ts", src)
	require.NoError(t, err)

	var dispatch *graph.Edge
	for _, ed := range edgesOfKind(result.Edges, graph.EdgeCalls) {
		if ed.Meta == nil {
			continue
		}
		if d, ok := ed.Meta["dispatch_decorator"].(string); ok && d == "UseGuards" {
			dispatch = ed
			break
		}
	}
	require.NotNil(t, dispatch, "expected a dispatch edge tagged UseGuards")
	assert.Equal(t, "x.controller.ts::XController.handle", dispatch.From)
	assert.Equal(t, "unresolved::*.canActivate", dispatch.To)
	assert.Equal(t, "AuthGuard", dispatch.Meta["receiver_type"])
}

func TestTSExtractor_NestJsUseInterceptorsDispatch(t *testing.T) {
	// Same shape for @UseInterceptors → intercept.
	src := []byte(`
import { Controller, Get, UseInterceptors } from '@nestjs/common';
import { CacheInterceptor } from './cache.interceptor';

@Controller('x')
export class XController {
  @Get()
  @UseInterceptors(CacheInterceptor)
  async handle(): Promise<void> {}
}
`)
	e := NewTypeScriptExtractor()
	result, err := e.Extract("x.controller.ts", src)
	require.NoError(t, err)

	var dispatch *graph.Edge
	for _, ed := range edgesOfKind(result.Edges, graph.EdgeCalls) {
		if ed.Meta == nil {
			continue
		}
		if d, ok := ed.Meta["dispatch_decorator"].(string); ok && d == "UseInterceptors" {
			dispatch = ed
			break
		}
	}
	require.NotNil(t, dispatch)
	assert.Equal(t, "unresolved::*.intercept", dispatch.To)
	assert.Equal(t, "CacheInterceptor", dispatch.Meta["receiver_type"])
}

func TestTSExtractor_NestJsMultipleGuards(t *testing.T) {
	// @UseGuards(A, B) must emit one edge per class argument.
	src := []byte(`
import { Controller, Get, UseGuards } from '@nestjs/common';

@Controller('x')
export class XController {
  @Get()
  @UseGuards(Auth, Role)
  async handle(): Promise<void> {}
}
`)
	e := NewTypeScriptExtractor()
	result, err := e.Extract("x.controller.ts", src)
	require.NoError(t, err)

	var count int
	seen := map[string]bool{}
	for _, ed := range edgesOfKind(result.Edges, graph.EdgeCalls) {
		if ed.Meta == nil {
			continue
		}
		if d, _ := ed.Meta["dispatch_decorator"].(string); d == "UseGuards" {
			count++
			seen[ed.Meta["receiver_type"].(string)] = true
		}
	}
	assert.Equal(t, 2, count, "expected one edge per guard class")
	assert.True(t, seen["Auth"] && seen["Role"])
}

func TestTSExtractor_NestJsNonDispatchDecoratorIgnored(t *testing.T) {
	// @Post / @Get / @Injectable / custom decorators must not produce
	// dispatch edges — only the explicit @Use* set above does.
	src := []byte(`
import { Controller, Get, Post } from '@nestjs/common';

@Controller('x')
export class XController {
  @Get()
  @Post('send')
  async handle(): Promise<void> {}
}
`)
	e := NewTypeScriptExtractor()
	result, err := e.Extract("x.controller.ts", src)
	require.NoError(t, err)

	for _, ed := range edgesOfKind(result.Edges, graph.EdgeCalls) {
		if ed.Meta == nil {
			continue
		}
		if _, ok := ed.Meta["dispatch_decorator"]; ok {
			t.Fatalf("unexpected dispatch edge: %+v", ed)
		}
	}
}

func TestTSExtractor_NestJsModuleUseClassBinding(t *testing.T) {
	// @Module({ providers: [{ provide: Abstract, useClass: Concrete }] })
	// should produce a Provides edge from the module to Concrete, tagged
	// provides_for: Abstract, so the resolver can pick Concrete for
	// receiver_type=Abstract calls.
	src := []byte(`
import { Module } from '@nestjs/common';
import { Notifier } from './notifier';
import { EmailNotifier } from './email';

@Module({
  providers: [
    { provide: Notifier, useClass: EmailNotifier },
  ],
})
export class NotificationsModule {}
`)
	e := NewTypeScriptExtractor()
	result, err := e.Extract("notif.module.ts", src)
	require.NoError(t, err)

	var binding *graph.Edge
	for _, ed := range edgesOfKind(result.Edges, graph.EdgeProvides) {
		if ed.Meta == nil {
			continue
		}
		if b, _ := ed.Meta["binding"].(string); b == "useClass" {
			binding = ed
			break
		}
	}
	require.NotNil(t, binding, "expected a useClass EdgeProvides")
	assert.Equal(t, "notif.module.ts::NotificationsModule", binding.From)
	assert.Equal(t, "Notifier", binding.Meta["provides_for"])
	assert.Contains(t, binding.To, "EmailNotifier")
}

func TestTSExtractor_NestJsModuleSkipsNonUseClass(t *testing.T) {
	// useValue / bare-class providers must not produce useClass edges —
	// those are the @Inject(TOKEN) feature's territory, handled separately.
	src := []byte(`
import { Module } from '@nestjs/common';

@Module({
  providers: [
    { provide: 'TOKEN', useValue: 42 },
    BareService,
  ],
})
export class M {}
`)
	e := NewTypeScriptExtractor()
	result, err := e.Extract("m.ts", src)
	require.NoError(t, err)

	for _, ed := range edgesOfKind(result.Edges, graph.EdgeProvides) {
		if ed.Meta == nil {
			continue
		}
		if b, _ := ed.Meta["binding"].(string); b == "useClass" {
			t.Fatalf("unexpected useClass edge: %+v", ed)
		}
	}
}

func TestTSExtractor_NestJsInjectConsumer(t *testing.T) {
	// @Inject(TOKEN) in a constructor param emits an EdgeConsumes from
	// the declaring class to the token, tagged via:"@Inject". Edge is
	// unresolved::<tok> — the resolver walks the graph to find the
	// matching `export const` node.
	src := []byte(`
import { Inject, Injectable } from '@nestjs/common';
import { DB_URL } from './tokens';

@Injectable()
export class ConfigService {
  constructor(@Inject(DB_URL) private readonly dbUrl: string) {}
}
`)
	e := NewTypeScriptExtractor()
	result, err := e.Extract("config.service.ts", src)
	require.NoError(t, err)

	var consume *graph.Edge
	for _, ed := range edgesOfKind(result.Edges, graph.EdgeConsumes) {
		if ed.Meta == nil {
			continue
		}
		if v, _ := ed.Meta["via"].(string); v == "@Inject" {
			consume = ed
			break
		}
	}
	require.NotNil(t, consume, "expected EdgeConsumes from @Inject")
	assert.Equal(t, "config.service.ts::ConfigService", consume.From)
	assert.Equal(t, "unresolved::DB_URL", consume.To)
	assert.Equal(t, "DB_URL", consume.Meta["di_token"])
}

func TestTSExtractor_NestJsUseValueProvider(t *testing.T) {
	// Non-useClass providers (useValue, useFactory, useExisting) emit
	// an EdgeProvides from the module to the token itself, so
	// find_usages on the token surfaces the module as a provider.
	src := []byte(`
import { Module } from '@nestjs/common';
import { DB_URL } from './tokens';

@Module({
  providers: [
    { provide: DB_URL, useValue: 'postgres://localhost' },
  ],
})
export class ConfigModule {}
`)
	e := NewTypeScriptExtractor()
	result, err := e.Extract("config.module.ts", src)
	require.NoError(t, err)

	var provide *graph.Edge
	for _, ed := range edgesOfKind(result.Edges, graph.EdgeProvides) {
		if ed.Meta == nil {
			continue
		}
		if b, _ := ed.Meta["binding"].(string); b == "useValue" {
			provide = ed
			break
		}
	}
	require.NotNil(t, provide, "expected a useValue EdgeProvides")
	assert.Equal(t, "config.module.ts::ConfigModule", provide.From)
	assert.Equal(t, "unresolved::DB_URL", provide.To)
	assert.Equal(t, "DB_URL", provide.Meta["di_token"])
}

func TestTSExtractor_NestJsStringLiteralToken(t *testing.T) {
	// `{ provide: 'X', useValue: ... }` — string-literal token form
	// should be treated identically to the identifier form.
	src := []byte(`
import { Module } from '@nestjs/common';

@Module({
  providers: [
    { provide: 'CACHE_TTL', useValue: 300 },
  ],
})
export class M {}
`)
	e := NewTypeScriptExtractor()
	result, err := e.Extract("m.ts", src)
	require.NoError(t, err)

	var seen bool
	for _, ed := range edgesOfKind(result.Edges, graph.EdgeProvides) {
		if ed.Meta == nil {
			continue
		}
		if tok, _ := ed.Meta["di_token"].(string); tok == "CACHE_TTL" {
			seen = true
			break
		}
	}
	assert.True(t, seen, "string-literal token should produce a binding edge")
}

func TestTSExtractor_NestJsFieldInject(t *testing.T) {
	// Field-level @Inject: decorators are CHILDREN of public_field_definition
	// in tree-sitter-typescript (unlike method decorators which are siblings).
	// Both explicit-token and implicit (paren-only) forms should work.
	src := []byte(`
import { Inject, Injectable } from '@nestjs/common';

@Injectable()
export class AuditService {
  @Inject('DB_URL')
  private readonly dbUrl!: string;

  @Inject(LOGGER)
  private readonly log!: Logger;
}
`)
	e := NewTypeScriptExtractor()
	result, err := e.Extract("audit.ts", src)
	require.NoError(t, err)

	var tokens []string
	for _, ed := range edgesOfKind(result.Edges, graph.EdgeConsumes) {
		if ed.Meta == nil {
			continue
		}
		if v, _ := ed.Meta["via"].(string); v == "@Inject" {
			tokens = append(tokens, ed.Meta["di_token"].(string))
		}
	}
	assert.Contains(t, tokens, "DB_URL")
	assert.Contains(t, tokens, "LOGGER")
}

func TestTSExtractor_NestJsFieldInject_ImplicitToken(t *testing.T) {
	// `@Inject()` with no argument — currently a no-op because the implicit
	// form needs the field's type annotation to be the token, and we don't
	// wire that path yet. Documented here so the behaviour is explicit and
	// a future change that adds implicit-token support has a test to flip.
	src := []byte(`
import { Inject } from '@nestjs/common';

export class X {
  @Inject()
  private readonly foo!: Foo;
}
`)
	e := NewTypeScriptExtractor()
	result, err := e.Extract("x.ts", src)
	require.NoError(t, err)

	for _, ed := range edgesOfKind(result.Edges, graph.EdgeConsumes) {
		if ed.Meta != nil {
			if v, _ := ed.Meta["via"].(string); v == "@Inject" {
				t.Fatalf("unexpected edge from implicit @Inject(): %+v", ed)
			}
		}
	}
}

func TestTSExtractor_NestJsDynamicModule(t *testing.T) {
	// static forRoot(...) returns a DynamicModule whose providers array
	// must be extracted identically to a @Module({ providers: [...] }).
	// origin meta surfaces the method name so agents can tell the
	// binding came from a dynamic module, not a decorator.
	src := []byte(`
import { DynamicModule, Module } from '@nestjs/common';
import { CACHE_TTL } from './tokens';

@Module({})
export class CacheModule {
  static forRoot(ttl: number): DynamicModule {
    return {
      module: CacheModule,
      providers: [
        { provide: CACHE_TTL, useValue: ttl },
      ],
      exports: [],
    };
  }
}
`)
	e := NewTypeScriptExtractor()
	result, err := e.Extract("cache.module.ts", src)
	require.NoError(t, err)

	var found *graph.Edge
	for _, ed := range edgesOfKind(result.Edges, graph.EdgeProvides) {
		if ed.Meta == nil {
			continue
		}
		if origin, _ := ed.Meta["origin"].(string); origin == "forRoot" {
			found = ed
			break
		}
	}
	require.NotNil(t, found, "expected a forRoot-origin Provides edge")
	assert.Equal(t, "CACHE_TTL", found.Meta["di_token"])
	assert.Equal(t, "useValue", found.Meta["binding"])
}

func TestTSExtractor_NestJsDynamicModule_IgnoresInstanceMethod(t *testing.T) {
	// Non-static forRoot (unlikely but possible in pathological code)
	// must not be treated as a dynamic-module factory — NestJS only
	// calls these at module registration, where they have to be static.
	src := []byte(`
import { Module } from '@nestjs/common';

@Module({})
export class X {
  forRoot() {
    return {
      providers: [{ provide: 'SHOULDNT_EMIT', useValue: 1 }],
    };
  }
}
`)
	e := NewTypeScriptExtractor()
	result, err := e.Extract("x.ts", src)
	require.NoError(t, err)
	for _, ed := range edgesOfKind(result.Edges, graph.EdgeProvides) {
		if ed.Meta != nil {
			if tok, _ := ed.Meta["di_token"].(string); tok == "SHOULDNT_EMIT" {
				t.Fatal("instance forRoot should not produce bindings")
			}
		}
	}
}

func TestTSExtractor_AngularInjectFunction(t *testing.T) {
	// Angular's `inject()` function-style DI inside a class field
	// initializer should type `this.<field>` as the target class so
	// the resolver can route method calls correctly.
	src := []byte(`
import { Injectable, inject } from '@angular/core';
import { UsersService } from './users.service';

@Injectable({ providedIn: 'root' })
export class AuthService {
  private readonly users = inject(UsersService);
  findUser(id: string) {
    return this.users.findOne(id);
  }
}
`)
	e := NewTypeScriptExtractor()
	result, err := e.Extract("auth.service.ts", src)
	require.NoError(t, err)

	// The findOne call should carry receiver_type=UsersService on its
	// edge Meta, which the resolver uses to pick UsersService.findOne
	// even if other classes in the repo define a findOne too.
	var call *graph.Edge
	for _, ed := range edgesOfKind(result.Edges, graph.EdgeCalls) {
		if ed.Meta != nil {
			if rt, _ := ed.Meta["receiver_type"].(string); rt == "UsersService" {
				call = ed
				break
			}
		}
	}
	require.NotNil(t, call, "expected a call with receiver_type=UsersService")
}

func TestTSExtractor_AngularInjectOnlyIdentifierArg(t *testing.T) {
	// `inject()` with a non-identifier argument (e.g. `inject(TOKEN)` where
	// TOKEN is imported as a non-class value) should still be accepted —
	// our extractor only gates on the argument being an identifier, not
	// on its being a class. A non-identifier argument yields no type.
	src := []byte(`
import { inject } from '@angular/core';

export class X {
  private thing = inject(something.else);
  foo() { return this.thing; }
}
`)
	e := NewTypeScriptExtractor()
	result, err := e.Extract("x.ts", src)
	require.NoError(t, err)
	// Should not panic; no assertion needed beyond parse cleanliness.
	_ = result
}

func TestTSExtractor_DocAndVisibility(t *testing.T) {
	src := []byte(`/**
 * Greets the world.
 */
export function hello() {}

/** internal helper. */
function helper() {}

/**
 * Server class.
 *
 * @remarks fancy.
 */
export class Server {
  private secret = "x";

  /** start it up. */
  public start() {}
}
`)
	e := NewTypeScriptExtractor()
	result, err := e.Extract("server.ts", src)
	require.NoError(t, err)

	byID := map[string]*graph.Node{}
	for _, n := range result.Nodes {
		byID[n.ID] = n
	}

	hello := byID["server.ts::hello"]
	require.NotNil(t, hello)
	if hello.Meta["doc"] != "Greets the world." {
		t.Fatalf("hello.doc = %q", hello.Meta["doc"])
	}
	if hello.Meta["visibility"] != "public" {
		t.Fatalf("hello.visibility = %q", hello.Meta["visibility"])
	}

	helper := byID["server.ts::helper"]
	require.NotNil(t, helper)
	if helper.Meta["visibility"] != "private" {
		t.Fatalf("helper.visibility = %q", helper.Meta["visibility"])
	}
	if helper.Meta["doc"] != "internal helper." {
		t.Fatalf("helper.doc = %q", helper.Meta["doc"])
	}

	server := byID["server.ts::Server"]
	require.NotNil(t, server)
	if server.Meta["visibility"] != "public" {
		t.Fatalf("Server.visibility = %q", server.Meta["visibility"])
	}
	if server.Meta["doc"] != "Server class." {
		t.Fatalf("Server.doc = %q", server.Meta["doc"])
	}

	start := byID["server.ts::Server.start"]
	require.NotNil(t, start)
	if start.Meta["visibility"] != "public" {
		t.Fatalf("Server.start.visibility = %q", start.Meta["visibility"])
	}
	if start.Meta["doc"] != "start it up." {
		t.Fatalf("Server.start.doc = %q", start.Meta["doc"])
	}

	secret := byID["server.ts::Server.secret"]
	require.NotNil(t, secret)
	if secret.Meta["visibility"] != "private" {
		t.Fatalf("Server.secret.visibility = %q", secret.Meta["visibility"])
	}
}

func TestTSExtractor_AnnotationEdges(t *testing.T) {
	src := []byte(`@Controller("/users")
export class UsersController {
  @Get("/:id")
  @UseGuards(AuthGuard)
  findOne() {}

  @Inject(USER_TOKEN)
  private userService: UserService;
}
`)
	e := NewTypeScriptExtractor()
	result, err := e.Extract("users.ts", src)
	require.NoError(t, err)

	// Annotation nodes deduped per name.
	annNodes := map[string]*graph.Node{}
	for _, n := range result.Nodes {
		if v, _ := n.Meta["kind"].(string); v == "annotation" {
			annNodes[n.ID] = n
		}
	}
	for _, want := range []string{"Controller", "Get", "UseGuards", "Inject"} {
		id := "annotation::typescript::" + want
		if _, ok := annNodes[id]; !ok {
			t.Fatalf("missing annotation node %s", id)
		}
	}

	// Edges from concrete symbols to annotation nodes.
	edges := map[string][]string{}
	argsByEdge := map[string]string{}
	for _, e := range result.Edges {
		if e.Kind != graph.EdgeAnnotated {
			continue
		}
		edges[e.From] = append(edges[e.From], e.To)
		if v, ok := e.Meta["args"].(string); ok {
			argsByEdge[e.From+"->"+e.To] = v
		}
	}

	classFromID := "users.ts::UsersController"
	if !contains(edges[classFromID], "annotation::typescript::Controller") {
		t.Fatalf("missing Controller edge from class, got %v", edges[classFromID])
	}
	if argsByEdge[classFromID+"->annotation::typescript::Controller"] != `"/users"` {
		t.Fatalf("Controller args = %q", argsByEdge[classFromID+"->annotation::typescript::Controller"])
	}

	methodID := "users.ts::UsersController.findOne"
	if !contains(edges[methodID], "annotation::typescript::Get") {
		t.Fatalf("missing @Get on findOne, got %v", edges[methodID])
	}
	if !contains(edges[methodID], "annotation::typescript::UseGuards") {
		t.Fatalf("missing @UseGuards on findOne, got %v", edges[methodID])
	}

	fieldID := "users.ts::UsersController.userService"
	if !contains(edges[fieldID], "annotation::typescript::Inject") {
		t.Fatalf("missing @Inject on field, got %v", edges[fieldID])
	}
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

func TestTSExtractor_GenericTypeParams(t *testing.T) {
	src := []byte(`export function map<T, R extends string>(in: T[], f: (t: T) => R): R[] {
  return in.map(f);
}

export class Cache<K extends string, V = unknown> {}
`)
	e := NewTypeScriptExtractor()
	result, err := e.Extract("u.ts", src)
	require.NoError(t, err)

	byID := map[string]*graph.Node{}
	for _, n := range result.Nodes {
		byID[n.ID] = n
	}

	mapFn := byID["u.ts::map"]
	require.NotNil(t, mapFn)
	tp, _ := mapFn.Meta["type_params"].([]map[string]string)
	require.Len(t, tp, 2)
	assert.Equal(t, "T", tp[0]["name"])
	assert.Equal(t, "R", tp[1]["name"])
	assert.Equal(t, "string", tp[1]["bound"])

	cache := byID["u.ts::Cache"]
	require.NotNil(t, cache)
	tp2, _ := cache.Meta["type_params"].([]map[string]string)
	require.Len(t, tp2, 2)
	assert.Equal(t, "K", tp2[0]["name"])
	assert.Equal(t, "string", tp2[0]["bound"])
	assert.Equal(t, "V", tp2[1]["name"])
	assert.Equal(t, "unknown", tp2[1]["default"])
}

func TestTSExtractor_ImportNodes(t *testing.T) {
	src := []byte(`import { Component } from "@nestjs/common";
import * as fs from "fs";
import { foo } from "./local";
`)
	e := NewTypeScriptExtractor()
	result, err := e.Extract("a.ts", src)
	require.NoError(t, err)

	imports := nodesOfKind(result.Nodes, graph.KindImport)
	require.GreaterOrEqual(t, len(imports), 3)

	byID := map[string]*graph.Node{}
	for _, n := range imports {
		byID[n.ID] = n
	}

	nest := byID[`a.ts::import::@nestjs/common`]
	require.NotNil(t, nest)
	assert.Equal(t, true, nest.Meta["is_external"])

	local := byID[`a.ts::import::./local`]
	require.NotNil(t, local)
	assert.Equal(t, false, local.Meta["is_external"])
}

// TestInterfaceMemberTypeReferences is part of the C10 set: a TS interface
// references the types used by its property and method-return members.
func TestInterfaceMemberTypeReferences(t *testing.T) {
	src := []byte("interface Svc {\n  repo: Repository;\n  find(id: string): User;\n}\n")
	res, err := NewTypeScriptExtractor().Extract("m.ts", src)
	require.NoError(t, err)
	refs := map[string]string{}
	for _, e := range res.Edges {
		if e.Kind == graph.EdgeReferences && e.From == "m.ts::Svc" {
			ctx, _ := e.Meta["ref_context"].(string)
			refs[e.To] = ctx
		}
	}
	assert.Equal(t, "field", refs["unresolved::Repository"], "property type ref")
	assert.Equal(t, "return_type", refs["unresolved::User"], "method return type ref")
}

// TestInstantiateNewExpression is part of the C10 set: `new Foo()` produces a
// typed instantiation edge attributed to the enclosing function.
func TestInstantiateNewExpression(t *testing.T) {
	src := []byte("class Foo {}\nfunction make() {\n  const a = new Foo();\n  return a;\n}\n")
	res, err := NewTypeScriptExtractor().Extract("m.ts", src)
	require.NoError(t, err)
	var sawInst bool
	for _, e := range res.Edges {
		if e.Kind == graph.EdgeInstantiates && e.From == "m.ts::make" && e.To == "unresolved::Foo" {
			sawInst = true
		}
	}
	assert.True(t, sawInst, "new Foo() should emit an instantiation edge from make")
}

// TestTemplateExprCallMining is part of the C10 set: a call inside a template
// substitution is mined as a call, a tagged-template tag is a call (not a
// function-as-value), and both are attributed to the enclosing function.
func TestTemplateExprCallMining(t *testing.T) {
	src := []byte("function tag(s){return s}\nfunction y(x){return x}\n" +
		"function f() {\n  const a = `${y(1)} done`;\n  const b = tag`x ${y(2)}`;\n}\n")
	res, err := NewTypeScriptExtractor().Extract("m.ts", src)
	require.NoError(t, err)
	var callY, callTag, fnvalTag bool
	for _, e := range res.Edges {
		if e.Kind == graph.EdgeCalls && e.From == "m.ts::f" && e.To == "unresolved::y" {
			callY = true
		}
		if e.Kind == graph.EdgeCalls && e.From == "m.ts::f" && e.To == "unresolved::tag" {
			callTag = true
		}
		if e.Meta != nil {
			if v, _ := e.Meta["via"].(string); v == "callback_candidate" {
				if nm, _ := e.Meta["fn_value_name"].(string); nm == "tag" {
					fnvalTag = true
				}
			}
		}
	}
	assert.True(t, callY, "a call inside a template substitution is mined")
	assert.True(t, callTag, "a tagged-template tag is a call")
	assert.False(t, fnvalTag, "a tagged-template tag is NOT a function-as-value candidate")
}

// Curried const-arrow middleware with generics — the store-middleware shape:
// the const binding extracts as a function with its parameters, exported type
// aliases extract as types, and a nested const-arrow helper inside the body
// extracts too. Pinned so selection-layer work can rely on these symbols
// existing; the historical miss on this shape was emission, not extraction.
func TestTypeScriptCurriedConstArrowMiddlewareExtraction(t *testing.T) {
	src := []byte(`export type StoreSetStateWithAction<T> = (state: T, replace?: boolean, action?: string) => void

const middlewareImpl: MiddlewareImpl =
  (fn, middlewareOptions = {}) =>
  (set, get, api) => {
    const setStateTracked: SetStateTracked = (...a) => set(...a)
    return fn(setStateTracked, get, api)
  }
`)
	e := NewTypeScriptExtractor()
	result, err := e.Extract("mw/middleware.ts", src)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, n := range result.Nodes {
		ids[n.ID] = string(n.Kind)
	}
	for id, kind := range map[string]string{
		"mw/middleware.ts::StoreSetStateWithAction":                  "type",
		"mw/middleware.ts::middlewareImpl":                           "function",
		"mw/middleware.ts::middlewareImpl#param:middlewareOptions@1": "param",
		"mw/middleware.ts::setStateTracked":                          "function",
	} {
		if ids[id] != kind {
			t.Fatalf("%s = %q, want %q (all: %v)", id, ids[id], kind, ids)
		}
	}
}

func TestTSExtractor_NamedFunctionExpression(t *testing.T) {
	cases := []struct {
		name    string
		file    string
		src     string
		want    string
		wantSig string
	}{
		{
			name:    "default export logical operand",
			file:    "adapter.ts",
			src:     "export default isHttpAdapterSupported && function httpAdapter(config: Config) {\n  return config;\n};\n",
			want:    "httpAdapter",
			wantSig: "function httpAdapter()",
		},
		{
			name:    "const initializer",
			file:    "wrap.ts",
			src:     "const wrapped = function inner(a: number) {\n  return a;\n};\n",
			want:    "inner",
			wantSig: "function inner()",
		},
		{
			name:    "var initializer",
			file:    "legacy.ts",
			src:     "var legacy = function legacyImpl() {\n  return 1;\n};\n",
			want:    "legacyImpl",
			wantSig: "function legacyImpl()",
		},
		{
			name:    "object property value",
			file:    "api.ts",
			src:     "const api = {\n  run: function runTask() {\n    return 1;\n  },\n};\n",
			want:    "runTask",
			wantSig: "function runTask()",
		},
		{
			name:    "module.exports assignment",
			file:    "cjs.ts",
			src:     "module.exports = function createServer(opts: Options) {\n  return opts;\n};\n",
			want:    "createServer",
			wantSig: "function createServer()",
		},
		{
			name:    "tsx grammar",
			file:    "view.tsx",
			src:     "const view = function renderView() {\n  return null;\n};\n",
			want:    "renderView",
			wantSig: "function renderView()",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := NewTypeScriptExtractor()
			result, err := e.Extract(tc.file, []byte(tc.src))
			require.NoError(t, err)

			fn := nodeNamed(t, result.Nodes, graph.KindFunction, tc.want)
			assert.Equal(t, tc.file, fn.FilePath)
			assert.Equal(t, "typescript", fn.Language)
			assert.Greater(t, fn.EndLine, fn.StartLine)
			assert.Equal(t, tc.wantSig, fn.Meta["signature"])

			var defined bool
			for _, ed := range edgesOfKind(result.Edges, graph.EdgeDefines) {
				if ed.From == tc.file && ed.To == fn.ID {
					defined = true
				}
			}
			assert.True(t, defined, "file must define %s", fn.ID)
		})
	}
}

func TestTSExtractor_AnonymousFunctionExpressionUnemitted(t *testing.T) {
	src := []byte(`const anon = function () {
  return 1;
};
module.exports = function () {
  return 2;
};
`)
	e := NewTypeScriptExtractor()
	result, err := e.Extract("anon.ts", src)
	require.NoError(t, err)
	assert.Empty(t, nodesOfKind(result.Nodes, graph.KindFunction))
}
