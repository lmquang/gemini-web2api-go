package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

const falseQuotaReply = "I have exhausted my tool calls for this turn. I cannot execute any more tools."
const recoveryToolReply = "```tool_call\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"main.go\"}}\n```"

func TestFalseToolQuotaConversation(t *testing.T) {
	for _, tc := range []struct {
		name, second string
		wantError    bool
	}{
		{"recover", recoveryToolReply, false},
		{"repeated", falseQuotaReply, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetConversationTestState(t)
			tools := []map[string]interface{}{{"type": "function", "function": map[string]interface{}{"name": "read_file", "parameters": map[string]interface{}{"type": "object"}}}}
			messages := []map[string]interface{}{{"role": "user", "content": "inspect main.go"}}
			calls := 0
			var sent strings.Builder
			old := runConversationTurn
			t.Cleanup(func() { runConversationTurn = old })
			runConversationTurn = func(prompt string, mc ModelConfig, st *convState, delta, reasoning func(string)) (*StreamResult, error) {
				calls++
				if calls > 2 {
					t.Fatal("unbounded recovery")
				}
				if calls == 2 && (st.turn != 0 || st.cid != "") {
					t.Fatal("recovery reused failed upstream head")
				}
				st.turn++
				st.cid = "advanced"
				text := falseQuotaReply
				if calls == 2 {
					text = tc.second
				}
				if delta != nil {
					delta(text)
				}
				return &StreamResult{Raw: conversationTestRaw(t, text), Emitted: text, UpstreamModel: "4 Neon"}, nil
			}
			text, toolCalls, _, err := callGeminiConv(messages, ModelConfig{}, tools, "auto", func(d string) { sent.WriteString(d) }, nil)
			if calls != 2 {
				t.Fatalf("attempts=%d, want 2; false refusal escaped: %q", calls, text)
			}
			if tc.wantError {
				if err == nil {
					t.Fatal("repeated refusal returned success")
				}
				if len(convStore) != 0 {
					t.Fatal("failed conversation head published")
				}
			} else if err != nil || len(toolCalls) != 1 || toolCalls[0].Function.Name != "read_file" {
				t.Fatalf("recovery missing real parsed tool call: text=%q calls=%v err=%v", text, toolCalls, err)
			}
			if strings.Contains(sent.String(), "exhausted") {
				t.Fatal("false refusal leaked into stream")
			}
		})
	}
}

func TestFalseToolQuotaPreservesReplies(t *testing.T) {
	for _, tc := range []struct {
		name, text, choice string
		noTools            bool
	}{
		{"final", "Finished reviewing main.go.", "auto", false},
		{"permission", "I cannot read files because permission was denied.", "auto", false},
		{"real quota", "The read_file tool returned quota exhausted. I cannot continue.", "auto", false},
		{"quote", "The model said: \"I have exhausted my tool calls for this turn.\" This is a bug.", "auto", false},
		{"multiline quoted prose", "Example response:\n\"\nI have exhausted my tool calls.\n\"", "auto", false},
		{"completed work", "I have no tool calls remaining to execute; the requested work is complete.", "auto", false},
		{"no tools", falseQuotaReply, "auto", true},
		{"disabled", falseQuotaReply, "none", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetConversationTestState(t)
			n := 0
			stubConversationTestTurn(t, func(_ string, _ ModelConfig, st *convState) (*StreamResult, error) {
				n++
				st.turn++
				return &StreamResult{Raw: conversationTestRaw(t, tc.text)}, nil
			})
			tools := conversationTestTools()
			if tc.noTools {
				tools = nil
			}
			text, calls, _, err := callGeminiConv([]map[string]interface{}{{"role": "user", "content": "review"}}, ModelConfig{}, tools, tc.choice, nil, nil)
			if err != nil || text != tc.text || len(calls) != 0 || n != 1 {
				t.Fatalf("reply changed: %q %v %v attempts=%d", text, calls, err, n)
			}
		})
	}
}

