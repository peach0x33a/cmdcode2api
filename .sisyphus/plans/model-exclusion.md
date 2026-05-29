# Model Exclusion Feature

## TL;DR
> **Summary**: Add config-driven model exclusion that filters excluded models from `/v1/models` and blocks them in `/v1/chat/completions`. Default config enables premium/non-opensource exclusions (`gpt-`, `claude-`, `gemini-`) and the first-run template documents how to opt out.
> **Deliverables**:
> - `isModelExcluded()` matching helper with prefix + suffix semantics
> - `ExcludeModels []string` config field
> - Filtered `/v1/models` endpoint
> - Blocked `/v1/chat/completions` with 404 error for excluded models
> - First-run config template documenting default-active premium exclusions
> - Full TDD test coverage (no live upstream calls)
> **Effort**: Short (~150 lines Go, ~120 lines tests)
> **Parallel**: YES - 2 waves
> **Critical Path**: Task 1 + Task 2 (parallel) → Task 3 + Task 4 (parallel) → Task 5 → Task 6

## Context
### Original Request
> 新增feat：排除模型。默认添加一个premium模型的模板，用于排除go套餐不可用的non-opensource(same as premium)模型

Add model exclusion feature. By default include a premium model template to exclude non-opensource models (GPT, Claude, Gemini) unavailable to go plan users.

### Interview Summary
- **Config approach**: Manual `exclude_models` list in `config.yaml` (not CLI flag)
- **Exclusion scope**: Both hide from `/v1/models` AND block `/v1/chat/completions`
- **Default behavior**: Premium exclusions are enabled by default for new configs; users can remove entries or set `exclude_models: []` to opt out
- **Premium models**: GPT, Claude, Gemini prefixes (user will provide exact list; prefix-based matching as default)
- **Test strategy**: TDD with Go `testing` + `httptest`
- **No UI**: This is a headless Go gateway

### Metis Review (gaps addressed)
- **Matching contract**: Prefix matching on both raw model name AND provider-qualified catalog ID suffix (after last `/`). Shared helper used in both filtering and blocking code paths. See matching table below.
- **Filter approach**: Non-destructive — keep `modelCatalog` intact; filter at read boundary via helper. Avoids test pollution and preserves debuggability.
- **Config template**: `yaml.Marshal` cannot emit comments. Solution: first-run config generation writes a raw comment header explaining default-active premium exclusions, then serializes the active config.
- **Edge cases**: Empty strings trimmed, nil/empty list = no exclusions, provider-qualified IDs matched via suffix.
- **Test pollution**: Use `t.Cleanup()` to restore `modelCatalog` between tests.
- **No upstream calls in excluded tests**: Use `httptest` to verify 404 is returned without CC API involvement.

### Matching Contract

```
isModelExcluded(modelID, excludes) → bool
```

A model is excluded if any configured entry is a prefix of:
1. The raw model string (as received from client), OR
2. The full provider catalog ID (e.g., `openai/gpt-4`), OR
3. The short catalog name after final `/` (e.g., `gpt-4` from `openai/gpt-4`)

| Exclude Entry | Model String | Excluded? |
|---|---:|---:|
| `gpt-` | `gpt-4` | ✅ yes (prefix match) |
| `gpt-` | `openai/gpt-4` | ✅ yes (suffix after `/` = `gpt-4`, matches `gpt-`) |
| `claude-` | `anthropic/claude-3-5-sonnet` | ✅ yes (suffix = `claude-3-5-sonnet`) |
| `gemini-` | `google/gemini-1.5-pro` | ✅ yes (suffix = `gemini-1.5-pro`) |
| `gpt-` | `deepseek-chat` | ❌ no |
| `gpt-` | `deepseek/deepseek-chat` | ❌ no |

## Work Objectives
### Core Objective
Add a configurable model exclusion system that hides models from the API listing and rejects their use in chat completions, with a documented premium-model template.

