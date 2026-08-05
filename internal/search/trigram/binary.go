package trigram

import (
	"bufio"
	"bytes"
)

// binarySniffBytes is how many leading bytes decide whether content is text.
// It is git's and GNU grep's heuristic: a NUL byte anywhere in the first
// 8000 bytes means "not text". Sniffing a fixed head rather than the whole
// file keeps the check O(1) per document.
const binarySniffBytes = 8000

// maxIndexedBytes is a per-document sanity ceiling: a single file this
// large is excluded from the index rather than allowed to dominate one
// repo's build.
//
// It is deliberately far above any file a person greps. Once binary
// content is rejected, real text indexes at 1.4x-5x its own size, so
// total memory is bounded by the caller's budget over whole repos, not
// by a per-file cap — and a low cap costs more than it saves. An earlier
// draft capped at 2 MiB and kept over-cap files as standing candidates
// merged into every query; on a JavaScript repo full of source maps that
// turned a memory bound into a per-query scan of every bundle. Excluding
// only the absurd cases avoids both failure modes.
//
// An excluded document never matches, the same contract Build already has
// for a file it cannot read.
//
// A var only so tests can lower it; nothing in production writes it.
var maxIndexedBytes int64 = 64 << 20

// IsBinary reports whether content should be treated as opaque bytes rather
// than searchable text.
//
// Binary content is excluded from the trigram index and from literal search
// entirely. That is both a correctness call and a memory one: a text query
// cannot meaningfully match compressed or encoded bytes, and high-entropy
// content produces a near-unique trigram per byte position, so indexing it
// costs roughly 50x the file's own size.
//
// The check is content-truth rather than extension- or language-based on
// purpose. Gortex's image extractor claims ".svg" alongside ".png", and SVG
// is text a user legitimately greps; classifying by asset class would drop
// it. Sniffing the bytes keeps SVG searchable and keeps PNG out.
func IsBinary(content []byte) bool {
	head := content
	if len(head) > binarySniffBytes {
		head = head[:binarySniffBytes]
	}
	return bytes.IndexByte(head, 0) >= 0
}

// readerIsBinary reports whether the buffered reader's upcoming bytes look
// binary, without consuming them. The reader must have been created with a
// buffer of at least binarySniffBytes so Peek can reach the whole window; a
// short read at EOF is not an error, it just means a small file.
func readerIsBinary(br *bufio.Reader) bool {
	head, err := br.Peek(binarySniffBytes)
	if err != nil && len(head) == 0 {
		return false
	}
	return bytes.IndexByte(head, 0) >= 0
}
