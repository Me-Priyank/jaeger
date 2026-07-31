// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestScenariosAreWellFormed guards the benchmark suite itself: a scenario whose
// ground truth points at spans the fixture does not contain would make every
// metric meaningless while still reporting numbers.
func TestScenariosAreWellFormed(t *testing.T) {
	for _, sc := range Scenarios() {
		t.Run(sc.Name, func(t *testing.T) {
			require.NotEmpty(t, sc.Skill, "scenario must name the skill it exercises")
			require.NotEmpty(t, sc.Solvability, "a scenario must state why it is trace-solvable")
			require.NotEmpty(t, sc.Truth.Rationale)

			_, ok := sc.Trace.span(sc.Truth.RootCauseSpanID)
			require.True(t, ok, "root cause %q must exist in the fixture", sc.Truth.RootCauseSpanID)
			require.NotEmpty(t, sc.Truth.EvidenceSpanIDs)
			for _, id := range sc.Truth.EvidenceSpanIDs {
				_, ok := sc.Trace.span(id)
				assert.True(t, ok, "evidence span %q must exist in the fixture", id)
			}
			for _, id := range sc.Truth.RequiresDetailOn {
				_, ok := sc.Trace.span(id)
				assert.True(t, ok, "detail-required span %q must exist in the fixture", id)
			}
			root, ok := sc.Trace.root()
			require.True(t, ok, "fixture must have exactly one root span")
			assert.Empty(t, root.ParentSpanID)
		})
	}
}

// TestSkillSetsMatchShippedSkills pins the scenarios to the skills that actually
// exist in mcptools/skills, so the suite cannot drift into measuring an assistant
// Jaeger does not have.
func TestSkillSetsMatchShippedSkills(t *testing.T) {
	shipped := map[string]bool{"detect-n-plus-one": true, "error-root-cause": true}
	for _, sc := range Scenarios() {
		assert.True(t, shipped[sc.Skill], "scenario %q references unknown skill %q", sc.Name, sc.Skill)
	}
}

func TestTracePathAndDepth(t *testing.T) {
	sc := errorCascadeScenario()
	// The deepest error span sits three levels below the root.
	assert.Equal(t, 3, sc.Trace.depth(sc.Truth.RootCauseSpanID))
	path := sc.Trace.path(sc.Truth.RootCauseSpanID)
	assert.Len(t, strings.Split(path, "/"), 4, "path encodes the full ancestry: %s", path)
	assert.True(t, strings.HasSuffix(path, sc.Truth.RootCauseSpanID))

	root, ok := sc.Trace.root()
	require.True(t, ok)
	assert.Equal(t, 0, sc.Trace.depth(root.SpanID))
	assert.Equal(t, root.SpanID, sc.Trace.path(root.SpanID))
}

// TestTracePathTerminatesOnCycle proves the ancestry walk cannot hang on a
// malformed fixture. A benchmark that deadlocks on bad input is worse than one
// that reports a wrong number.
func TestTracePathTerminatesOnCycle(t *testing.T) {
	cyclic := Trace{TraceID: "t", Spans: []Span{
		{SpanID: "a", ParentSpanID: "b"},
		{SpanID: "b", ParentSpanID: "a"},
	}}
	assert.NotEmpty(t, cyclic.path("a"))
	assert.LessOrEqual(t, cyclic.depth("a"), 2)
}

func TestToolsetSurfaces(t *testing.T) {
	tr := nPlusOneScenario().Trace
	assert.Equal(t, []string{"get_trace_topology", "get_trace_errors", "get_span_details"},
		NewAnalyticalToolset(tr).Tools())
	assert.Equal(t, []string{"get_services", "get_trace_spans", "get_span_details"},
		NewGranularToolset(tr).Tools())
}

// TestRefusedToolIsMeasuredNotFatal proves a call to a tool the variant does not
// expose becomes a recorded error, which is what the call-error-rate metric is
// built from.
func TestRefusedToolIsMeasuredNotFatal(t *testing.T) {
	tr := nPlusOneScenario().Trace
	res := NewGranularToolset(tr).Call("get_trace_topology", nil)
	require.Error(t, res.Err)
	assert.Empty(t, res.Payload)

	rec := newRecordingToolset(NewGranularToolset(tr))
	rec.Call("get_trace_topology", nil) // refused
	rec.Call("get_trace_spans", nil)    // accepted
	m := evaluate(rec, GroundTruth{}, Answer{})
	assert.Equal(t, 2, m.Calls)
	assert.InDelta(t, 0.5, m.CallErrorRate, 1e-9)
}

