package router

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	"golang.org/x/sync/singleflight"
)

type compiledIndex struct {
	version       uint64
	tools         []Tool
	toolDocuments []string
	toolLexical   *lexicalIndex
	toolVectors   [][]float32
	domains       []string
	domainDocs    []string
	domainLexical *lexicalIndex
	domainVectors [][]float32
	domainTools   map[string][]int
}

type routeCacheEntry struct {
	result    RouteResult
	expiresAt time.Time
}

// Router is safe for concurrent Route calls. Refresh creates a new immutable
// index and swaps it in only after compilation succeeds.
type Router struct {
	registry     *Registry
	embedder     Embedder
	reranker     Reranker
	cfg          Config
	mu           sync.RWMutex
	index        *compiledIndex
	sf           singleflight.Group
	embedCache   map[string][]float32
	cacheMu      sync.Mutex
	routeCache   map[string]routeCacheEntry
	routeCacheMu sync.RWMutex
}

func New(registry *Registry, embedder Embedder, cfg Config, options ...Option) *Router {
	defaults := DefaultConfig()
	if cfg.MaxDomainTools <= 0 {
		cfg.MaxDomainTools = defaults.MaxDomainTools
	}
	if cfg.DomainTopK <= 0 {
		cfg.DomainTopK = defaults.DomainTopK
	}
	if cfg.CandidateTopK <= 0 {
		cfg.CandidateTopK = defaults.CandidateTopK
	}
	if cfg.LexicalWeight == 0 && cfg.SemanticWeight == 0 {
		cfg.LexicalWeight, cfg.SemanticWeight = defaults.LexicalWeight, defaults.SemanticWeight
	}
	if cfg.MinConfidence == 0 {
		cfg.MinConfidence = defaults.MinConfidence
	} else if cfg.MinConfidence < 0 {
		cfg.MinConfidence = 0
	}
	if cfg.MinMargin == 0 {
		cfg.MinMargin = defaults.MinMargin
	} else if cfg.MinMargin < 0 {
		cfg.MinMargin = 0
	}
	if cfg.RerankerWeight == 0 {
		cfg.RerankerWeight = defaults.RerankerWeight
	} else if cfg.RerankerWeight < 0 {
		cfg.RerankerWeight = 0
	}
	if cfg.BM25FastPath == 0 {
		cfg.BM25FastPath = defaults.BM25FastPath
	} else if cfg.BM25FastPath < 0 {
		cfg.BM25FastPath = 0
	}
	if cfg.RouteCacheTTL == 0 {
		cfg.RouteCacheTTL = defaults.RouteCacheTTL
	}
	result := &Router{
		registry:   registry,
		embedder:   embedder,
		cfg:        cfg,
		embedCache: make(map[string][]float32),
		routeCache: make(map[string]routeCacheEntry),
	}
	for _, option := range options {
		option(result)
	}
	result.loadEmbeddingCache()
	return result
}

type Option func(*Router)

func WithReranker(reranker Reranker) Option {
	return func(router *Router) { router.reranker = reranker }
}