### Deliverables
1. `isModelExcluded()` helper in `internal/app/models.go`
2. `ExcludeModels []string` field on `Config` struct
3. Filtered `/v1/models` response
4. Blocked excluded models in `/v1/chat/completions` (404 error)
5. Premium defaults and opt-out guidance in first-run config generation
6. Full test coverage (matching helper, config, handler, wiring)

### Definition of Done
```bash
# All tests pass
go test ./internal/app/...
# → ok  cmdcode2api/internal/app  (cached)

# Specific test commands:
go test ./internal/app -run 'TestIsModelExcludedPrefixMatch'      # → PASS
go test ./internal/app -run 'TestIsModelExcludedProviderQualified' # → PASS
go test ./internal/app -run 'TestLoadConfigExcludeModels'          # → PASS
go test ./internal/app -run 'TestHandleModelsExcludesPrefixes'     # → PASS
go test ./internal/app -run 'TestChatCompletionsBlocksExcluded'    # → PASS
go test ./internal/app -run 'TestChatCompletionsAllowsNonExcluded' # → PASS

# Build succeeds
go build ./cmd/cmdcode2api
# → no errors

# Start server, verify /v1/models excludes when configured
# (manual verification via curl)
```

### Must Have
- `exclude_models` in `config.yaml` as `[]string`
- Matching semantics per contract table above
- `/v1/models` returns only non-excluded models
- `/v1/chat/completions` returns 404 for excluded model requests
- Shared matching helper used by both code paths
- Nil/empty `exclude_models` = no exclusions (backward compatible)
- First-run generated config documents default-active premium exclusions and how to opt out
- No live upstream calls in tests

### Must NOT Have (guardrails)
- ❌ CLI flag (`--exclude-models`)
- ❌ Environment variable for exclusions
- ❌ UI or admin endpoint
- ❌ Dynamic runtime config reload
- ❌ Regex or glob matching (prefix-only)
- ❌ Destructive mutation of `modelCatalog`
- ❌ Changes to `resolveModelName()` or upstream model fetching
- ❌ Model categorization/classification system
- ❌ Pricing or plan metadata
- ❌ Auth behavior changes
- ❌ Streaming behavior changes
- ❌ Changes to error format (use existing `writeError()`)

## Verification Strategy
> ZERO HUMAN INTERVENTION - all verification is agent-executed.
- Test decision: TDD (RED-GREEN-REFACTOR) with Go `testing` + `httptest`
- QA policy: Every task has agent-executed scenarios with exact commands
- Evidence: `.sisyphus/evidence/task-{N}-{slug}.txt` (test output captures)
- No live Command Code API calls in tests — use `httptest` exclusively

## Execution Strategy
### Parallel Execution Waves
> Target: 2-4 tasks per wave.

**Wave 1**: Foundation — matching helper + config field (parallel, no dependencies)
**Wave 2**: Integration — filter `/v1/models` + block chat completions (parallel, depends on Wave 1)
**Wave 3**: Wiring + Template — route update + config generation template (depends on Wave 2)

### Dependency Matrix
```
Task 1 (matching helper) ──┬──► Task 3 (filter /v1/models) ──┬──► Task 5 (wire routes)
Task 2 (config field) ─────┘   Task 4 (block chat) ──────────┘   Task 6 (config template)
```

### Agent Dispatch Summary
| Wave | Tasks | Categories |
|------|-------|------------|
| 1 | 1, 2 | quick, quick |
| 2 | 3, 4 | quick, quick |
| 3 | 5, 6 | quick, quick |

## TODOs