func TestBadArgumentsAreCallErrors(t *testing.T) {
	ts := NewGranularToolset(nPlusOneScenario().Trace)
	require.Error(t, ts.Call("get_span_details", map[string]any{"span_ids": "not-a-slice"}).Err)
	require.Error(t, ts.Call("get_span_details", map[string]any{"span_ids": []string{}}).Err)
	require.Error(t, ts.Call("no_such_tool", nil).Err)
}

// TestGetSpanDetailsReportsMissingSpans mirrors types.GetSpanDetailsOutput, whose
// Error field names IDs that were not found.
func TestGetSpanDetailsReportsMissingSpans(t *testing.T) {
	sc := errorCascadeScenario()
	res := NewAnalyticalToolset(sc.Trace).Call("get_span_details",
		map[string]any{"span_ids": []string{sc.Truth.RootCauseSpanID, "deadbeef"}})
	require.NoError(t, res.Err)
	var out struct {
		Spans []spanDetail `json:"spans"`
		Error string       `json:"error"`
	}
	require.NoError(t, json.Unmarshal(res.Payload, &out))
	assert.Len(t, out.Spans, 1)
	assert.Contains(t, out.Error, "deadbeef")
	assert.Equal(t, []string{sc.Truth.RootCauseSpanID}, res.SpansDetailed)
}

// TestTopologyIsCompactAndDetailIsNot pins the payload axis the whole experiment
// rests on: the structural overview must be materially cheaper per span than full
// detail, or "context bloat" would not be measuring anything.
func TestTopologyIsCompactAndDetailIsNot(t *testing.T) {
	sc := errorCascadeScenario()
	ts := NewAnalyticalToolset(sc.Trace)
	topo := ts.Call("get_trace_topology", nil)
	all := make([]string, 0, len(sc.Trace.Spans))
	for _, s := range sc.Trace.Spans {
		all = append(all, s.SpanID)
	}
	detail := ts.Call("get_span_details", map[string]any{"span_ids": all})
	require.NoError(t, topo.Err)
	require.NoError(t, detail.Err)
	assert.Less(t, len(topo.Payload), len(detail.Payload),
		"a structural overview must cost less than full detail for the same spans")
	assert.Empty(t, topo.SpansDetailed, "topology must not surface attributes or events")
	assert.NotEmpty(t, topo.SpansSummarized)
}

// TestBareEnumerationIsNotEvidence is the correctness guard on steps-to-evidence:
// get_trace_spans surfaces every span ID but nothing to reason from, so it must
// not satisfy evidence on its own.
func TestBareEnumerationIsNotEvidence(t *testing.T) {
	sc := nPlusOneScenario()
	rec := newRecordingToolset(NewGranularToolset(sc.Trace))
	rec.Call("get_trace_spans", nil)
	m := evaluate(rec, sc.Truth, Answer{RootCauseSpanID: sc.Truth.RootCauseSpanID})
	assert.True(t, m.Correct, "the answer is right")
	assert.False(t, m.Justified, "but bare IDs cannot justify it")
	assert.Equal(t, -1, m.StepsToEvidence)
}

// TestCorrectButUnjustifiedIsDistinguished is the reason correctness alone is not
// the score: an agent can name the right span without having seen why.
func TestCorrectButUnjustifiedIsDistinguished(t *testing.T) {
	sc := errorCascadeScenario()
	rec := newRecordingToolset(NewAnalyticalToolset(sc.Trace))
	rec.Call("get_trace_topology", nil) // summarizes every span, details none
	m := evaluate(rec, sc.Truth, Answer{RootCauseSpanID: sc.Truth.RootCauseSpanID})
	assert.True(t, m.Correct)
	assert.False(t, m.Justified,
		"the scenario requires attribute-level detail, which topology never returns")
}

// TestAllCellsSolveBothScenarios is the headline correctness claim: every
// combination of tool variant and skill narrative reaches the verified root
// cause with justification, so the metric differences between cells are pure
// efficiency rather than one arm simply failing.
func TestAllCellsSolveBothScenarios(t *testing.T) {
	for _, r := range RunMatrix(Scenarios()) {
		t.Run(r.Scenario+"/"+r.Toolset+"/"+r.Strategy, func(t *testing.T) {
			assert.True(t, r.Metrics.Correct, "must identify the ground-truth root cause")
			assert.True(t, r.Metrics.Justified, "must assemble the required evidence")
			assert.Positive(t, r.Metrics.StepsToEvidence)
			assert.Zero(t, r.Metrics.CallErrorRate, "a well-formed narrative makes no invalid calls")
		})
	}
}

