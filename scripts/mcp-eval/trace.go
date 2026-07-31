// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"strings"
)

// Span is a single unit of work in a fixture trace. The fields mirror the ones
// Jaeger's MCP tools expose (see mcptools/internal/types), so a tool result
// rendered from a fixture has the same shape an agent sees in production.
type Span struct {
	SpanID       string
	ParentSpanID string
	Service      string
	Name         string
	StartUs      int64
	DurationUs   int64
	// Status is "Unset", "Ok" or "Error", matching the MCP status vocabulary.
	Status        string
	StatusMessage string
	Attributes    map[string]any
	Events        []Event
}

// Event is a timestamped occurrence within a span (e.g. an exception).
type Event struct {
	Name       string
	TimeUs     int64
	Attributes map[string]any
}

// Trace is a fixture trace: a flat span list plus the trace ID the tools key on.
type Trace struct {
	TraceID string
	Spans   []Span
}

// span returns the span with the given ID.
func (t Trace) span(id string) (Span, bool) {
	for i := range t.Spans {
		if t.Spans[i].SpanID == id {
			return t.Spans[i], true
		}
	}
	return Span{}, false
}

// root returns the span with no parent. A fixture always has exactly one.
func (t Trace) root() (Span, bool) {
	for i := range t.Spans {
		if t.Spans[i].ParentSpanID == "" {
			return t.Spans[i], true
		}
	}
	return Span{}, false
}

// path renders the slash-delimited ancestry of a span, the encoding
// get_trace_topology uses to convey tree structure in a flat list
// (types.TopologySpan.Path). It stops at the root, and refuses to revisit a span,
// so a malformed fixture with a parent cycle cannot loop forever.
func (t Trace) path(id string) string {
	var ids []string
	seen := map[string]bool{}
	for cur := id; cur != "" && !seen[cur]; {
		seen[cur] = true
		ids = append([]string{cur}, ids...)
		s, ok := t.span(cur)
		if !ok {
			break
		}
		cur = s.ParentSpanID
	}
	return strings.Join(ids, "/")
}

// depth returns how many ancestors a span has; the root is depth 0. It is used
// to find the deepest error span, which the error-root-cause skill treats as the
// most likely originating failure.
func (t Trace) depth(id string) int {
	d := 0
	seen := map[string]bool{}
	for cur := id; cur != "" && !seen[cur]; {
		seen[cur] = true
		s, ok := t.span(cur)
		if !ok || s.ParentSpanID == "" {
			break
		}
		d++
		cur = s.ParentSpanID
	}
	return d
}

// errorSpans returns every span whose status is Error, in declaration order.
func (t Trace) errorSpans() []Span {
	var out []Span
	for i := range t.Spans {
		if t.Spans[i].Status == statusError {
			out = append(out, t.Spans[i])
		}
	}
	return out
}

const statusError = "Error"

// spanIDs is a helper for building evidence sets in scenario definitions.
func spanIDs(spans []Span) []string {
	out := make([]string, 0, len(spans))
	for i := range spans {
		out = append(out, spans[i].SpanID)
	}
	return out
}

// dbSpan builds a database child span, the repeated unit an N+1 pattern produces.
func dbSpan(id, parent string, startUs, durUs int64, statement string) Span {
	return Span{
		SpanID:       id,
		ParentSpanID: parent,
		Service:      "postgres",
		Name:         "SELECT orders",
		StartUs:      startUs,
		DurationUs:   durUs,
		Status:       "Unset",
		Attributes: map[string]any{
			"db.system":     "postgresql",
			"db.statement":  statement,
			"net.peer.name": "orders-db",
		},
	}
}

// String renders a span compactly for debug output and test failure messages.
func (s Span) String() string {
	return fmt.Sprintf("%s(%s/%s status=%s dur=%dus)", s.SpanID, s.Service, s.Name, s.Status, s.DurationUs)
}
