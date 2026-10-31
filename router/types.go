package router

import (
	"context"
	"encoding/json"
	"time"
)

// Tool is the router's provider-neutral representation of an MCP tool.
type Tool struct {
	ID          string          `json:"id"`
	Server      string          `json:"server"`
	Name        string          `json:"name"`
	Domain      string          `json:"domain,omitempty"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
	ReadOnly    bool            `json:"read_only,omitempty"`
}

// Embedder is optional. Without one, the router remains a fast lexical router.
// Implementations should return one L2-normalized vector per input string.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// Reranker performs second-stage selection over the domain shortlist.
// Implementations commonly use a small, low-temperature LLM.
type Reranker interface {
	Rerank(ctx context.Context, utterance, conversationContext string, tools []Tool) (map[string]float64, error)
}

type Config struct {
	MaxDomainTools     int
	DomainTopK         int
	CandidateTopK      int
	LexicalWeight      float64
	SemanticWeight     float64
	RerankerWeight     float64
	MinConfidence      float64
	MinMargin          float64
	IncludeContext     bool
	DescriptionPolicy  DescriptionPolicy
	BM25FastPath       float64
	RouteCacheTTL      time.Duration
	EmbeddingCachePath string
	// TracingEnabled emits one OpenTelemetry trace per Route call, with a
	// child span for each routing stage. Off by default. The same traces are
	// enabled when MCP_ROUTER_TRACING is 1, true, yes, or on. Spans are
	// exported only after a TracerProvider is installed; cmd/routerd does
	// that when tracing is turned on.
	TracingEnabled bool
}

func DefaultConfig() Config {
	return Config{
		MaxDomainTools:    20,
		DomainTopK:        2,
		CandidateTopK:     5,
		LexicalWeight:     0.45,
		SemanticWeight:    0.55,
		RerankerWeight:    0.65,
		MinConfidence:     0.42,
		MinMargin:         0.06,
		IncludeContext:    true,
		DescriptionPolicy: DefaultDescriptionPolicy(),
		BM25FastPath:      8.0,
		RouteCacheTTL:     30 * time.Second,
	}
}

type RouteRequest struct {
	Utterance string `json:"utterance"`
	Context   string `json:"context,omitempty"`
	Final     bool   `json:"final"`
}

type Decision string

const (
	DecisionSelected Decision = "selected"
	DecisionClarify  Decision = "clarify"
	DecisionNoTool   Decision = "no_tool"
)

type Candidate struct {
	Tool          Tool    `json:"tool"`
	Score         float64 `json:"score"`
	LexicalScore  float64 `json:"lexical_score"`
	SemanticScore float64 `json:"semantic_score,omitempty"`
	Rank          int     `json:"rank"`
}

type Trace struct {
	RegistryVersion uint64        `json:"registry_version"`
	Domains         []string      `json:"domains"`
	DomainLatency   time.Duration `json:"domain_latency"`
	SelectLatency   time.Duration `json:"select_latency"`
	TotalLatency    time.Duration `json:"total_latency"`
	UsedEmbeddings  bool          `json:"used_embeddings"`
	UsedReranker    bool          `json:"used_reranker"`
}

type RouteResult struct {
	Decision   Decision    `json:"decision"`
	Confidence float64     `json:"confidence"`
	Margin     float64     `json:"margin"`
	Candidates []Candidate `json:"candidates"`
	Reason     string      `json:"reason"`
	Trace      Trace       `json:"trace"`
}
