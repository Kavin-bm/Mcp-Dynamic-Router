// Package eval scores the router against a labelled set of utterances.
// Datasets may be JSONL, a JSON array, or YAML. A case with an empty
// expected_tool is an abstention: the router should return decision no_tool.
package eval

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kavinbm16/mcp-dynamic-router/router"
	"gopkg.in/yaml.v3"
)

// Case is one labelled routing example.
type Case struct {
	Utterance        string `json:"utterance" yaml:"utterance"`
	Context          string `json:"context,omitempty" yaml:"context,omitempty"`
	ExpectedTool     string `json:"expected_tool,omitempty" yaml:"expected_tool,omitempty"`
	ExpectedDecision string `json:"expected_decision,omitempty" yaml:"expected_decision,omitempty"`
}

// CaseResult is the outcome of one case. Rank is the 1-based position of
// ExpectedTool in the returned shortlist, or 0 when that tool is absent.
type CaseResult struct {
	Index            int
	Utterance        string
	Context          string
	ExpectedTool     string
	ExpectedDecision string
	Decision         string
	TopTool          string
	Rank             int
	Latency          time.Duration
	Passed           bool
	Reason           string
}

// Summary is the aggregate report printed by cmd/router-eval.
type Summary struct {
	Cases       int
	ToolCases   int
	NoToolCases int
	Top1        int
	TopK        int
	K           int
	FalseRoutes int
	Latencies   []time.Duration
	Results     []CaseResult
	Failures    []CaseResult
}

// Router is the subset of *router.Router the harness calls.
type Router interface {
	Route(context.Context, router.RouteRequest) (router.RouteResult, error)
}

// LoadCases reads a JSONL, JSON, or YAML dataset. The format is taken from
// the file extension (.jsonl, .ndjson, .json, .yaml, .yml).
func LoadCases(path string) ([]Case, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read cases: %w", err)
	}
	var cases []Case
	switch strings.ToLower(filepath.Ext(path)) {
	case ".yaml", ".yml":
		cases, err = parseYAMLCases(data)
	case ".jsonl", ".ndjson":
		cases, err = parseJSONLCases(data)
	case ".json":
		cases, err = parseJSONCases(data)
	default:
		return nil, fmt.Errorf("unsupported cases extension %q (use .jsonl, .json, .yaml, or .yml)", filepath.Ext(path))
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cases, nil
}

// LoadTools reads the JSON array of tools used to build an eval index.
func LoadTools(path string) ([]router.Tool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read tools: %w", err)
	}
	var tools []router.Tool
	if err := json.Unmarshal(data, &tools); err != nil {
		return nil, fmt.Errorf("parse tools %s: %w", path, err)
	}
	if len(tools) == 0 {
		return nil, fmt.Errorf("tools file %s is empty", path)
	}
	return tools, nil
}

// BuildRouter loads tools into a registry and compiles an index with cfg.
// Server names are applied in sorted order so the registry version is stable.
func BuildRouter(ctx context.Context, tools []router.Tool, embedder router.Embedder, cfg router.Config) (*router.Router, error) {
	registry := router.NewRegistry()
	byServer := make(map[string][]router.Tool)
	for _, tool := range tools {
		server := strings.TrimSpace(tool.Server)
		if server == "" {
			server = "default"
		}
		byServer[server] = append(byServer[server], tool)
	}
	servers := make([]string, 0, len(byServer))
	for server := range byServer {
		servers = append(servers, server)
	}
	sort.Strings(servers)
	for _, server := range servers {
		if err := registry.ReplaceServer(server, byServer[server]); err != nil {
			return nil, err
		}
	}
	engine := router.New(registry, embedder, cfg)
	if err := engine.Refresh(ctx); err != nil {
		return nil, err
	}
	return engine, nil
}

// Evaluate routes every case and scores retrieval plus abstention.
// topK is the depth of the top-k hit (defaults to 5 when topK < 1).
// A tool case passes when ExpectedTool is rank 1. An abstention case
// (empty ExpectedTool) passes when the decision is no_tool. If
// ExpectedDecision is set, it must match as well. Top-k can only see
// candidates the router returned, which DefaultConfig caps at CandidateTopK.
func Evaluate(ctx context.Context, engine Router, cases []Case, topK int) (Summary, error) {
	if topK < 1 {
		topK = 5
	}
	summary := Summary{Cases: len(cases), K: topK, Results: make([]CaseResult, 0, len(cases))}
	for i, test := range cases {
		result, err := engine.Route(ctx, router.RouteRequest{Utterance: test.Utterance, Context: test.Context, Final: true})
		if err != nil {
			return Summary{}, fmt.Errorf("case %d: %w", i+1, err)
		}
		outcome := scoreCase(i+1, test, result)
		summary.Latencies = append(summary.Latencies, outcome.Latency)
		summary.Results = append(summary.Results, outcome)
		if test.ExpectedTool == "" {
			summary.NoToolCases++
			if result.Decision != router.DecisionNoTool {
				summary.FalseRoutes++
			}
		} else {
			summary.ToolCases++
			if outcome.Rank == 1 {
				summary.Top1++
			}
			if outcome.Rank >= 1 && outcome.Rank <= topK {
				summary.TopK++
			}
		}
		if !outcome.Passed {
			summary.Failures = append(summary.Failures, outcome)
		}
	}
	return summary, nil
}