func (r *Router) Refresh(ctx context.Context) error {
	snapshot := r.registry.Snapshot()
	if len(snapshot.Tools) == 0 {
		return fmt.Errorf("cannot build router index: registry is empty")
	}

	// Collapse concurrent Refresh calls for the same registry version into a
	// single build pass. This prevents duplicate Embed calls when multiple
	// server notifications fire simultaneously.
	sfKey := fmt.Sprintf("refresh-v%d", snapshot.Version)
	_, err, _ := r.sf.Do(sfKey, func() (any, error) {
		index := &compiledIndex{version: snapshot.Version, tools: snapshot.Tools, domainTools: make(map[string][]int)}
		domainDocuments := make(map[string][]string)
		for toolIndex, tool := range snapshot.Tools {
			domain := ExtractDomain(tool)
			index.domainTools[domain] = append(index.domainTools[domain], toolIndex)
			document := tool.Name + " " + tool.Description
			index.toolDocuments = append(index.toolDocuments, document)
			domainDocuments[domain] = append(domainDocuments[domain], document)
		}
		for domain := range domainDocuments {
			index.domains = append(index.domains, domain)
		}
		sort.Strings(index.domains)
		for _, domain := range index.domains {
			index.domainDocs = append(index.domainDocs, domain+" "+strings.Join(domainDocuments[domain], " "))
		}
		index.toolLexical = newLexicalIndex(index.toolDocuments)
		index.domainLexical = newLexicalIndex(index.domainDocs)

		if r.embedder != nil {
			toolVectors, err := r.embedBatch(ctx, index.toolDocuments)
			if err != nil {
				return nil, fmt.Errorf("embed tool descriptions: %w", err)
			}
			domainVectors, err := r.embedBatch(ctx, index.domainDocs)
			if err != nil {
				return nil, fmt.Errorf("embed domain descriptions: %w", err)
			}
			if len(toolVectors) != len(index.tools) || len(domainVectors) != len(index.domains) {
				return nil, fmt.Errorf("embedder returned an unexpected vector count")
			}
			index.toolVectors, index.domainVectors = toolVectors, domainVectors

			// Clean up stale embeddings from cache
			validTexts := make(map[string]struct{}, len(index.toolDocuments)+len(index.domainDocs))
			for _, text := range index.toolDocuments {
				validTexts[text] = struct{}{}
			}
			for _, text := range index.domainDocs {
				validTexts[text] = struct{}{}
			}
			r.cacheMu.Lock()
			for k := range r.embedCache {
				if _, ok := validTexts[k]; !ok {
					delete(r.embedCache, k)
				}
			}
			r.cacheMu.Unlock()
			r.saveEmbeddingCache()
		}

		r.mu.Lock()
		r.index = index
		r.mu.Unlock()
		return nil, nil
	})
	return err
}

func (r *Router) Route(ctx context.Context, request RouteRequest) (result RouteResult, err error) {
	ctx = r.withTracing(ctx)
	var span traceSpan
	ctx, span = r.startStage(ctx, spanRoute,
		attribute.Int("router.utterance_chars", utf8.RuneCountInString(strings.TrimSpace(request.Utterance))),
		attribute.Bool("router.has_context", strings.TrimSpace(request.Context) != ""),
		attribute.Bool("router.final", request.Final),
	)
	defer func() {
		if err == nil {
			annotateResult(span, result)
		}
		finishSpan(span, err)
	}()

	query := strings.TrimSpace(request.Utterance)
	contextStr := strings.TrimSpace(request.Context)
	_, cacheSpan := r.startStage(ctx, spanCache)
	cached, found := r.lookupRouteCache(query, contextStr)
	if found {
		setStageAttrs(cacheSpan, attribute.Bool(attrCacheHit, true))
		finishSpan(cacheSpan, nil)
		result = cached
		return result, nil
	}
	setStageAttrs(cacheSpan, attribute.Bool(attrCacheHit, false))
	finishSpan(cacheSpan, nil)

	fragments := splitIntents(request.Utterance)
	if len(fragments) <= 1 {
		result, err = r.routeSingle(ctx, request)
		if err != nil {
			return RouteResult{}, err
		}
		if result.Decision != DecisionNoTool && r.cfg.RouteCacheTTL > 0 {
			r.writeRouteCache(query, contextStr, result, r.cfg.RouteCacheTTL)
		}
		return result, nil
	}

	_, splitSpan := r.startStage(ctx, spanIntent, attribute.Int("router.fragment_count", len(fragments)))
	finishSpan(splitSpan, nil)

	log.Printf("[Router] Multi-intent detected: %d fragments: %v", len(fragments), fragments)
	results := make([]RouteResult, len(fragments))
	type routeJobResult struct {
		index  int
		result RouteResult
		err    error
	}
	ch := make(chan routeJobResult, len(fragments))
	var wg sync.WaitGroup
	for idx, frag := range fragments {
		wg.Add(1)
		go func(i int, f string) {
			defer wg.Done()
			req := request
			req.Utterance = f
			fragCtx, fragSpan := r.startStage(ctx, spanFragment,
				attribute.Int("router.fragment_index", i),
				attribute.Int("router.utterance_chars", utf8.RuneCountInString(strings.TrimSpace(f))),
			)
			res, fragErr := r.routeSingle(fragCtx, req)
			if fragErr == nil {
				annotateResult(fragSpan, res)
			}
			finishSpan(fragSpan, fragErr)
			ch <- routeJobResult{index: i, result: res, err: fragErr}
		}(idx, frag)
	}
	wg.Wait()
	close(ch)

	for job := range ch {
		if job.err != nil {
			return RouteResult{}, job.err
		}
		results[job.index] = job.result
	}
	_, mergeSpan := r.startStage(ctx, spanMerge, attribute.Int("router.fragment_count", len(results)))
	merged := mergeRouteResults(results)
	annotateResult(mergeSpan, merged)
	finishSpan(mergeSpan, nil)
	if merged.Decision != DecisionNoTool && r.cfg.RouteCacheTTL > 0 {
		r.writeRouteCache(query, contextStr, merged, r.cfg.RouteCacheTTL)
	}
	result = merged
	return result, nil
}

