// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
)

// The response shapes below mirror mcptools/internal/types. They are restated
// here rather than imported because that package is under cmd/.../internal/ and
// is not importable from this tree; a production harness would instead drive the
// real MCP server over stdio and read its responses. Keeping the shapes faithful
// matters because payload size is what the context-bloat metric measures.

// topologySpan mirrors types.TopologySpan: deliberately compact, carrying no
// attributes or events so a structural overview stays cheap.
type topologySpan struct {
	Path       string `json:"path"`
	Service    string `json:"service"`
	SpanName   string `json:"span_name"`
	StartTime  string `json:"start_time"`
	DurationUs int64  `json:"duration_us"`
	Status     string `json:"status"`
}

// spanDetail mirrors types.SpanDetail: full OTLP data including attributes and
// events. This is the verbose end of the payload axis.
type spanDetail struct {
	SpanID       string         `json:"span_id"`
	TraceID      string         `json:"trace_id"`
	ParentSpanID string         `json:"parent_span_id,omitempty"`
	Service      string         `json:"service"`
	SpanName     string         `json:"span_name"`
	StartTime    string         `json:"start_time"`
	DurationUs   int64          `json:"duration_us"`
	Status       spanStatus     `json:"status"`
	Attributes   map[string]any `json:"attributes,omitempty"`
	Events       []spanEvent    `json:"events,omitempty"`
}

type spanStatus struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

type spanEvent struct {
	Name       string         `json:"name"`
	Timestamp  string         `json:"timestamp"`
	Attributes map[string]any `json:"attributes,omitempty"`
}

// ToolResult is one observation returned to the agent.
type ToolResult struct {
	// Payload is the JSON the agent would receive. Its size is the context-bloat
	// measurement.
	Payload []byte
	// SpansSeen are the span IDs this result surfaced at all, even bare.
	SpansSeen []string
	// SpansSummarized are the span IDs returned with enough content to reason
	// about — service, operation name, timing and status. Knowing that a span
	// exists is not evidence of anything, which is why this is tracked apart from
	// SpansSeen: an enumeration tool that returns bare IDs advances the former and
	// not the latter.
	SpansSummarized []string
	// SpansDetailed are the span IDs whose attributes and events were included.
	// Seeing what a span is remains different from seeing why it failed.
	SpansDetailed []string
	// Err is set when the call was rejected, e.g. a tool the variant does not
	// expose or a malformed argument. It drives the call-error-rate metric.
	Err error
}

// Toolset is one variant of the MCP tool layer. The two implementations below
// are the A/B arms of the "granular data-fetching vs high-level analytical"
// axis in jaegertracing/jaeger#9135.
type Toolset interface {
	// Name identifies the variant in reports.
	Name() string
	// Tools lists the tool names this variant exposes.
	Tools() []string
	// Call invokes a tool. Calling a tool the variant does not expose returns a
	// ToolResult with Err set rather than panicking, because a refused call is a
	// measurable outcome, not a harness failure.
	Call(name string, args map[string]any) ToolResult
}

// tools implements every tool once; the variants below choose which to expose.
// Sharing the implementations keeps the arms differing only in tool surface,
// which is the variable under test.
type tools struct{ trace Trace }

func fmtTime(us int64) string {
	return time.Unix(0, us*int64(time.Microsecond)).UTC().Format(time.RFC3339Nano)
}

func (t tools) toDetail(s Span) spanDetail {
	d := spanDetail{
		SpanID: s.SpanID, TraceID: t.trace.TraceID, ParentSpanID: s.ParentSpanID,
		Service: s.Service, SpanName: s.Name, StartTime: fmtTime(s.StartUs),
		DurationUs: s.DurationUs,
		Status:     spanStatus{Code: s.Status, Message: s.StatusMessage},
		Attributes: s.Attributes,
	}
	for _, e := range s.Events {
		d.Events = append(d.Events, spanEvent{
			Name: e.Name, Timestamp: fmtTime(e.TimeUs), Attributes: e.Attributes,
		})
	}
	return d
}

// marshal renders a payload. A marshal failure is returned as a tool error so a
// bad fixture surfaces as a measurable call error rather than a silent zero-byte
// result that would flatter the context-bloat metric.
func marshal(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("failed to encode tool response: %w", err)
	}
	return b, nil
}

// getServices lists distinct service names. Discovery-oriented and very cheap,
// but it surfaces no span IDs, so it cannot advance evidence on its own.
func (t tools) getServices() ToolResult {
	set := map[string]bool{}
	for i := range t.trace.Spans {
		set[t.trace.Spans[i].Service] = true
	}
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	slices.Sort(names)
	payload, err := marshal(map[string]any{"services": names})
	return ToolResult{Payload: payload, Err: err}
}

// getTraceSpans returns only span IDs and their parents: the structure of the
// trace with none of its content.
//
// This tool does not exist in Jaeger today. It is the proposed granular variant
// of get_trace_topology, isolating the question the A/B asks: if the server
// stops summarizing (no service, name, timing or status in the overview), does
// the agent pay for it in extra calls and a larger total context? Producing such
// a variant is itself a deliverable of jaegertracing/jaeger#9135.
func (t tools) getTraceSpans() ToolResult {
	type edge struct {
		SpanID   string `json:"span_id"`
		ParentID string `json:"parent_span_id,omitempty"`
	}
	out := struct {
		TraceID string `json:"trace_id"`
		Spans   []edge `json:"spans"`
	}{TraceID: t.trace.TraceID}
	var seen []string
	for i := range t.trace.Spans {
		s := &t.trace.Spans[i]
		out.Spans = append(out.Spans, edge{SpanID: s.SpanID, ParentID: s.ParentSpanID})
		seen = append(seen, s.SpanID)
	}
	payload, err := marshal(out)
	return ToolResult{Payload: payload, SpansSeen: seen, Err: err}
}