func TestFalseToolQuotaDetection(t *testing.T) {
	for _, text := range []string{
		falseQuotaReply,
		"I've exhausted my tool calls for this turn.",
		"I have 0 tool calls remaining.",
		"Tôi đã hết lượt gọi công cụ trong lượt này.",
		"Mình đã sử dụng hết số lượt gọi tool cho phiên này.",
	} {
		if !falseToolQuota(text) {
			t.Errorf("missed direct claim: %q", text)
		}
	}
	for _, text := range []string{
		"I have completed the review. No more tools are needed.",
		"I cannot call tools because you denied permission.",
		"Tool calls exhausted is an error description.",
		"```text\nI have exhausted my tool calls.\n```",
		"> I have exhausted my tool calls.",
		"The log contains 'I have exhausted my tool calls.'",
		"Example: 'The model stopped. I have exhausted my tool calls.'",
		"The model reports exhaustion. I have enough tool calls left.",
	} {
		if falseToolQuota(text) {
			t.Errorf("matched explanation or legitimate answer: %q", text)
		}
	}
}

func TestFalseToolQuotaHTTP(t *testing.T) {
	for _, multi := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			for _, endpoint := range []string{"chat", "responses"} {
				for _, outcome := range []string{"recover", "repeated", "final", "real_error", "none", "no_tools"} {
					t.Run(fmt.Sprintf("%s/multi=%t/stream=%t/%s", endpoint, multi, stream, outcome), func(t *testing.T) {
						resetConversationTestState(t)
						rtMu.Lock()
						rtVal.MultiTurn = multi
						rtMu.Unlock()
						attempts := 0
						upstream := func(_ string, _ ModelConfig, delta, reason func(string)) (*StreamResult, error) {
							attempts++
							text := falseQuotaReply
							if outcome == "recover" || outcome == "repeated" {
								text = `I have exhausted my tool calls. Please say "continue".`
							}
							if outcome == "final" {
								text = "Review complete."
							}
							if outcome == "real_error" {
								text = "The read_file tool returned quota exhausted. I cannot continue."
							}
							if outcome == "recover" && attempts == 2 {
								text = "Checking.\n" + recoveryToolReply
							}
							if delta != nil {
								delta(text)
							}
							return &StreamResult{Raw: conversationTestRaw(t, text), Emitted: text, UpstreamModel: "4 Neon"}, nil
						}
						oldConv, oldSingle := runConversationTurn, generateToolRequest
						t.Cleanup(func() { runConversationTurn = oldConv; generateToolRequest = oldSingle })
						runConversationTurn = func(p string, m ModelConfig, st *convState, d, r func(string)) (*StreamResult, error) {
							st.turn++
							return upstream(p, m, d, r)
						}
						generateToolRequest = func(p, _ string, m ModelConfig, _ []pendingUpload, d, r func(string)) (*StreamResult, error) {
							return upstream(p, m, d, r)
						}
						choice := "auto"
						if outcome == "none" {
							choice = "none"
						}
						req := map[string]interface{}{"model": "gemini-3.5-flash-lite", "stream": stream, "tool_choice": choice, "tools": conversationTestTools()}
						if outcome == "no_tools" {
							delete(req, "tools")
						}
						if endpoint == "chat" {
							req["messages"] = []map[string]interface{}{{"role": "user", "content": "inspect main.go"}}
						} else {
							req["input"] = "inspect main.go"
						}
						body, err := json.Marshal(req)
						if err != nil {
							t.Fatal(err)
						}
						w := httptest.NewRecorder()
						r := httptest.NewRequest("POST", "/v1/"+endpoint, bytes.NewReader(body))
						if endpoint == "chat" {
							handleChatCompletions(w, r)
						} else {
							handleResponses(w, r)
						}
						got := w.Body.String()
						wantAttempts := 1
						if outcome == "recover" || outcome == "repeated" {
							wantAttempts = 2
						}
						if attempts != wantAttempts {
							t.Fatalf("attempts=%d want=%d body=%s", attempts, wantAttempts, got)
						}
						if outcome == "repeated" {
							if !strings.Contains(got, "unsupported_tool_exhaustion") || strings.Contains(got, "rate_limit_exceeded") {
								t.Fatalf("missing structured bridge failure: %s", got)
							}
							if len(convStore) != 0 {
								t.Fatal("failed head published")
							}
						} else if w.Code != 200 {
							t.Fatalf("status=%d body=%s", w.Code, got)
						}
						if outcome != "repeated" {
							if outcome == "recover" && strings.Contains(got, "exhausted") {
								t.Fatal("rejected prose leaked")
							}
							chunks := []map[string]interface{}{}
							if stream {
								for _, line := range strings.Split(got, "\n") {
									if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
										continue
									}
									var chunk map[string]interface{}
									if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err != nil {
										t.Fatal(err)
									}
									chunks = append(chunks, chunk)
								}
							} else {
								var obj map[string]interface{}
								if err := json.Unmarshal(w.Body.Bytes(), &obj); err != nil {
									t.Fatal(err)
								}
								chunks = append(chunks, obj)
							}
							dispatched := 0
							text := ""
							for _, obj := range chunks {
								if endpoint == "chat" {
									choices, _ := obj["choices"].([]interface{})
									if len(choices) == 0 {
										continue
									}
									c := choices[0].(map[string]interface{})
									m, _ := c["message"].(map[string]interface{})
									if stream {
										m, _ = c["delta"].(map[string]interface{})
									}
									calls, _ := m["tool_calls"].([]interface{})
									dispatched += len(calls)
									text += getStr(m, "content")
								} else if stream {
									if getStr(obj, "type") == "response.output_item.added" {
										item, _ := obj["item"].(map[string]interface{})
										if getStr(item, "type") == "function_call" {
											dispatched++
										}
									}
									if getStr(obj, "type") == "response.output_text.delta" {
										text += getStr(obj, "delta")
									}
								} else {
									items, _ := obj["output"].([]interface{})
									for _, v := range items {
										item := v.(map[string]interface{})
										if getStr(item, "type") == "function_call" {
											dispatched++
										} else {
											parts, _ := item["content"].([]interface{})
											for _, p := range parts {
												text += getStr(p.(map[string]interface{}), "text")
											}
										}
									}
								}
							}
							wantCalls := 0
							wantText := falseQuotaReply
							switch outcome {
							case "recover":
								wantCalls = 1
								wantText = "Checking."
							case "final":
								wantText = "Review complete."
							case "real_error":
								wantText = "The read_file tool returned quota exhausted. I cannot continue."
							}
							if dispatched != wantCalls || strings.TrimSpace(text) != wantText {
								t.Fatalf("changed or duplicate output: calls=%d want=%d text=%q want=%q", dispatched, wantCalls, text, wantText)
							}
						}
					})
				}
			}
		}
	}
}

