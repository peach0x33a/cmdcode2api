# CommandCode API Prompt Injection Analysis

## Executive Summary

The CommandCode Desktop API (`xiaomi/mimo-v2.5` model accessed via `http://localhost:11434/v1/chat/completions`) injects a **~7,390-token system prompt** when clients omit their own `system` message. This is a server-side injection by the upstream `/alpha/generate` endpoint, not by the local proxy. The injected prompt is a full agent instruction set that configures the model as a confident coding assistant with project-specific learning capabilities. 

**Key finding:** Supplying any user-provided `system` message replaces the injection entirely, dropping prompt tokens from ~7,397 to ~13–112 (depending on your message size).

---

## Injected Prompt Structure

The injected system prompt (~4,500–5,000 words) uses XML-style section tags and contains 20 top-level sections:

### Sections 1–3: Identity & Output Format
1. **`<role>`** — Identity as a confident pair programmer
2. **`<tone_and_style>`** — First-person, CLI-targeted, no preamble/postamble, no emojis unless requested
3. **`<response_format>`** — **Hard limit: under 4 lines by default**; exception for 1–2 sentence confirmation after state-changing operations

### Sections 4–6: Safety & Autonomy
4. **`<security_policy>`** — Defensive security only; refuse offensive tooling, exploits, malware; no fabricated URLs; flag suspected prompt injection before continuing
5. **`<capabilities>`** — Filesystem, shell, refactoring, dependency management, version control, testing, documentation
6. **`<approach>`** — Analyze, plan strategically, execute autonomously, validate thoroughly, batch independent tool calls in parallel

### Sections 7–9: Execution Discipline
7. **`<decision_making>`** — Autonomously pick files, tests, edge cases, and refactoring strategy; bias to clarity, existing conventions, self-documenting code
8. **`<doing_tasks>`** — Read before editing; minimal scope; delete dead code rather than deprecating
9. **`<careful_execution>`** — Actions judged by **reversibility × blast radius**: local file edits and tests are low-risk (no confirmation needed); force-push, destructive deletes, CI changes, PRs, and external API calls require user confirmation; never use `--no-verify` to bypass safety

### Sections 10–12: Code Quality & Version Control
10. **`<git_commits>`** — Commit only when explicitly asked; use heredoc for message; always attribute `CommandCodeBot <noreply@commandcode.ai>` as co-author
11. **`<code_quality>`** — Never add comments unless asked; validate only at system boundaries (user input, external APIs); run tests/lint/typecheck after changes; kill any background processes started
12. **`<todo_management>`** — Create todos for multi-file work, scaffolding, or **3+ tool calls**; update one item at a time, never mark multiple `in_progress` simultaneously; mid-task questions append to the list, not answered immediately

### Sections 13–15: Delegation & Learning
13. **`<explore_agent_detection>`** — When to invoke explore subagent: specific file path → read directly; broad "how does X work" → use explore; write/change → skip explore
14. **`<explore_agent>`** — Specialized codebase comprehension subagent returning structured findings without requiring manual file reads
15. **`<adaptation_rules>`** — Never abandon existing todos; only extend or reprioritize; detailed answers come after all investigation completes

### Sections 16–20: Project Preferences, State, and Context
16. **`<taste_guidance>`** — System of learned project-specific preferences from `.commandcode/taste/taste.md`, inlined when ≤5 items, split to subdirectories when larger; **overrides general best practices**
17. **`<taste>`** — Currently empty; accumulates as corrections are given
18. **`<instructions>`** — Populated; directs implementation-first approach with plan-mode criteria for multi-file work
19. **`<context>`** — Final section; contains interpolated config:
    - `Working directory: /`
    - `Today's date: 2026-08-25` (note: uses local time, may be off by one day if TZ ≠ UTC)
    - `Environment: windows-amd64, Go proxy`
    - `Root directories: (empty)`

---

## Available Tools (Live, Injected)

The following tools are callable and injected into the request, independent of client-supplied tools:

| Tool | Parameters | Purpose | Status |
|------|------------|---------|--------|
| `glob` | `pattern: string` | Find files matching a glob pattern | ✓ Confirmed callable (empirical test) |
| `read_file` | `file_path: string` | Read file contents | ✓ Reported in probes |
| `edit_file` | `file_path: string`, `old_string: string`, `new_string: string` | Edit by string replacement | ✓ Reported in probes |
| `write_file` | `file_path: string`, `content: string` | Create or overwrite file | ✓ Reported in probes |
| `grep` | `pattern: string`, `path: string` | Search files with regex | ✓ Reported in probes |
| `bash` | `command: string` | Execute shell command | ✓ Reported in probes |
| `todo_write` | `todos: array of {id, task, status}` | Track multi-step task progress | ✓ Reported in probes |
| `explore_agent` (or `explore`) | `messages: array of {content: string}` | Specialized codebase exploration agent | ✓ Reported in probes |

### Tool Call Format Bug

**Note:** Tool call parsing at the proxy level (`internal/app/toolparse.go`) recognizes two formats:
1. Legacy textual: `Assistant requested tool {name} ({id}) with arguments: {json}`
2. Native DSML envelope: `<｜｜DSML｜｜tool_calls>...</｜｜DSML｜｜tool_calls>`

