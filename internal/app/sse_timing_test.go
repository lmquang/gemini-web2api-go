package app

import (
	"net/http/httptest"
	"testing"
)

func TestToolFenceAndDispatchTiming(t *testing.T) {
	s := newSSEWriter(httptest.NewRecorder(), "test", 0, "neon")
	gate := newToolFenceGate(s.SendContent)
	s.SendReasoning("reasoning")
	gate.Push(toolFenceOpen + `{"name":"read_file"}`)
	if !gate.firstClosedFenceAt.IsZero() || !s.firstToolAt.IsZero() {
		t.Fatal("incomplete fence recorded a tool event")
	}
	gate.Push(toolFenceClose)
	if gate.firstClosedFenceAt.IsZero() || !s.firstToolAt.IsZero() {
		t.Fatal("fence completion must be recorded separately from SSE dispatch")
	}
	s.SendToolCalls([]ToolCall{{ID: "call_test", Type: "function"}})
	if s.firstReasoningAt.IsZero() || s.firstToolAt.IsZero() || s.firstToolAt.Before(gate.firstClosedFenceAt) || !s.firstContentAt.IsZero() {
		t.Fatal("unexpected reasoning, tool, or answer timing")
	}
}

func TestSSEFirstContentTiming(t *testing.T) {
	s := newSSEWriter(httptest.NewRecorder(), "test", 0, "neon")
	s.SendReasoning("thinking")
	s.SendContent("")
	if !s.firstContentAt.IsZero() {
		t.Fatal("reasoning or empty delta counted as answer text")
	}
	s.SendContent("first")
	first := s.firstContentAt
	if first.IsZero() {
		t.Fatal("answer text did not set timestamp")
	}
	s.SendContent("second")
	if s.firstContentAt != first {
		t.Fatal("later text replaced first timestamp")
	}
}
