package contracts

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestContainsOpenAPIMarker(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		src  string
		want bool
	}{
		{name: "lower", src: `{"openapi":"3.1.0"}`, want: true},
		{name: "mixed", src: `{"OpEnApI":"3.1.0"}`, want: true},
		{name: "swagger", src: "SWAGGER: '2.0'", want: true},
		{name: "short", src: "open", want: false},
		{name: "absent", src: `{"name":"service"}`, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, containsOpenAPIMarker([]byte(tc.src)))
		})
	}
}

func TestOpenAPIExtractor_NonSpecPrefilterDoesNotAllocateBySourceSize(t *testing.T) {
	extractor := &OpenAPIExtractor{}
	small := []byte("not an api specification")
	large := bytes.Repeat([]byte("not an api specification\n"), 1<<16)

	smallAllocs := testing.AllocsPerRun(5, func() {
		assert.Empty(t, extractor.Extract("small.json", small, nil, nil))
	})
	largeAllocs := testing.AllocsPerRun(5, func() {
		assert.Empty(t, extractor.Extract("large.json", large, nil, nil))
	})

	assert.LessOrEqual(t, largeAllocs, smallAllocs+1,
		"rejecting a non-spec file must not allocate source-sized copies")
}

func BenchmarkOpenAPIExtractor_NonSpecPrefilter(b *testing.B) {
	extractor := &OpenAPIExtractor{}
	src := bytes.Repeat([]byte("not an api specification\n"), 1<<16)
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		extractor.Extract("large.json", src, nil, nil)
	}
}
