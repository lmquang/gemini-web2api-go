package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const toolReplyLimit = 256 * 1024

// ToolContractError reports bridge validation, never a provider quota verdict.
type ToolContractError struct{ Code string }

func (e *ToolContractError) Error() string {
	if e.Code == "tool_response_buffer_exceeded" {
		return "tool-enabled response exceeded validation buffer limit"
	}
	return "upstream repeated an unsupported tool-exhaustion claim after one corrective attempt"
}

var quotaClaim = regexp.MustCompile(`(?i)^(?:(?:unfortunately|sorry)[,:]?\s+)?(?:i (?:have |have already |already )?(?:exhausted (?:all )?my tool(?:[ -]call)?(?:s| budget| allowance)|(?:used up|run out of|reached) (?:all )?my tool(?:[ -]call)?(?:s| budget| allowance| limit))|(?:tôi|mình|em) (?:đã |hiện đã |hiện )?(?:hết (?:lượt|số lượt|quota|hạn mức) (?:gọi |sử dụng )?(?:công cụ|tool)|(?:sử dụng|dùng) hết (?:lượt|số lượt|quota|hạn mức) (?:gọi |sử dụng )?(?:công cụ|tool)|(?:không còn|còn 0) lượt (?:gọi |sử dụng )?(?:công cụ|tool)))`)

// Only direct first-person assertions count. Quote state spans lines, while
// code fences and blockquotes never supply assertions. Removing a quoted span
// must not discard an unsupported claim elsewhere on the same line.
func falseToolQuota(text string) bool {
	var quote rune
	var fence string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if fence != "" {
			if strings.HasPrefix(line, fence) {
				fence = ""
			}
			continue
		}
		if quote == 0 && (strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~")) {
			fence = line[:3]
			continue
		}
		if quote == 0 && strings.HasPrefix(line, ">") {
			continue
		}
		line = quotaUnquotedLine(line, &quote)
		for _, sentence := range regexpSentence.Split(line, -1) {
			sentence = strings.Trim(strings.TrimSpace(sentence), "*_")
			sentence = strings.ReplaceAll(sentence, "**", "")
			sentence = strings.ReplaceAll(sentence, "__", "")
			sentence = strings.ReplaceAll(strings.ToLower(sentence), "i've ", "i have ")
			sentence = strings.ReplaceAll(sentence, "i’ve ", "i have ")
			if quotaClaim.MatchString(sentence) || zeroToolClaim.MatchString(sentence) {
				return true
			}
		}
	}
	return false
}

// Single apostrophes within words are contractions, not opening quotes.
func quotaUnquotedLine(line string, quote *rune) string {
	var out strings.Builder
	var previous rune
	escaped := false
	for offset, r := range line {
		contraction := false
		if r == '\'' || r == '’' {
			next, _ := utf8.DecodeRuneInString(line[offset+utf8.RuneLen(r):])
			contraction = unicode.IsLetter(previous) && unicode.IsLetter(next)
		}
		if *quote != 0 {
			switch {
			case escaped:
				escaped = false
			case r == '\\':
				escaped = true
			case r == *quote && !contraction:
				*quote = 0
			}
		} else {
			switch r {
			case '"', '`':
				*quote = r
			case '“':
				*quote = '”'
			case '‘':
				*quote = '’'
			case '\'':
				if previous == 0 || unicode.IsSpace(previous) || strings.ContainsRune("(:", previous) {
					*quote = r
				}
			}
			if *quote == 0 {
				out.WriteRune(r)
			} else {
				out.WriteByte(' ')
			}
		}
		previous = r
	}
	return out.String()
}

// A lack of pending work ("remaining to execute") is not an allowance claim.
var zeroToolClaim = regexp.MustCompile(`(?i)^(?:(?:unfortunately|sorry)[,:]?\s+)?i (?:have (?:zero|0|no) (?:remaining tool calls(?: left)?|tool calls (?:left|remaining))|am allowed (?:zero|0|no) tool calls)(?:\s*[.!?,;:]|\s*$|\s+(?:for|in) (?:this|the current) (?:turn|session)\b)`)
var regexpSentence = regexp.MustCompile(`[.!?]+\s+`)

