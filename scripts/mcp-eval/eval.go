// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package main

// CallRecord is one observed tool invocation.
type CallRecord struct {
	Tool string
	// Bytes is the size of the returned payload, the per-call contribution to
	// context bloat.
	Bytes int
	// Failed is true when the tool rejected the call.
	Failed bool
}

// recordingToolset wraps a Toolset and records every call. The agent cannot tell
// it apart from the real thing, so instrumenting costs the agent nothing and the
// trajectory is captured without the agent cooperating.
type recordingToolset struct {
	inner Toolset
	calls []CallRecord
	// summarized and detailed record the call index at which each span first
	// became usable at that level, so steps-to-evidence can be computed as the
	// trajectory unfolds rather than reconstructed afterwards.
	summarized map[string]int
	detailed   map[string]int
}

func newRecordingToolset(inner Toolset) *recordingToolset {
	return &recordingToolset{inner: inner, summarized: map[string]int{}, detailed: map[string]int{}}
}

func (r *recordingToolset) Name() string    { return r.inner.Name() }
func (r *recordingToolset) Tools() []string { return r.inner.Tools() }

func (r *recordingToolset) Call(name string, args map[string]any) ToolResult {
	res := r.inner.Call(name, args)
	r.calls = append(r.calls, CallRecord{Tool: name, Bytes: len(res.Payload), Failed: res.Err != nil})
	idx := len(r.calls) // 1-based: "after N calls"
	for _, id := range res.SpansSummarized {
		if _, ok := r.summarized[id]; !ok {
			r.summarized[id] = idx
		}
	}
	for _, id := range res.SpansDetailed {
		if _, ok := r.detailed[id]; !ok {
			r.detailed[id] = idx
		}
	}
	return res
}

// Metrics are the three trajectory measures named in jaegertracing/jaeger#9135,
// plus the correctness check that makes them meaningful.
type Metrics struct {
	// Calls is the total number of tool invocations.
	Calls int
	// CallErrorRate is the fraction of calls the tool layer rejected. It proxies
	// how well the tool surface guides correct parameter construction.
	CallErrorRate float64
	// StepsToEvidence is the number of calls after which every span the ground
	// truth requires had been surfaced, with detail where the answer depends on
	// attributes or events. It is -1 when the agent never assembled the evidence,
	// which is materially different from answering quickly.
	StepsToEvidence int
	// ContextBytes is the total payload the agent had to read. Bytes are a
	// deterministic proxy for tokens; see the README for why that substitution is
	// acceptable here and where it breaks down.
	ContextBytes int
	// Correct reports whether the agent named the ground-truth root cause.
	Correct bool
	// Justified reports whether the evidence was actually assembled. An answer
	// that is Correct but not Justified was reached without support, which is the
	// failure mode a correctness-only score hides.
	Justified bool
}

// Result is one cell of the experiment matrix.
type Result struct {
	Scenario string
	Toolset  string
	Strategy string
	Metrics  Metrics
	Calls    []CallRecord
}

// evaluate scores a finished trajectory against the ground truth.
func evaluate(r *recordingToolset, truth GroundTruth, ans Answer) Metrics {
	m := Metrics{Calls: len(r.calls), StepsToEvidence: -1}
	failed := 0
	for _, c := range r.calls {
		m.ContextBytes += c.Bytes
		if c.Failed {
			failed++
		}
	}
	if m.Calls > 0 {
		m.CallErrorRate = float64(failed) / float64(m.Calls)
	}

	// Evidence is complete at the call index by which the last required span
	// became usable, so the metric answers "how many calls did the agent have to
	// make before it could justify an answer", not "how many did it make in
	// total". Evidence requires summary level rather than a bare ID: an
	// enumeration that returns only span IDs tells the agent a span exists but
	// nothing it could reason from, so counting it as evidence would credit the
	// granular arm for information it has not actually received.
	step := 0
	complete := true
	for _, id := range truth.EvidenceSpanIDs {
		at, ok := r.summarized[id]
		if !ok {
			complete = false
			break
		}
		step = max(step, at)
	}
	if complete {
		for _, id := range truth.RequiresDetailOn {
			at, ok := r.detailed[id]
			if !ok {
				complete = false
				break
			}
			step = max(step, at)
		}
	}
	if complete {
		m.StepsToEvidence = step
		m.Justified = true
	}
	m.Correct = ans.RootCauseSpanID == truth.RootCauseSpanID
	return m
}

// RunCell executes one (scenario, toolset, agent) combination.
func RunCell(sc Scenario, ts Toolset, ag Agent) Result {
	rec := newRecordingToolset(ts)
	ans := ag.Solve(sc, rec)
	return Result{
		Scenario: sc.Name,
		Toolset:  ts.Name(),
		Strategy: ag.Name(),
		Metrics:  evaluate(rec, sc.Truth, ans),
		Calls:    rec.calls,
	}
}

// RunMatrix executes the full factorial: every scenario against both tool
// variants and both skill narratives. Both axes vary together because the
// premise of #9135 is that tool shape and skill narrative interact, so measuring
// either alone would miss the interaction.
func RunMatrix(scenarios []Scenario) []Result {
	strategies := []Agent{ScriptedAgent{Strategy: Procedural}, ScriptedAgent{Strategy: GoalOriented}}
	var out []Result
	for i := range scenarios {
		sc := scenarios[i]
		variants := []Toolset{NewAnalyticalToolset(sc.Trace), NewGranularToolset(sc.Trace)}
		for _, ts := range variants {
			for _, ag := range strategies {
				out = append(out, RunCell(sc, ts, ag))
			}
		}
	}
	return out
}