- [x] 1. Implement `isModelExcluded()` matching helper (TDD: RED → GREEN)

  **What to do**: Create `isModelExcluded(model string, excludes []string) bool` in `internal/app/models.go`. First write failing tests, then implement the helper. Matching semantics per contract table: checks if any exclude entry is a prefix of (a) the raw model string, (b) the full provider-qualified ID, (c) the short suffix after the last `/`.
  **Must NOT do**: Do NOT change `modelCatalog`. Do NOT add regex or glob support. Do NOT modify `availableModels()` yet. Do NOT wire to handlers yet.

  **Implementation pattern**:
  ```go
  // isModelExcluded returns true if model matches any exclude prefix.
  // Matching rules:
  //   1. Raw string: model itself
  //   2. Full ID: if model contains "/", the whole string
  //   3. Short suffix: text after the last "/" in model
  func isModelExcluded(model string, excludes []string) bool {
      if len(excludes) == 0 {
          return false
      }
      candidates := []string{model}
      // Extract suffix after last "/" for provider-qualified IDs
      if idx := strings.LastIndex(model, "/"); idx >= 0 {
          candidates = append(candidates, model[idx+1:])
      }
      for _, c := range candidates {
          if c == "" {
              continue
          }
          for _, e := range excludes {
              e = strings.TrimSpace(e)
              if e == "" {
                  continue
              }
              if strings.HasPrefix(c, e) {
                  return true
              }
          }
      }
      return false
  }
  ```

  **Recommended Agent Profile**:
  - Category: `quick` - Reason: single function, ~20 lines implementation + ~60 lines tests
  - Skills: [`tdd`] - TDD RED-GREEN-REFACTOR loop
  - Omitted: all others — no external deps, no UI

  **Parallelization**: Can Parallel: YES | Wave 1 | Blocks: [3, 4] | Blocked By: []

  **References**:
  - Pattern: `internal/app/models.go:10` — `modelCatalog` package var; new helper lives alongside
  - Pattern: `internal/app/models.go:55-61` — `availableModels()` iteration style
  - Test: `internal/app/cc_test.go` — test style (same-package, table-driven optional)
  - Types: `internal/app/types.go:101-106` — `ModelInfo.ID` is the string to match against

  **Acceptance Criteria** (agent-executable only):
  - [ ] `go test ./internal/app -run 'TestIsModelExcludedPrefixMatch'` — PASS
  - [ ] `go test ./internal/app -run 'TestIsModelExcludedProviderQualified'` — PASS
  - [ ] `go test ./internal/app -run 'TestIsModelExcludedEmptyList'` — PASS
  - [ ] `go test ./internal/app -run 'TestIsModelExcludedWhitespaceEntry'` — PASS

  **QA Scenarios** (MANDATORY):
  ```
  Scenario: Prefix match on raw model name
    Tool: Bash
    Steps: go test ./internal/app -run 'TestIsModelExcludedPrefixMatch' -v
    Expected: PASS. Tests: "gpt-4" with ["gpt-"] → true, "claude-3" with ["claude-"] → true, "gemini-pro" with ["gpt-"] → false, "deepseek-chat" with ["gpt-","claude-","gemini-"] → false
    Evidence: .sisyphus/evidence/task-1-prefix-match.txt

  Scenario: Prefix match on provider-qualified catalog IDs
    Tool: Bash
    Steps: go test ./internal/app -run 'TestIsModelExcludedProviderQualified' -v
    Expected: PASS. Tests: "openai/gpt-4" with ["gpt-"] → true (suffix match), "anthropic/claude-3" with ["claude-"] → true, "deepseek/deepseek-chat" with ["gpt-"] → false
    Evidence: .sisyphus/evidence/task-1-provider-qualified.txt

  Scenario: Empty/nil exclude list returns false
    Tool: Bash
    Steps: go test ./internal/app -run 'TestIsModelExcludedEmptyList' -v
    Expected: PASS. Tests: any model with nil or [] → false
    Evidence: .sisyphus/evidence/task-1-empty-list.txt
  ```

  **Commit**: YES | Message: `feat(models): add isModelExcluded matching helper` | Files: `internal/app/models.go`, `internal/app/models_test.go` (new)

