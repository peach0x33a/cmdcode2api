# Model Exclusion - Learnings

## Conventions
- Same-package tests in `internal/app/` (package `app`)
- Test style: `t.Fatalf`, `httptest.NewRecorder`, `httptest.NewRequest`
- Error format: `writeError(w, status, type, msg)` → JSON `{"error":{"message":"...","type":"..."}}`
- YAML config: `gopkg.in/yaml.v3`, `[]string` natively supported

## Gotchas
- `modelCatalog` is package-level var — tests MUST restore with `t.Cleanup()`
- `yaml.Marshal` cannot emit comments — use raw template string for first-run config
- If `defaultConfig()` populates `ExcludeModels`, `yaml.Marshal` will serialize an active `exclude_models` block; template comments must not say “uncomment to enable” in that opt-out design.
- `handleModels` currently doesn't take `cfg` — signature must change to accept `*Config`
- `handleChatCompletions` already receives `cfg` — no signature change needed
- Resetting `modelCatalog` in tests: `old := modelCatalog; t.Cleanup(func() { modelCatalog = old })`
- `isModelExcluded()` implements prefix-matching on both raw model name and suffix (after `/`) for provider-qualified IDs
- `strings.TrimSpace` is used on exclude entries to handle whitespace; empty entries are skipped
- nil and empty exclude lists both short-circuit to `false` via `len(excludes) == 0`

## Task 2: Config struct — ExcludeModels field

- Added `ExcludeModels []string` with `yaml:"exclude_models"` tag to `Config` struct
- yaml.v3 deserializes absent key → nil, empty list `[]` → empty slice (not nil)
- `defaultConfig()` now intentionally enables premium model exclusions by default: `gpt-`, `claude-`, `gemini-`.
- Test pattern: inline YAML string → `yaml.Unmarshal` → assert field
- RED phase: compile error because struct field doesn't exist yet
- GREEN phase: single field addition passes all 3 new tests + existing tests
- No changes to `loadConfig()`, `saveConfig()`, or any other file

## Task 3: handleModels filtering

### Changes made
- **handler.go**: Changed `handleModels` from `func(w, r)` to `func(cfg *Config) http.HandlerFunc` with model filtering via `isModelExcluded()`
- **handler_test.go**: Added 3 tests (`TestHandleModelsExcludesPrefixes`, `TestHandleModelsNoExclusions`, `TestHandleModelsAllExcluded`) 
- **server.go**: Fixed call site `handleModels(cfg)` to match new signature (mechanical change)

### Key findings
- `handleModels` change breaks server.go compilation because `mux.HandleFunc` expects `http.HandlerFunc`, not `func(*Config) http.HandlerFunc`. Minimal fix: pass `cfg` as argument.
- Test pattern: save/restore `modelCatalog` with `oldCatalog := modelCatalog; t.Cleanup(func() { modelCatalog = oldCatalog })`
- `ModelList` and `ModelInfo` types are in the same package, usable directly in tests via `json.NewDecoder(rec.Body).Decode(&resp)`
- Server.go line 70 needed `handleModels(cfg)` instead of `handleModels`

## Task 4: Exclusion check in handleChatCompletions

### Changes made
- **handler.go**: Added 3-line exclusion check after model-not-empty validation:
  ```go
  if isModelExcluded(req.Model, cfg.ExcludeModels) {
      writeError(w, 404, "invalid_request_error", fmt.Sprintf("model %q is not available", req.Model))
      return
  }
  ```
- **handler_test.go**: Added 3 tests:
  - `TestChatCompletionsBlocksExcludedModel` — 404 with `invalid_request_error` when model matches prefix
  - `TestChatCompletionsAllowsNonExcludedModel` — non-excluded model passes gate (fails later at Send, but NOT 400/404)
  - `TestChatCompletionsBlocksProviderQualified` — `openai/gpt-4` blocked by `gpt-` prefix matching suffix

### Key findings
- Exclusion check goes BETWEEN model-empty check and messages-empty check (after model validated non-empty, before messages validated)
- `fmt.Sprintf("model %q is not available", req.Model)` produces `model "gpt-4" is not available` — proper JSON escaping
- For tests that pass the exclusion gate but have no real CCClient, use `&CCClient{Client: &http.Client{}}` to avoid nil pointer panic in `c.Client.Do()`
- The `AllowsNonExcludedModel` test verifies the gate doesn't block by checking status != 400 and != 404 — the empty client causes 502 instead, confirming the gate was passed
- `isModelExcluded` already handles provider-qualified names via suffix extraction (strips `openai/` → checks `gpt-4` against prefix `gpt-`)
- RED phase: both block tests fail with 502 (Send fails without exclusion check); GREEN phase: both pass (exclusion returns 404 before Send)
- `TestChatCompletions'` regex pattern matches all 5 tests (2 existing + 3 new)

## Task 6: First-run config template with default active exclusions

### Changes made
- **config.go**: Added `writeConfigTemplate(path, cfg)` — wraps `yaml.Marshal` output with a comment header explaining default-active `exclude_models` and how to disable it
- **app.go**: Replaced `saveConfig` → `writeConfigTemplate` in both first-run paths (normal line 67, OAuth line 34). OAuth final save (line 46) stays as `saveConfig`.
- **config_test.go**: Added/updated 2 tests — `TestWriteConfigTemplateIncludesDefaultExclusionComment` (verifies default-active explanatory copy and no duplicate commented key) and `TestWriteConfigTemplateDefaultExclusionLoadsActive` (verifies production-path default exclusions survive load)

### Key findings
- TDD: RED phase had `undefined: writeConfigTemplate` compile error; GREEN phase implemented function
- Tests must use `defaultConfig()` for template behavior, not a hand-built `Config{APIKey: ...}`, because production first-run config includes default `ExcludeModels`.
- Template uses raw string concatenation (not `yaml.Marshal` which can't emit comments)

## Post-review fixes
- Updated first-run template copy to match opt-out defaults.
- Strengthened non-excluded chat-completion test to reject both 400 and 404 gate failures.
- Updated server startup log to report `models: N loaded, M available` after applying exclusions.
- Verified with `go test ./internal/app/...`, `go test ./...`, `go build ./cmd/cmdcode2api`, and LSP diagnostics.
- `os` already imported in `config.go` and `app.go` — no import changes needed in those files
- `config_test.go` needed `"os"` and `"strings"` imports added for `os.ReadFile` and `strings.Contains`
