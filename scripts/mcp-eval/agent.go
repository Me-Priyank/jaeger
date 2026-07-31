// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"slices"
	"strings"
)

// detailBatchSize mirrors the guidance on types.GetSpanDetailsInput ("recommended
// to limit to 20 spans or fewer"), so an agent that needs detail on a wide trace
// pays for it in extra calls rather than one unbounded request.
const detailBatchSize = 20

// nPlusOneThreshold is the sibling count above which the detect-n-plus-one skill
// flags a group, taken from step 3 of its SKILL.md.
const nPlusOneThreshold = 10

// Answer is what an agent concludes.
type Answer struct {
	RootCauseSpanID string
	// Cited are the span IDs the agent claims as supporting evidence.
	Cited []string
}

// Agent solves a scenario using only the tools it is given.
//
// The implementation in this file is deliberately scripted rather than
// LLM-driven. An evaluation harness is a measuring instrument, and an instrument
// has to be calibrated against a known signal before it is trusted with a noisy
// one: with a deterministic agent every metric below is reproducible and unit
// testable, so a change in a number can only come from a change in the tool or
// skill variant. Substituting a real model means implementing this same
// interface over an MCP client and a chat completion loop; nothing else in the
// harness changes. See the README for what that swap does and does not prove.
type Agent interface {
	Name() string
	Solve(sc Scenario, ts Toolset) Answer
}

// Strategy is the skill-narrative axis of jaegertracing/jaeger#9135: a strict
// step-by-step procedure versus a loose goal-oriented instruction.
type Strategy int

const (
	// Procedural follows the shipped SKILL.md steps literally and in order,
	// including verification steps whose data an earlier call already returned.
	Procedural Strategy = iota
	// GoalOriented pursues the answer directly and skips any step whose
	// information it already holds.
	GoalOriented
)

func (s Strategy) String() string {
	if s == Procedural {
		return "procedural"
	}
	return "goal-oriented"
}

// ScriptedAgent executes a skill narrative deterministically. It reads only what
// the tools return: it never inspects the fixture directly, so the calls it needs
// are a genuine consequence of the tool variant's shape.
type ScriptedAgent struct{ Strategy Strategy }

// Name identifies the skill-narrative arm in reports.
func (a ScriptedAgent) Name() string { return a.Strategy.String() }

// view is the agent's accumulated knowledge, built purely from tool payloads.
type view struct {
	parent  map[string]string
	service map[string]string
	name    map[string]string
	status  map[string]string
	detail  map[string]bool
	order   []string
}

func newView() *view {
	return &view{
		parent: map[string]string{}, service: map[string]string{},
		name: map[string]string{}, status: map[string]string{}, detail: map[string]bool{},
	}
}

func (v *view) note(id string) {
	if _, seen := v.parent[id]; !seen {
		if !contains(v.order, id) {
			v.order = append(v.order, id)
		}
	}
}

func contains(xs []string, x string) bool {
	for _, s := range xs {
		if s == x {
			return true
		}
	}
	return false
}

// absorbTopology parses a get_trace_topology payload, deriving parentage from the
// slash-delimited Path field.
func (v *view) absorbTopology(payload []byte) {
	var out struct {
		Spans []struct {
			Path       string `json:"path"`
			Service    string `json:"service"`
			SpanName   string `json:"span_name"`
			DurationUs int64  `json:"duration_us"`
			Status     string `json:"status"`
		} `json:"spans"`
	}
	if json.Unmarshal(payload, &out) != nil {
		return
	}
	for _, s := range out.Spans {
		ids := strings.Split(s.Path, "/")
		id := ids[len(ids)-1]
		parent := ""
		if len(ids) > 1 {
			parent = ids[len(ids)-2]
		}
		v.note(id)
		v.parent[id], v.service[id], v.name[id], v.status[id] = parent, s.Service, s.SpanName, s.Status
	}
}

// absorbEdges parses a get_trace_spans payload: structure only, no content.
func (v *view) absorbEdges(payload []byte) {
	var out struct {
		Spans []struct {
			SpanID   string `json:"span_id"`
			ParentID string `json:"parent_span_id"`
		} `json:"spans"`
	}
	if json.Unmarshal(payload, &out) != nil {
		return
	}
	for _, s := range out.Spans {
		v.note(s.SpanID)
		v.parent[s.SpanID] = s.ParentID
	}
}

// absorbDetails parses any payload carrying full spanDetail records.
func (v *view) absorbDetails(payload []byte) {
	var out struct {
		Spans []spanDetail `json:"spans"`
	}
	if json.Unmarshal(payload, &out) != nil {
		return
	}
	for i := range out.Spans {
		s := &out.Spans[i]
		v.note(s.SpanID)
		v.parent[s.SpanID], v.service[s.SpanID] = s.ParentSpanID, s.Service
		v.name[s.SpanID], v.status[s.SpanID] = s.SpanName, s.Status.Code
		v.detail[s.SpanID] = true
	}
}

// depth counts ancestors using only parentage the agent has observed.
func (v *view) depth(id string) int {
	d, seen := 0, map[string]bool{}
	for cur := id; cur != "" && !seen[cur]; {
		seen[cur] = true
		p := v.parent[cur]
		if p == "" {
			break
		}
		d++
		cur = p
	}
	return d
}