func (r *Router) routeSingle(ctx context.Context, request RouteRequest) (RouteResult, error) {
	started := time.Now()
	r.mu.RLock()
	index := r.index
	r.mu.RUnlock()
	if index == nil {
		return RouteResult{}, fmt.Errorf("router index is not built; call Refresh first")
	}
	query := strings.TrimSpace(request.Utterance)
	if r.cfg.IncludeContext && strings.TrimSpace(request.Context) != "" {
		query += " context " + request.Context
	}
	if query == "" {
		result := RouteResult{Decision: DecisionNoTool, Reason: "empty utterance"}
		r.finishDecide(ctx, result)
		return result, nil
	}

	// Try BM25 Fast-Path
	if r.cfg.BM25FastPath > 0 {
		toolLexicalScores := index.toolLexical.scores(query)
		bestIdx := -1
		bestScore := -1.0
		for idx, score := range toolLexicalScores {
			if score > bestScore {
				bestScore = score
				bestIdx = idx
			}
		}

		_, fastSpan := r.startStage(ctx, spanFastPath, attribute.Float64("router.bm25_threshold", r.cfg.BM25FastPath))
		if bestIdx != -1 && bestScore >= r.cfg.BM25FastPath {
			bestTool := index.tools[bestIdx]
			candidate := Candidate{
				Tool:         bestTool,
				Score:        1.0,
				LexicalScore: 1.0,
				Rank:         1,
			}
			selectLatency := time.Since(started)
			result := RouteResult{
				Decision:   DecisionSelected,
				Confidence: 1.0,
				Margin:     1.0,
				Candidates: []Candidate{candidate},
				Reason:     fmt.Sprintf("fast-path: top candidate passed lexical BM25 threshold (score=%.2f >= %.2f)", bestScore, r.cfg.BM25FastPath),
				Trace: Trace{
					RegistryVersion: index.version,
					Domains:         []string{ExtractDomain(bestTool)},
					DomainLatency:   0,
					SelectLatency:   selectLatency,
					TotalLatency:    time.Since(started),
					UsedEmbeddings:  false,
					UsedReranker:    false,
				},
			}
			setStageAttrs(fastSpan,
				attribute.Bool(attrFastPath, true),
				attribute.Float64("router.bm25_score", bestScore),
				attribute.String("router.tool_id", bestTool.ID),
			)
			annotateResult(fastSpan, result)
			finishSpan(fastSpan, nil)
			r.finishDecide(ctx, result)
			return result, nil
		}
		recorded := bestScore
		if bestIdx == -1 {
			recorded = 0
		}
		setStageAttrs(fastSpan, attribute.Bool(attrFastPath, false), attribute.Float64("router.bm25_score", recorded))
		finishSpan(fastSpan, nil)
	}

	domainStarted := time.Now()
	domainCtx, domainSpan := r.startStage(ctx, spanDomain)
	domainLexical := normalizeScores(index.domainLexical.scores(query))
	var embedSpan traceSpan
	if r.embedder != nil && len(index.domainVectors) > 0 {
		_, embedSpan = r.startStage(domainCtx, spanEmbed)
	}
	domainSemantic, queryVector, usedEmbeddings, err := r.semanticScores(domainCtx, query, index.domainVectors)
	if embedSpan != nil {
		if err != nil {
			setStageAttrs(embedSpan, attribute.Bool("router.embedding_fallback", true))
			finishSpan(embedSpan, err)
		} else {
			setStageAttrs(embedSpan, attribute.Bool("router.used_embeddings", usedEmbeddings))
			finishSpan(embedSpan, nil)
		}
	}
	if err != nil {
		log.Printf("[Router] Warning: semantic embedding failed (%v); falling back to lexical routing", err)
		usedEmbeddings = false
		domainSemantic = make([]float64, len(index.domainVectors))
	}
	lexicalWeight, semanticWeight := r.weights(usedEmbeddings)
	domainScores := fuse(domainLexical, domainSemantic, lexicalWeight, semanticWeight)
	domainRanks := rankedIndices(domainScores)
	toolLexical := normalizeScores(index.toolLexical.scores(query))
	toolSemantic := make([]float64, len(index.tools))
	if usedEmbeddings {
		for toolIndex, vector := range index.toolVectors {
			toolSemantic[toolIndex] = cosine01(queryVector, vector)
		}
	}
	preliminaryToolScores := fuse(toolLexical, toolSemantic, lexicalWeight, semanticWeight)
	selectedDomains, allowedTools := r.selectDomains(index, domainRanks, preliminaryToolScores)
	domainLatency := time.Since(domainStarted)
	setStageAttrs(domainSpan,
		attribute.Int("router.domain_count", len(selectedDomains)),
		attribute.StringSlice("router.selected_domains", selectedDomains),
		attribute.Int("router.domain_tool_count", len(allowedTools)),
		attribute.Bool("router.used_embeddings", usedEmbeddings),
	)
	finishSpan(domainSpan, nil)

	selectStarted := time.Now()
	selectCtx, selectSpan := r.startStage(ctx, spanSelect)
	toolScores := preliminaryToolScores
	usedReranker := false
	if r.reranker != nil {
		shortlist := make([]Tool, 0, len(allowedTools))
		for toolIndex := range allowedTools {
			shortlist = append(shortlist, index.tools[toolIndex])
		}
		_, rerankSpan := r.startStage(selectCtx, spanRerank, attribute.Int(attrCandidateCount, len(shortlist)))
		reranked, rerankErr := r.reranker.Rerank(selectCtx, request.Utterance, request.Context, shortlist)
		if rerankErr != nil {
			setStageAttrs(rerankSpan, attribute.Bool("router.reranker_fallback", true))
			finishSpan(rerankSpan, rerankErr)
			log.Printf("[Router] Warning: reranker failed (%v); falling back to preliminary scores", rerankErr)
		} else {
			usedReranker = true
			weight := math.Max(0, math.Min(1, r.cfg.RerankerWeight))
			for toolIndex := range allowedTools {
				if score, exists := reranked[index.tools[toolIndex].ID]; exists {
					toolScores[toolIndex] = (1-weight)*toolScores[toolIndex] + weight*math.Max(0, math.Min(1, score))
				}
			}
			finishSpan(rerankSpan, nil)
		}
	}
	candidates := make([]Candidate, 0, len(allowedTools))
	for toolIndex := range allowedTools {
		candidates = append(candidates, Candidate{Tool: index.tools[toolIndex], Score: toolScores[toolIndex], LexicalScore: toolLexical[toolIndex], SemanticScore: toolSemantic[toolIndex]})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Score == candidates[j].Score {
			return candidates[i].Tool.ID < candidates[j].Tool.ID
		}
		return candidates[i].Score > candidates[j].Score
	})
	if len(candidates) > r.cfg.CandidateTopK {
		candidates = candidates[:r.cfg.CandidateTopK]
	}
	for candidateIndex := range candidates {
		candidates[candidateIndex].Rank = candidateIndex + 1
	}
	selectLatency := time.Since(selectStarted)

	result := decide(candidates, r.cfg)
	result.Trace = Trace{RegistryVersion: index.version, Domains: selectedDomains, DomainLatency: domainLatency, SelectLatency: selectLatency, TotalLatency: time.Since(started), UsedEmbeddings: usedEmbeddings, UsedReranker: usedReranker}
	setStageAttrs(selectSpan, attribute.Bool("router.used_reranker", usedReranker), attribute.Bool("router.used_embeddings", usedEmbeddings))
	annotateResult(selectSpan, result)
	finishSpan(selectSpan, nil)
	r.finishDecide(ctx, result)
	return result, nil
}

