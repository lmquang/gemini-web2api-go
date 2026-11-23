# False tool quota recovery

Status: OPEN. This branch adds deterministic detection and bounded recovery to
commit `0f57cd6181d50f340cb43b13ef10f5ccd813744d`. An isolated authenticated Neon
OpenCode TUI coding fixture passed. Controlled live fault injection also recovered
after a real upstream head advance, using only `4 Neon`. The original private
history replay remains unverified. These narrower results do not close the bug.

## Request behavior

`validatedToolGeneration` runs before chat/Responses output and before
`callGeminiConv` publishes a conversation head. The native Gemini adapter uses
the same paths. Validation applies only to named client tools allowed by the
current `tool_choice`, with no current tool result reporting a recognized quota
or permission error. Media generation keeps its existing path.

The detector looks for direct first-person English or Vietnamese assertions of
exhausted tool calls when no tool call parsed. It excludes quoted spans, including
multi-line quotes and contractions within quotes, fenced code using backticks or
tildes, and blockquotes. An unquoted claim still counts when the same line also
contains a quoted instruction such as `Please say "continue"`. Markdown emphasis
and straight or curly `I've` contractions do not hide a claim. A lack of pending
work, such as `no tool calls remaining to execute`, is not a budget assertion.

Normal final answers, permission denials, and direct or JSON tool errors remain
unchanged. Source text merely mentioning quota exhaustion does not count as a
tool error.

Detection is deliberately narrow and heuristic. Other wording, unrecognized
quote formatting, or unusual tool-error formats can escape or confuse it. This
is not proof that provider quota exists or that a model must use a tool on every
turn.

A recognized unsupported claim triggers one corrective generation. The retry
receives the full original history, schemas, and permission instructions plus a
bridge correction. It retains the requested model and pins the authenticated
account reported by the initial generation. The conversation path discards the
rejected advanced head and starts a new upstream conversation on that account,
keeping its preferred egress. It never restores a consumed stale head. Only an
accepted logical assistant reply publishes a new head. The corrective generation
disables transport retries and account rotation; initial generation keeps its
existing retry policy. Real model downgrade errors during correction also
retain the configured account/model cooldown, without rotating the bound account.
A reported model mismatch does not by itself establish the reason for the downgrade.

The bridge never executes client tools. An accepted parsed call goes to the
client once. A normal final answer remains valid without a tool call. A repeated
claim returns `unsupported_tool_exhaustion`, an `upstream_error` with HTTP 502 or
the existing SSE failure event after streaming headers have started. It does not
return `insufficient_quota` or attribute model prose to provider enforcement.

## Streaming and resource limits

Tool-enabled validation withholds text and reasoning until a complete response
passes inspection. Existing SSE keepalives and Responses lifecycle events can
still arrive during this wait. Rejected prose and reasoning never reach the
client. Once accepted output flushes, the bridge never starts another corrective
generation.

The semantic output limit is 256 KiB for text plus reasoning. Deltas count toward
that limit without creating a second delta queue. StreamGenerate raw response
reads and assembled WebChannel response frames have an 8 MiB limit per response.
Exceeding either limit cancels validation and returns
`tool_response_buffer_exceeded`; the bridge does not silently stream unchecked
content. These limits bound retained response data, not total process RSS:
scanning, JSON decoding, upstream tracking, and response formatting also allocate
memory.

Both attempts share the earlier of the caller deadline and the configured
`request_timeout_sec`. Cancellation during initial generation, corrective
generation, or accepted-output flush prevents head publication. Slot ownership
stays in the existing generation functions, which release slots before returning.
WebChannel now inherits the caller context; its handshake key lookup keeps its
existing behavior. Live WebChannel cancellation has not been verified.

Buffering delays first text/reasoning until the accepted generation finishes.
A corrective attempt adds its generation time within the same deadline. Logs
report withheld milliseconds, semantic/wire byte counts, corrective attempt
count, and reported upstream model without prompt or cookie contents. The
synthetic validation benchmark measured about 8 microseconds and 5.4 KB of Go
allocations for a 16-byte answer, and 4.6 ms and 1.76 MB for a 128 KiB answer on
the local i5-12400F. These are allocations, not peak RSS. Measurements exclude
network latency, response transport, and handler formatting; they are not live
Neon latency measurements.

## Deterministic verification

The initial regression failed before runtime changes: one generation returned
`I have exhausted my tool calls for this turn` as successful prose. The wire
limit and ordinary zero-call final-answer checks also failed before their fixes.

Run:

