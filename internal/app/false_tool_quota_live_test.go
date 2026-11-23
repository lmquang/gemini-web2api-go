package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestLiveFalseToolQuotaRecovery is opt-in and uses TestMain's isolated database.
// Its first reply is replaced after a real generation advances the upstream
// head. The claim is injected, not observed from the provider; all generations
// use the authenticated account and strict model validation.
func TestLiveFalseToolQuotaRecovery(t *testing.T) {
	cookieFile := os.Getenv("GEMINI_LIVE_TEST_COOKIE_FILE")
	if cookieFile == "" {
		t.Skip("set GEMINI_LIVE_TEST_COOKIE_FILE explicitly to enable authenticated live verification")
	}
	resetConversationTestState(t)
	oldConfig := cfg
	t.Cleanup(func() { cfg = oldConfig })
	cfg = defaultConfig()
	cfg.DBPath = oldConfig.DBPath
	initRuntimeConfig()
	rtMu.Lock()
	rtVal.StrictModel = true
	rtVal.FallbackAnon = false
	rtVal.AnonFirst = false
	rtVal.MultiTurn = true
	rtVal.WebChannel = false
	rtVal.LogRequests = false
	rtVal.RequestTimeout = 300
	rtVal.RetryAttempts = 1
	rtVal.PerIPConcurrent = 1
	rtVal.PerIPRPM, rtVal.PerIPRPH = 0, 0
	rtMu.Unlock()
	report := map[string]interface{}{"initial_reply_source": "injected_test_fixture", "provider_claim_reproduced": false, "passed": false, "phase": "import"}
	recordError := func(err error) {
		report["error_type"] = fmt.Sprintf("%T", err)
		var contract *ToolContractError
		if errors.As(err, &contract) {
			report["error_code"] = contract.Code
		}
		var downgrade *ModelDowngradedError
		if errors.As(err, &downgrade) {
			report["requested_upstream_model"] = downgrade.Requested
			report["reported_upstream_model"] = downgrade.Reported
			report["model_downgrade_rejected"] = true
		}
	}
	t.Cleanup(func() {
		if path := os.Getenv("GEMINI_LIVE_TEST_REPORT"); path != "" {
			raw, err := json.MarshalIndent(report, "", "  ")
			if err != nil {
				t.Error("cannot encode live verification metadata")
				return
			}
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Error("cannot write live verification metadata")
			}
		}
	})
	raw, err := os.ReadFile(cookieFile)
	if err != nil {
		t.Fatal("cannot read the explicitly supplied cookie file")
	}
	cookie := strings.TrimSpace(string(raw))
	if strings.HasPrefix(cookie, "curl ") {
		t.Fatal("normalize Copy as cURL to a private cookie header or pool JSON before running this test")
	}
	if len(cookie) >= 7 && strings.EqualFold(cookie[:7], "cookie:") {
		cookie = strings.TrimSpace(cookie[7:])
	}
	id, err := accountAdd("isolated-live-recovery", cookie, "")
	if err != nil {
		t.Fatal("explicit cookie file could not be imported; credential values withheld")
	}
	t.Cleanup(func() {
		if err := accountDelete(id); err != nil {
			t.Error("cannot remove temporary live account")
		}
	})
	account := accountByID(id)
	if account == nil {
		t.Fatal("temporary live account disappeared")
	}
	report["phase"] = "login"
	login := checkAccountCookie(*account)
	if !login.OK {
		category := "unclassified"
		for _, candidate := range []struct{ match, name string }{{"没有登录态", "missing_login_marker"}, {"重定向", "login_redirect"}, {"OSID", "missing_osid"}, {"certificate", "tls_certificate"}, {"permission denied", "filesystem_permission"}, {"限流", "rate_limited"}, {"timeout", "network_timeout"}, {"connect", "connection_failure"}, {"拿不到出口", "slot_unavailable"}} {
			if strings.Contains(login.Detail, candidate.match) {
				category = candidate.name
				break
			}
		}
		report["login_failure_category"] = category

		t.Fatal("live account did not pass login validation; anonymous fallback remains disabled")
	}
	report["login_validated"] = true
	t.Log("login validated; testing an injected refusal against a real corrective generation")

	workspace := t.TempDir()
	code := uuid.NewString()
	path := filepath.Join(workspace, "evidence.txt")
	if err := os.WriteFile(path, []byte("Verification code: "+code+"\n"), 0o600); err != nil {
		t.Fatal("cannot create actual tool fixture")
	}
	tools := []map[string]interface{}{{"type": "function", "function": map[string]interface{}{
		"name": "read_file", "description": "Read evidence.txt in the client tool workspace. This is the only allowed path.",
		"parameters": map[string]interface{}{"type": "object", "properties": map[string]interface{}{"path": map[string]interface{}{"type": "string", "enum": []string{"evidence.txt"}}}, "required": []string{"path"}, "additionalProperties": false},
	}}}
	messages := []map[string]interface{}{{"role": "user", "content": "Call read_file with path evidence.txt to obtain its verification code. After receiving the actual result, return that exact code. Do not guess file content or give a plan instead of reading."}}
	mc := Models["gemini-4-neon"]
	mc.PinAccountID = id
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	mc.requestContext = ctx
	old := runConversationTurn
	t.Cleanup(func() { runConversationTurn = old })
	attempts := 0
	var sent strings.Builder
	runConversationTurn = func(prompt string, model ModelConfig, st *convState, delta, reasoning func(string)) (*StreamResult, error) {
		attempts++
		if st.accountID != id || !st.isLogin {
			t.Fatal("live recovery lost the authenticated account")
		}
		if attempts == 1 {
			result, err := old(prompt, model, st, delta, reasoning)
			if err != nil {
				return result, err
			}
			if result == nil || result.UpstreamModel != "4 Neon" || st.cid == "" || st.turn != 1 {
				t.Fatal("initial generation did not establish a real authenticated 4 Neon head")
			}
			report["initial_upstream_model"] = result.UpstreamModel
			report["initial_upstream_advanced"] = true
			report["phase"] = "injected_reply"
			if sent.Len() != 0 {
				t.Fatal("initial upstream output escaped before validation")
			}
			result.Raw = conversationTestRaw(t, `I have exhausted my tool calls. Please say "continue".`)
			t.Log("real upstream head advanced; injecting one unsupported refusal before client publication")
			return result, nil
		}
		if attempts == 2 {
			report["phase"] = "corrective_generation"
			if st.cid != "" || st.turn != 0 {
				t.Fatal("corrective attempt reused the rejected upstream head")
			}
		}
		return old(prompt, model, st, delta, reasoning)
	}
	report["phase"] = "initial_generation"
	text, calls, result, err := callGeminiConv(messages, mc, tools, "auto", func(d string) { sent.WriteString(d) }, nil)
	report["first_request_attempts"] = attempts
	if err != nil {
		recordError(err)
		t.Fatal("live recovery request failed; details withheld, no success verdict")
	}
	if attempts != 2 || len(calls) != 1 || calls[0].Function.Name != "read_file" {
		t.Fatal("expected one corrective generation producing one real read_file call")
	}
	if result == nil || result.UpstreamModel != "4 Neon" || result.AccountID != id {
		t.Fatal("corrective reply lacks authenticated upstream 4 Neon attribution")
	}
	if strings.Contains(sent.String(), "exhausted my tool calls") {
		t.Fatal("injected refusal reached the streaming callback")
	}
	report["corrective_upstream_model"] = result.UpstreamModel
	report["recovery_request_total_ms"] = result.TotalMs
	var args map[string]interface{}
	if json.Unmarshal([]byte(calls[0].Function.Arguments), &args) != nil || args["path"] != "evidence.txt" || len(args) != 1 {
		t.Fatal("generated tool arguments exceeded fixture permissions")
	}
	actual, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("actual client file read failed")
	}
	report["actual_client_tool_executed"] = true
	t.Log("corrective reply confirmed 4 Neon and one read_file call; actual file read complete")
	messages = conversationMessagesWithReply(messages, text, calls)
	messages = append(messages, map[string]interface{}{"role": "tool", "name": "read_file", "tool_call_id": calls[0].ID, "content": string(actual)})
	report["phase"] = "tool_result_continuation"
	final, more, result, err := callGeminiConv(messages, mc, tools, "auto", nil, nil)
	report["actual_upstream_generations"] = attempts
	if err != nil {
		recordError(err)
		t.Fatal("live tool-result continuation failed; fixture has not passed")
	}
	if len(more) != 0 || !strings.Contains(final, code) {
		t.Fatal("final answer did not use the actual tool result")
	}
	if result == nil || result.UpstreamModel != "4 Neon" || result.AccountID != id {
		t.Fatal("final answer lacks authenticated upstream 4 Neon attribution")
	}
	report["final_upstream_model"] = result.UpstreamModel
	report["final_total_ms"] = result.TotalMs
	report["phase"] = "complete"
	report["passed"] = true
	t.Log("injected refusal recovered through real 4 Neon; actual read_file result grounded the final answer")
}