func (r *Router) finishDecide(ctx context.Context, result RouteResult) {
	_, decideSpan := r.startStage(ctx, spanDecide)
	annotateResult(decideSpan, result)
	finishSpan(decideSpan, nil)
}

func (r *Router) semanticScores(ctx context.Context, query string, vectors [][]float32) ([]float64, []float32, bool, error) {
	if r.embedder == nil || len(vectors) == 0 {
		return make([]float64, len(vectors)), nil, false, nil
	}
	val, err, _ := r.sf.Do(query, func() (any, error) {
		queryVectors, err := r.embedder.Embed(ctx, []string{query})
		if err != nil {
			return nil, fmt.Errorf("embed query: %w", err)
		}
		if len(queryVectors) != 1 {
			return nil, fmt.Errorf("embedder returned %d query vectors", len(queryVectors))
		}
		return queryVectors[0], nil
	})
	if err != nil {
		return nil, nil, false, err
	}
	queryVector := val.([]float32)
	scores := make([]float64, len(vectors))
	for index, vector := range vectors {
		scores[index] = cosine01(queryVector, vector)
	}
	return scores, queryVector, true, nil
}

// embedBatch resolves embeddings for a list of documents.
// It checks the in-memory cache to skip already computed documents,
// batches the misses, requests their embeddings from the embedder,
// and saves the results back to the cache.
func (r *Router) embedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	r.cacheMu.Lock()
	results := make([][]float32, len(texts))
	var misses []string
	var missIndices []int
	for idx, text := range texts {
		if vec, found := r.embedCache[text]; found {
			results[idx] = vec
		} else {
			misses = append(misses, text)
			missIndices = append(missIndices, idx)
		}
	}
	r.cacheMu.Unlock()

	log.Printf("[Embedding Cache] Batch requested: total=%d, hits=%d, misses=%d", len(texts), len(texts)-len(misses), len(misses))

	if len(misses) > 0 {
		vectors, err := r.embedder.Embed(ctx, misses)
		if err != nil {
			return nil, err
		}
		if len(vectors) != len(misses) {
			return nil, fmt.Errorf("embedder returned unexpected vector count: expected %d, got %d", len(misses), len(vectors))
		}
		r.cacheMu.Lock()
		for idx, vec := range vectors {
			originalIdx := missIndices[idx]
			results[originalIdx] = vec
			r.embedCache[misses[idx]] = vec
		}
		r.cacheMu.Unlock()
		r.saveEmbeddingCache()
	}
	return results, nil
}