- [x] 2. Add `ExcludeModels` to Config struct (TDD: RED → GREEN)

  **What to do**: Add `ExcludeModels []string` field to `Config` struct in `internal/app/config.go` with `yaml:"exclude_models"` tag. Write test that loads a YAML file containing `exclude_models` list and verifies correct deserialization. Test absent field leaves it nil/empty.
  **Must NOT do**: Do NOT add CLI flag. Do NOT change `saveConfig()` behavior. Do NOT break absent/empty `exclude_models` backward compatibility.

  **Implementation**:
  ```go
  type Config struct {
      APIKey        string   `yaml:"api_key"`
      CommandCode   struct {
          APIKey  string `yaml:"api_key"`
          BaseURL string `yaml:"base_url"`
      } `yaml:"commandcode"`
      Host          string   `yaml:"host"`
      Port          int      `yaml:"port"`
      ExcludeModels []string `yaml:"exclude_models"` // NEW
      Debug         bool     `yaml:"-"` // runtime flag, not persisted
  }
  ```

  **Recommended Agent Profile**:
  - Category: `quick` - Reason: one struct field + test, ~10 lines
  - Skills: [`tdd`] - TDD RED-GREEN
  - Omitted: all others

  **Parallelization**: Can Parallel: YES | Wave 1 | Blocks: [3, 4, 5] | Blocked By: []

  **References**:
  - Pattern: `internal/app/config.go:10-19` — Config struct
  - Pattern: `internal/app/config.go:43-56` — `loadConfig()` with `yaml.Unmarshal`
  - Pattern: `internal/app/config_test.go:5-16` — test style
  - Dependency: `gopkg.in/yaml.v3` — already imported, `[]string` natively supported

  **Acceptance Criteria** (agent-executable only):
  - [ ] `go test ./internal/app -run 'TestLoadConfigExcludeModels'` — PASS (YAML with list deserializes correctly)
  - [ ] `go test ./internal/app -run 'TestLoadConfigNoExcludeModels'` — PASS (absent field = nil/empty, no error)
  - [ ] `go test ./internal/app -run 'TestLoadConfigEmptyExcludeModels'` — PASS (empty list = no exclusions)

  **QA Scenarios** (MANDATORY):
  ```
  Scenario: Config loads exclude_models from YAML
    Tool: Bash
    Steps: go test ./internal/app -run 'TestLoadConfigExcludeModels' -v
    Expected: PASS. YAML with exclude_models: [gpt-, claude-, gemini-] → cfg.ExcludeModels = ["gpt-","claude-","gemini-"]
    Evidence: .sisyphus/evidence/task-2-config-load.txt

  Scenario: Config without exclude_models field works
    Tool: Bash
    Steps: go test ./internal/app -run 'TestLoadConfigNoExcludeModels' -v
    Expected: PASS. YAML without exclude_models → cfg.ExcludeModels == nil or len 0, no panic
    Evidence: .sisyphus/evidence/task-2-config-absent.txt

  Scenario: Empty exclude_models list works
    Tool: Bash
    Steps: go test ./internal/app -run 'TestLoadConfigEmptyExcludeModels' -v
    Expected: PASS. YAML with exclude_models: [] → cfg.ExcludeModels == [] or nil
    Evidence: .sisyphus/evidence/task-2-config-empty.txt
  ```

  **Commit**: YES | Message: `feat(config): add ExcludeModels field to Config struct` | Files: `internal/app/config.go`, `internal/app/config_test.go`

