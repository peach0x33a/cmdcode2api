# `/v1/responses` + Per-Account Usage GUI

## TL;DR
Two features: (1) implement `POST /v1/responses` so Responses-API clients (Codex CLI) can connect, and (2) add per-account usage tracking + a "Usage" tab in the web UI (usage is currently global-only). One shared prerequisite refactor unblocks both.

## Ground-truth corrections vs. initial framing

1. **No shared `CCRequest`-building helper needed.** `openAIToCC` (cc.go) already handles system extraction, base64 images, tool-schema defaulting, model resolution, token clamping, tool-call history flattening. Cheapest correct reuse: adapt `ResponsesRequest → ChatRequest` and feed the existing pipe unchanged. A parallel `responsesToCC` would duplicate ~150 lines and fork every future CC-side fix.
2. **The real shared extraction is the dispatch block**, `handler.go:44-96` (validation + `sendWithFailover` + 5-branch error mapping) — ~55 lines both handlers need identically. This is also where feature 2's account identity has to surface. **One refactor serves both features** — the only true dependency between them.
3. **Per-account usage does NOT go on `GET /usage`.** That endpoint is deliberately unauthenticated (`server.go:21` bypasses `authMiddleware`) with wildcard CORS (`server.go:59`) — adding account names there leaks configured account identities to any open browser tab. Use **`GET /admin/usage`** instead: `isAdminPath` (`ui.go:49`) already gates on loopback+Host+Origin with zero middleware changes, and it belongs beside `/admin/models`/`/admin/connection`. `/usage` stays byte-identical.

---

## Phase 0 — Shared dispatch refactor (blocks everything)

**File: `internal/app/handler.go`**

1. Add `type ccDispatch struct { resp *http.Response; account string }`.
2. Change `sendWithFailover` to return `(ccDispatch, error)`, populating `account: acct.Name` at the existing success point (`handler.go:178-181`, alongside `pool.MarkHealthy(acct.Name)`) — correct across failover by construction.
3. Extract `dispatchToCC(w, ctx, pool, cfg, req) (ccDispatch, bool)` containing current lines 44-96 (model-empty / excluded-model / empty-messages / `pool.Len()==0` guards, `sendWithFailover` call, all 5 `errors.As` branches). Writes its own error responses, returns `ok=false`. `handleChatCompletions` shrinks to: decode → `dispatchToCC` → branch on `req.Stream` → `usage.save()`.
   - Keep `len(req.Messages)==0` check inside `dispatchToCC` — harmless for the Responses adapter, which always produces ≥1 message or fails earlier.
4. Add account-aware variants, keeping current names as shims so no existing test call sites change:
   - `handleNonStreamForAccount(w, resp, model, usage, cfg, account string)` — body moves here; `handleNonStream(...)` becomes a one-liner passing `""`.
   - `handleStreamForAccount(w, resp, model, usage, cfg, includeUsage bool, account string)` — body moves here; `handleStreamWithOptions`/`handleStream` become shims passing `""`. Mirrors the existing `handleStream` → `handleStreamWithOptions` shim idiom.
5. Swap the two `usage.Record(...)` call sites (`handler.go:430`, `handler.go:537`) to `usage.RecordFor(account, ...)`.

**Risk:** low. **Acceptance:** existing test suite passes unchanged.

---

## Feature 2 — Per-account usage (do this first: smaller, de-risks Phase 0)

### 2.1 Go: `internal/app/usage.go`

Keep existing global `atomic.Int64` fields as-is. Add:

```go
perAccountMu sync.Mutex
perAccount   map[string]*accountUsage // lazily initialized
```

- **Concurrency: `sync.Mutex` + plain map** (values as plain `int64` under the same mutex). Matches the existing coarse-mutex pattern (`accountState`, `AccountPool`); write path is once per completed request. *Alternative: `sync.Map` + atomic values — lock-free but more machinery for zero measurable gain here.*
- `RecordFor(account string, prompt, completion, cacheRead, cacheWrite int)`: calls existing `Record(...)` for globals, then if `account != ""`, locks, **lazily allocates the map if nil** (mandatory — ~15 tests construct `&UsageTracker{}` as a zero value; a nil-map write panics), and accumulates.
- Empty `account` records globals only.
- New types:
  ```go
  type AccountUsage struct {
      Account string `json:"account"`
      TotalRequests, PromptTokens, CompletionTokens, CacheReadTokens, CacheWriteTokens int64
  }
  type UsageReport struct {
      UsageSnapshot
      Accounts []AccountUsage `json:"accounts,omitempty"`
  }
  ```