func (r *Router) loadEmbeddingCache() {
	if r.cfg.EmbeddingCachePath == "" {
		return
	}
	data, err := os.ReadFile(r.cfg.EmbeddingCachePath)
	if err != nil {
		return
	}
	var loaded map[string][]float32
	if err := json.Unmarshal(data, &loaded); err == nil {
		r.cacheMu.Lock()
		for k, v := range loaded {
			r.embedCache[k] = v
		}
		r.cacheMu.Unlock()
		log.Printf("[Embedding Cache] Loaded %d vectors from %s", len(loaded), r.cfg.EmbeddingCachePath)
	}
}

func (r *Router) saveEmbeddingCache() {
	if r.cfg.EmbeddingCachePath == "" {
		return
	}
	r.cacheMu.Lock()
	data, err := json.Marshal(r.embedCache)
	r.cacheMu.Unlock()
	if err != nil {
		return
	}
	_ = os.WriteFile(r.cfg.EmbeddingCachePath, data, 0o600)
}

func (r *Router) weights(semantic bool) (float64, float64) {
	if !semantic {
		return 1, 0
	}
	total := r.cfg.LexicalWeight + r.cfg.SemanticWeight
	return r.cfg.LexicalWeight / total, r.cfg.SemanticWeight / total
}

func (r *Router) selectDomains(index *compiledIndex, ranks []int, toolScores []float64) ([]string, map[int]struct{}) {
	domains, allowed := make([]string, 0, r.cfg.DomainTopK), make(map[int]struct{})
	for _, rank := range ranks {
		if len(domains) >= r.cfg.DomainTopK || len(allowed) >= r.cfg.MaxDomainTools {
			break
		}
		domain := index.domains[rank]
		domains = append(domains, domain)
		toolIndices := append([]int(nil), index.domainTools[domain]...)
		sort.SliceStable(toolIndices, func(i, j int) bool {
			if toolScores[toolIndices[i]] == toolScores[toolIndices[j]] {
				return index.tools[toolIndices[i]].ID < index.tools[toolIndices[j]].ID
			}
			return toolScores[toolIndices[i]] > toolScores[toolIndices[j]]
		})
		for _, toolIndex := range toolIndices {
			if len(allowed) >= r.cfg.MaxDomainTools {
				break
			}
			allowed[toolIndex] = struct{}{}
		}
	}
	return domains, allowed
}