- [x] 3. Filter `/v1/models` endpoint using exclusion helper (TDD: RED → GREEN)

  **What to do**: Modify `handleModels` to use `isModelExcluded()` and filter `modelCatalog` before serving. Change handler signature to accept `*Config`. Write test that sets `modelCatalog`, configures exclusions, calls handler, and verifies response contains only non-excluded models.
  **Must NOT do**: Do NOT mutate `modelCatalog`. Do NOT change the `handleModels` HTTP response format (still `ModelList`). Do NOT add query parameters to the endpoint. Do NOT remove the `object: "list"` wrapper.

  **Implementation pattern**:
  ```go
  func handleModels(cfg *Config) http.HandlerFunc {
      return func(w http.ResponseWriter, r *http.Request) {
          w.Header().Set("Content-Type", "application/json")
          filtered := make([]ModelInfo, 0, len(modelCatalog))
          for _, m := range modelCatalog {
              if !isModelExcluded(m.ID, cfg.ExcludeModels) {
                  filtered = append(filtered, m)
              }
          }
          json.NewEncoder(w).Encode(ModelList{Object: "list", Data: filtered})
      }
  }
  ```

  **Recommended Agent Profile**:
  - Category: `quick` - Reason: modify one handler, write one test, ~20 lines
  - Skills: [`tdd`] - TDD RED-GREEN with httptest
  - Omitted: all others

  **Parallelization**: Can Parallel: YES | Wave 2 | Blocks: [5] | Blocked By: [1, 2]

  **References**:
  - Pattern: `internal/app/handler.go:331-334` — current `handleModels` implementation
  - Helper: `internal/app/models.go` — `isModelExcluded()` from Task 1
  - Test: `internal/app/handler_test.go:11-24` — httptest handler test style
  - Test: `internal/app/server_test.go:46` — `TestAvailableModelsUsesCatalog` for catalog setup pattern
  - Types: `internal/app/types.go:101-111` — `ModelInfo`, `ModelList`

  **Acceptance Criteria** (agent-executable only):
  - [ ] `go test ./internal/app -run 'TestHandleModelsExcludesPrefixes'` — PASS (only non-excluded models returned)
  - [ ] `go test ./internal/app -run 'TestHandleModelsNoExclusions'` — PASS (nil ExcludeModels = all models returned)
  - [ ] `go test ./internal/app -run 'TestHandleModelsAllExcluded'` — PASS (all excluded = empty data array, not error)

  **QA Scenarios** (MANDATORY):
  ```
  Scenario: Prefix-excluded models are filtered from /v1/models
    Tool: Bash
    Steps: |
      go test ./internal/app -run 'TestHandleModelsExcludesPrefixes' -v
    Expected: PASS. catalog has openai/gpt-4, anthropic/claude-3, google/gemini-1.5, deepseek/chat.
      excludes = ["gpt-","claude-","gemini-"].
      Response status 200, data contains only ["deepseek/chat"], NOT gpt/claude/gemini models.
    Evidence: .sisyphus/evidence/task-3-filtered-models.txt

  Scenario: No exclusions returns full catalog
    Tool: Bash
    Steps: go test ./internal/app -run 'TestHandleModelsNoExclusions' -v
    Expected: PASS. ExcludeModels is nil → all models returned unchanged.
    Evidence: .sisyphus/evidence/task-3-no-exclusions.txt

  Scenario: All models excluded returns empty data array
    Tool: Bash
    Steps: go test ./internal/app -run 'TestHandleModelsAllExcluded' -v
    Expected: PASS. ExcludeModels matches every model prefix → response 200 with data: [], no error.
    Evidence: .sisyphus/evidence/task-3-all-excluded.txt
  ```

  **Commit**: YES | Message: `feat(handler): filter excluded models from /v1/models` | Files: `internal/app/handler.go`, `internal/app/handler_test.go`