// getTraceTopology returns the whole tree in compact form: every span ID is
// surfaced, but no attributes or events. This is the tool INSTRUCTIONS.md steers
// the agent toward first.
func (t tools) getTraceTopology() ToolResult {
	out := struct {
		TraceID string         `json:"trace_id"`
		Spans   []topologySpan `json:"spans"`
	}{TraceID: t.trace.TraceID}
	var seen []string
	for i := range t.trace.Spans {
		s := &t.trace.Spans[i]
		out.Spans = append(out.Spans, topologySpan{
			Path: t.trace.path(s.SpanID), Service: s.Service, SpanName: s.Name,
			StartTime: fmtTime(s.StartUs), DurationUs: s.DurationUs, Status: s.Status,
		})
		seen = append(seen, s.SpanID)
	}
	payload, err := marshal(out)
	return ToolResult{Payload: payload, SpansSeen: seen, SpansSummarized: seen, Err: err}
}

// getTraceErrors returns only the error spans, but as full spanDetail. It is the
// clearest example of the two payload axes disagreeing: analytical in what it
// selects, granular in what it returns.
func (t tools) getTraceErrors() ToolResult {
	errs := t.trace.errorSpans()
	out := struct {
		TraceID         string       `json:"trace_id"`
		TotalErrorCount int          `json:"total_error_count"`
		Spans           []spanDetail `json:"spans,omitempty"`
	}{TraceID: t.trace.TraceID, TotalErrorCount: len(errs)}
	var seen []string
	for i := range errs {
		out.Spans = append(out.Spans, t.toDetail(errs[i]))
		seen = append(seen, errs[i].SpanID)
	}
	payload, err := marshal(out)
	return ToolResult{Payload: payload, SpansSeen: seen, SpansSummarized: seen, SpansDetailed: seen, Err: err}
}

// getSpanDetails returns full OTLP detail for the requested spans. Unknown IDs
// are reported in the error field, mirroring types.GetSpanDetailsOutput.
func (t tools) getSpanDetails(ids []string) ToolResult {
	if len(ids) == 0 {
		return ToolResult{Err: errors.New("get_span_details requires at least one span_id")}
	}
	out := struct {
		TraceID string       `json:"trace_id"`
		Spans   []spanDetail `json:"spans,omitempty"`
		Error   string       `json:"error,omitempty"`
	}{TraceID: t.trace.TraceID}
	var seen, missing []string
	for _, id := range ids {
		s, ok := t.trace.span(id)
		if !ok {
			missing = append(missing, id)
			continue
		}
		out.Spans = append(out.Spans, t.toDetail(s))
		seen = append(seen, id)
	}
	if len(missing) > 0 {
		out.Error = fmt.Sprintf("spans not found: %v", missing)
	}
	payload, err := marshal(out)
	return ToolResult{Payload: payload, SpansSeen: seen, SpansSummarized: seen, SpansDetailed: seen, Err: err}
}

// dispatch routes a call to a tool if allowed is true for that name.
func (t tools) dispatch(allowed map[string]bool, name string, args map[string]any) ToolResult {
	if !allowed[name] {
		return ToolResult{Err: fmt.Errorf("tool %q is not available in this variant", name)}
	}
	switch name {
	case "get_services":
		return t.getServices()
	case "get_trace_spans":
		return t.getTraceSpans()
	case "get_trace_topology":
		return t.getTraceTopology()
	case "get_trace_errors":
		return t.getTraceErrors()
	case "get_span_details":
		ids, ok := args["span_ids"].([]string)
		if !ok {
			return ToolResult{Err: errors.New("get_span_details requires a []string 'span_ids' argument")}
		}
		return t.getSpanDetails(ids)
	default:
		return ToolResult{Err: fmt.Errorf("unknown tool %q", name)}
	}
}

// GranularToolset exposes only low-level data-fetching tools: the server returns
// structure and raw spans but never summarizes, so any conclusion about status or
// timing requires fetching full span detail.
type GranularToolset struct{ tools }

// NewGranularToolset builds the granular arm over a fixture trace.
func NewGranularToolset(t Trace) GranularToolset { return GranularToolset{tools{trace: t}} }

func (GranularToolset) Name() string { return "granular" }

func (GranularToolset) Tools() []string {
	return []string{"get_services", "get_trace_spans", "get_span_details"}
}

func (g GranularToolset) Call(name string, args map[string]any) ToolResult {
	allowed := map[string]bool{"get_services": true, "get_trace_spans": true, "get_span_details": true}
	return g.dispatch(allowed, name, args)
}

// AnalyticalToolset exposes high-level tools that summarize server-side, plus
// get_span_details for drill-down. This is the surface both shipped skills
// declare in their allowed-tools front matter.
type AnalyticalToolset struct{ tools }

// NewAnalyticalToolset builds the analytical arm over a fixture trace.
func NewAnalyticalToolset(t Trace) AnalyticalToolset { return AnalyticalToolset{tools{trace: t}} }

func (AnalyticalToolset) Name() string { return "analytical" }

func (AnalyticalToolset) Tools() []string {
	return []string{"get_trace_topology", "get_trace_errors", "get_span_details"}
}

func (a AnalyticalToolset) Call(name string, args map[string]any) ToolResult {
	allowed := map[string]bool{"get_trace_topology": true, "get_trace_errors": true, "get_span_details": true}
	return a.dispatch(allowed, name, args)
}
