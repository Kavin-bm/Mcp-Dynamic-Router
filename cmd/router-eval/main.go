package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/kavinbm16/mcp-dynamic-router/embedding"
	"github.com/kavinbm16/mcp-dynamic-router/eval"
	"github.com/kavinbm16/mcp-dynamic-router/router"
)

func main() {
	toolsPath := flag.String("tools", "testdata/tools.json", "path to tool registry JSON")
	casesPath := flag.String("cases", "testdata/cases.jsonl", "path to labelled cases (.jsonl, .json, .yaml, or .yml)")
	ollamaURL := flag.String("ollama", "", "optional Ollama URL; empty uses the lexical router only")
	model := flag.String("model", "nomic-embed-text", "Ollama embedding model")
	topK := flag.Int("top-k", 5, "candidate depth counted as a top-k hit")
	minTop1 := flag.Float64("min-top1", -1, "exit 1 when top-1 accuracy percent is below this value; negative disables the check")
	strict := flag.Bool("strict", false, "exit 1 when any labelled case fails")
	verbose := flag.Bool("verbose", false, "print one line per case in addition to the summary")
	flag.Parse()

	tools, err := eval.LoadTools(*toolsPath)
	fatalIf(err)
	var embedder router.Embedder
	if *ollamaURL != "" {
		embedder = &embedding.Ollama{BaseURL: *ollamaURL, Model: *model}
	}
	engine, err := eval.BuildRouter(context.Background(), tools, embedder, router.DefaultConfig())
	fatalIf(err)
	cases, err := eval.LoadCases(*casesPath)
	fatalIf(err)
	summary, err := eval.Evaluate(context.Background(), engine, cases, *topK)
	fatalIf(err)
	if *verbose {
		fmt.Print(summary.FormatVerbose())
	} else {
		fmt.Print(summary.Format())
	}
	if *strict && len(summary.Failures) > 0 {
		os.Exit(1)
	}
	if *minTop1 >= 0 {
		accuracy := 0.0
		if summary.ToolCases > 0 {
			accuracy = 100 * float64(summary.Top1) / float64(summary.ToolCases)
		}
		if accuracy < *minTop1 {
			fmt.Fprintf(os.Stderr, "top1 %.2f%% is below min-top1 %.2f%%\n", accuracy, *minTop1)
			os.Exit(1)
		}
	}
}

func fatalIf(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