- [x] 4. Block excluded models in `/v1/chat/completions` (TDD: RED → GREEN)

  **What to do**: Add exclusion check in `handleChatCompletions` after the existing model-required validation (line 35-38), before `cc.Send()`. If `isModelExcluded(req.Model, cfg.ExcludeModels)` returns true, return 404 error via `writeError()` using the existing OpenAI-compatible error JSON shape. Write test with `httptest` that verifies 404 status, correct error JSON shape, and that `CCClient.Send()` is never called.
  **Must NOT do**: Do NOT change error format — use existing `writeError()` exactly. Do NOT add custom error type. Do NOT call CC API when model is excluded. Do NOT change streaming or non-streaming behavior.

  **Implementation** (insert after line 38 in handler.go):
  ```go
  // After: if req.Model == "" { ... }
  // Add:
  if isModelExcluded(req.Model, cfg.ExcludeModels) {
      writeError(w, 404, "invalid_request_error", fmt.Sprintf("model %q is not available", req.Model))
      return
  }
  ```

  **Recommended Agent Profile**:
  - Category: `quick` - Reason: 3-line insertion + test, ~25 lines
  - Skills: [`tdd`] - TDD RED-GREEN with httptest
  - Omitted: all others

  **Parallelization**: Can Parallel: YES | Wave 2 | Blocks: [5] | Blocked By: [1, 2]

  **References**:
  - Pattern: `internal/app/handler.go:35-38` — existing model-required validation (place new check right after)
  - Pattern: `internal/app/handler.go:338-350` — `writeError()` signature and usage
  - Pattern: `internal/app/handler_test.go:11-24` — test style for handler validation
  - Helper: `internal/app/models.go` — `isModelExcluded()` from Task 1
  - Format: `fmt.Sprintf("model %q is not available", req.Model)` — matches existing error message style

  **Acceptance Criteria** (agent-executable only):
  - [ ] `go test ./internal/app -run 'TestChatCompletionsBlocksExcludedModel'` — PASS (404, correct error JSON, upstream NOT called)
  - [ ] `go test ./internal/app -run 'TestChatCompletionsAllowsNonExcludedModel'` — PASS (non-excluded model passes through)

  **QA Scenarios** (MANDATORY):
  ```
  Scenario: Chat completion with excluded model returns 404
    Tool: Bash
    Steps: go test ./internal/app -run 'TestChatCompletionsBlocksExcludedModel' -v
    Expected: PASS. cfg.ExcludeModels = ["gpt-"]. Request model="gpt-4".
      → Status 404. Body contains {"error":{"message":"model \"gpt-4\" is not available","type":"invalid_request_error"}}.
      CCClient.Send() is never invoked.
    Evidence: .sisyphus/evidence/task-4-block-excluded.txt

  Scenario: Chat completion with allowed model passes through
    Tool: Bash
    Steps: go test ./internal/app -run 'TestChatCompletionsAllowsNonExcludedModel' -v
    Expected: PASS. cfg.ExcludeModels = ["gpt-"]. Request model="deepseek-chat".
      → Exclusion gate passes, no 400/404. (Test stops before actual CC call.)
    Evidence: .sisyphus/evidence/task-4-allow-non-excluded.txt

  Scenario: Provider-qualified model name is blocked by prefix
    Tool: Bash
    Steps: go test ... run 'TestChatCompletionsBlocksProviderQualified'
    Expected: PASS. cfg.ExcludeModels = ["gpt-"]. Request model="openai/gpt-4".
      → Status 404 (suffix match). Error message references "openai/gpt-4".
    Evidence: .sisyphus/evidence/task-4-block-qualified.txt
  ```

  **Commit**: YES | Message: `feat(handler): block excluded models in chat completions` | Files: `internal/app/handler.go`, `internal/app/handler_test.go`