```sh
go test ./... -count=1
go test ./internal/app -run '^TestFalseToolQuota' -count=1
go test ./internal/app -run '^$' -bench '^BenchmarkFalseToolQuotaValidation$' -benchmem
CGO_ENABLED=1 go test -race ./internal/app \
  -run 'TestFalseToolQuota|TestChatCancellation|TestGenerationRetryWaitCancels|TestGenerationTransportCancels|Test.*Conversation|Test.*Timing|TestWC' \
  -count=1
go test ./internal/app -run '^$' -fuzz '^FuzzFalseToolQuotaQuotedText$' -fuzztime=5s
go vet ./...
git diff --check
```

The HTTP matrix covers chat and Responses, streaming and nonstreaming, single
and multi-turn, recovery to a parsed tool call, repeated refusal, final answers,
actual tool-error prose, absent tools, and disabled tools. It asserts one tool
and one text dispatch, absence of rejected prose, and structured bridge failure.
Further tests cover consumed continuation heads, full-history recovery,
model/account/deadline preservation, cancellation with slot reuse, grounded tool
restrictions, quoted source, and buffer limits. These tests use a fake upstream;
they do not establish real client tool execution or reproduce the full private
OpenCode history.

Additional regressions first failed on claims followed by a quoted instruction,
Markdown claims, multi-line quoted explanations, tilde code fences, and completed
work described as having no pending calls. These now pass, including the full
chat/Responses streaming matrix using the quoted-instruction refusal. A bounded
fuzz run exercised 111,016 quote/escape inputs without failure. This is bounded
fuzz evidence, not exhaustive language coverage.

A separate regression reproduced a corrective model downgrade bypassing cooldown.
It now asserts the original downgrade error and attribution, no output/head
publication, exactly one corrective attempt, and cooldown for only the requested
model.

The host lacks GCC. Targeted race checks passed in a Docker `golang:1.26`
container with CGO enabled and source mounted read-only. Patch-scoped
`golangci-lint` reported zero issues. Full tests, vet, and diff checks passed.

## Opt in live recovery harness

`TestLiveFalseToolQuotaRecovery` skips unless
`GEMINI_LIVE_TEST_COOKIE_FILE` explicitly names a current working cookie header
or the cookie pool JSON format. Normalize Copy as cURL exports privately first;
the test rejects them rather than treating the entire command as a cookie.
Never put credentials in command arguments, test fixtures, or source control.

```sh
GEMINI_LIVE_TEST_COOKIE_FILE=/private/current-cookie.json \
GEMINI_LIVE_TEST_REPORT=/private/recovery-verdict.json \
go test ./internal/app -run '^TestLiveFalseToolQuotaRecovery$' \
  -count=1 -timeout=12m -v
```

The live check uses TestMain's temporary database, one authenticated account,
strict Neon, disabled anonymous fallback, a 300 s shared request deadline, and
one transport attempt. It runs a real initial generation, verifies the upstream
head advanced, then replaces its reply with the synthetic quoted-instruction
refusal before any client publication. The corrective generation must use a new
upstream head on the same account and return a valid `read_file` call. The client
harness reads an actual file containing a code unknown to the model, supplies the
result, and requires that code in the final authenticated Neon answer.

This is controlled fault injection, not a provider-originated refusal or replay
of the affected history. The metadata report explicitly sets
`provider_claim_reproduced=false`. Normal offline tests do not read credentials
or make these live requests. Live failures remain failures, including stale
credentials and real model downgrades; the test does not enable fallback to pass.

An earlier variant injected the initial reply before contacting the provider. It
passed in 198.814 s: its recovery request completed in 164.624 s, and the
tool-result continuation completed in 33.641 s. Both reported `4 Neon`. One actual client
file read grounded the final answer. The stronger variant above additionally
checks a genuinely advanced upstream head. Its first run advanced a real Neon
head but strict validation rejected a model downgrade during correction. That
exposed the missing corrective cooldown regression described above; it was not
accepted as a successful recovery.

A later run of the stronger variant passed in 360.870 s. All three actual
generations reported `4 Neon`. The initial generation really advanced the head;
its injected refusal never reached the client. The same-account corrective call
started a fresh upstream head and returned one valid tool call. The client read
its actual file and the final answer contained the unseen code from that result.
The first logical request used exactly two generations and completed in
265.440 s, within the unchanged 300 s deadline. The tool-result continuation took
94.794 s. This later pass does not erase the earlier real downgrade failure or
establish its cause.

A normalized credential snapshot failed login validation while the isolated
bridge retained refreshed cookies. Temporary live-test copies were normalized privately and, when needed,
refreshed from that same local account after checking cookie identity. Raw cookie
values never entered test output or the report. Temporary normalized cookie
copies were deleted after verification; the user-supplied file was unchanged.

