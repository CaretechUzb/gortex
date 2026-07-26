//go:build !windows && !static_link

package languages

/*
#cgo linux LDFLAGS: -ldl
#include <dlfcn.h>
#include <stdlib.h>

// A tree-sitter grammar shared object exports
//   const TSLanguage *tree_sitter_NAME(void);
// ts_grammar_call invokes a dlsym'd entry point and returns the
// TSLanguage pointer as an opaque void*.
typedef const void *(*ts_grammar_fn)(void);
static const void *ts_grammar_call(void *fn) {
	return ((ts_grammar_fn)fn)();
}
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// loadGrammarLanguage dlopen's a compiled tree-sitter grammar shared
// object, looks up the named entry point, invokes it, and returns the
// resulting TSLanguage pointer. The dlopen handle is intentionally
// leaked: the TSLanguage must outlive every parse, and the set of
// custom grammars is fixed for the process lifetime.
func loadGrammarLanguage(libPath, symbol string) (unsafe.Pointer, error) {
	cPath := C.CString(libPath)
	defer C.free(unsafe.Pointer(cPath))

	handle := C.dlopen(cPath, C.RTLD_NOW|C.RTLD_LOCAL)
	if handle == nil {
		return nil, fmt.Errorf("dlopen: %s", C.GoString(C.dlerror()))
	}

	cSym := C.CString(symbol)
	defer C.free(unsafe.Pointer(cSym))
	C.dlerror() // clear any stale error before dlsym
	fn := C.dlsym(handle, cSym)
	if fn == nil {
		err := C.GoString(C.dlerror())
		C.dlclose(handle)
		return nil, fmt.Errorf("dlsym %s: %s", symbol, err)
	}

	lang := C.ts_grammar_call(fn)
	if lang == nil {
		C.dlclose(handle)
		return nil, fmt.Errorf("entry point %s returned a nil TSLanguage", symbol)
	}
	return unsafe.Pointer(lang), nil
}