- [x] 5. Wire config to handlers and update server routes (auto-wired as side effect of Task 3)

  **What to do**: Update `handleModels` signature in `server.go:70` to pass `cfg`. Update any other references. Run full test suite to confirm no breakage. The key change is on line 70: `mux.HandleFunc("/v1/models", handleModels)` → `mux.HandleFunc("/v1/models", handleModels(cfg))`.
  **Must NOT do**: Do NOT change the route path. Do NOT add new routes. Do NOT change middleware chain. Do NOT change `handleChatCompletions` wiring (it already receives `cfg`). Do NOT change `availableModels()`.

  **Verification**: After wiring, all existing tests must pass plus all new tests from Tasks 1-4.
  ```bash
  go test ./internal/app/... -v
  # All tests: PASS
  ```

  **Recommended Agent Profile**:
  - Category: `quick` - Reason: one-line route change + full test run
  - Skills: [] — no specialized skills needed
  - Omitted: all

  **Parallelization**: Can Parallel: NO | Wave 3 | Blocks: [6] | Blocked By: [3, 4]

  **References**:
  - Pattern: `internal/app/server.go:70` — current `mux.HandleFunc("/v1/models", handleModels)` line
  - Pattern: `internal/app/server.go:69` — `mux.HandleFunc("/v1/chat/completions", handleChatCompletions(cc, cfg, usage))` — existing pattern for passing config to handler
  - Test: `internal/app/server_test.go:46` — `TestAvailableModelsUsesCatalog` needs updating

  **Acceptance Criteria** (agent-executable only):
  - [ ] `go test ./internal/app/...` — all tests PASS
  - [ ] `go build ./cmd/cmdcode2api` — compiles without errors

  **QA Scenarios** (MANDATORY):
  ```
  Scenario: Full test suite passes after wiring
    Tool: Bash
    Steps: go test ./internal/app/... -v
    Expected: All tests PASS. No regression in existing tests. New exclusion tests all pass.
    Evidence: .sisyphus/evidence/task-5-full-test-suite.txt

  Scenario: Binary compiles cleanly
    Tool: Bash
    Steps: go build ./cmd/cmdcode2api
    Expected: Exit code 0, no errors, binary produced.
    Evidence: .sisyphus/evidence/task-5-build.txt
  ```

  **Commit**: YES | Message: `feat(server): wire exclude_models config to /v1/models handler` | Files: `internal/app/server.go`, `internal/app/server_test.go`