func TestFalseToolQuotaCancellationReleasesSlot(t *testing.T) {
	for _, stage := range []string{"initial", "retry", "flush"} {
		t.Run(stage, func(t *testing.T) {
			resetConversationTestState(t)
			rtMu.Lock()
			rtVal.PerIPConcurrent = 1
			rtVal.PerIPRPM = 0
			rtVal.PerIPRPH = 0
			rtMu.Unlock()
			slotsMu.Lock()
			oldSlots := slots
			slots = map[int64]*ipSlot{}
			slotsMu.Unlock()
			t.Cleanup(func() { slotsMu.Lock(); slots = oldSlots; slotsMu.Unlock() })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			attempts := 0
			old := generateConversationOnSlot
			t.Cleanup(func() { generateConversationOnSlot = old })
			generateConversationOnSlot = func(_ string, m ModelConfig, st *convState, d, r func(string), _ Proxy) (*StreamResult, error) {
				attempts++
				st.turn++
				if stage == "initial" || (stage == "retry" && attempts == 2) {
					cancel()
					return nil, m.generationContext().Err()
				}
				text := falseQuotaReply
				if attempts == 2 {
					text = "Accepted final."
				}
				d(text)
				return &StreamResult{Raw: conversationTestRaw(t, text), Emitted: text}, nil
			}
			_, _, _, err := callGeminiConv([]map[string]interface{}{{"role": "user", "content": "inspect"}}, ModelConfig{requestContext: ctx}, conversationTestTools(), "auto", func(string) {
				if stage == "flush" {
					cancel()
				}
			}, nil)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("want cancellation, got %v", err)
			}
			if len(convStore) != 0 {
				t.Fatal("cancelled head published")
			}
			p, ok, err := acquireSlot(0)
			if !ok {
				t.Fatalf("slot leaked: %v", err)
			}
			releaseSlot(p.ID)
		})
	}
}

