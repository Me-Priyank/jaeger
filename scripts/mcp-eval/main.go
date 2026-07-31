// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

// Command mcp-eval runs an A/B evaluation of Jaeger's MCP tool surface and Skill
// narratives against a suite of deterministic, trace-solvable fault scenarios.
//
// It is a proof of concept for the LFX 2026 Term 3 project "Benchmarking the AI
// Assistant's MCP Tools and Skills" (jaegertracing/jaeger#9135). See the README
// in this directory for the hypothesis under test and the limits of what a
// scripted agent can establish.
//
// Usage:
//
//	go run ./scripts/mcp-eval            # print the Markdown report
//	go run ./scripts/mcp-eval -scenarios # describe the benchmark suite
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
)

func main() {
	describe := flag.Bool("scenarios", false, "describe the benchmark suite and exit")
	flag.Parse()

	scenarios := Scenarios()
	if *describe {
		fmt.Print(describeScenarios(scenarios))
		return
	}
	if _, err := os.Stdout.WriteString(Report(RunMatrix(scenarios))); err != nil {
		fmt.Fprintln(os.Stderr, "failed to write report:", err)
		os.Exit(1)
	}
}

// describeScenarios renders the suite with the ground truth and, most
// importantly, the solvability argument for each scenario. Publishing that
// argument is what makes the benchmark auditable: a reviewer can disagree that a
// fault is genuinely trace-solvable, which they cannot do if only the answer is
// recorded.
func describeScenarios(scenarios []Scenario) string {
	var b strings.Builder
	b.WriteString("# Benchmark suite\n")
	for i := range scenarios {
		sc := &scenarios[i]
		fmt.Fprintf(&b, "\n## %s\n\nSkill: %s\nSpans: %d\n\n%s\n\n",
			sc.Name, sc.Skill, len(sc.Trace.Spans), sc.Description)
		fmt.Fprintf(&b, "- Root cause: `%s`\n", sc.Truth.RootCauseSpanID)
		fmt.Fprintf(&b, "- Evidence required: %v\n", sc.Truth.EvidenceSpanIDs)
		if len(sc.Truth.RequiresDetailOn) > 0 {
			fmt.Fprintf(&b, "- Detail required on: %v\n", sc.Truth.RequiresDetailOn)
		}
		fmt.Fprintf(&b, "- Rationale: %s\n", sc.Truth.Rationale)
		fmt.Fprintf(&b, "- Why trace-solvable: %s\n", sc.Solvability)
	}
	return b.String()
}
