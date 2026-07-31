// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import "fmt"

// Scenario is one deterministic, trace-solvable fault: a fixture trace plus the
// ground truth an agent is expected to arrive at.
//
// "Trace-solvable" is the load-bearing property and the hardest part of building
// this suite. A scenario qualifies only if the root cause is deducible from span
// data alone — structure, timing, status and attributes — with no application
// logs, no source code and no out-of-band knowledge. Each scenario below records
// why it qualifies in Solvability, so the claim is reviewable rather than asserted.
type Scenario struct {
	Name        string
	Description string
	Trace       Trace
	Truth       GroundTruth
	// Solvability states the span-only reasoning chain that reaches the answer.
	// It is documentation, not executable, but it is what makes the suite
	// auditable: a scenario whose Solvability cannot be written honestly does not
	// belong in the benchmark.
	Solvability string
	// Skill names the in-tree skill this scenario exercises
	// (mcptools/skills/<name>/SKILL.md).
	Skill string
}

// GroundTruth is the verified answer to a scenario.
type GroundTruth struct {
	// RootCauseSpanID is the span an ideal agent should name.
	RootCauseSpanID string
	// EvidenceSpanIDs are the spans an agent must actually observe before its
	// answer counts as justified rather than guessed. Steps-to-evidence measures
	// how many tool calls it takes to surface all of them, which is why a correct
	// answer alone is not a sufficient metric: an agent can be right by luck.
	EvidenceSpanIDs []string
	// RequiresDetailOn lists spans whose answer-bearing information lives in
	// attributes or events, so a structural view alone cannot supply it. This is
	// what distinguishes "the agent saw the span" from "the agent saw the reason".
	RequiresDetailOn []string
	Rationale        string
}

// Scenarios returns the built-in benchmark suite. Both scenarios are derived from
// the skills already shipped in mcptools/skills, so the suite measures the
// assistant Jaeger actually has rather than a hypothetical one.
func Scenarios() []Scenario {
	return []Scenario{nPlusOneScenario(), errorCascadeScenario()}
}

// nPlusOneScenario builds a trace where one handler issues 24 near-identical
// child queries, the pattern mcptools/skills/detect-n-plus-one/SKILL.md targets.
func nPlusOneScenario() Scenario {
	const (
		traceID  = "1a2b3c4d5e6f70819a2b3c4d5e6f7081"
		rootID   = "aa00000000000001"
		handleID = "aa00000000000002"
		authID   = "aa00000000000003"
	)
	spans := []Span{
		{
			SpanID: rootID, Service: "frontend", Name: "GET /orders",
			StartUs: 0, DurationUs: 512_000, Status: "Ok",
			Attributes: map[string]any{"http.method": "GET", "http.route": "/orders", "http.status_code": 200},
		},
		{
			SpanID: authID, ParentSpanID: rootID, Service: "auth", Name: "verify-token",
			StartUs: 1_000, DurationUs: 4_000, Status: "Ok",
			Attributes: map[string]any{"auth.method": "jwt"},
		},
		{
			SpanID: handleID, ParentSpanID: rootID, Service: "orders-service", Name: "ListOrders",
			StartUs: 6_000, DurationUs: 500_000, Status: "Ok",
			Attributes: map[string]any{"orders.page_size": 24},
		},
	}
	// 24 sequential, near-identical children: same operation name, similar
	// duration, non-overlapping in time. Sequential execution is what separates a
	// real N+1 from intentional parallel fan-out (the skill's first gotcha).
	var repeated []Span
	start := int64(8_000)
	for i := 0; i < 24; i++ {
		id := fmt.Sprintf("bb%014d", i+1)
		dur := int64(19_000 + (i%3)*400) // within 2x of the median, per the skill
		repeated = append(repeated, dbSpan(id, handleID, start, dur,
			fmt.Sprintf("SELECT * FROM orders WHERE customer_id = $1 -- row %d", i+1)))
		start += dur + 500
	}
	spans = append(spans, repeated...)

	return Scenario{
		Name:  "n-plus-one",
		Skill: "detect-n-plus-one",
		Description: "A ListOrders handler issues one database query per row instead of a " +
			"single batched query, inflating wall-clock time.",
		Trace: Trace{TraceID: traceID, Spans: spans},
		Truth: GroundTruth{
			RootCauseSpanID: handleID,
			// The parent plus a representative sample of the repeated children: an
			// agent must see the parent and enough siblings to establish a pattern,
			// not merely one slow query.
			EvidenceSpanIDs: append([]string{handleID}, spanIDs(repeated[:3])...),
			// The repetition is visible from structure and timing alone, so no
			// attribute-level detail is required to reach the answer.
			RequiresDetailOn: nil,
			Rationale: "24 sibling spans share the operation name SELECT orders under one parent, " +
				"run sequentially, and have durations within 2x of the median, which is the " +
				"N+1 signature rather than parallel fan-out.",
		},
		Solvability: "Span-only: the sibling count, the shared operation name, the " +
			"non-overlapping start/duration windows and the parent-child edges are all " +
			"carried in the trace. No application log or query plan is needed.",
	}
}