## Isolated Docker verification

A local container named `false-tool-quota-recovery` serves
`http://127.0.0.1:18083`. It uses image
`gemini-web2api:false-tool-quota-recovery` and its own named volume
`false-tool-quota-recovery-data`. Settings verified through its admin API:

```json
{
  "multi_turn": true,
  "strict_model": true,
  "fallback_anon": false,
  "anon_first": false,
  "web_channel": false,
  "default_model": "gemini-4-neon",
  "request_timeout_sec": 300
}
```

Local generated admin/API tokens reside in the private temporary build directory
under `/tmp/false-tool-quota-recovery-docker-*`, in `local.env` with mode 0600.
They are excluded from the Docker build context and repository. Do not dump
container environment variables or copy this file into source control.

The first authorized Firefox cookie failed two login checks (441 ms and
856 ms). Its local account was disabled. After the user updated the same file,
the replacement imported into local account 2 and passed login validation in
547 ms. Cookie values were never printed. Anonymous fallback remains off.

Two streaming tool turns reported upstream `4 Neon`, account 2, and status 200
in request logs, taking 100.443 s and 98.191 s. A private harness executed actual
local file reads and supplied their results. These successful turns establish
model and tool interoperability; they do not replay the affected private history.
The final-answer request then timed out at the 300 s deadline with HTTP 502 and
no completed answer, so the full two-file probe did not pass. No live corrective
recovery was demonstrated by these successful tool turns.

An initial TUI launch selected an unavailable built-in free model before loading
the configured selection; it returned no successful answer. The TUI then selected
the isolated strict Neon provider. An overlapping probe caused local slot errors;
those failures do not count as model verification. Subsequent external probes
run sequentially. OpenCode also sends a background title request concurrently
with its coding request. Local concurrency was temporarily raised from 1 to 2
for the TUI check, preserving strict model validation and disabled anonymous
fallback. Two separate Escape keys, 200 ms apart, interrupted an active TUI
request: slot usage fell from 1 to 0 while the terminal stayed open, and the bridge
logged `canceled=true`. A combined pair of Escape bytes had not stopped it. This
is a live Stop check. Completed tool execution was checked separately below.

No original private replay payload was found at the provided Linux locations.
The three supplied historical session IDs were absent from both legacy and V2
session tables in the Linux OpenCode database. Private probe artifacts remain
under the isolated temporary directory and are not committed.

## Authenticated OpenCode TUI result

OpenCode v2.0.22 session `ses_df77c6ea1ffefxUgGVnb8Z6MYt` completed the isolated
calculator fixture and returned to idle with outcome `succeeded`. The persisted
tool transcript establishes this sequence:

1. Two successful `read` results contained the actual source and test contents.
2. `shell` executed `python3 test_calculator.py` and returned `AssertionError`.
3. `edit` changed only the calculator implementation from subtraction to addition.
4. `shell` executed the same test and returned `NEON_FIXTURE_PASS`.
5. The model returned its final answer and the TUI became idle.

An independent test rerun passed. SHA-256 comparison confirmed the test file was
unchanged. This verdict uses actual tool states, tool results, file changes, and
test execution; neither HTTP 200 nor a marker in model prose alone establishes it.

The six successful bridge requests for this phase were 22, 23, 25, 26, 27, and 28.
Each reported upstream `4 Neon`, local account 2, and status 200. Their durations
were 54.988 s, 73.117 s, 121.489 s, 115.347 s, 157.223 s, and 134.139 s. Request
24 failed in 135 ms with status 502; OpenCode then retried with full history in a
fresh upstream conversation. That client retry was not false-quota recovery.
No unsupported tool-exhaustion claim occurred during the successful TUI phase.

The first TUI read used a relative filename and hit the fixture's path rule.
The shell alternative `cat calculator.py` was also denied. The model accurately
reported those restrictions, and source stayed unchanged. The final successful
follow-up used explicitly permitted absolute paths and the allowed test command.
The protected test and configuration were not editable by the model.

Local concurrency returned from the temporary value 2 to 1 before this successful
phase. Final admin checks confirmed concurrency 1, zero inflight requests,
`strict_model=true`, `fallback_anon=false`, and `web_channel=false`. The isolated
TUI remains open and idle for inspection. Private transcripts and a structured
verification verdict remain in the temporary directory.

## Natural walkthrough and implementation scenario