func TestFalseToolQuotaBufferLimits(t *testing.T) {
	for _, kind := range []string{"delta", "final", "wire", "wire_line"} {
		t.Run(kind, func(t *testing.T) {
			resetConversationTestState(t)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if kind == "wire_line" {
					_, _ = io.WriteString(w, strings.Repeat("x", 9*1024*1024))
					return
				}
				_, _ = io.Copy(w, io.LimitReader(zeroReader{}, 9*1024*1024))
			}))
			defer upstream.Close()
			attempts := 0
			sent := 0
			res, err := validatedToolGeneration(ModelConfig{}, true, "request", "request", func(string) { sent++ }, nil,
				func(_ string, mc ModelConfig, _ bool, d, r func(string)) (*StreamResult, error) {
					attempts++
					if strings.HasPrefix(kind, "wire") {
						_, raw, _, _, err := doGeminiRequest(mc.generationContext(), upstream.URL, "", nil, "", nil)
						return &StreamResult{Raw: string(raw)}, err
					}
					text := strings.Repeat("x", toolReplyLimit+1)
					if kind == "delta" {
						d(text)
					}
					return &StreamResult{Raw: conversationTestRaw(t, text)}, nil
				})
			var contract *ToolContractError
			if !errors.As(err, &contract) || contract.Code != "tool_response_buffer_exceeded" || attempts != 1 || sent != 0 {
				t.Fatalf("limit failed: err=%v attempts=%d sent=%d result=%t", err, attempts, sent, res != nil)
			}
		})
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = '\n'
	}
	return len(p), nil
}