func fuse(lexical, semantic []float64, lexicalWeight, semanticWeight float64) []float64 {
	result := make([]float64, len(lexical))
	for index := range result {
		result[index] = lexicalWeight * lexical[index]
		if index < len(semantic) {
			result[index] += semanticWeight * semantic[index]
		}
	}
	return result
}

func decide(candidates []Candidate, cfg Config) RouteResult {
	if len(candidates) == 0 {
		return RouteResult{Decision: DecisionNoTool, Reason: "no tools in selected domains"}
	}
	confidence, margin := candidates[0].Score, candidates[0].Score
	if len(candidates) > 1 {
		margin -= candidates[1].Score
	}
	result := RouteResult{Confidence: confidence, Margin: margin, Candidates: candidates}
	switch {
	case confidence < cfg.MinConfidence:
		result.Decision, result.Reason = DecisionNoTool, "top candidate is below the confidence threshold"
	case margin < cfg.MinMargin:
		result.Decision, result.Reason = DecisionClarify, "top candidates are too close to choose safely"
	default:
		result.Decision, result.Reason = DecisionSelected, "top candidate passed confidence and margin thresholds"
	}
	return result
}

func cosine01(left, right []float32) float64 {
	if len(left) == 0 || len(left) != len(right) {
		return 0
	}
	var dot, leftNorm, rightNorm float64
	for index := range left {
		dot += float64(left[index] * right[index])
		leftNorm += float64(left[index] * left[index])
		rightNorm += float64(right[index] * right[index])
	}
	if leftNorm == 0 || rightNorm == 0 {
		return 0
	}
	cosine := dot / math.Sqrt(leftNorm*rightNorm)
	return math.Max(0, math.Min(1, (cosine+1)/2))
}

var intentSplitRe = regexp.MustCompile(
	`(?i)\s*,?\s+(?:and(?:\s+also)?|also|then|after\s+that|while\s+you(?:'re|\s+are)\s+at\s+it)\s+`,
)

func splitIntents(query string) []string {
	query = strings.TrimSpace(query)
	matches := intentSplitRe.FindAllStringIndex(query, -1)
	if len(matches) == 0 {
		return []string{query}
	}

	var out []string
	last := 0
	for _, m := range matches {
		start, end := m[0], m[1]

		// Guard: if the text immediately following the separator starts with a
		// digit, treat the connector as part of the same clause (e.g. "add 3
		// and also 5 to the list" should NOT be split). Skip this separator but
		// still advance last so later iterations read from the correct position.
		after := strings.TrimSpace(query[end:])
		if len(after) > 0 && after[0] >= '0' && after[0] <= '9' {
			// Do not commit this split point; leave last unchanged so the
			// separator text stays attached to the current fragment.
			continue
		}

		fragment := strings.TrimSpace(query[last:start])
		// Always advance past this separator span regardless of whether fragment
		// is empty (e.g. separator at the very start of the string).
		last = end
		if fragment != "" {
			out = append(out, fragment)
		}
	}

	final := strings.TrimSpace(query[last:])
	if final != "" {
		out = append(out, final)
	}

	if len(out) == 0 {
		return []string{query}
	}
	return out
}