func scoreCase(index int, test Case, result router.RouteResult) CaseResult {
	outcome := CaseResult{
		Index:            index,
		Utterance:        test.Utterance,
		Context:          test.Context,
		ExpectedTool:     test.ExpectedTool,
		ExpectedDecision: test.ExpectedDecision,
		Decision:         string(result.Decision),
		Latency:          result.Trace.TotalLatency,
		Passed:           true,
	}
	if len(result.Candidates) > 0 {
		outcome.TopTool = result.Candidates[0].Tool.ID
	}
	var reasons []string
	if test.ExpectedTool != "" {
		for i, candidate := range result.Candidates {
			if candidate.Tool.ID == test.ExpectedTool {
				outcome.Rank = i + 1
				break
			}
		}
		if outcome.Rank != 1 {
			reasons = append(reasons, "expected tool is not rank 1")
		}
	} else if result.Decision != router.DecisionNoTool {
		reasons = append(reasons, "expected no_tool")
	}
	if test.ExpectedDecision != "" && outcome.Decision != test.ExpectedDecision {
		reasons = append(reasons, "decision mismatch")
	}
	if len(reasons) > 0 {
		outcome.Passed = false
		outcome.Reason = strings.Join(reasons, "; ")
	}
	return outcome
}

// Format prints the summary and, when present, one line per failure.
func (s Summary) Format() string {
	var buf bytes.Buffer
	s.write(&buf, false)
	return buf.String()
}

// FormatVerbose prints the summary, one line per case, then failures.
func (s Summary) FormatVerbose() string {
	var buf bytes.Buffer
	s.write(&buf, true)
	return buf.String()
}

func (s Summary) write(w io.Writer, verbose bool) {
	fmt.Fprintf(w, "cases=%d tool_cases=%d no_tool_cases=%d\n", s.Cases, s.ToolCases, s.NoToolCases)
	fmt.Fprintf(w, "top1=%.2f%% top%d=%.2f%% false_route=%.2f%% p50=%s p95=%s\n",
		percent(s.Top1, s.ToolCases), s.K, percent(s.TopK, s.ToolCases), percent(s.FalseRoutes, s.NoToolCases),
		percentile(s.Latencies, 0.50), percentile(s.Latencies, 0.95))
	fmt.Fprintf(w, "failures=%d\n", len(s.Failures))
	if verbose {
		for _, outcome := range s.Results {
			status := "ok"
			if !outcome.Passed {
				status = "FAIL"
			}
			fmt.Fprintf(w, "%s index=%d utterance=%q expected_tool=%q decision=%s top_tool=%q rank=%d latency=%s\n",
				status, outcome.Index, outcome.Utterance, outcome.ExpectedTool, outcome.Decision, outcome.TopTool, outcome.Rank, outcome.Latency)
		}
	}
	for _, outcome := range s.Failures {
		fmt.Fprintf(w, "FAIL index=%d utterance=%q expected_tool=%q expected_decision=%q decision=%s top_tool=%q rank=%d reason=%q\n",
			outcome.Index, outcome.Utterance, outcome.ExpectedTool, outcome.ExpectedDecision, outcome.Decision, outcome.TopTool, outcome.Rank, outcome.Reason)
	}
}

func parseJSONLCases(data []byte) ([]Case, error) {
	var cases []Case
	scanner := bufio.NewScanner(bytes.NewReader(data))
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || bytes.HasPrefix(line, []byte("#")) {
			continue
		}
		var test Case
		if err := json.Unmarshal(line, &test); err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo, err)
		}
		cases = append(cases, test)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return validateCases(cases)
}

func parseJSONCases(data []byte) ([]Case, error) {
	var list []Case
	if err := json.Unmarshal(data, &list); err == nil {
		return validateCases(list)
	}
	var wrapped struct {
		Cases []Case `json:"cases"`
	}
	if err := json.Unmarshal(data, &wrapped); err != nil {
		return nil, fmt.Errorf("parse json cases: %w", err)
	}
	return validateCases(wrapped.Cases)
}

func parseYAMLCases(data []byte) ([]Case, error) {
	var list []Case
	if err := yaml.Unmarshal(data, &list); err == nil {
		return validateCases(list)
	} else {
		var wrapped struct {
			Cases []Case `yaml:"cases"`
		}
		if wrappedErr := yaml.Unmarshal(data, &wrapped); wrappedErr != nil {
			return nil, fmt.Errorf("parse yaml cases: %w", err)
		}
		return validateCases(wrapped.Cases)
	}
}

func validateCases(cases []Case) ([]Case, error) {
	if len(cases) == 0 {
		return nil, fmt.Errorf("dataset contains no cases")
	}
	for i, test := range cases {
		if strings.TrimSpace(test.Utterance) == "" {
			return nil, fmt.Errorf("case %d: utterance is required", i+1)
		}
		switch test.ExpectedDecision {
		case "", "selected", "clarify", "no_tool":
		default:
			return nil, fmt.Errorf("case %d: expected_decision %q must be selected, clarify, or no_tool", i+1, test.ExpectedDecision)
		}
	}
	return cases, nil
}

func percentile(values []time.Duration, quantile float64) time.Duration {
	if len(values) == 0 {
		return 0
	}
	copied := append([]time.Duration(nil), values...)
	sort.Slice(copied, func(i, j int) bool { return copied[i] < copied[j] })
	index := int(float64(len(copied)-1) * quantile)
	return copied[index]
}

func percent(value, total int) float64 {
	if total == 0 {
		return 0
	}
	return 100 * float64(value) / float64(total)
}