func activeClientTools(tools []map[string]interface{}, choice interface{}) bool {
	mode, forced := parseToolChoice(choice)
	if mode == "none" {
		return false
	}
	for _, tool := range tools {
		fn := tool
		if f, ok := tool["function"].(map[string]interface{}); ok {
			fn = f
		}
		name := getStr(fn, "name")
		if name != "" && (forced == "" || name == forced) {
			return true
		}
	}
	return false
}

var toolRestriction = regexp.MustCompile(`(?i)^(?:error[:\s]+)?(?:quota.{0,32}(?:exhaust|exceed)|rate.?limit|permission denied|access denied|hết (?:lượt|hạn mức)|không (?:được phép|có quyền))`)

func toolResultRestriction(messages []map[string]interface{}) bool {
	for i := len(messages) - 1; i >= 0 && getStr(messages[i], "role") == "tool"; i-- {
		text := strings.TrimSpace(contentToString(messages[i]["content"]))
		var obj map[string]interface{}
		if json.Unmarshal([]byte(text), &obj) == nil {
			if nested, ok := obj["error"].(map[string]interface{}); ok {
				text = getStr(nested, "message")
			} else if msg, ok := obj["error"].(string); ok {
				text = msg
			}
		}
		if toolRestriction.MatchString(strings.TrimSpace(text)) {
			return true
		}
	}
	return false
}

const toolCorrection = "\n\n[Bridge validation correction]\nThe previous generation claimed its client tool-call allowance was exhausted without a supporting tool result. No bridge tool-call budget was exhausted. Re-evaluate the original request using the supplied tool contract. Emit a valid tool_call only when needed and permitted, or give the final answer. Preserve all user permissions and real tool errors; do not invent a quota. The bridge does not execute client tools."

// validatedToolGeneration withholds both text and reasoning until the full reply
// passes validation. It reuses the existing raw response instead of queueing
// deltas, and shares one deadline across the initial and corrective generations.
func validatedToolGeneration(mc ModelConfig, enabled bool, prompt, recoveryPrompt string,
	onDelta, onReasoning func(string), generate func(string, ModelConfig, bool, func(string), func(string)) (*StreamResult, error)) (*StreamResult, error) {
	if !enabled {
		return generate(prompt, mc, false, onDelta, onReasoning)
	}
	ctx := mc.generationContext()
	var cancel context.CancelFunc
	if seconds := rtCfg().RequestTimeout; seconds > 0 {
		ctx, cancel = context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
	defer cancel()
	budget := &toolResponseBudget{cancel: cancel}
	ctx = context.WithValue(ctx, toolResponseBudgetKey{}, budget)
	mc.requestContext = ctx
	start := time.Now()
	for attempt := 0; attempt < 2; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		budget.corrective = attempt == 1
		if budget.corrective {
			logf("[tool-recovery] corrective_attempt=1 outcome=started")
		}
		exceeded := false
		size := 0
		hold := func(d string) {
			size += len(d)
			if size > toolReplyLimit {
				exceeded = true
				cancel()
			}
		}
		res, err := generate(prompt, mc, attempt == 1, hold, hold)
		if res != nil {
			res.Emitted = ""
			res.EmittedReasoning = ""
			res.TotalMs = time.Since(start).Milliseconds()
		}
		if exceeded || budget.exceeded {
			return res, &ToolContractError{Code: "tool_response_buffer_exceeded"}
		}
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		if err != nil {
			return res, err
		}
		if res == nil {
			return nil, errors.New("upstream returned no result")
		}
		if len(res.Raw) > toolWireLimit {
			return res, &ToolContractError{Code: "tool_response_buffer_exceeded"}
		}
		text := extractResponseText(res.Raw)
		if len(text)+len(res.Reasoning) > toolReplyLimit {
			return res, &ToolContractError{Code: "tool_response_buffer_exceeded"}
		}
		clean, calls := parseToolCalls(text)
		if strings.TrimSpace(clean) == "" && len(calls) == 0 {
			return res, errors.New("upstream returned no usable content or tool call")
		}
		if len(calls) == 0 && falseToolQuota(text) {
			if attempt == 1 {
				logf("[tool-recovery] corrective_attempts=1 outcome=repeated_claim upstream_model=%q", res.UpstreamModel)
				return res, &ToolContractError{Code: "unsupported_tool_exhaustion"}
			}
			logf("[tool-recovery] corrective_attempts=0 outcome=claim_detected upstream_model=%q", res.UpstreamModel)
			if res.AccountID > 0 {
				mc.PinAccountID = res.AccountID
			}
			prompt = recoveryPrompt + toolCorrection
			continue
		}
		if attempt == 1 {
			logf("[tool-recovery] corrective_attempts=1 outcome=accepted upstream_model=%q", res.UpstreamModel)
		}
		logf("[tool-validation] output_bytes=%d wire_bytes=%d withheld_ms=%d corrective_attempts=%d", len(text)+len(res.Reasoning), len(res.Raw), time.Since(start).Milliseconds(), attempt)
		if onReasoning != nil && res.Reasoning != "" {
			onReasoning(res.Reasoning)
			res.EmittedReasoning = res.Reasoning
		}
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if onDelta != nil && text != "" {
			onDelta(text)
			res.Emitted = text
		}
		if err := ctx.Err(); err != nil {
			return res, err
		}
		return res, nil
	}
	return nil, errors.New("tool validation attempt limit reached")
}