- `Report() UsageReport` — globals + accounts **sorted by name** (deterministic diffs/UI order). `UsageSnapshot`/`Snapshot()` stay untouched — `/usage` unaffected.

### 2.2 Persistence back-compat — `usage.json`

**Recommendation:** embed `UsageSnapshot` anonymously in `UsageReport` — Go inlines anonymous struct fields, so the new on-disk shape is a strict superset of the old one. An existing flat file decodes cleanly with `Accounts == nil`. No migration code, no version field. `loadUsage()` changes only its target type; `save()` marshals `Report()` instead of `Snapshot()`.

*Alternative (rejected): nest globals under a `"total"` key — cleaner-looking but breaks every existing `usage.json` on disk (silent reset to zero).*

**Known, accepted divergence:** global totals will exceed the sum of per-account rows (pre-feature history has no attribution; empty-account records only hit globals). UI must not compute totals by summing rows — document on `UsageReport`.

**Testability:** use `t.Chdir(t.TempDir())` (Go 1.25) rather than adding a path parameter to `UsageTracker`.

### 2.3 Go: `internal/app/server.go` + `admin.go`

Register beside other admin routes (~line 121): `mux.HandleFunc("/admin/usage", handleAdminUsage(usage))`. Handler in `admin.go` next to `handleAdminModels`/`handleAdminConnection`, same GET-only + `writeError(405)` guard. Returns `usage.Report()`.

### 2.4 Web UI

