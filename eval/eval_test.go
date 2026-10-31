package eval

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kavinbm16/mcp-dynamic-router/router"
)

func TestLabelledRoutingAccuracy(t *testing.T) {
	tools, err := LoadTools("../testdata/tools.json")
	if err != nil {
		t.Fatal(err)
	}
	engine, err := BuildRouter(context.Background(), tools, nil, router.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	jsonl, err := LoadCases("../testdata/cases.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	yamlCases, err := LoadCases("../testdata/cases.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !sameCases(jsonl, yamlCases) {
		t.Fatalf("cases.jsonl and cases.yaml diverged\n jsonl=%+v\n yaml=%+v", jsonl, yamlCases)
	}
	for _, path := range []string{"../testdata/cases.jsonl", "../testdata/cases.yaml"} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			cases, err := LoadCases(path)
			if err != nil {
				t.Fatal(err)
			}
			summary, err := Evaluate(context.Background(), engine, cases, 5)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("\n%s", summary.FormatVerbose())
			if len(summary.Failures) > 0 {
				t.Fatalf("labelled set has failures:\n%s", summary.Format())
			}
		})
	}
}

func TestEvaluateReportsMissesAndAbstentions(t *testing.T) {
	engine := scriptRouter{results: []router.RouteResult{
		{
			Decision:   router.DecisionSelected,
			Confidence: 0.9,
			Margin:     0.4,
			Candidates: []router.Candidate{{Tool: router.Tool{ID: "weather.get_forecast"}, Score: 0.9, Rank: 1}},
			Trace:      router.Trace{TotalLatency: 3 * time.Microsecond},
		},
		{
			Decision:   router.DecisionClarify,
			Confidence: 0.5,
			Margin:     0.01,
			Candidates: []router.Candidate{
				{Tool: router.Tool{ID: "calendar.create_event"}, Score: 0.5, Rank: 1},
				{Tool: router.Tool{ID: "weather.get_forecast"}, Score: 0.49, Rank: 2},
			},
			Trace: router.Trace{TotalLatency: 5 * time.Microsecond},
		},
		{
			Decision:   router.DecisionSelected,
			Confidence: 0.8,
			Candidates: []router.Candidate{{Tool: router.Tool{ID: "weather.get_forecast"}, Score: 0.8, Rank: 1}},
			Trace:      router.Trace{TotalLatency: time.Microsecond},
		},
	}}
	cases := []Case{
		{Utterance: "forecast", ExpectedTool: "weather.get_forecast", ExpectedDecision: "selected"},
		{Utterance: "mixed", ExpectedTool: "weather.get_forecast"},
		{Utterance: "hello", ExpectedTool: ""},
	}
	summary, err := Evaluate(context.Background(), &engine, cases, 5)
	if err != nil {
		t.Fatal(err)
	}
	if summary.ToolCases != 2 || summary.NoToolCases != 1 || summary.Top1 != 1 || summary.TopK != 2 || summary.FalseRoutes != 1 {
		t.Fatalf("unexpected counts: %+v", summary)
	}
	if len(summary.Failures) != 2 {
		t.Fatalf("expected two failures, got %+v", summary.Failures)
	}
	if summary.Failures[0].Rank != 2 || !strings.Contains(summary.Failures[0].Reason, "not rank 1") {
		t.Fatalf("rank miss: %+v", summary.Failures[0])
	}
	if !strings.Contains(summary.Failures[1].Reason, "expected no_tool") {
		t.Fatalf("abstain miss: %+v", summary.Failures[1])
	}
	report := summary.Format()
	if !strings.Contains(report, "top1=50.00%") || !strings.Contains(report, "top5=100.00%") || !strings.Contains(report, "false_route=100.00%") || !strings.Contains(report, "failures=2") {
		t.Fatalf("report:\n%s", report)
	}
}

func TestLoadCasesFormats(t *testing.T) {
	dir := t.TempDir()
	jsonl := filepath.Join(dir, "cases.jsonl")
	if err := os.WriteFile(jsonl, []byte("\n# comment\n{\"utterance\":\"forecast\",\"expected_tool\":\"weather.get_forecast\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	wrapped := filepath.Join(dir, "cases.json")
	if err := os.WriteFile(wrapped, []byte(`{"cases":[{"utterance":"forecast","expected_decision":"selected"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	yamlPath := filepath.Join(dir, "cases.yaml")
	if err := os.WriteFile(yamlPath, []byte("cases:\n  - utterance: forecast\n    expected_tool: weather.get_forecast\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{jsonl, wrapped, yamlPath} {
		cases, err := LoadCases(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if len(cases) != 1 || cases[0].Utterance != "forecast" {
			t.Fatalf("%s parsed %#v", path, cases)
		}
	}
	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("- utterance: hi\n  expected_decision: maybe\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCases(bad); err == nil {
		t.Fatal("expected invalid decision to fail")
	}
	if _, err := LoadCases(filepath.Join(dir, "cases.txt")); err == nil {
		t.Fatal("expected unsupported extension to fail")
	}
}

type scriptRouter struct {
	results []router.RouteResult
	next    int
}

func (s *scriptRouter) Route(context.Context, router.RouteRequest) (router.RouteResult, error) {
	result := s.results[s.next]
	s.next++
	return result, nil
}

func sameCases(left, right []Case) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i].Utterance != right[i].Utterance || left[i].Context != right[i].Context || left[i].ExpectedTool != right[i].ExpectedTool || left[i].ExpectedDecision != right[i].ExpectedDecision {
			return false
		}
	}
	return true
}