func upstreamErrorObject(err error) map[string]string {
	obj := map[string]string{"message": err.Error(), "type": "upstream_error"}
	var contract *ToolContractError
	if errors.As(err, &contract) {
		obj["code"] = contract.Code
	}
	return obj
}

// The wire cap also bounds repeated cumulative frames, before JSON extraction.
// It applies only while tool validation is active; other requests keep their
// existing media and response limits.
const toolWireLimit = 8 * 1024 * 1024

type toolResponseBudgetKey struct{}
type toolResponseBudget struct {
	cancel     context.CancelFunc
	exceeded   bool
	corrective bool
}
type toolBoundedBody struct {
	io.ReadCloser
	remaining int
	budget    *toolResponseBudget
}

func (b *toolBoundedBody) Read(p []byte) (int, error) {
	if len(p) > b.remaining+1 {
		p = p[:b.remaining+1]
	}
	n, err := b.ReadCloser.Read(p)
	b.remaining -= n
	if b.remaining < 0 {
		b.budget.exceeded = true
		b.budget.cancel()
		return n, &ToolContractError{Code: "tool_response_buffer_exceeded"}
	}
	return n, err
}
func toolValidationBody(ctx context.Context, body io.ReadCloser) io.ReadCloser {
	if budget, ok := ctx.Value(toolResponseBudgetKey{}).(*toolResponseBudget); ok {
		return &toolBoundedBody{ReadCloser: body, remaining: toolWireLimit, budget: budget}
	}
	return body
}

func generationRetryLimit(mc ModelConfig) int {
	if budget, ok := mc.generationContext().Value(toolResponseBudgetKey{}).(*toolResponseBudget); ok && budget.corrective {
		return 1
	}
	return rtCfg().RetryAttempts
}

func toolValidationReadError(ctx context.Context, err error) error {
	if budget, ok := ctx.Value(toolResponseBudgetKey{}).(*toolResponseBudget); ok && errors.Is(err, bufio.ErrTooLong) {
		budget.exceeded = true
		budget.cancel()
		return &ToolContractError{Code: "tool_response_buffer_exceeded"}
	}
	return err
}

func upstreamHTTPErrorObject(err error) map[string]string {
	var contract *ToolContractError
	if errors.As(err, &contract) {
		return upstreamErrorObject(err)
	}
	return map[string]string{"message": "upstream error: " + err.Error()}
}