func TestFalseToolQuotaContinuationPublishesOnlyAcceptedHead(t *testing.T) {
	for _, repeated := range []bool{false, true} {
		t.Run(fmt.Sprint(repeated), func(t *testing.T) {
			resetConversationTestState(t)
			tools := conversationTestTools()
			mc := ModelConfig{}
			prior := []map[string]interface{}{{"role": "user", "content": "original task"}, {"role": "assistant", "content": "plan"}}
			scope := convScopeKey(mc, tools, "auto", "")
			keys := convPrefixKeys(prior, scope)
			convPut(keys[len(keys)-1], &convState{cid: "old-head", turn: 4, cookie: "test-only", accountID: 17, proxyID: 3})
			messages := append(prior, map[string]interface{}{"role": "user", "content": "update code as planned"})
			attempts := 0
			stubConversationTestTurn(t, func(prompt string, m ModelConfig, st *convState) (*StreamResult, error) {
				attempts++
				if attempts == 1 {
					if st.cid != "old-head" || st.turn != 4 || strings.Contains(prompt, "original task") {
						t.Fatal("continuation fixture did not hit old head")
					}
				} else {
					if st.cid != "" || st.turn != 0 || st.accountID != 17 || st.proxyID != 3 || st.cookie != "test-only" {
						t.Fatal("retry lost attribution or reused advanced state")
					}
					if !strings.Contains(prompt, "original task") || !strings.Contains(prompt, "update code as planned") || !strings.Contains(prompt, "read_file") {
						t.Fatal("retry lost full history or schemas")
					}
				}
				st.cid = fmt.Sprintf("advanced-%d", attempts)
				st.turn++
				text := falseQuotaReply
				if attempts == 2 && !repeated {
					text = recoveryToolReply
				}
				return &StreamResult{Raw: conversationTestRaw(t, text), AccountID: 17, ProxyID: 3, UpstreamModel: "4 Neon"}, nil
			})
			text, calls, _, err := callGeminiConv(messages, mc, tools, "auto", nil, nil)
			if attempts != 2 {
				t.Fatalf("attempts=%d", attempts)
			}
			if repeated {
				if err == nil || len(convStore) != 0 {
					t.Fatal("failed head escaped")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				next := append(conversationMessagesWithReply(messages, text, calls), map[string]interface{}{"role": "tool", "tool_call_id": calls[0].ID, "content": "actual file bytes"})
				st, _ := convTake(convPrefixKeys(next, scope))
				if st == nil || st.cid != "advanced-2" {
					t.Fatal("accepted head not reusable with actual tool results")
				}
			}
		})
	}
}

func TestFalseToolQuotaGroundedRestrictionUnchanged(t *testing.T) {
	resetConversationTestState(t)
	attempts := 0
	stubConversationTestTurn(t, func(_ string, _ ModelConfig, st *convState) (*StreamResult, error) {
		attempts++
		st.turn++
		return &StreamResult{Raw: conversationTestRaw(t, falseQuotaReply)}, nil
	})
	messages := []map[string]interface{}{{"role": "user", "content": "inspect"}, {"role": "tool", "name": "read_file", "content": "quota exhausted"}}
	text, _, _, err := callGeminiConv(messages, ModelConfig{}, conversationTestTools(), "auto", nil, nil)
	if err != nil || text != falseQuotaReply || attempts != 1 {
		t.Fatalf("real restriction changed: text=%q err=%v attempts=%d", text, err, attempts)
	}
}

func BenchmarkFalseToolQuotaValidation(b *testing.B) {
	for _, size := range []int{16, 128 * 1024} {
		b.Run(fmt.Sprintf("%d_bytes", size), func(b *testing.B) {
			text := "Review complete." + strings.Repeat("x", size-len("Review complete."))
			inner := make([]interface{}, 44)
			inner[4] = []interface{}{[]interface{}{"rc", []interface{}{text}, nil, nil, nil, nil, nil, nil, []interface{}{2}, "en"}}
			body, err := json.Marshal(inner)
			if err != nil {
				b.Fatal(err)
			}
			payload, err := json.Marshal([]interface{}{[]interface{}{"wrb.fr", nil, string(body)}})
			if err != nil {
				b.Fatal(err)
			}
			raw := string(payload)
			if got := extractResponseText(raw); got != text {
				b.Fatal("invalid benchmark fixture")
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, err := validatedToolGeneration(ModelConfig{}, true, "request", "request", func(string) {}, nil, func(_ string, _ ModelConfig, _ bool, d, r func(string)) (*StreamResult, error) {
					d(text)
					return &StreamResult{Raw: raw}, nil
				})
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestFalseToolQuotaKeepsNormalZeroCallAnswer(t *testing.T) {
	text := "I have no tool calls to report. The answer is 42."
	if falseToolQuota(text) {
		t.Fatal("ordinary final answer mistaken for exhaustion")
	}
}

func TestFalseToolQuotaRetryPreservesContract(t *testing.T) {
	resetConversationTestState(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	mc := Models["gemini-4-neon"]
	mc.requestContext = ctx
	mc.PinAccountID = 91
	attempts := 0
	sent := 0
	var deadline time.Time
	res, err := validatedToolGeneration(mc, true, "original full prompt", "original full prompt", func(string) { sent++ }, func(string) { sent++ },
		func(prompt string, got ModelConfig, retry bool, d, r func(string)) (*StreamResult, error) {
			attempts++
			if retry && generationRetryLimit(got) != 1 {
				t.Fatal("corrective generation permits hidden transport retries")
			}
			next, ok := got.generationContext().Deadline()
			if !ok {
				t.Fatal("deadline lost")
			}
			if attempts == 1 {
				deadline = next
			} else if !deadline.Equal(next) {
				t.Fatal("retry extended deadline")
			}
			if got.HexID != mc.HexID || got.Mode != mc.Mode || got.Thinking != mc.Thinking || got.PinAccountID != 91 {
				t.Fatal("model or account changed")
			}
			if attempts == 2 && (!retry || !strings.HasPrefix(prompt, "original full prompt")) {
				t.Fatal("retry contract lost")
			}
			text := falseQuotaReply
			if retry {
				text = "Done."
			}
			d(text)
			r("private reasoning")
			if sent != 0 {
				t.Fatal("text or reasoning escaped before validation")
			}
			return &StreamResult{Raw: conversationTestRaw(t, text), Emitted: text, Reasoning: "private reasoning", EmittedReasoning: "private reasoning", AccountID: 91, ProxyID: 7, UpstreamModel: "4 Neon"}, nil
		})
	if err != nil || res.AccountID != 91 || res.ProxyID != 7 || res.UpstreamModel != "4 Neon" || sent != 2 || attempts != 2 {
		t.Fatalf("contract failed: result=%+v err=%v sent=%d attempts=%d", res, err, sent, attempts)
	}
}

func TestFalseToolQuotaToolSourceDoesNotCountAsRestriction(t *testing.T) {
	messages := []map[string]interface{}{{"role": "tool", "content": "<file>\n// quota exhausted means an error in this example\n</file>"}}
	if toolResultRestriction(messages) {
		t.Fatal("quoted source disabled recovery")
	}
	for _, result := range []string{"quota exhausted", "Error: permission denied", `{"error":{"message":"quota exceeded"}}`} {
		messages[0]["content"] = result
		if !toolResultRestriction(messages) {
			t.Errorf("missed actual restriction: %s", result)
		}
	}
}

func TestFalseToolQuotaLogsCountActualCorrection(t *testing.T) {
	for _, outcome := range []string{"accepted", "repeated", "upstream_error", "cancelled"} {
		t.Run(outcome, func(t *testing.T) {
			resetConversationTestState(t)
			rtMu.Lock()
			rtVal.LogRequests = true
			rtMu.Unlock()
			file, err := os.CreateTemp(t.TempDir(), "recovery-log-")
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := file.Close(); err != nil {
					t.Error(err)
				}
			}()
			old := os.Stderr
			os.Stderr = file
			t.Cleanup(func() { os.Stderr = old })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			attempts := 0
			_, _ = validatedToolGeneration(ModelConfig{requestContext: ctx}, true, "private-prompt", "private-prompt", nil, nil,
				func(_ string, _ ModelConfig, retry bool, d, r func(string)) (*StreamResult, error) {
					attempts++
					text := falseQuotaReply
					if retry {
						switch outcome {
						case "accepted":
							text = "Done."
						case "upstream_error":
							return nil, errors.New("transport unavailable")
						case "cancelled":
							cancel()
							return nil, ctx.Err()
						}
					}
					return &StreamResult{Raw: conversationTestRaw(t, text), UpstreamModel: "4 Neon"}, nil
				})
			os.Stderr = old
			content, err := os.ReadFile(file.Name())
			if err != nil {
				t.Fatal(err)
			}
			logs := string(content)
			if attempts != 2 || strings.Count(logs, "corrective_attempt=1 outcome=started") != 1 {
				t.Fatalf("wrong attempt log: attempts=%d log=%s", attempts, logs)
			}
			if strings.Contains(logs, "private-prompt") || strings.Contains(logs, falseQuotaReply) {
				t.Fatal("private content logged")
			}
			if outcome != "accepted" && strings.Contains(logs, "outcome=accepted") {
				t.Fatal("failed correction logged as success")
			}
		})
	}
}

func TestFalseToolQuotaQuoteScope(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		want       bool
	}{
		{"quoted instruction after claim", `I have exhausted my tool calls. Please say "continue".`, true},
		{"Vietnamese quoted instruction", `Tôi đã hết lượt gọi công cụ. Hãy nói "tiếp tục".`, true},
		{"bold claim", "**I have exhausted my tool calls for this turn.**", true},
		{"curly contraction", "I’ve exhausted my tool calls for this turn.", true},
		{"zero calls with introduction", "Unfortunately, I have 0 tool calls remaining.", true},
		{"zero remaining calls left", "Sorry, I have no remaining tool calls left.", true},
		{"zero calls with consequence", "I have 0 tool calls left, so I cannot execute another tool.", true},
		{"emphasized claim phrase", "Mình đã **hết lượt gọi công cụ** trong lượt này.", true},
		{"multiline double quote", "Example response:\n\"\nI have exhausted my tool calls.\n\"", false},
		{"multiline curly quote", "Example response:\n“\nTôi đã hết lượt gọi công cụ.\n”", false},
		{"single quote with contraction", "Example: 'I can't proceed.\nI have exhausted my tool calls.\n'", false},
		{"curly quote with contraction", "Example: ‘I can’t proceed.\nI have exhausted my tool calls.\n’", false},
		{"tilde code fence", "~~~text\nI have exhausted my tool calls.\n~~~", false},
		{"no pending work", "I have no tool calls remaining to execute; the requested work is complete.", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := falseToolQuota(tc.text); got != tc.want {
				t.Fatalf("detected=%t want=%t", got, tc.want)
			}
		})
	}
}

func TestFalseToolQuotaQuotedInstructionRecovery(t *testing.T) {
	resetConversationTestState(t)
	attempts := 0
	stubConversationTestTurn(t, func(_ string, _ ModelConfig, st *convState) (*StreamResult, error) {
		attempts++
		st.turn++
		text := `I have exhausted my tool calls. Please say "continue".`
		if attempts == 2 {
			text = recoveryToolReply
		}
		return &StreamResult{Raw: conversationTestRaw(t, text)}, nil
	})
	text, calls, _, err := callGeminiConv([]map[string]interface{}{{"role": "user", "content": "inspect main.go"}}, ModelConfig{}, conversationTestTools(), "auto", nil, nil)
	if err != nil || attempts != 2 || len(calls) != 1 {
		t.Fatalf("refusal bypassed recovery: attempts=%d calls=%d text=%q err=%v", attempts, len(calls), text, err)
	}
}

func FuzzFalseToolQuotaQuotedText(f *testing.F) {
	for _, seed := range []string{falseQuotaReply, `I can't proceed. "I have exhausted my tool calls."`, "Tôi đã hết lượt gọi công cụ.\n~~~"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		quoted := strconv.Quote(text)
		if falseToolQuota("Quoted example: " + quoted) {
			t.Fatal("quoted text became an exhaustion assertion")
		}
		if !falseToolQuota(quoted + "\n" + falseQuotaReply) {
			t.Fatal("quoted text hid a separate direct assertion")
		}
	})
}

func TestFalseToolQuotaCorrectiveDowngradeKeepsCooldown(t *testing.T) {
	resetConversationTestState(t)
	resetDowngrades(t)
	rtMu.Lock()
	rtVal.StrictModel = true
	rtVal.StrictModelCooldownMin = 300
	rtMu.Unlock()
	mc := Models["gemini-4-neon"]
	const accountID int64 = 7171
	downgrade := &ModelDowngradedError{Requested: mc.Upstream, Reported: "3.5 Flash-Lite", AccountID: accountID}
	attempts := 0
	var sent strings.Builder
	old := runConversationTurn
	t.Cleanup(func() { runConversationTurn = old })
	runConversationTurn = func(_ string, _ ModelConfig, st *convState, d, r func(string)) (*StreamResult, error) {
		attempts++
		st.accountID = accountID
		st.turn++
		if attempts == 1 {
			d(falseQuotaReply)
			return &StreamResult{Raw: conversationTestRaw(t, falseQuotaReply), AccountID: accountID}, nil
		}
		if attempts > 2 {
			t.Fatal("corrective downgrade caused hidden retries")
		}
		return &StreamResult{AccountID: accountID, UpstreamModel: downgrade.Reported}, downgrade
	}
	_, _, res, err := callGeminiConv([]map[string]interface{}{{"role": "user", "content": "inspect main.go"}}, mc, conversationTestTools(), "auto", func(d string) { sent.WriteString(d) }, nil)
	if err != downgrade || res == nil || res.AccountID != accountID || res.UpstreamModel != downgrade.Reported || attempts != 2 {
		t.Fatalf("downgrade attribution or retry bound lost: err=%v attempts=%d", err, attempts)
	}
	if sent.Len() != 0 || len(convStore) != 0 {
		t.Fatal("failed corrective generation published output or a conversation head")
	}
	cooling, delay := coolingAccounts(mc)
	if !cooling[accountID] || delay <= 0 {
		t.Fatal("corrective downgrade bypassed configured account/model cooldown")
	}
	other, _ := coolingAccounts(Models["gemini-3.5-flash-lite"])
	if other[accountID] {
		t.Fatal("downgrade cooled unrelated model")
	}
}
