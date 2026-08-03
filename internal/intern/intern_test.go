package intern

import (
	"fmt"
	"sync"
	"testing"
	"unsafe"
)

// backing returns the address of a string's backing array, so two
// strings can be checked for *sharing storage* rather than just being
// byte-equal.
func backing(s string) uintptr {
	return uintptr(unsafe.Pointer(unsafe.StringData(s)))
}

func TestString_EqualInputsShareBacking(t *testing.T) {
	// Two independently-allocated but equal strings.
	a := fmt.Sprintf("repo/%s", "pkg/foo.go::Bar")
	b := fmt.Sprintf("repo/%s", "pkg/foo.go::Bar")
	if backing(a) == backing(b) {
		t.Fatal("test setup: a and b unexpectedly already share storage")
	}
	ia := String(a)
	ib := String(b)
	if ia != ib {
		t.Fatalf("interned values differ: %q vs %q", ia, ib)
	}
	if backing(ia) != backing(ib) {
		t.Fatal("interned equal strings do not share a backing array")
	}
}

func TestString_Empty(t *testing.T) {
	if got := String(""); got != "" {
		t.Fatalf("String(\"\") = %q, want empty", got)
	}
}

func TestString_DistinctInputsDistinctValues(t *testing.T) {
	if String("alpha") == String("beta") {
		t.Fatal("distinct strings interned to the same value")
	}
}

// TestString_Concurrent races many goroutines interning an overlapping
// set of strings. Run under -race; also asserts every goroutine agrees
// on one canonical backing array per value.
func TestString_Concurrent(t *testing.T) {
	const workers = 32
	const keys = 64
	var wg sync.WaitGroup
	results := make([]uintptr, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			var last uintptr
			for i := 0; i < keys; i++ {
				s := String(fmt.Sprintf("concurrent/key/%d", i))
				if i == keys/2 {
					last = backing(s)
				}
			}
			results[w] = last
		}(w)
	}
	wg.Wait()
	for w := 1; w < workers; w++ {
		if results[w] != results[0] {
			t.Fatalf("worker %d saw a different backing array for the same key", w)
		}
	}
}

func TestResetStartsFreshGeneration(t *testing.T) {
	Reset()
	t.Cleanup(func() { Reset() })

	firstInput := fmt.Sprintf("reset/%s", "same-value")
	first := String(firstInput)
	if removed := Reset(); removed != 1 {
		t.Fatalf("Reset removed %d strings, want 1", removed)
	}
	if first != "reset/same-value" {
		t.Fatalf("pre-reset string changed: %q", first)
	}

	secondInput := fmt.Sprintf("reset/%s", "same-value")
	second := String(secondInput)
	if backing(first) == backing(second) {
		t.Fatal("equal strings unexpectedly retained one canonical backing across reset")
	}
	third := String(fmt.Sprintf("reset/%s", "same-value"))
	if backing(second) != backing(third) {
		t.Fatal("new generation did not canonicalize equal strings")
	}
}

func TestResetConcurrentWithString(t *testing.T) {
	Reset()
	t.Cleanup(func() { Reset() })

	const workers = 16
	const rounds = 256
	start := make(chan struct{})
	errCh := make(chan string, 1)
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			<-start
			for i := 0; i < rounds; i++ {
				want := fmt.Sprintf("reset-concurrent/%d/%d", worker, i%32)
				if got := String(want); got != want {
					select {
					case errCh <- fmt.Sprintf("String(%q) = %q", want, got):
					default:
					}
					return
				}
			}
		}(worker)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < rounds; i++ {
			Reset()
		}
	}()
	close(start)
	wg.Wait()
	select {
	case err := <-errCh:
		t.Fatal(err)
	default:
	}
	Reset()
	if got := Len(); got != 0 {
		t.Fatalf("Len after final Reset = %d, want 0", got)
	}
}