// knownIDs returns observed span IDs in a stable order.
func (v *view) knownIDs() []string {
	out := append([]string(nil), v.order...)
	slices.Sort(out)
	return out
}

// Solve runs the narrative against whichever tool variant it is handed.
func (a ScriptedAgent) Solve(sc Scenario, ts Toolset) Answer {
	available := map[string]bool{}
	for _, t := range ts.Tools() {
		available[t] = true
	}
	v := newView()

	// Step 1: obtain a structural view. The analytical arm returns structure and
	// summary together; the granular arm returns edges only, so status and names
	// have to be bought separately below.
	if available["get_trace_topology"] {
		v.absorbTopology(ts.Call("get_trace_topology", nil).Payload)
	} else {
		v.absorbEdges(ts.Call("get_trace_spans", nil).Payload)
	}

	// Step 2: the error-root-cause narrative starts from the error list. It is
	// only worth calling for a scenario about failures.
	if sc.Skill == "error-root-cause" && available["get_trace_errors"] {
		v.absorbDetails(ts.Call("get_trace_errors", nil).Payload)
	}

	// Step 3: buy whatever the structural view did not include. Without a
	// summarizing overview the agent cannot know any span's status or name, so it
	// must fetch detail for every span it has heard of.
	if missingSummary(v) {
		for _, batch := range chunk(v.knownIDs(), detailBatchSize) {
			v.absorbDetails(ts.Call("get_span_details", map[string]any{"span_ids": batch}).Payload)
		}
	}

	switch sc.Skill {
	case "detect-n-plus-one":
		return a.solveNPlusOne(v, ts)
	default:
		return a.solveErrorRootCause(v, ts)
	}
}

// missingSummary reports whether the agent still lacks status for spans it knows
// about, which is the case exactly when the overview tool did not summarize.
func missingSummary(v *view) bool {
	for _, id := range v.order {
		if v.status[id] == "" {
			return true
		}
	}
	return false
}

// solveNPlusOne groups siblings by operation name and flags the largest group
// above the skill's threshold.
func (a ScriptedAgent) solveNPlusOne(v *view, ts Toolset) Answer {
	groups := map[string][]string{}
	for _, id := range v.knownIDs() {
		if p := v.parent[id]; p != "" {
			groups[p+"\x00"+v.name[id]] = append(groups[p+"\x00"+v.name[id]], id)
		}
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	best, bestKey := 0, ""
	for _, k := range keys {
		if len(groups[k]) > best {
			best, bestKey = len(groups[k]), k
		}
	}
	if best <= nPlusOneThreshold {
		return Answer{}
	}
	parent := strings.Split(bestKey, "\x00")[0]
	siblings := groups[bestKey]

	// The procedural narrative performs step 4 verbatim ("Inspect repeated
	// siblings with get_span_details to confirm they target the same downstream
	// service"), even though the overview already established the pattern. The
	// goal-oriented narrative treats the structural evidence as sufficient. This
	// is the single behavioural difference between the arms here, and the cost of
	// that extra verification is what the metrics price.
	if a.Strategy == Procedural {
		for _, batch := range chunk(siblings, detailBatchSize) {
			ts.Call("get_span_details", map[string]any{"span_ids": batch})
		}
	}
	cited := append([]string{parent}, siblings...)
	return Answer{RootCauseSpanID: parent, Cited: cited}
}

// solveErrorRootCause walks to the deepest error span whose own children carry no
// error, which is the skill's definition of the originating failure.
func (a ScriptedAgent) solveErrorRootCause(v *view, ts Toolset) Answer {
	kids := map[string][]string{}
	for _, id := range v.knownIDs() {
		if p := v.parent[id]; p != "" {
			kids[p] = append(kids[p], id)
		}
	}
	var candidate string
	bestDepth := -1
	for _, id := range v.knownIDs() {
		if v.status[id] != statusError {
			continue
		}
		erroredChild := false
		for _, c := range kids[id] {
			if v.status[c] == statusError {
				erroredChild = true
				break
			}
		}
		if erroredChild {
			continue
		}
		if d := v.depth(id); d > bestDepth {
			candidate, bestDepth = id, d
		}
	}
	if candidate == "" {
		return Answer{}
	}
	// The reason for the failure lives in attributes and events, so detail on the
	// candidate is required before the answer is justified. The goal-oriented
	// narrative skips the call when an earlier tool already returned that detail;
	// the procedural narrative performs step 4 regardless.
	if !v.detail[candidate] || a.Strategy == Procedural {
		ts.Call("get_span_details", map[string]any{"span_ids": []string{candidate}})
	}
	var chain []string
	for cur := candidate; cur != ""; cur = v.parent[cur] {
		chain = append([]string{cur}, chain...)
	}
	return Answer{RootCauseSpanID: candidate, Cited: chain}
}

// chunk splits ids into batches of at most size.
func chunk(ids []string, size int) [][]string {
	var out [][]string
	for i := 0; i < len(ids); i += size {
		end := min(i+size, len(ids))
		out = append(out, ids[i:end])
	}
	return out
}