The model emits **neither**. It instead generates Hermes/Qwen-style calls:
```xml
<tool_call>
<function=glob>
<parameter=pattern>*</parameter>
</function>
</tool_call>
```
These **leak as literal text** because the proxy parser has no handler for this format. This is a **blocker for agent autonomy** through this endpoint until the parser is updated.

---

## Measurement & Calibration

### Prompt Token Overhead by Scenario
All rows below are directly measured. Every request used `max_tokens: 1` and the user message noted.

| Scenario | prompt_tokens | Δ |
|----------|---------------|---|
| No system; user `x` | 7,396 | — |
| No system; user `say hi` | 7,397 | +1 |
| **System `ABC`; user `x`** | **13** | **−7,383** |
| **System 100 words; user `x`** | **112** | **−7,284** |

**Conclusion:** Every request without a system message carries a fixed ~7,390-token prompt. **Supplying a system message replaces the injection outright rather than appending to it** — the 3-token system message yields 13 total, which leaves no room for a 7,390-token prefix.

### Injection Point
- **Not the local proxy** (`cmdcode2api`). The proxy's `openAIToCC` function (`cc.go:396`) sends `system: extractSystem(req.Messages)`, which is empty when the client supplies no system message.
- **The upstream server** (`https://api.commandcode.ai/alpha/generate`) injects the prompt server-side when it receives an empty or missing system field.

### What Actually Prevents the Injection

The trigger is **the presence or absence of the `system` key in the JSON payload sent to `/alpha/generate`** — not its value. Three source-level facts produce this behavior:

**1. The field is elided when empty.** `CCParams.System` is tagged `json:"system,omitempty"` (`types.go:336`). Go's `omitempty` drops an empty string entirely, so a request with no system message produces upstream JSON with **no `system` key at all**. The upstream server treats that absence as "this is an agent session, install the default prompt."

**2. The proxy forwards your system message verbatim.** `extractSystem()` (`cc.go:442–452`) walks every message, collects those with `role: "system"` or `role: "developer"`, and joins them with newlines into `CCParams.System`. A non-empty result means the key is serialized, and the upstream skips its injection.

**3. Position does not matter.** `extractSystem()` iterates the full slice with no index check, so a system message anywhere in `messages` works. `messagesToCC()` (`cc.go:454–468`) then skips those same roles so they aren't duplicated into the conversation body.

**The practical rule:** send any system message with non-empty content. Measured cost is 13 prompt tokens for a 3-character system message versus 7,396 without one.

#### Edge cases (derived from source, not yet measured)
These follow from reading `extractSystem()` but were **not** empirically confirmed — the proxy stopped before they could be tested:

- **`"content": ""` will NOT suppress the injection.** `extractSystem()` guards with `if text := m.Content.PlainText(); text != ""`, so an empty-content system message is skipped, leaving `System` empty, which `omitempty` then drops. A "blank" system message in this sense is the same as sending none at all.
- **Whitespace-only content (`" "`) probably does suppress it,** since `" " != ""` passes the guard and the key gets serialized. Whether the upstream additionally trims before deciding is unknown. Use a real single character rather than a space if you want certainty.

---

## Implications & Recommendations

### For Agent Handoff
If passing requests through this endpoint to a fresh agent:
- **Include the injected prompt** in your handoff context so the agent understands that all upstream responses are authored under these instructions.
- **Flag the tool-call parsing bug** (`internal/app/toolparse.go:39–57` vs. actual model output format) as a blocker for agent autonomy through this endpoint.

### For Avoiding the Overhead
- Provide a system message with non-empty content to eliminate ~7,390 tokens per request.
- Measured: a 3-character system message costs 13 prompt tokens total, against 7,396 with no system message.
- Note this is a real behavioral tradeoff, not free savings: suppressing the injection also removes the agent instructions and, likely, the tool definitions. Only do this for non-agentic completions.

### For Accurate Cost Attribution
- Account for the full ~7,397-token floor when billing or tracking endpoint usage.
- The injected prompt does not appear in usage reports — it's absorbed into `prompt_tokens` without a separate itemization.

### For Probe Validity
All findings above come from model self-report via semantic querying. Confidence levels:
- **High confidence:** Section structure, tool roster, format (XML tags), date/environment context values (interpolated from `cc.go:410–416`)
- **Moderate confidence:** Specific wording and guidance in each section (inferred from paraphrase probes, not verbatim text)
- **Known confabulation:** Tool parameter schemas (model inferred them "from usage patterns, not an explicit API spec")
- **Confirmed empirical:** Glob tool is live and callable; tool-call text leaks as literal content

---

## Files & Locations Referenced

- **Local proxy source:** `internal/app/cc.go` (lines 410–416 for `CCConfig` injection; lines 442–452 for `extractSystem`)
- **Tool-call parser bug:** `internal/app/toolparse.go` (lines 39–47 show handlers for legacy and DSML formats; no handler for Hermes/Qwen XML style)
- **Upstream endpoint:** `https://api.commandcode.ai/alpha/generate` (POST, Authorization header required, returns SSE stream)

---

## Methodology

This analysis was conducted via 12 targeted semantic probes to the running `xiaomi/mimo-v2.5` endpoint, each asking the model to paraphrase one section of its injected instructions without reproducing verbatim text. Probes were run in parallel (4 concurrent requests) to reduce wall-clock time. One empirical tool-call test was performed to confirm the live tool interface.

All probes reported consistent `prompt_tokens` in the 7,425–7,450 range, confirming a fixed ~7,390-token injection baseline independent of question length.