After the user authorized OpenCode testing instead of supplying the missing
history, a new isolated OpenCode v2.0.22 session
`ses_df72e2fcdffeuAEgAlIMsDNfJP` tested the production Docker image built from
commit `893576ee74e131126f0c6265e331bd0a8d5d210e`. This was a new scenario,
not a replay of either affected historical session. A private loopback relay
captured request bodies and responses without authorization headers.

The fixture contained duplicated pricing calculations, protected behavior tests,
and a passing baseline. The first prompt asked for a walkthrough and refactor
plan without edits. The intended second prompt was `update the code as your plan`.
Only the implementation file could be edited; the test command was explicitly
allowed. Background title generation was disabled, concurrency stayed at 1,
strict model checking stayed enabled, and anonymous fallback stayed disabled.

The walkthrough failed before the second prompt could be sent:

| Bridge request | Upstream model | Status | Duration | Result |
| --- | --- | --- | --- | --- |
| 29 | 4 Neon | 200 | 12.729 s | Actual client `read` completed |
| 30 | 4 Hydrogen High | 429 | 57.742 s | Strict `model_downgraded` error |

OpenCode returned to idle with outcome `failed`. All fixture files remained
unchanged. Both captured requests still supplied four client tools. No false
tool-exhaustion claim occurred, and no corrective attempt was established by
this scenario. The upstream model mismatch does not establish why the provider
changed models. The bridge preserved that real restriction rather than treating
it as model prose or accepting another model. Private captures and the structured
failure verdict remain outside the repository. This attempt did not reach the
implementation phase; a later fresh-session attempt completed both phases below.

Full offline tests and vet passed again during this verification. Production and 9Router
were not changed.

## Fresh-session walkthrough and implementation result

At the user's request, a new session received exactly
`walkthrough codebase and suggest to refactor`. The first fresh session
`ses_df729ddbaffeYKntv6kSgCLWs8` failed immediately on the previous downgrade's
in-memory account/model cooldown. It made no upstream request and executed no
tools. It is not a successful Neon test.

The isolated local bridge was restarted once to clear that cooldown and its
conversation cache. No runtime flags changed. Another new OpenCode session,
`ses_df7294f95ffekV9fssKnUpZuox`, received the same exact prompt with strict Neon,
anonymous fallback disabled, concurrency 1, and the same protected pricing fixture.

This session completed the full sequence:

1. Actual `read` calls listed the fixture directory, read `pricing.py`, and read
   `test_pricing.py`.
2. The model returned a grounded walkthrough and proposed extracting the shared
   subtotal and validation into a helper. The TUI returned to idle successfully.
3. In the same session, the next user prompt was exactly
   `update the code as your plan`.
4. Actual `edit` changed `pricing.py`, extracting `_subtotal` and removing
   duplicated validation while preserving both public functions.
5. Actual `shell` ran `python3 test_pricing.py`; its tool result contained
   `PLAN_IMPLEMENT_PASS`.
6. The model returned a final answer and the TUI returned to idle successfully.

Independent test execution passed. SHA-256 comparison confirmed the protected
test and README stayed unchanged; only the implementation changed. The persisted
tool states, actual file change, shell result, independent test, and terminal
idle outcome establish completion together.

All seven bridge requests (32–38) reported upstream `4 Neon`, account 2, and
status 200. Durations were 42.325 s, 53.108 s, 177.713 s, 20.881 s, 92.601 s,
66.134 s, and 15.981 s. Each captured request still supplied four client tools.
The last three requests covered editing, test execution, and final confirmation.
Private request/response captures, the full transcript, and a structured success
verdict remain outside the repository.

No false tool-exhaustion claim occurred in this successful session. It establishes
a natural multi-turn OpenCode coding flow on the release image, not a reproduction
of the original refusal or a live exercise of corrective recovery. The original
downgrade and cooldown failures remain part of the record. Production and 9Router
were not changed.

## Remaining verification gaps

The affected history fixture and original-symptom replay with actual tool results
are still missing. A provider-originated false-exhaustion claim has not been
reproduced in this worktree. Offline regressions and controlled live fault
injection establish narrower recovery behavior; they do not replace that replay.
The separate two-file probe timed out on its final answer after 300 s, the
TUI sequence encountered one transient upstream continuation failure, and one
advanced-head fault-injection run encountered a real model downgrade. These
failures remain part of the verification record despite the completed TUI fixture.

After these isolated checks, the user explicitly requested CI publishing and a
k3s update. The GHCR image was deployed; see the [deployment record](false-tool-quota-deployment.md).
Production runtime settings and 9Router were not changed. Deployment does not
close the original-symptom replay gap.
