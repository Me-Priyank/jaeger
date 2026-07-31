// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Report renders results as a Markdown table, grouped by scenario. Markdown
// because the output is meant to be pasted into an issue or a findings document,
// which is the deliverable jaegertracing/jaeger#9135 asks for.
func Report(results []Result) string {
	byScenario := map[string][]Result{}
	var order []string
	for _, r := range results {
		if _, ok := byScenario[r.Scenario]; !ok {
			order = append(order, r.Scenario)
		}
		byScenario[r.Scenario] = append(byScenario[r.Scenario], r)
	}

	var b strings.Builder
	b.WriteString("# MCP tool and skill A/B results\n")
	for _, name := range order {
		fmt.Fprintf(&b, "\n## Scenario: %s\n\n", name)
		b.WriteString("| tools | skill narrative | calls | steps to evidence | context bytes | call error rate | correct | justified |\n")
		b.WriteString("|---|---|---:|---:|---:|---:|:---:|:---:|\n")
		rows := byScenario[name]
		slices.SortStableFunc(rows, func(a, b Result) int {
			if c := cmp.Compare(a.Toolset, b.Toolset); c != 0 {
				return c
			}
			return cmp.Compare(a.Strategy, b.Strategy)
		})
		for _, r := range rows {
			m := r.Metrics
			fmt.Fprintf(&b, "| %s | %s | %d | %s | %s | %.0f%% | %s | %s |\n",
				r.Toolset, r.Strategy, m.Calls, steps(m.StepsToEvidence),
				thousands(m.ContextBytes), m.CallErrorRate*100,
				tick(m.Correct), tick(m.Justified))
		}
	}
	b.WriteString("\n")
	b.WriteString(deltas(results))
	return b.String()
}

// deltas summarizes the analytical-versus-granular comparison per scenario and
// narrative, because the absolute numbers matter less than the direction and size
// of the difference between arms.
func deltas(results []Result) string {
	type key struct{ scenario, strategy string }
	byKey := map[key]map[string]Metrics{}
	var order []key
	for _, r := range results {
		k := key{r.Scenario, r.Strategy}
		if _, ok := byKey[k]; !ok {
			byKey[k] = map[string]Metrics{}
			order = append(order, k)
		}
		byKey[k][r.Toolset] = r.Metrics
	}
	var b strings.Builder
	b.WriteString("## Analytical vs granular\n\n")
	b.WriteString("| scenario | skill narrative | calls saved | context bytes saved |\n")
	b.WriteString("|---|---|---:|---:|\n")
	for _, k := range order {
		a, okA := byKey[k]["analytical"]
		g, okG := byKey[k]["granular"]
		if !okA || !okG {
			continue
		}
		fmt.Fprintf(&b, "| %s | %s | %+d | %+d |\n",
			k.scenario, k.strategy, g.Calls-a.Calls, g.ContextBytes-a.ContextBytes)
	}
	return b.String()
}

func steps(n int) string {
	if n < 0 {
		return "never"
	}
	return strconv.Itoa(n)
}

func tick(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// thousands renders a byte count with separators so large payloads are legible
// at a glance in the report.
func thousands(n int) string {
	s := strconv.Itoa(n)
	if len(s) <= 3 {
		return s
	}
	var parts []string
	for len(s) > 3 {
		parts = append([]string{s[len(s)-3:]}, parts...)
		s = s[:len(s)-3]
	}
	return strings.Join(append([]string{s}, parts...), ",")
}