// TestAnalyticalToolsReduceCostOnWideTraces is the finding the harness exists to
// produce, asserted so a regression in the tool shapes would fail the build:
// on the 27-span N+1 trace the summarizing overview costs strictly fewer calls
// and bytes than bare enumeration plus drill-down.
func TestAnalyticalToolsReduceCostOnWideTraces(t *testing.T) {
	byArm := map[string]Metrics{}
	for _, r := range RunMatrix([]Scenario{nPlusOneScenario()}) {
		if r.Strategy == GoalOriented.String() {
			byArm[r.Toolset] = r.Metrics
		}
	}
	a, g := byArm["analytical"], byArm["granular"]
	assert.Less(t, a.Calls, g.Calls)
	assert.Less(t, a.ContextBytes, g.ContextBytes)
	assert.Less(t, a.StepsToEvidence, g.StepsToEvidence)
}

// TestProceduralCostsMoreThanGoalOriented pins the skill-narrative axis: following
// a verification step whose data an earlier call already returned is not free.
func TestProceduralCostsMoreThanGoalOriented(t *testing.T) {
	for _, sc := range Scenarios() {
		byStrategy := map[string]Metrics{}
		for _, r := range RunMatrix([]Scenario{sc}) {
			if r.Toolset == "analytical" {
				byStrategy[r.Strategy] = r.Metrics
			}
		}
		proc, goal := byStrategy[Procedural.String()], byStrategy[GoalOriented.String()]
		assert.Greater(t, proc.Calls, goal.Calls, "scenario %s", sc.Name)
		assert.Greater(t, proc.ContextBytes, goal.ContextBytes, "scenario %s", sc.Name)
	}
}

// TestMatrixIsDeterministic is the property that makes the harness a measuring
// instrument rather than an anecdote generator. It is also what a real model
// cannot provide, which is why the scripted agent exists.
func TestMatrixIsDeterministic(t *testing.T) {
	first := RunMatrix(Scenarios())
	for i := 0; i < 5; i++ {
		assert.Equal(t, first, RunMatrix(Scenarios()), "run %d diverged", i)
	}
}

func TestReportRendersEveryCell(t *testing.T) {
	results := RunMatrix(Scenarios())
	out := Report(results)
	for _, sc := range Scenarios() {
		assert.Contains(t, out, "## Scenario: "+sc.Name)
	}
	assert.Contains(t, out, "## Analytical vs granular")
	assert.Contains(t, out, "steps to evidence")
	// One data row per cell, plus the delta table's rows.
	assert.Equal(t, len(results), strings.Count(out, "| 0% |"),
		"every cell should render with a zero call-error rate")
}

func TestReportHandlesNeverJustified(t *testing.T) {
	out := Report([]Result{{
		Scenario: "s", Toolset: "analytical", Strategy: "goal-oriented",
		Metrics: Metrics{Calls: 1, StepsToEvidence: -1},
	}})
	assert.Contains(t, out, "never")
	// With only one arm present the delta table must skip the pair rather than
	// render a misleading half-comparison.
	assert.NotContains(t, out, "| s | goal-oriented |")
}

func TestThousandsFormatting(t *testing.T) {
	cases := map[int]string{0: "0", 999: "999", 1000: "1,000", 21505: "21,505", 1234567: "1,234,567"}
	for in, want := range cases {
		assert.Equal(t, want, thousands(in))
	}
}

func TestDescribeScenariosPublishesSolvability(t *testing.T) {
	out := describeScenarios(Scenarios())
	for _, sc := range Scenarios() {
		assert.Contains(t, out, sc.Solvability, "the solvability argument must be auditable")
		assert.Contains(t, out, sc.Truth.RootCauseSpanID)
	}
}

func TestSpanStringIsReadable(t *testing.T) {
	sc := errorCascadeScenario()
	s, ok := sc.Trace.span(sc.Truth.RootCauseSpanID)
	require.True(t, ok)
	str := s.String()
	assert.Contains(t, str, s.SpanID)
	assert.Contains(t, str, "payment-service")
	assert.Contains(t, str, statusError)
}

func TestChunkRespectsBatchSize(t *testing.T) {
	ids := make([]string, 45)
	for i := range ids {
		ids[i] = string(rune('a' + i%26))
	}
	batches := chunk(ids, detailBatchSize)
	require.Len(t, batches, 3)
	assert.Len(t, batches[0], detailBatchSize)
	assert.Len(t, batches[2], 5)
	assert.Empty(t, chunk(nil, detailBatchSize))
}