// errorCascadeScenario builds a failed trace whose deepest error span is the real
// cause, with a timed-out parent that masks it. This is the pattern
// mcptools/skills/error-root-cause/SKILL.md targets, including its gotcha that a
// timeout can hide the true failure in a cancelled child.
func errorCascadeScenario() Scenario {
	const (
		traceID    = "9f8e7d6c5b4a39281f8e7d6c5b4a3928"
		rootID     = "cc00000000000001"
		gatewayID  = "cc00000000000002"
		checkoutID = "cc00000000000003"
		paymentID  = "cc00000000000004"
		fraudID    = "cc00000000000005"
		cacheID    = "cc00000000000006"
	)
	spans := []Span{
		{
			SpanID: rootID, Service: "frontend", Name: "POST /checkout",
			StartUs: 0, DurationUs: 3_000_000, Status: statusError,
			StatusMessage: "upstream request failed",
			Attributes:    map[string]any{"http.method": "POST", "http.status_code": 500},
		},
		{
			SpanID: gatewayID, ParentSpanID: rootID, Service: "api-gateway", Name: "forward /checkout",
			StartUs: 1_000, DurationUs: 2_990_000, Status: statusError,
			StatusMessage: "502 Bad Gateway from checkout",
		},
		{
			SpanID: checkoutID, ParentSpanID: gatewayID, Service: "checkout-service", Name: "SubmitOrder",
			StartUs: 2_000, DurationUs: 2_980_000, Status: statusError,
			StatusMessage: "deadline exceeded calling payment",
			Attributes:    map[string]any{"rpc.service": "payment", "timeout_ms": 3000},
		},
		{
			// A healthy sibling: present so "any error span" is not trivially the answer.
			SpanID: cacheID, ParentSpanID: checkoutID, Service: "redis", Name: "GET cart",
			StartUs: 2_500, DurationUs: 900, Status: "Ok",
		},
		{
			// The deepest error span, and the true cause. Its status message names a
			// symptom; the actual reason lives in the exception event, so a structural
			// view alone is not enough to report it.
			SpanID: paymentID, ParentSpanID: checkoutID, Service: "payment-service", Name: "AuthorizeCard",
			StartUs: 3_000, DurationUs: 2_975_000, Status: statusError,
			StatusMessage: "connection refused",
			Attributes: map[string]any{
				"rpc.service":   "fraud-check",
				"net.peer.name": "fraud-check.internal",
				"net.peer.port": 8443,
				"retry.count":   3,
			},
			Events: []Event{{
				Name:   "exception",
				TimeUs: 2_970_000,
				Attributes: map[string]any{
					"exception.type":    "ConnectionRefused",
					"exception.message": "dial tcp 10.0.4.19:8443: connect: connection refused",
				},
			}},
		},
		{
			// Cancelled when its parent gave up: no error status despite being on the
			// failure path. The skill's gotcha about timed-out parents masking causes.
			SpanID: fraudID, ParentSpanID: paymentID, Service: "fraud-check", Name: "Score",
			StartUs: 4_000, DurationUs: 12_000, Status: "Unset",
			Attributes: map[string]any{"rpc.cancelled": true},
		},
	}

	return Scenario{
		Name:  "error-cascade",
		Skill: "error-root-cause",
		Description: "A checkout request fails at the frontend, but the originating error is " +
			"three levels down where payment-service cannot reach fraud-check.",
		Trace: Trace{TraceID: traceID, Spans: spans},
		Truth: GroundTruth{
			RootCauseSpanID: paymentID,
			EvidenceSpanIDs: []string{rootID, gatewayID, checkoutID, paymentID},
			// The status message says "connection refused" but the actionable reason
			// (which host and port, and that it was retried) is only in the attributes
			// and the exception event, so the agent must fetch detail on this span.
			RequiresDetailOn: []string{paymentID},
			Rationale: "payment-service is the deepest span with Error status whose own children " +
				"carry no error, so it originates the failure the ancestors report as timeouts " +
				"and 502s.",
		},
		Solvability: "Span-only: status codes, the parent-child chain and the exception event " +
			"attributes identify both the originating service and the reason. The cancelled " +
			"fraud-check child is present to ensure a naive deepest-span heuristic does not win " +
			"by accident.",
	}
}
