package mcp

import (
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/search/rerank"
)

// Task-derived path corroboration for source candidates.
//
// A task names the answer's path about as often as it names its symbols: the
// flag, environment name, quoted span or camel-cased identifier an author
// quotes is frequently a segment of the file that implements it. The artifact
// lane already ranks those spans by distinctiveness, but it serves only
// artifact-intent tasks, so source candidates carried no path-level signal at
// all — a declaration in the file the task all but spells out ranked on
// retrieval score alone.
//
// Corroboration is a tie-break, never a retrieval channel. It can trade the
// weakest unreserved row of a bounded page for one better-corroborated row just
// below the cut, and it can settle a leading-file election ranking left
// scattered. It can never displace a row another evidence lane reserved.

const (
	// explorePathProbeLimit bounds the probes derived per localization call.
	explorePathProbeLimit = 8
	// explorePathProbeTokenCap bounds the tokens compared per probe.
	explorePathProbeTokenCap = 4
	// explorePathProbeSegmentCap bounds the trailing path segments compared, so
	// a deep tree's shared parents cannot corroborate everything beneath them.
	explorePathProbeSegmentCap = 6
	// explorePathProbeMatchCap bounds the probes one path may score with, so a
	// path echoing half the task cannot outscore a precise match.
	explorePathProbeMatchCap = 3
	// explorePathProbeScanCap bounds the candidates scored per call.
	explorePathProbeScanCap = 128
	// explorePathProbeLiftReach bounds how far below the cut a lift may reach.
	explorePathProbeLiftReach = 8
	// explorePathProbeMinToken is the shortest token worth comparing.
	explorePathProbeMinToken = 3
	// explorePathProbeSignal carries a candidate's path corroboration score.
	explorePathProbeSignal = "explore_path_probe"
)

type explorePathProbe struct {
	tokens []string
	weight int
}

// explorePathProbes is one task's ranked probe set. The zero value corroborates
// nothing, so a caller outside localization can hold one unconditionally.
type explorePathProbes struct {
	probes []explorePathProbe
}

// newExplorePathProbes derives the probe set once per call. Probes arrive in
// priority order and keep it as a descending weight, so the spans an author
// made distinctive outrank the ones that merely look like identifiers.
func newExplorePathProbes(task string) explorePathProbes {
	ranked := rankedExploreTaskProbes(task, make(map[string]struct{}), explorePathProbeLimit)
	out := explorePathProbes{probes: make([]explorePathProbe, 0, len(ranked))}
	for _, probe := range ranked {
		tokens := explorePathProbeTokens(probe.value)
		if len(tokens) == 0 {
			continue
		}
		out.probes = append(out.probes, explorePathProbe{
			tokens: tokens,
			weight: explorePathProbeLimit - len(out.probes),
		})
	}
	return out
}

func (p explorePathProbes) empty() bool { return len(p.probes) == 0 }

// scorePath sums the weights of the probes a path corroborates. A probe counts
// only when every one of its tokens names a compared path segment, so sharing a
// single word with a path is worth nothing.
func (p explorePathProbes) scorePath(path string) int {
	if len(p.probes) == 0 {
		return 0
	}
	tokens := explorePathSegmentTokens(path)
	if len(tokens) == 0 {
		return 0
	}
	score, matched := 0, 0
	for _, probe := range p.probes {
		if matched == explorePathProbeMatchCap {
			break
		}
		if !explorePathProbeMatches(probe, tokens) {
			continue
		}
		matched++
		score += probe.weight
	}
	return score
}

func explorePathProbeMatches(probe explorePathProbe, tokens map[string]struct{}) bool {
	if len(probe.tokens) == 0 {
		return false
	}
	for _, token := range probe.tokens {
		if _, present := tokens[token]; !present {
			return false
		}
	}
	return true
}

// explorePathProbeTokens keeps a probe's distinctive words. Generic and
// stopword tokens are dropped rather than required, and a probe left with
// nothing but boilerplate corroborates nothing.
func explorePathProbeTokens(value string) []string {
	tokens := make([]string, 0, explorePathProbeTokenCap)
	for _, raw := range rerank.Tokenize(value) {
		token := exploreTerminalTermRoot(strings.ToLower(raw))
		if len(token) < explorePathProbeMinToken {
			continue
		}
		if _, stop := assistStopWords[token]; stop {
			continue
		}
		if _, generic := exploreTerminalGenericTerms[token]; generic {
			continue
		}
		tokens = append(tokens, token)
		if len(tokens) == explorePathProbeTokenCap {
			break
		}
	}
	return tokens
}