func mergeRouteResults(results []RouteResult) RouteResult {
	var selected []RouteResult
	var clarifying []RouteResult
	var noTool []RouteResult

	for _, res := range results {
		switch res.Decision {
		case DecisionSelected:
			selected = append(selected, res)
		case DecisionClarify:
			clarifying = append(clarifying, res)
		default:
			noTool = append(noTool, res)
		}
	}

	var merged RouteResult
	var mergedCandidates []Candidate
	var reasons []string

	if len(selected) > 0 {
		merged.Decision = DecisionSelected
		// Collect only the TOP candidate (Rank 1) from each successful fragment to avoid flooding
		for _, res := range selected {
			if len(res.Candidates) > 0 {
				mergedCandidates = append(mergedCandidates, res.Candidates[0])
			}
			reasons = append(reasons, res.Reason)
		}
		// Sort merged candidates by score descending
		sort.SliceStable(mergedCandidates, func(i, j int) bool {
			return mergedCandidates[i].Score > mergedCandidates[j].Score
		})
		// Re-assign ranks
		for i := range mergedCandidates {
			mergedCandidates[i].Rank = i + 1
		}
		merged.Candidates = mergedCandidates
		merged.Reason = "multi-intent: " + strings.Join(reasons, "; ")
		if len(mergedCandidates) > 0 {
			merged.Confidence = mergedCandidates[0].Score
			if len(mergedCandidates) > 1 {
				merged.Margin = mergedCandidates[0].Score - mergedCandidates[1].Score
			} else {
				merged.Margin = mergedCandidates[0].Score
			}
		}
	} else if len(clarifying) > 0 {
		merged.Decision = DecisionClarify
		for _, res := range clarifying {
			mergedCandidates = append(mergedCandidates, res.Candidates...)
			reasons = append(reasons, res.Reason)
		}
		merged.Candidates = mergedCandidates
		merged.Reason = "multi-intent clarification: " + strings.Join(reasons, "; ")
		if len(mergedCandidates) > 0 {
			merged.Confidence = mergedCandidates[0].Score
		}
	} else {
		merged.Decision = DecisionNoTool
		for _, res := range noTool {
			reasons = append(reasons, res.Reason)
		}
		merged.Reason = "multi-intent no tool: " + strings.Join(reasons, "; ")
	}

	// Merge traces
	var totalLatency time.Duration
	var domainLatency time.Duration
	var selectLatency time.Duration
	var usedEmbeddings bool
	var usedReranker bool
	var registryVersion uint64

	for _, res := range results {
		totalLatency += res.Trace.TotalLatency
		domainLatency += res.Trace.DomainLatency
		selectLatency += res.Trace.SelectLatency
		if res.Trace.UsedEmbeddings {
			usedEmbeddings = true
		}
		if res.Trace.UsedReranker {
			usedReranker = true
		}
		registryVersion = res.Trace.RegistryVersion
	}

	merged.Trace = Trace{
		RegistryVersion: registryVersion,
		TotalLatency:    totalLatency,
		DomainLatency:   domainLatency,
		SelectLatency:   selectLatency,
		UsedEmbeddings:  usedEmbeddings,
		UsedReranker:    usedReranker,
	}

	return merged
}

func normalizeCacheKey(query, context string) string {
	q := strings.ToLower(strings.TrimSpace(query))
	c := strings.ToLower(strings.TrimSpace(context))
	return fmt.Sprintf("%d:%s::%d:%s", len(q), q, len(c), c)
}

func (r *Router) lookupRouteCache(query, context string) (RouteResult, bool) {
	r.routeCacheMu.RLock()
	defer r.routeCacheMu.RUnlock()

	key := normalizeCacheKey(query, context)
	entry, found := r.routeCache[key]
	if !found || time.Now().After(entry.expiresAt) {
		return RouteResult{}, false
	}
	return entry.result, true
}

func (r *Router) writeRouteCache(query, context string, result RouteResult, ttl time.Duration) {
	r.routeCacheMu.Lock()
	defer r.routeCacheMu.Unlock()

	// Simple eviction if cache gets too large
	if len(r.routeCache) >= 1000 {
		now := time.Now()
		evicted := false
		for k, v := range r.routeCache {
			if now.After(v.expiresAt) {
				delete(r.routeCache, k)
				evicted = true
			}
		}
		// If no expired entries were found, evict one pseudo-random entry to enforce cap
		if !evicted && len(r.routeCache) >= 1000 {
			for k := range r.routeCache {
				delete(r.routeCache, k)
				break
			}
		}
	}

	key := normalizeCacheKey(query, context)
	r.routeCache[key] = routeCacheEntry{
		result:    result,
		expiresAt: time.Now().Add(ttl),
	}
}