- `webui-src/src/types.ts` — add `AccountUsage`, `UsageReport` mirroring Go tags.
- `webui-src/src/api.ts` — add shape guards (match `isAdminModel` style) + `fetchUsage(): Promise<UsageReport>` hitting `/admin/usage`.
- `webui-src/src/Usage.tsx` — new. **Follow `Models.tsx`, not `Accounts.tsx`**: load-once + `ListPanel` refresh button, no `setInterval` (usage isn't a liveness signal). Contents: subtitle line; lifetime-totals line from global fields (explicitly not a summed footer); table columns Account | Requests | Prompt | Completion | Cache read | Cache write, `.toLocaleString()` formatting, `"—"` for zero; empty state matching Models/Accounts wording. No charting — internal admin tool.
- `webui-src/src/Tabs.tsx` — `TabId` += `"usage"`, add to `TAB_LABELS`, order `["accounts","models","usage","setup"]`.
- `webui-src/src/App.tsx` — ⚠️ `VALID_TABS` (line 7) duplicates `TAB_ORDER` and will silently reject `#usage` deep links if forgotten. Export `TAB_ORDER` from `Tabs.tsx`, import in `App.tsx`, delete `VALID_TABS`. Add `{active === "usage" ? <Usage /> : null}`.
- No `index.css` changes needed (`table`, `.card`, `.muted`, `.subtitle` already exist).
- Rebuild: `cd internal/app/webui-src && npm install && npm run build`, commit `internal/app/webui/dist/`.

### 2.5 Highest risk

Not concurrency (coarse mutex, once per request — settled by a `-race` test). The real risk is **silent mis-attribution**: crediting the account that *failed* rather than the one that *served*. De-risk with an integration test: two `httptest` upstreams (first 401, second valid CC SSE with a `finish` event carrying usage), drive `handleChatCompletions`, assert `Report().Accounts` has exactly one entry — the healthy account, right token counts. Model on `TestChatCompletionsFailsOverToNextAccountOnAuthError` (`handler_test.go:130`).

### 2.6 Tests

New `internal/app/usage_test.go`:
- `TestRecordForInitializesNilMapOnZeroValueTracker` (the panic guard)
- `TestRecordForEmptyAccountOnlyUpdatesGlobals`
- `TestReportSortsAccountsByName`
- `TestLoadUsageAcceptsLegacyFlatFile` (`t.Chdir` + old 5-field JSON, assert globals restored / `Accounts` empty)
- `TestSaveLoadRoundTripsPerAccount`
- `TestRecordForConcurrent` (N goroutines × M accounts, verify sums under `-race`)

`handler_test.go`: `TestChatCompletionsCreditsUsageToServingAccount` (the failover test above).

`ui_test.go`: `TestAdminUsageAllowsLoopbackWithoutBearerToken`, `TestAdminUsageRejectsNonLoopbackWithoutBearerToken`, `TestAdminUsageRejectsNonGET`.

`server_test.go`: `TestUsageEndpointShapeUnchanged` (assert `GET /usage` still unauthenticated, body has no `accounts` key — regression guard on the security decision above).

---

## Feature 1 — `POST /v1/responses`

### 3.1 File organization

**Recommendation:** two new files — `internal/app/responses_types.go` (all Responses wire types, ~20 structs) and `internal/app/responses.go` (adapter, handlers, SSE emitter). *Alternative (rejected): a third section in `types.go` — pushes it past ~700 lines mixing three unrelated protocols.*

### 3.2 Scope decisions

| Decision | Recommendation | Alternative / tradeoff |
|---|---|---|
| `previous_response_id` | Reject 400: "not supported; gateway is stateless — send full conversation in `input`." | Implement a response store — real statefulness, needs persistence + eviction, zero benefit for Codex (it resends full input). |
| `store` | Accept and ignore; always report `"store": false`. | 400 on `store:true` — honest but breaks OpenAI SDK clients that default it to `true`. |
| Non-`function` tool types (`local_shell`, `web_search`, `custom`) | Reject 400 naming the type. | Silently drop — model can't call the tool, client hangs with no error. |
| `input_image` with `file_id` | Reject 400 (CC needs inline base64, same as `parseDataURL`'s existing constraint). | — |
| Statefulness / `include: ["reasoning.encrypted_content"]` | Ignore unknown fields; emit no encrypted reasoning. | — |

### 3.3 Request adapter — `responsesToChatRequest(*ResponsesRequest) (*ChatRequest, error)`

Returns `*invalidRequestError` so `dispatchToCC`'s existing branch (`handler.go:76-80`) maps it to 400 for free.

Mapping:
- `instructions` → prepended `Message{Role:"system"}`.
- `input` as string → single user `Message`.
- `input` as array, per item `type`:
  - `message` → `Message{Role: item.Role}`; `input_text`/`output_text` → text `ContentPart`; `input_image` → `ContentPart{Type:"image_url", ImageURL:&ImageURL{URL: item.ImageURL}}` — **note: Responses puts the URL as a plain string, not a nested object.**
  - `function_call` → assistant message with `ToolCalls:[{ID: item.CallID, Type:"function", Function:{Name, Arguments}}]` — **use `call_id`, not `id`** (Codex echoes `call_id`).
  - `function_call_output` → `Message{Role:"tool", ToolCallID: item.CallID, Content: TextContent(item.Output)}` → existing tool-role branch (`cc.go:480`).
  - `reasoning` → skip. Anything else → 400.
- `tools[]` (flat: `{type,name,description,parameters}`) → nested `Tool{Type:"function", Function:{...}}`. Flat-vs-nested is the most common integration bug here.
- `max_output_tokens` → `MaxCompletionTokens`.
- `stream` → `Stream`.

### 3.4 Response emitter — the high-risk piece

Add `writeNamedSSE(w, flusher, event string, payload any)` (existing `writeSSE` is hardcoded to `ChatStreamChunk`, no `event:` line; Codex requires the named form).

**Isolate protocol state in a `responsesEmitter` struct** writing to an `io.Writer` + `http.Flusher` (unit-testable without HTTP). Owns: `seq int` (every event carries monotonic `sequence_number`), `outputIndex int` (allocated lazily, never reused), open-item state for message/reasoning items, generated IDs (`msg_`, `rs_`, `fc_`, `resp_` via existing `randomHex`).

**Item-lifecycle rule:** allocate reasoning item on first reasoning delta, message item on first text delta; keep both open until finish; never reopen. At finish, close in allocation order, then emit each collected tool call as `output_item.added` → `function_call_arguments.delta` (one shot) → `function_call_arguments.done` → `output_item.done`.

Event sequence:
```
response.created            {response:{...status:"in_progress"}}
response.in_progress
  [reasoning] response.reasoning_summary_part.added → .reasoning_summary_text.delta* → .done → part.done
  [text]      response.output_item.added → response.content_part.added
              → response.output_text.delta* → response.output_text.done
              → response.content_part.done → response.output_item.done
  [tools]     response.output_item.added → response.function_call_arguments.delta
              → response.function_call_arguments.done → response.output_item.done
response.completed          {response:{...status:"completed", output:[...], usage:{...}}}
```
Failure paths: `response.incomplete` (finish `length` → `incomplete_details.reason:"max_output_tokens"`) and `response.failed`/`error` for upstream stream errors, replacing `writeSSEError`+`[DONE]`. **Responses has no `[DONE]` sentinel** — `response.completed` terminates the stream.

### 3.5 `handleResponsesStream` / `handleResponsesNonStream`

Structural mirrors of `handleStreamForAccount`/`handleNonStreamForAccount`, driven by the same `newCCEventNormalizer()`.

⚠️ **Must run `NewToolCallParser()` over both text and reasoning**, same `toolCallDeduper` + `validateToolCall` + `hasUnsafeArguments` logic — easiest thing to omit, and omitting it means Codex receives DSML tool-call markup as literal text instead of executable calls. Copy the two-parser + `flushParser` + `finishStream` skeleton from `handler.go:227-352` verbatim in structure.

Usage mapping (`response.completed`, field names differ from Chat Completions):
```
usage: { input_tokens, input_tokens_details:{cached_tokens}, output_tokens, output_tokens_details:{reasoning_tokens}, total_tokens }
```
from `normalizer.FinalUsageInfo()` + `normalizer.Usage()`'s cacheRead. Both handlers end with `usage.RecordFor(account, ...)` — free from Phase 0.

Finish-reason mapping via `resolveFinishReason`, then: `stop`→`completed`, `length`→`incomplete`+`incomplete_details.reason:"max_output_tokens"`, `tool_calls`→`completed` (function_call items in `output` are the signal), `content_filter`→`incomplete`.

### 3.6 Handler + route

`handleResponses(pool, cfg, usage)` in `responses.go`, mirrors `handleChatCompletions`: `MaxBytesReader` → debug body log → decode `ResponsesRequest` → `responsesToChatRequest` → `dispatchToCC` → stream/non-stream branch → `usage.save()`.

`server.go`: `mux.HandleFunc("/v1/responses", handleResponses(pool, cfg, usage))` beside `/v1/chat/completions`. No auth/CORS changes needed — inherits bearer auth + wildcard CORS automatically (comment this so nobody "fixes" it later).

`/v1/models` needs no Responses-specific variant. Image input and tool calling ride the shared `openAIToCC` path.

### 3.7 Highest risk

The named-event SSE protocol: event ordering, monotonic `sequence_number`, `call_id` vs `id` on function-call items. Wrong ordering → client connects, streams nothing, reports no error.

De-risk, in order:
1. `responsesEmitter` against `io.Writer`, not `http.ResponseWriter` — full transcript assertions, no HTTP.
2. Golden-transcript tests: fixed CC SSE input → assert exact ordered `event:` name list, `sequence_number` strictly increasing, `output_index` never reused.
3. Keep the `clients.ts` Codex warning until a real `codex -c model_provider=cmdcdswitcher` run succeeds end-to-end — green Go tests are not proof Codex is happy.

### 3.8 Tests

New `internal/app/responses_test.go`. Helper: `decodeResponsesEvents(t, body)` mirroring `decodeStreamPayloads` (`protocol_lifecycle_test.go:343`) but for `event:`/`data:` pairs; reuse `streamResponse(body)` for fake upstreams.

Adapter (table-driven): `TestResponsesStringInputBecomesUserMessage`, `TestResponsesInstructionsBecomeSystem`, `TestResponsesFunctionCallOutputBecomesToolMessage`, `TestResponsesFunctionCallItemUsesCallID`, `TestResponsesInputImageMapsToImageURLPart`, `TestResponsesRejectsFileIDImage`, `TestResponsesFlatToolConvertsToNestedChatTool`, `TestResponsesRejectsNonFunctionToolType`, `TestResponsesRejectsPreviousResponseID`, `TestResponsesMaxOutputTokensReachesCCParams`.

Streaming: `TestResponsesStreamEventOrderForTextOnly`, `TestResponsesStreamSequenceNumbersMonotonic`, `TestResponsesStreamEmitsFunctionCallItemLifecycle`, `TestResponsesStreamRecoversDSMLToolCallFromText` (port a case from `dsmlrecovery_test.go`), `TestResponsesStreamReasoningSummaryDeltas`, `TestResponsesStreamIncompleteOnTruncation`, `TestResponsesStreamEmitsFailedOnUpstreamError`, `TestResponsesStreamNoEventsAfterCompleted` (mirror `TestNoEmissionAfterDone`, `closure_test.go:37`), `TestResponsesStreamRejectsWhenFinishNeverArrives` (mirror `handler_test.go:473`).

Non-stream: `TestResponsesNonStreamBuildsOutputArray`, `TestResponsesNonStreamUsageFieldNames`, `TestResponsesNonStreamToolCallsBecomeFunctionCallItems`.

`server_test.go`: `TestResponsesRouteRequiresBearerToken`, `TestResponsesRouteRejectsGET`.

---

## Phase ordering (dependency-correct)

| # | Work | Depends on |
|---|---|---|
| 0 | `ccDispatch` + `dispatchToCC` + `*ForAccount` shims in `handler.go`; existing suite green | — |
| 1 | `usage.go` per-account tracker + `UsageReport` persistence + `usage_test.go` | 0 |
| 2 | `handleAdminUsage` in `admin.go` + route in `server.go` + `ui_test.go`/`server_test.go` auth tests | 1 |
| 3 | `types.ts`, `api.ts`, `Usage.tsx`, `Tabs.tsx` (+ `App.tsx` `VALID_TABS` dedupe); `npm run build`; commit `webui/dist/` | 2 |
| 4 | `responses_types.go` + `responsesToChatRequest` + adapter tests | 0 |
| 5 | `responsesEmitter` + `writeNamedSSE` + golden-transcript tests (no HTTP) | 4 |
| 6 | `handleResponsesStream`/`handleResponsesNonStream`/`handleResponses` + route + handler tests | 5, 0 |
| 7 | Live Codex CLI smoke test | 6 |
| 8 | Docs: `README.md` (Features list, Endpoints section, curl example), `README.zh-CN.md` (same), `clients.ts` — delete Codex note at line ~19; rebuild + commit `webui/dist/` again | 7 |

**Why feature 2 first:** smaller change, exercises Phase 0 under real tests before feature 1 builds three files on top of it, and `/v1/responses` per-account usage falls out free at Phase 6. If Codex support is more urgent, swap 1-3 with 4-7 — Phase 0 is the only hard dependency — but Phase 6 must not skip the `RecordFor` call.

## Doc/UI updates required (easy to miss)

- `README.md` line 12 (Features endpoint list), line 229+ (`## Endpoints`) — add `POST /v1/responses` and `GET /admin/usage`; note `/usage` unchanged, `/admin/usage` loopback-gated.
- `README.zh-CN.md` — mirrored sections.
- `README.md` line 157 — `exclude_models` doc currently only mentions `/v1/models`/`/v1/chat/completions`; now also rejects via `/v1/responses` (through `dispatchToCC`).
- `webui-src/src/clients.ts` line ~19 — delete the Codex `note`. Only after Phase 7.
- Two `npm run build` + `dist/` commits (Phase 3, Phase 8) unless the Usage tab build is deferred to Phase 8 for one combined commit.

## Unrelated cleanup spotted

`nul` in repo root is untracked — a Windows `> nul` redirect artifact. Delete it and add `nul` to `.gitignore` before it gets committed.
