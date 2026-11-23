package app

import (
	"strings"
	"testing"
)

func TestPromptReanchorsCurrentToolAvailability(t *testing.T) {
	messages := []map[string]interface{}{
		{"role": "assistant", "content": "I have 0 tool calls remaining."},
		{"role": "user", "content": "update the code as your plan"},
	}
	prompt := buildPrompt(messages, conversationTestTools(), "auto")
	for _, want := range []string{"Available tools", "tools listed in this request are available now", "Do not invent a tool-call", "earlier assistant quota claim", "EXTERNAL client tools", "NOT the user's whole task", "inspect actual source and configuration"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("missing current tool contract %q", want)
		}
	}
	if strings.LastIndex(prompt, "tools listed in this request are available now") < strings.Index(prompt, "0 tool calls remaining") {
		t.Fatal("current availability must follow stale assistant claims")
	}
}

func TestPromptDoesNotEnableExplicitlyDisabledTools(t *testing.T) {
	messages := []map[string]interface{}{{"role": "user", "content": "Summarize only. Tools are disabled."}}
	for _, tools := range [][]map[string]interface{}{nil, conversationTestTools()} {
		prompt := buildPrompt(messages, tools, "none")
		if strings.Contains(prompt, "tools listed in this request are available now") || strings.Contains(prompt, "Available tools") {
			t.Fatal("tool_choice=none must not advertise available tools")
		}
	}
}
