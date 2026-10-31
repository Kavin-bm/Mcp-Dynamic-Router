package router

import (
	"context"
	"os"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// traceSpan is the OpenTelemetry span handle used by the routing hot path.
// A nil span means tracing is off for this request; helpers no-op.
type traceSpan = trace.Span

const tracerName = "github.com/kavinbm16/mcp-dynamic-router/router"

const (
	spanRoute    = "router.route"
	spanCache    = "router.cache_lookup"
	spanIntent   = "router.intent_split"
	spanFragment = "router.fragment"
	spanFastPath = "router.bm25_fast_path"
	spanDomain   = "router.domain_filter"
	spanEmbed    = "router.embed_query"
	spanSelect   = "router.tool_select"
	spanRerank   = "router.rerank"
	spanDecide   = "router.decide"
	spanMerge    = "router.merge"

	attrCandidateCount = "router.candidate_count"
	attrTopScore       = "router.top_score"
	attrSecondScore    = "router.second_score"
	attrMargin         = "router.margin"
	attrCacheHit       = "router.cache_hit"
	attrFastPath       = "router.fast_path"
	attrDecision       = "router.decision"
)

type tracingCtxKey struct{}

// envTracingEnabled reports whether MCP_ROUTER_TRACING turns traces on.
// Accepted values: 1, true, yes, on (case-insensitive). Keep this list in
// sync with telemetry.Enabled.
func envTracingEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MCP_ROUTER_TRACING"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func (r *Router) tracingOn() bool {
	return r.cfg.TracingEnabled || envTracingEnabled()
}

func (r *Router) withTracing(ctx context.Context) context.Context {
	return context.WithValue(ctx, tracingCtxKey{}, r.tracingOn())
}

func tracingFrom(ctx context.Context) bool {
	enabled, _ := ctx.Value(tracingCtxKey{}).(bool)
	return enabled
}

func (r *Router) startStage(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, traceSpan) {
	if !tracingFrom(ctx) {
		return ctx, nil
	}
	return otel.Tracer(tracerName).Start(ctx, name, trace.WithAttributes(attrs...))
}

func finishSpan(span traceSpan, err error) {
	if span == nil {
		return
	}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}

func setStageAttrs(span traceSpan, attrs ...attribute.KeyValue) {
	if span == nil || len(attrs) == 0 {
		return
	}
	span.SetAttributes(attrs...)
}

// annotateResult records the shortlist the caller actually receives.
// candidate_count is the returned list, top_score is Confidence, and margin
// is the gap between the top two scores when two candidates exist. With a
// single candidate, margin is the value stored on the result (the top score
// for decide, and 1 for the BM25 fast-path).
func annotateResult(span traceSpan, result RouteResult) {
	if span == nil {
		return
	}
	count := len(result.Candidates)
	attrs := []attribute.KeyValue{
		attribute.Int(attrCandidateCount, count),
		attribute.Float64(attrTopScore, result.Confidence),
		attribute.Float64(attrMargin, result.Margin),
		attribute.String(attrDecision, string(result.Decision)),
		attribute.Int64("router.registry_version", int64(result.Trace.RegistryVersion)),
		attribute.Bool("router.used_embeddings", result.Trace.UsedEmbeddings),
		attribute.Bool("router.used_reranker", result.Trace.UsedReranker),
	}
	if count > 0 {
		attrs = append(attrs, attribute.String("router.top_tool_id", result.Candidates[0].Tool.ID))
	}
	if count > 1 {
		attrs = append(attrs, attribute.Float64(attrSecondScore, result.Candidates[1].Score))
	}
	span.SetAttributes(attrs...)
}
