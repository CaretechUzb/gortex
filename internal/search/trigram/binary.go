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

// maxIndexedBytes caps how large a single document may be and still enter
// the trigram index.
//
// The index cost of a document tracks its count of DISTINCT trigrams, not
// its byte length, and every distinct trigram costs a map entry plus a
// posting slice — order 50 bytes each. Real source is repetitive (measured
// 1.4x-5x its own size), but a large generated or minified artifact is not,
// so one file can cost tens of times what it appears to.
//
// A document over the cap is not dropped from search: it is recorded as
// unindexed and merged into every candidate set, so it is still opened and
// verified. The cap trades a little scan time on a rare file for a bounded
// index.
const maxIndexedBytes = 2 << 20

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
