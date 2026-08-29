package graph

import "testing"

// The obvious `prefix || 0x7F` upper bound is wrong on any corpus with
// non-ASCII paths: every UTF-8 continuation byte is >= 0x80, so that bound
// stops short of exactly the ids whose next byte is multi-byte. Incrementing
// the last byte of the prefix does not have that hole.
func TestPrefixUpperBound(t *testing.T) {
	for _, tc := range []struct {
		name, prefix, want string
		ok                 bool
	}{
		{name: "ascii", prefix: "local/", want: "local0", ok: true},
		{name: "synthetic", prefix: "local::", want: "local:;", ok: true},
		{name: "trailing 0xFF rolls over", prefix: "a\xff", want: "b", ok: true},
		{name: "all 0xFF has no bound", prefix: "\xff\xff", ok: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := PrefixUpperBound(tc.prefix)
			if ok != tc.ok {
				t.Fatalf("PrefixUpperBound(%q) ok = %v, want %v", tc.prefix, ok, tc.ok)
			}
			if ok && got != tc.want {
				t.Fatalf("PrefixUpperBound(%q) = %q, want %q", tc.prefix, got, tc.want)
			}
		})
	}
}

// The bound has to admit every id under the prefix, including one whose next
// byte is a UTF-8 lead byte. This is the case `prefix || CHAR(127)` drops.
func TestPrefixUpperBoundAdmitsNonASCIISuffixes(t *testing.T) {
	const prefix = "local/"
	id := prefix + "модели/договор.py::Класс"

	upper, ok := PrefixUpperBound(prefix)
	if !ok {
		t.Fatal("PrefixUpperBound reported no bound for an ASCII prefix")
	}
	if !(id >= prefix && id < upper) {
		t.Fatalf("id %q falls outside range [%q, %q)", id, prefix, upper)
	}
	if naive := prefix + "\x7f"; id < naive {
		t.Fatalf("fixture no longer exercises the trap: %q sorts below %q", id, naive)
	}
}