- [x] 6. Add premium model exclusion defaults and first-run opt-out guidance

  **What to do**: Modify first-run config generation in `app.go` to produce a `config.yaml` that documents default-active premium model exclusions. Since `yaml.Marshal` cannot emit YAML comments, generate the file using a raw string header when no config exists. The serialized config should include the active `exclude_models` block with gpt-, claude-, gemini- prefixes and the header should explain how to disable it.

  **Must NOT do**: Do NOT change runtime config loading. Do NOT change `saveConfig()` behavior for normal saves. Do NOT add a duplicate commented `# exclude_models:` key. Do NOT break OAuth flow config writes.

  **Implementation approach**:

  In `app.go`, replace the first-run config save (around lines 62-69) with a template-based write:

  ```go
  // 首次运行 — 生成带模板注释的配置
  if cfg == nil {
      cfg2, err := defaultConfig()
      if err != nil { ... }
      if err := writeConfigTemplate(cfgPath, &cfg2); err != nil {
          log.Fatalf("create config: %v", err)
      }
      // ... rest of first-run messaging
  }
  ```

  Add to `config.go`:
  ```go
  func writeConfigTemplate(path string, cfg *Config) error {
      data, err := yaml.Marshal(cfg)
      if err != nil {
          return err
      }
      template := "# cmdcode2api configuration\n" +
          "# See README.md for all options.\n" +
          "\n" +
          "# exclude_models is enabled by default for premium/non-open-source models\n" +
          "# (e.g., GPT, Claude, Gemini) that may be unavailable on certain plans.\n" +
          "# Remove entries below or set exclude_models: [] to make all models available.\n" +
          "\n" +
          string(data)
      return os.WriteFile(path, []byte(template), 0600)
  }
  ```

  **Recommended Agent Profile**:
  - Category: `quick` - Reason: one new function, modify first-run path, ~20 lines
  - Skills: [] — simple string template
  - Omitted: all

  **Parallelization**: Can Parallel: NO | Wave 3 | Blocks: [] | Blocked By: [5]

  **References**:
  - Pattern: `internal/app/app.go:62-69` — current first-run config generation
  - Pattern: `internal/app/app.go:70-82` — first-run message to user
  - Pattern: `internal/app/config.go:58-64` — current `saveConfig()` (complement, not replace)
  - OAuth: `internal/app/app.go:45-48` — OAuth flow uses `saveConfig()` — must NOT break
  - Types: `internal/app/config.go:10-19` — Config struct with new ExcludeModels field

  **Acceptance Criteria** (agent-executable only):
  - [ ] `go test ./internal/app -run 'TestWriteConfigTemplateIncludesDefaultExclusionComment'` — PASS (generated file explains default-active premium exclusions and has no duplicate commented key)
  - [ ] `go test ./internal/app -run 'TestWriteConfigTemplateDefaultExclusionLoadsActive'` — PASS (production-path generated config loads active defaults)
  - [ ] `go test ./internal/app/...` — all tests PASS
  - [ ] First-run generates config.yaml with commented template (manual check: delete config.yaml, run binary)

  **QA Scenarios** (MANDATORY):
  ```
  Scenario: Generated config explains default-active premium exclusions
    Tool: Bash
    Steps: go test ./internal/app -run 'TestWriteConfigTemplateIncludesDefaultExclusionComment' -v
    Expected: PASS. Output contains default-active explanatory copy, contains active `exclude_models`, and does not contain duplicate `# exclude_models:`.
    Evidence: .sisyphus/evidence/task-6-template-comment.txt

  Scenario: Exclude models defaults load active
    Tool: Bash
    Steps: go test ./internal/app -run 'TestWriteConfigTemplateDefaultExclusionLoadsActive' -v
    Expected: PASS. Loading the generated config → cfg.ExcludeModels is ["gpt-", "claude-", "gemini-"].
    Evidence: .sisyphus/evidence/task-6-template-inactive.txt

  Scenario: Full regression after template addition
    Tool: Bash
    Steps: go test ./internal/app/... -v
    Expected: All tests PASS, including OAuth config save flow.
    Evidence: .sisyphus/evidence/task-6-regression.txt
  ```

  **Commit**: YES | Message: `feat(config): add premium model exclusion template to first-run config` | Files: `internal/app/config.go`, `internal/app/app.go`, `internal/app/config_test.go`

## Final Verification Wave (MANDATORY — after ALL implementation tasks)
> 4 review agents run in PARALLEL. ALL must APPROVE. Present consolidated results to user and get explicit "okay" before completing.
> **Do NOT auto-proceed after verification. Wait for user's explicit approval before marking work complete.**
> **Never mark F1-F4 as checked before getting user's okay.** Rejection or user feedback → fix → re-run → present again → wait for okay.
- [x] F1. Plan Compliance Audit — oracle
- [x] F2. Code Quality Review — unspecified-high
- [x] F3. Real Manual QA — unspecified-high (+ playwright if UI)
- [x] F4. Scope Fidelity Check — deep

## Commit Strategy
| # | Message | Files |
|---|---------|-------|
| 1 | `feat(models): add isModelExcluded matching helper` | `models.go`, `models_test.go` (new) |
| 2 | `feat(config): add ExcludeModels field to Config struct` | `config.go`, `config_test.go` |
| 3 | `feat(handler): filter excluded models from /v1/models` | `handler.go`, `handler_test.go` |
| 4 | `feat(handler): block excluded models in chat completions` | `handler.go`, `handler_test.go` |
| 5 | `feat(server): wire exclude_models config to /v1/models handler` | `server.go`, `server_test.go` |
| 6 | `feat(config): add premium model exclusion template to first-run config` | `config.go`, `app.go`, `config_test.go` |
| review-fix | `fix(config): align exclusion template with default behavior` | `config.go`, `config_test.go`, `handler_test.go`, `server.go`, `.sisyphus/**` |

All commits pass: `go test ./internal/app/...`

## Success Criteria
1. `exclude_models` in `config.yaml` is loaded correctly as `[]string`
2. `/v1/models` returns only non-excluded models
3. `/v1/chat/completions` returns 404 with clear error for excluded models
4. Absent/empty `exclude_models` preserves all existing behavior
5. First-run `config.yaml` contains active premium defaults and clear opt-out guidance
6. All 6 atomic commits pass full test suite
7. Zero live upstream calls in tests