// explorePathSegmentTokens tokenizes a path's trailing segments with the file
// extension dropped — the extension names a language, not a subject.
func explorePathSegmentTokens(path string) map[string]struct{} {
	path = strings.Trim(strings.ReplaceAll(strings.TrimSpace(path), "\\", "/"), "/")
	if path == "" {
		return nil
	}
	if ext := filepath.Ext(path); ext != "" {
		path = strings.TrimSuffix(path, ext)
	}
	segments := strings.Split(path, "/")
	if len(segments) > explorePathProbeSegmentCap {
		segments = segments[len(segments)-explorePathProbeSegmentCap:]
	}
	tokens := make(map[string]struct{}, len(segments)*3)
	for _, segment := range segments {
		for _, raw := range rerank.Tokenize(segment) {
			token := exploreTerminalTermRoot(strings.ToLower(raw))
			if len(token) < explorePathProbeMinToken {
				continue
			}
			tokens[token] = struct{}{}
		}
	}
	return tokens
}

// stampExplorePathProbeScores records path corroboration once per call, on the
// bounded ranked prefix the page and its leading-file election can still reach.
// Scoring is memoized per file and is pure string work — no store query, no
// source hydration. Every later stage reads the stamp instead of rescoring.
func stampExplorePathProbeScores(candidates []*rerank.Candidate, probes explorePathProbes) {
	if probes.empty() {
		return
	}
	byFile := make(map[string]int, explorePathProbeScanCap)
	for index, candidate := range candidates {
		if index == explorePathProbeScanCap {
			return
		}
		if candidate == nil || candidate.Node == nil || candidate.Node.FilePath == "" {
			continue
		}
		score, memoized := byFile[candidate.Node.FilePath]
		if !memoized {
			score = probes.scorePath(candidate.Node.FilePath)
			byFile[candidate.Node.FilePath] = score
		}
		if score <= 0 {
			continue
		}
		// Reranker clones may share one signal map, so tag a copy — the same rule
		// the anchor lane follows.
		signals := make(map[string]float64, len(candidate.Signals)+1)
		for name, value := range candidate.Signals {
			signals[name] = value
		}
		signals[explorePathProbeSignal] = float64(score)
		candidate.Signals = signals
	}
}

func explorePathProbeScore(candidate *rerank.Candidate) int {
	if candidate == nil || candidate.Signals == nil {
		return 0
	}
	return int(candidate.Signals[explorePathProbeSignal])
}

// liftExplorePathProbeCandidates admits the best-corroborated candidate that
// ranked just below the bounded cut, in exchange for the weakest row inside it
// that no evidence lane reserved. The admitted row must be strictly better
// corroborated than the row it replaces, so a page already holding the
// probe-named file keeps its order, and response width never changes.
func liftExplorePathProbeCandidates(
	candidates []*rerank.Candidate,
	maxSymbols int,
	reserved map[string]struct{},
) []*rerank.Candidate {
	if maxSymbols <= 1 || len(candidates) <= maxSymbols {
		return candidates
	}
	reach := min(len(candidates), maxSymbols+explorePathProbeLiftReach)
	admit, admitScore := -1, 0
	for index := maxSymbols; index < reach; index++ {
		if score := explorePathProbeScore(candidates[index]); score > admitScore {
			admit, admitScore = index, score
		}
	}
	if admit < 0 {
		return candidates
	}
	// The head is retrieval's own answer and is never traded.
	replace := -1
	for index := maxSymbols - 1; index >= 1; index-- {
		candidate := candidates[index]
		if candidate == nil || candidate.Node == nil {
			replace = index
			break
		}
		if _, protected := reserved[candidate.Node.ID]; protected {
			continue
		}
		if explorePathProbeScore(candidate) >= admitScore {
			return candidates
		}
		replace = index
		break
	}
	if replace < 0 {
		return candidates
	}
	lifted := append([]*rerank.Candidate(nil), candidates...)
	lifted[replace], lifted[admit] = lifted[admit], lifted[replace]
	return lifted
}
