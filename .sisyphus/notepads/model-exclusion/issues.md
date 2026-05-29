# Model Exclusion - Issues

## Resolved: config template described opt-in behavior after defaults became opt-out

- **Status**: Resolved
- **Found during**: Post-commit review of the model-exclusion commits
- **Problem**: `defaultConfig()` enables `exclude_models` by default (`gpt-`, `claude-`, `gemini-`), but `writeConfigTemplate()` still said to uncomment a commented `exclude_models` block. The generated config could therefore contain misleading instructions and an active `exclude_models` block.
- **Fix**: Updated the template copy to say `exclude_models` is enabled by default and can be disabled by removing entries or setting `exclude_models: []`.
- **Regression coverage**: `TestWriteConfigTemplateIncludesDefaultExclusionComment` and `TestWriteConfigTemplateDefaultExclusionLoadsActive` now exercise the real `defaultConfig()` → `writeConfigTemplate()` → `loadConfig()` path.
- **Verification**: `go test ./internal/app/...`, `go test ./...`, `go build ./cmd/cmdcode2api`, and LSP diagnostics all passed.
