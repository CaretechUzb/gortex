package frameworkgate

import (
	"reflect"
	"testing"
)

// The most important property in this package: anything that looks like
// "no configuration" must admit every framework. Resolving an absent or
// blank allow-list to "nothing runs" would silently strip every framework
// edge from the graph.
func TestUnsetAllowsEverything(t *testing.T) {
	cases := map[string]Set{
		"zero value":    {},
		"nil slice":     New(nil),
		"empty slice":   New([]string{}),
		"blank entries": New([]string{"", "   ", "\t"}),
	}
	for name, s := range cases {
		if !s.Allows("django") || !s.Allows("anything-at-all") {
			t.Errorf("%s: must allow every framework", name)
		}
		if s.Configured() {
			t.Errorf("%s: must not report Configured", name)
		}
	}
}

func TestNew_ExactMatch(t *testing.T) {
	s := New([]string{"odoo", "django"})
	for _, name := range []string{"odoo", "django"} {
		if !s.Allows(name) {
			t.Errorf("Allows(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"drupal", "celery-dispatch", "odoox", "djang"} {
		if s.Allows(name) {
			t.Errorf("Allows(%q) = true, want false", name)
		}
	}
}

func TestNew_PrefixWildcard(t *testing.T) {
	s := New([]string{"godot*"})
	for _, name := range []string{"godot", "godot-autoload", "godot-preload-alias"} {
		if !s.Allows(name) {
			t.Errorf("Allows(%q) = false, want true", name)
		}
	}
	if s.Allows("gin-middleware") {
		t.Error("prefix godot* must not admit gin-middleware")
	}
}

// A framework name contains "-", "." and "/", and those must never behave
// as glob metacharacters — the matcher is deliberately not path.Match.
func TestNew_NoGlobMetacharacters(t *testing.T) {
	s := New([]string{"net/http", "a-b.c"})
	if !s.Allows("net/http") || !s.Allows("a-b.c") {
		t.Error("literal names must match themselves")
	}
	if s.Allows("aXbYc") {
		t.Error("'.' must not act as a single-character wildcard")
	}
}

func TestNew_StarAllowsEverythingExplicitly(t *testing.T) {
	for _, pattern := range []string{"*", " * "} {
		s := New([]string{pattern})
		if !s.Allows("anything-at-all") {
			t.Errorf("pattern %q must allow everything", pattern)
		}
		if !s.Configured() {
			t.Errorf("pattern %q is an explicit choice and must report Configured", pattern)
		}
	}
}

func TestAllowNone(t *testing.T) {
	s := AllowNone()
	if s.Allows("django") || s.Allows("odoo") {
		t.Error("AllowNone must admit nothing")
	}
	if !s.Configured() {
		t.Error("AllowNone is an explicit choice and must report Configured")
	}
	// Spelled in config as the literal `none`.
	if got := New([]string{"none"}); got.Allows("django") {
		t.Error("the `none` sentinel must admit nothing")
	}
}

func TestNew_CaseAndWhitespace(t *testing.T) {
	s := New([]string{"  Celery-Dispatch  ", "\tDRUPAL\n"})
	if !s.Allows("celery-dispatch") || !s.Allows("drupal") {
		t.Error("entries must be trimmed and matched case-insensitively")
	}
	if !s.Allows("  CELERY-dispatch ") {
		t.Error("the queried name must also be trimmed and lowered")
	}
}

func TestAllows_EmptyNameNeverMatchesAConfiguredList(t *testing.T) {
	s := New([]string{"django"})
	if s.Allows("") || s.Allows("   ") {
		t.Error("an empty pass name must not match an exact entry")
	}
}

func TestPatterns_ReturnsCopy(t *testing.T) {
	s := New([]string{"django", "drupal"})
	got := s.Patterns()
	got[0] = "mutated"
	if s.Patterns()[0] != "django" {
		t.Error("Patterns must return a defensive copy")
	}
}

// On an allow-list a typo does not merely fail to take effect — it
// silently drops the framework the user meant to keep, so it must be
// reported.
func TestUnknown(t *testing.T) {
	known := []string{"celery-dispatch", "django", "drupal"}

	s := New([]string{"celery_dispatch", "drupal", "nope"})
	if got, want := s.Unknown(known), []string{"celery_dispatch", "nope"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Unknown = %q, want %q", got, want)
	}
	if got := New([]string{"zzz*", "*"}).Unknown(known); got != nil {
		t.Errorf("wildcard entries must never be reported unknown, got %q", got)
	}
	if got := New([]string{"django"}).Unknown(known); got != nil {
		t.Errorf("a matching entry must not be reported, got %q", got)
	}
}

// Union is the multi-repository rule: admission is permissive, so a
// repository that did not restrict a framework keeps it.
func TestUnion(t *testing.T) {
	u := Union(New([]string{"django"}), New([]string{"drupal"}))
	if !u.Allows("django") || !u.Allows("drupal") {
		t.Error("Union must allow what either side allows")
	}
	if u.Allows("celery-dispatch") {
		t.Error("Union must not allow what neither side allows")
	}
}

// A single unconfigured repository re-admits the full registry — the safe
// direction, since that repository never opted out of anything.
func TestUnion_UnconfiguredOperandAllowsEverything(t *testing.T) {
	narrow := New([]string{"django"})
	for _, u := range []Set{Union(narrow, Set{}), Union(Set{}, narrow)} {
		if !u.Allows("celery-dispatch") {
			t.Error("union with an unconfigured Set must allow everything")
		}
		if u.Configured() {
			t.Error("union with an unconfigured Set must itself be unconfigured")
		}
	}
}

func TestUnion_Associative(t *testing.T) {
	a := New([]string{"django", "godot*"})
	b := New([]string{"drupal"})
	c := New([]string{"odoo"})

	left, right := Union(Union(a, b), c), Union(a, Union(b, c))
	for _, name := range []string{"django", "godot-autoload", "drupal", "odoo", "celery-dispatch"} {
		if left.Allows(name) != right.Allows(name) {
			t.Errorf("Union not associative for %q", name)
		}
	}
	if !left.Allows("odoo") || left.Allows("celery-dispatch") {
		t.Errorf("unexpected fold result: %q", left.Patterns())
	}
}

func TestIntersect(t *testing.T) {
	i := Intersect(New([]string{"django", "drupal"}), New([]string{"drupal", "odoo"}))
	if !i.Allows("drupal") {
		t.Error("Intersect must keep what both sides allow")
	}
	if i.Allows("django") || i.Allows("odoo") {
		t.Error("Intersect must drop what only one side allows")
	}
}

func TestIntersect_UnconfiguredOperandDefersToTheOther(t *testing.T) {
	narrow := New([]string{"django"})
	for _, i := range []Set{Intersect(narrow, Set{}), Intersect(Set{}, narrow)} {
		if !i.Allows("django") || i.Allows("drupal") {
			t.Error("intersection with an unconfigured Set must defer to the configured one")
		}
	}
}

func TestIntersect_DisjointAllowsNothing(t *testing.T) {
	i := Intersect(New([]string{"django"}), New([]string{"odoo"}))
	if i.Allows("django") || i.Allows("odoo") {
		t.Error("disjoint allow-lists must admit nothing")
	}
	if !i.Configured() {
		t.Error("a disjoint intersection is an explicit empty admission, not 'unset'")
	}
}

// Intersect must agree with Allows. Comparing raw pattern strings made it
// disagree on every wildcard — "allow everything" ∩ "django" came back as
// AllowNone, the silent-blinding direction this package exists to avoid.
func TestIntersect_RespectsWildcardSemantics(t *testing.T) {
	cases := []struct {
		name   string
		a, b   []string
		allow  []string
		refuse []string
	}{
		{
			name:  "explicit star is the identity",
			a:     []string{"*"},
			b:     []string{"django"},
			allow: []string{"django"}, refuse: []string{"flask"},
		},
		{
			name:  "star on the right is the identity too",
			a:     []string{"django"},
			b:     []string{"*"},
			allow: []string{"django"}, refuse: []string{"flask"},
		},
		{
			name:  "prefix covers an exact name",
			a:     []string{"godot*"},
			b:     []string{"godot-autoload"},
			allow: []string{"godot-autoload"}, refuse: []string{"godot-scene", "flask"},
		},
		{
			name:  "nested prefixes narrow to the longer stem",
			a:     []string{"godot*"},
			b:     []string{"god*"},
			allow: []string{"godot-autoload"}, refuse: []string{"godzilla"},
		},
		{
			name:   "disjoint prefixes share nothing",
			a:      []string{"godot*"},
			b:      []string{"django*"},
			refuse: []string{"godot-autoload", "django-urls"},
		},
		{
			name:  "plain exact intersection still works",
			a:     []string{"flask", "django"},
			b:     []string{"django", "rails"},
			allow: []string{"django"}, refuse: []string{"flask", "rails"},
		},
		{
			name:   "AllowNone stays none",
			a:      []string{"none"},
			b:      []string{"django"},
			refuse: []string{"django"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Intersect(New(tc.a), New(tc.b))
			for _, name := range tc.allow {
				if !got.Allows(name) {
					t.Errorf("Intersect(%v, %v).Allows(%q) = false, want true", tc.a, tc.b, name)
				}
			}
			for _, name := range tc.refuse {
				if got.Allows(name) {
					t.Errorf("Intersect(%v, %v).Allows(%q) = true, want false", tc.a, tc.b, name)
				}
			}
		})
	}
}

// Intersect must admit exactly what both sides admit, for every name.
func TestIntersect_MatchesAllowsConjunction(t *testing.T) {
	sets := [][]string{
		{"*"}, {"none"}, {"django"}, {"godot*"}, {"god*"},
		{"flask", "django"}, {"godot*", "flask"},
	}
	names := []string{"django", "flask", "rails", "godot-autoload", "godot-scene", "godzilla"}
	for _, a := range sets {
		for _, b := range sets {
			sa, sb := New(a), New(b)
			got := Intersect(sa, sb)
			for _, n := range names {
				want := sa.Allows(n) && sb.Allows(n)
				if got.Allows(n) != want {
					t.Errorf("Intersect(%v, %v).Allows(%q) = %v, want %v", a, b, n, got.Allows(n), want)
				}
			}
		}
	}
}
