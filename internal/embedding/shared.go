package embedding

import (
	"context"
	"os"
	"strings"
	"sync"
	"time"
)

// sharedStatic memoises a single process-wide StaticProvider. The baked
// GloVe vectors are ~3.7MB compressed and decompress into a ~20k-entry
// map that is safe for concurrent reads, so one instance serves every
// rerank call. Constructed lazily on first use.
var (
	sharedStaticOnce sync.Once
	sharedStaticInst *StaticProvider
)

// SharedStatic returns the process-wide static word-vector provider,
// constructing it on first call. Returns nil only when the baked
// vectors fail to load (a corrupt build); callers treat nil as "no
// semantic-cosine channel". Safe for concurrent use.
func SharedStatic() *StaticProvider {
	sharedStaticOnce.Do(func() {
		p, err := NewStaticProvider()
		if err != nil {
			return
		}
		sharedStaticInst = p
	})
	return sharedStaticInst
}

// EmbedTextFunc adapts a provider into the plain func the rerank
// Context wants: text -> normalised vector, errors and nil providers
// collapsing to a nil result the signal reads as "cannot embed".
func EmbedTextFunc(p Provider) func(string) []float32 {
	if p == nil {
		return nil
	}
	return func(text string) []float32 {
		vec, err := p.Embed(context.Background(), text)
		if err != nil {
			return nil
		}
		return vec
	}
}

// sharedCode holds the process-wide code-embedding provider used by the
// rerank's semantic-cosine channel: the bundled static code model (potion)
// when its files resolve — explicit dir, exec-adjacent sidecar, per-user
// models dir, or a checksum-verified first-use download — and the baked
// GloVe word vectors otherwise, so an offline install without the sidecar
// still gets a semantic channel.
//
// It is deliberately NOT a sync.Once. The potion matrix is tens of MiB and
// a daemon that stops searching should not hold it for the rest of its
// life: the loader records when it was last used and a reaper drops it
// after an idle period, so the next search pays a reload instead of the
// process paying rent forever.
var (
	sharedCodeMu     sync.Mutex
	sharedCodeInst   Provider
	sharedCodeLoaded bool
	sharedCodeUsedAt time.Time
	sharedCodeEnable = true
	sharedCodeReaper bool
)

// codeEmbedderIdleTTL is how long the code embedder survives with no
// rerank traffic. Long enough that a working session never reloads,
// short enough that an idle daemon gives the memory back.
const codeEmbedderIdleTTL = 30 * time.Minute

// SetCodeEmbedderEnabled turns the rerank semantic-cosine channel on or
// off process-wide, and drops an already-loaded model when turning it off.
// The daemon calls this from its resolved configuration; the channel is on
// by default, which is the historical behaviour.
func SetCodeEmbedderEnabled(enabled bool) {
	sharedCodeMu.Lock()
	defer sharedCodeMu.Unlock()
	sharedCodeEnable = enabled
	if !enabled {
		sharedCodeInst = nil
		sharedCodeLoaded = false
	}
}

// SharedCodeEmbedder returns the process-wide code embedder for the
// rerank's semantic-cosine channel, loading it on first use and reloading
// it after an idle eviction. Never returns an error — the fallback chain
// ends at the baked static provider; nil only when even that failed to
// load, or when the channel is disabled. Safe for concurrent use.
// GORTEX_POTION=0 pins the GloVe fallback (diagnostic escape hatch).
func SharedCodeEmbedder() Provider {
	sharedCodeMu.Lock()
	defer sharedCodeMu.Unlock()
	if !sharedCodeEnable {
		return nil
	}
	sharedCodeUsedAt = time.Now()
	if !sharedCodeLoaded {
		sharedCodeInst = buildCodeEmbedder()
		sharedCodeLoaded = true
		startCodeEmbedderReaperLocked()
	}
	return sharedCodeInst
}

func buildCodeEmbedder() Provider {
	if v := strings.TrimSpace(os.Getenv("GORTEX_POTION")); v == "0" || strings.EqualFold(v, "false") || strings.EqualFold(v, "off") {
		return staticOrNil()
	}
	dir := resolvePotionDir()
	if dir == "" {
		if d, err := downloadPotion(); err == nil {
			dir = d
		}
	}
	if dir != "" {
		if p, err := NewPotionProviderFromDir(dir); err == nil {
			return p
		}
	}
	return staticOrNil()
}

// startCodeEmbedderReaperLocked launches the idle reaper once. It drops
// the instance and exits; a later load starts a fresh one. The caller must
// hold sharedCodeMu.
func startCodeEmbedderReaperLocked() {
	if sharedCodeReaper {
		return
	}
	sharedCodeReaper = true
	go func() {
		ticker := time.NewTicker(codeEmbedderIdleTTL / 4)
		defer ticker.Stop()
		for range ticker.C {
			sharedCodeMu.Lock()
			idle := sharedCodeLoaded && time.Since(sharedCodeUsedAt) >= codeEmbedderIdleTTL
			if idle {
				if c, ok := sharedCodeInst.(interface{ Close() error }); ok {
					_ = c.Close()
				}
				sharedCodeInst = nil
				sharedCodeLoaded = false
				sharedCodeReaper = false
			}
			sharedCodeMu.Unlock()
			if idle {
				return
			}
		}
	}()
}

// staticOrNil adapts SharedStatic's concrete return into the Provider
// interface without wrapping a typed nil.
func staticOrNil() Provider {
	if p := SharedStatic(); p != nil {
		return p
	}
	return nil
}
