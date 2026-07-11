# Tool Calling Architecture and Porting Guide

## Purpose

This document describes the complete tool-calling implementation in M365Bridge and turns it into a reusable design for other projects. It covers three separate mechanisms:

1. M365 backend tools, which execute inside Microsoft infrastructure.
2. Caller-defined simulated tools, which are declared and executed by an API client.
3. Opt-in built-in coding tools, which execute locally inside the API server workspace.

These mechanisms share response types but have different trust boundaries, execution owners, and lifecycle rules. They must not be treated as interchangeable.

## Scope and source of truth

The production implementation is distributed across these files:

| Concern                                                | Source                                   |
|--------------------------------------------------------|------------------------------------------|
| Shared caller-defined tool types and text formatting   | `pkg/toolcalling/toolcalling.go::19`     |
| OpenAI and Anthropic simulated prompts and parsers     | `pkg/toolcalling/simulated.go::21`       |
| HTTP adaptation and local tool loop                    | `pkg/servers/api.go::542`                |
| Provider streaming and non-streaming output            | `pkg/servers/api.go::1218`               |
| M365 SignalR tool-event extraction                     | `pkg/client/client.go::295`              |
| Message-history conversion                             | `pkg/payload/payload.go::115`            |
| M365 option filtering                                  | `pkg/payload/payload.go::552`            |
| Tool message-type mapping and local-tool configuration | `pkg/models/models.go::93`               |
| Built-in tool schemas                                  | `pkg/codingtools/codingtools.go::77`     |
| Built-in tool execution                                | `pkg/codingtools/codingtools.go::95`     |
| Unix process-tree handling                             | `pkg/codingtools/process_unix.go::12`    |
| Windows process-tree handling                          | `pkg/codingtools/process_windows.go::11` |
| OpenAI integration checks                              | `test_toolcalling_openai.sh::1`          |
| Anthropic integration checks                           | `test_toolcalling_anthropic.sh::1`       |
| Runtime dependencies                                   | `Dockerfile::16`                         |

The implementation, not this document, remains authoritative when behavior changes.

## Terminology

### Backend tool

A tool selected and executed by M365 itself. Examples include web search, code interpreter, plugins, and actions. The proxy observes M365 SignalR messages and converts recognizable activity into compatible tool-call objects. The caller does not execute these operations.

### Caller-defined tool

A function supplied in an OpenAI or Anthropic request. M365 has no native protocol for these definitions. The proxy therefore asks the model to simulate a provider-native response, parses the response, and returns the call to the caller. The caller executes the tool and sends its result in a later request.

### Built-in coding tool

A caller-defined tool whose implementation is owned by the proxy. When local coding tools are enabled, the proxy executes recognized calls inside a configured workspace and continues the model loop without returning each intermediate call to the API client.

### Simulated response

A complete OpenAI Chat Completions or Anthropic Messages JSON object generated as text by the M365 model. The proxy extracts, validates, filters, and translates this object into the actual HTTP response.

### Provider-native output

Output that follows the public protocol used by the caller. OpenAI receives `tool_calls` or Responses API `function_call` items. Anthropic receives `tool_use` blocks. Anthropic output is not converted through an OpenAI intermediate representation at the wire boundary.

## Architecture overview

```text
Client request
  |
  +-- no tools -----------------------------------------------+
  |                                                            |
  |   provider request -> M365 payload -> SignalR stream       |
  |   -> direct provider-native text streaming                 |
  |                                                            |
  +-- caller-defined tools ------------------------------------+
  |                                                            |
  |   full provider request JSON                               |
  |   -> simulated-response prompt                             |
  |   -> strip M365 code-interpreter option sets               |
  |   -> M365 SignalR text                                     |
  |   -> enumerate and score JSON candidates                   |
  |   -> allowlist declared tool names                         |
  |   -> provider-native tool-call response                    |
  |   -> caller executes tool and sends result                 |
  |                                                            |
  +-- enabled local built-in tools ----------------------------+
      |
      simulated tool call
      -> classify local versus caller-owned call
      -> validate and execute local operation
      -> serialize structured result
      -> append result to next M365 turn
      -> repeat with duplicate and iteration guards
      -> return final answer or caller-owned call
```

The central adapter is `pkg/servers/api.go`. The M365 transport stays stateless across requests. Per-request text, thinking, tool calls, finish reason, and conversation ID travel through `client.StreamChunk` at `pkg/client/client.go::248`.

## Why simulation is required

M365 Copilot accepts a user message, backend options, and an upstream conversation ID. It does not accept arbitrary OpenAI `tools` or Anthropic tool schemas as a native execution contract. Forwarding tool definitions directly would therefore not cause M365 to emit caller-compatible function calls.

The implemented bridge uses two coordinated controls:

1. Prompt control: embed the complete provider request and require a provider-shaped JSON response.
2. Infrastructure control: remove M365 code-interpreter option sets whenever caller-defined tools are present.

The second control is essential. Without it, M365 can intercept file or code requests and run its own sandbox instead of producing a call for a client-declared tool. The option filtering is implemented by `codeInterpreterOptions` and `getOptions` at `pkg/payload/payload.go::57` and `pkg/payload/payload.go::552`.

## Three distinct execution paths

### M365 backend tools

M365 emits tool activity as SignalR type 1 `update` messages. The transport maps known `messageType` values through `models.ToolMessageType`:

| M365 message type     | Compatible function name |
|-----------------------|--------------------------|
| `InternalSearchQuery` | `search`                 |
| `GeneratedCode`       | `code_interpreter`       |
| `TriggerPlugin`       | `trigger_plugin`         |
| `InvokeAction`        | `invoke_action`          |

The map is defined at `pkg/models/models.go::93`. `extractToolCall` converts message text into arguments at `pkg/client/client.go::552`:

- `InternalSearchQuery` becomes `{"query":"..."}`.
- `GeneratedCode` becomes `{"code":"..."}`.
- Other mapped events become `{"input":"..."}`.
- `messageId` becomes the call ID. A UUID is generated if it is absent.

M365 may also expose web-search terms in a Progress message's `searchQueries` field. Each query becomes a `search` call through `makeSearchToolCall` at `pkg/client/client.go::598`.

The transport collects backend calls and publishes them only on the final `StreamChunk`. SignalR type 2 provides the final conversation ID, and type 3 ends the request and assigns `tool_calls` when any calls were collected. See `pkg/client/client.go::466` and `pkg/client/client.go::473`.

When caller-defined tools are active, handlers deliberately discard backend tool calls. This prevents M365-invented `code_interpreter` or other backend operations from being misrepresented as declared client functions. Examples are `pkg/servers/api.go::1336`, `pkg/servers/api.go::1447`, and `pkg/servers/api.go::1722`.

### Caller-defined simulated tools

Caller-defined tools are available whenever a request carries tool definitions. There is no global mode switch. The historical mode toggle and global tool-calling configuration were removed, and simulated prompting became the standard path for tool-bearing requests.

The proxy performs these steps:

1. Decode the provider request.
2. Preserve the original complete request body.
3. Optionally add built-in definitions.
4. Replace the request's `tools` field with the effective tool list.
5. Build a provider-specific simulation prompt around that complete request.
6. Replace the last user message with the simulation prompt.
7. send `hasTools=true` to payload construction so conflicting M365 options are removed.
8. Buffer model text when parsing requires the complete JSON object.
9. Extract and score candidate JSON objects.
10. Keep only calls whose names occur in the effective request tool list.
11. Emit provider-native output.

The full request is embedded because the model needs messages, tool schemas, `tool_choice`, stream preference, and other provider semantics together. Sending only tool definitions loses information needed to choose and format calls correctly.

### Built-in local coding tools

Built-in tools are disabled by default. `M365_ENABLE_CODE_TOOLS` creates a `codingtools.Manager` during API server startup at `pkg/servers/api.go::150`. A startup failure is fatal if the workspace cannot be canonicalized or limits are invalid.

When enabled:

- A built-in name already declared by the caller is marked local.
- `M365_AUTO_EXPOSE_TOOLS=1` appends every missing built-in schema to the effective request.
- With auto-exposure disabled, the caller must explicitly declare the built-in tool it wants the server to execute.
- Caller-owned names remain caller-owned and are returned to the caller.
- Local calls are executed by the server and fed back to M365 until a final answer or caller-owned call appears.

The preparation logic is at `pkg/servers/api.go::557`. Automatic schema conversion preserves each provider's shape:

- OpenAI: `{type:"function", function:{name,description,parameters}}`
- Anthropic: `{name,description,input_schema}`

## Shared contracts

### Effective tool definition

`toolcalling.ToolDef` supports both provider shapes at `pkg/toolcalling/toolcalling.go::19`:

```go
type ToolDef struct {
    Type        string
    Function    ToolDefFunc
    Name        string
    Description string
    InputSchema map[string]any
}
```

`ToolName` returns `Function.Name` for OpenAI and otherwise returns the flat Anthropic `Name`.

### Internal parsed call

The simulation parser returns:

```go
type ToolCall struct {
    ID        string
    Name      string
    Arguments json.RawMessage
}
```

The HTTP adapter converts it to `client.ToolCall`, whose `Function.Arguments` is a JSON string. This shared transport form can then be rendered into OpenAI, Anthropic, or Responses API output.

### Simulation result

`SimulatedResult` at `pkg/toolcalling/simulated.go::102` carries:

- `Content`: final assistant text when no accepted call exists.
- `ToolCalls`: accepted parsed calls.
- `FinishReason`: internal `tool_calls` or `stop`.
- `HasPayload`: whether a usable provider-shaped object was found.

`HasPayload` distinguishes valid plain-text provider output from raw M365 prose that failed simulation parsing.

### Tool-result text

M365 receives tool history as plain text because it has no caller-defined tool-result wire protocol:

```text
[Previous Tool Call: TOOL_NAME]
Arguments: JSON

[Tool Result (call_id: CALL_ID)]
RESULT
```

OpenAI and Anthropic request decoding performs this conversion in `payload.Message.UnmarshalJSON` at `pkg/payload/payload.go::115`. Local loop results use:

```text
[Tool Result for TOOL_NAME (call_id: CALL_ID)]
SERIALIZED_RESULT
```

The formatter is at `pkg/toolcalling/simulated.go::670`.

## Prompt contracts

### OpenAI prompt

`BuildSimulatedPrompt` at `pkg/toolcalling/simulated.go::21` instructs M365 to:

- Treat the embedded JSON as a complete `POST /v1/chat/completions` request.
- Return exactly one Markdown `json` code block and no surrounding prose.
- Return one final object even when the client requested streaming.
- Put calls in `choices[0].message.tool_calls`.
- Set `choices[0].finish_reason` to `tool_calls`.
- Encode `function.arguments` as a JSON string.
- Use only tool names declared by the caller.
- Never invent or use built-in `code_interpreter` or `web_search` names.

OpenAI `tool_choice` handling:

| Input                                   | Prompt behavior                              |
|-----------------------------------------|----------------------------------------------|
| `required`                              | At least one call is required.               |
| `auto`, `none`, empty                   | No additional forced-call sentence is added. |
| `{type:"function",function:{name:"x"}}` | A call to `x` is required.                   |

The current `none` handling relies on the embedded request semantics rather than adding an explicit prohibition sentence. A port should preserve this only if its target model reliably follows the embedded field.

### Anthropic prompt

`BuildSimulatedPromptAnthropic` at `pkg/toolcalling/simulated.go::62` requests a complete Anthropic Messages object:

- Calls are `content` blocks with `type: "tool_use"`.
- `stop_reason` is `tool_use` when calls exist.
- `input` is a JSON object, not a string.
- Only request-declared names are valid.
- Streaming requests still produce one completed JSON object for parser consumption.

Anthropic `tool_choice.type` handling:

| Input         | Prompt behavior                                                                 |
|---------------|---------------------------------------------------------------------------------|
| `any`         | At least one call is required.                                                  |
| `tool`        | A specific call is required. The selected name remains in the embedded request. |
| `auto`, empty | The model may call a tool or answer directly.                                   |

### Prompt injection point

`injectSimulatedPrompt` and `injectSimulatedPromptAnthropic` replace the last user message rather than adding an unrelated trailing system message. See `pkg/servers/api.go::2366` and `pkg/servers/api.go::2382`.

`BuildConversationPayload` sends only the latest message because M365 tracks prior turns through `ConversationId`. Earlier system messages are prefixed to that latest text. It does not send a `messageHistory` array. See `pkg/payload/payload.go::369`.

## JSON extraction and parser strategy

Model output is not trusted to contain one perfect fenced object. `enumerateJSONCandidates` at `pkg/toolcalling/simulated.go::532` generates candidates from:

1. The complete trimmed response.
2. Every fenced code block body.
3. Balanced object and array segments found in the response.

Candidate strings are deduplicated while preserving discovery order. Balanced scanning understands quoted strings and escapes, and limits emitted segments to 128 to bound work.

### Why candidate scoring exists

The embedded request itself is JSON and models can echo it. The response may also contain nested JSON, prose, or multiple code blocks. Selecting the first parseable object is therefore unsafe.

OpenAI scoring at `pkg/toolcalling/simulated.go::461` rewards:

- A non-empty `choices` array.
- `choices[0].message`.
- Assistant role.
- Non-empty `tool_calls` or content.
- `finish_reason`.
- `object: "chat.completion"`.
- IDs beginning with `chatcmpl`.

It penalizes request-like objects containing messages or input plus tools or tool-choice signals.

Anthropic scoring at `pkg/toolcalling/simulated.go::271` rewards:

- A non-empty content array.
- `tool_use` and text blocks.
- Assistant role.
- `type: "message"`.
- `stop_reason`.
- IDs beginning with `msg_`.

It penalizes request-like objects and OpenAI `choices` arrays. A best score of zero or less is unusable.

### Parsing tolerance

The OpenAI parser accepts both canonical wrapped calls and a flat fallback:

```json
{"id":"call_x","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a\"}"}}
```

```json
{"id":"call_x","name":"read_file","arguments":{"path":"a"}}
```

Arguments already encoded as strings are retained. Object or array arguments are serialized. Missing arguments become `{}`. Missing IDs are generated.

The Anthropic parser expects `content` blocks but also accepts a top-level `text` fallback. Missing `tool_use.input` becomes `{}`, and missing IDs are generated.

### Name allowlisting

Both parsers receive names extracted from the effective tool list through `toolNamesFromDefs`, which delegates provider normalization to the provider-neutral `toolcalling.ToolName` helper. It reads OpenAI's nested `function.name` and Anthropic's flat `name`. Any generated call with a different name is discarded. This is the final defense against M365-invented backend tools leaking into caller-defined output. The extraction path is covered by `TestToolNamesFromDefsSupportsProviderShapes` at `pkg/servers/api_test.go::10`.

A port must use the same provider-neutral normalization for preparation and allowlisting. Applying an OpenAI-only extractor to Anthropic definitions produces an empty allowlist and disables name filtering. Apply the allowlist after parsing and before execution or response emission. Prompt instructions alone are not a security boundary.

### Plain-text fallback and finish reasons

If a valid provider-shaped payload contains no accepted calls, its content is returned and the finish reason becomes `stop` or Anthropic `end_turn`.

If no usable payload exists, raw M365 text is returned. Backend calls are still discarded for tool-bearing simulated requests, and the finish reason is reset so the API never reports `tool_calls` or `tool_use` without corresponding call blocks. See `pkg/servers/api.go::1472` and `pkg/servers/api.go::1872`.

## Provider endpoint behavior

### OpenAI Chat Completions

Endpoint: `POST /v1/chat/completions`

Request fields used by tool calling:

- `tools`
- `tool_choice`
- `messages`
- `stream`
- `session_id` or `user`
- optional session suffix in `model`

Non-streaming calls appear under `choices[0].message.tool_calls`, with `content: null` when there is no text and `finish_reason: "tool_calls"`. See `pkg/servers/api.go::1439`.

Streaming emits one or more `chat.completion.chunk` events with `delta.tool_calls`, followed by a final chunk whose finish reason is `tool_calls`, then `[DONE]`. See `pkg/servers/api.go::1217`.

### OpenAI Completions

Endpoint: `POST /v1/completions`

The FIM prompt is converted into one chat message, then the complete Completions request is embedded in the OpenAI simulation prompt. See `pkg/servers/api.go::782`.

Non-streaming tool calls are a non-standard top-level `tool_calls` extension on the text-completion response. Streaming detects calls and sets the final `finish_reason` to `tool_calls`, but does not emit call bodies in text-completion chunks. Consumers needing standard function-call output should use Chat Completions or Responses.

Built-in local execution is not wired into `/v1/completions`. This endpoint supports caller-defined simulated call detection only.

### Anthropic Messages

Endpoint: `POST /v1/messages`

Anthropic definitions remain flat. A non-streaming call is emitted as:

```json
{
  "type": "message",
  "role": "assistant",
  "content": [
    {
      "type": "tool_use",
      "id": "toolu_or_generated_id",
      "name": "tool_name",
      "input": {}
    }
  ],
  "stop_reason": "tool_use"
}
```

Streaming remains Anthropic-native:

1. `message_start`
2. optional thinking events
3. optional text block events
4. one `content_block_start` and `content_block_stop` pair per `tool_use`
5. `message_delta` with `stop_reason: "tool_use"`
6. `message_stop`

See `pkg/servers/api.go::1551`.

### OpenAI Responses

Endpoint: `POST /v1/responses`

Responses input conversion recognizes:

- Plain string input.
- Message items.
- `function_call` history items, converted into assistant text as `Tool call: NAME(ARGUMENTS)`.
- `function_call_output` result items, converted into a tool-role message whose plain text includes the original `call_id` and output.
- Reasoning items, which are skipped because M365 generates its own reasoning.

The `call_id` must remain in the message content because `BuildConversationPayload` sends only the last message's text and does not serialize `Message.Name` or `Message.ToolCallID` as separate M365 fields. See `responsesInputToMessages` at `pkg/servers/api.go::2639`, `BuildConversationPayload` at `pkg/payload/payload.go::369`, and `TestResponsesInputToMessagesPreservesFunctionCallOutputID` at `pkg/servers/api_test.go::22`.

Compatible output uses one `function_call` item per call with `call_id`, `name`, and string `arguments`. The response builder is at `pkg/servers/api.go::2742`.

Both streaming and non-streaming modes support simulated calls. Built-in local execution also applies to this endpoint through `runToolLoop` at `pkg/servers/api.go::2623`.

### Responses compaction

Endpoint: `POST /v1/responses/compact`

Compaction is a context summarization surface, not a tool execution surface. Do not copy tool-loop assumptions into compaction unless a target project explicitly defines tool semantics for that endpoint.

### Anthropic Complete and token counting

`/v1/complete` has no caller-defined tool request fields. `/v1/messages/count_tokens` includes raw tool definitions in the counted JSON but does not invoke tools. See `pkg/servers/api.go::878` and `pkg/servers/api.go::1015`.

## Streaming and buffering

Tool simulation requires the final JSON object, so text cannot be parsed safely chunk by chunk. A tool-call object may span arbitrary SignalR chunks. Therefore:

- Requests without tools stream text immediately.
- Requests with tools buffer model text until the M365 stream ends.
- Thinking content can still be emitted as provider-compatible reasoning before text parsing in ordinary simulated handlers.
- Once parsing completes, buffered text or tool calls are emitted in provider-native form.
- Built-in local-tool loops buffer the complete multi-round operation even if `stream: true`, then replay a provider-compatible buffered response.

This behavior is visible in OpenAI streaming at `pkg/servers/api.go::1240` and Anthropic streaming at `pkg/servers/api.go::1593`.

A port must document this latency and chunking difference. Setting `stream: true` does not guarantee immediate content when tool selection depends on full-response parsing.

## Caller-executed continuation

The caller-defined flow stops when the proxy emits a caller-owned call. The API client then:

1. Executes the declared function.
2. Resends the original user message and assistant call history.
3. Adds the matching tool result.
4. Uses the same session identifier.

OpenAI history:

```json
[
  {"role":"assistant","content":null,"tool_calls":[...]},
  {"role":"tool","tool_call_id":"call_id","content":"result"}
]
```

Anthropic history:

```json
[
  {"role":"assistant","content":[{"type":"tool_use","id":"call_id","name":"tool","input":{}}]},
  {"role":"user","content":[{"type":"tool_result","tool_use_id":"call_id","content":"result"}]}
]
```

`payload.Message.UnmarshalJSON` converts both histories into M365-readable text. It explicitly accepts `content: null` for OpenAI assistant call messages. Anthropic `tool_result.content` may be a string or an array containing text blocks.

## Local built-in execution loop

`runToolLoop` at `pkg/servers/api.go::606` owns one request-local loop:

1. Call M365 with the current upstream conversation ID.
2. Update the conversation ID from the completed SignalR invocation.
3. Parse provider-specific simulated output.
4. Return text if there is no accepted call.
5. Partition calls into local and caller-owned groups.
6. Immediately return caller-owned calls to the client.
7. Reject the request if the iteration limit would be exceeded.
8. Reject a repeated local call with the same name and exact argument bytes.
9. Decode arguments into an object.
10. Execute each local call with the HTTP request context.
11. JSON-serialize each structured result.
12. Append results as a user message.
13. Build and inject a new simulation prompt with `stream:false`.
14. Repeat.

### Mixed local and caller-owned calls

If a model emits both categories in one response, caller-owned calls are returned immediately. Local calls from that same model response are not executed. This avoids hidden local side effects while the caller is expected to act.

### Duplicate guard

The duplicate key is:

```text
tool_name + NUL + raw_argument_JSON
```

It applies across the complete request-local loop. This prevents a stuck model from repeating an identical side effect. Semantically equivalent JSON with different byte ordering is not deduplicated.

### Iteration limit

`M365_CODE_TOOL_MAX_ITERATIONS` defaults to 10. The guard fails the request with `coding tool iteration limit reached`. It is a hard resource and behavior boundary, not a suggestion to the model.

### Argument decoding failure

If parsed argument bytes cannot be decoded into an object, the loop currently executes the tool with an empty argument map. The tool's own required-field validation then returns a structured error. Ports may instead choose fail-closed decoding, but must preserve deterministic error feedback.

### Result feedback

Every local result is serialized as:

```json
{
  "tool": "read_file",
  "success": true,
  "output": "...",
  "error": "...",
  "exit_code": 0,
  "truncated": false,
  "timed_out": false
}
```

Optional fields use `omitempty`. Tool failures are model-visible results unless result serialization itself fails. This lets the model correct arguments or explain the failure.

## Built-in tool catalog

All schemas set `type: "object"`, reject additional properties, and mark required inputs explicitly. The source is `pkg/codingtools/codingtools.go::77`.

| Tool            | Inputs                                              | Operation                                                                       |
|-----------------|-----------------------------------------------------|---------------------------------------------------------------------------------|
| `list_files`    | optional `path`, optional `recursive` boolean       | Lists workspace-relative files, sorted lexically.                               |
| `read_file`     | required `path`                                     | Reads up to the configured byte limit.                                          |
| `write_file`    | required `path`, required `content`                 | Creates parents and writes a UTF-8 file with mode `0644`.                       |
| `search_files`  | required `query`, optional `path`                   | Performs literal, case-sensitive substring search with `path:line:text` output. |
| `shell_command` | required non-empty `command`                        | Executes through the platform shell with workspace as `cwd`.                    |
| `git_status`    | no inputs                                           | Runs `git status --short`.                                                      |
| `git_diff`      | optional `staged` boolean                           | Runs `git diff`, optionally with `--staged`.                                    |
| `git_log`       | optional `limit`, integer 1 through 100, default 10 | Runs `git log --oneline -N`.                                                    |
| `run_tests`     | optional non-empty `command`                        | Runs the supplied shell command or defaults to `go test ./...`.                 |
| `apply_patch`   | required non-empty `patch`                          | Validates patch paths, then runs `git apply --whitespace=error -`.              |

### Tool behavior details

#### `list_files`

- Defaults to `.`.
- Does not follow or return symlinks.
- Skips protected files and directories.
- Non-recursive mode returns direct child directories with a trailing slash and skips their contents.
- Applies the output-byte limit after sorting and joining results.

#### `read_file`

- Requires a workspace-relative path.
- Uses a limited reader of `MaxReadBytes + 1` to detect truncation.
- Returns the first `MaxReadBytes` and sets `truncated` when larger.
- The description says UTF-8, but no explicit UTF-8 validation is performed.

#### `write_file`

- Uses `MaxReadBytes` as the write-input limit.
- Validates the target and nearest existing parent before creating directories.
- Creates parent directories with `0755` and writes with `0644`.
- Replaces existing file contents.

#### `search_files`

- Uses literal `strings.Contains`, not regular expressions.
- Skips symlinks and protected paths.
- Silently skips unreadable files and files larger than `MaxReadBytes`.
- Loads each accepted file before scanning lines.
- Bounds the final combined output by `MaxOutput`.

#### Shell and test commands

- Use the canonical workspace as the working directory.
- Merge stdout and stderr into one bounded buffer.
- Return the process exit code when available.
- Set `timed_out` and a stable error when the context deadline expires.
- `shell_command` and custom `run_tests` are unrestricted shell command surfaces inside the operating-system permissions of the server process.

#### Git commands

`git_status`, `git_diff`, and `git_log` use direct argv execution rather than a shell. This avoids shell interpolation for these fixed operations.

#### `apply_patch`

- Rejects patch input larger than `MaxReadBytes`.
- Examines every `---` and `+++` header path.
- Allows `/dev/null` for file creation or deletion.
- Removes conventional `a/` or `b/` prefixes before safety validation.
- Rejects empty, absolute, traversing, protected, or symlink-escaping paths.
- Uses direct argv execution and sends patch bytes on stdin.
- Rejects whitespace errors through `git apply --whitespace=error`.

## Workspace safety model

The manager establishes one canonical workspace at startup:

1. Require a non-empty configured path.
2. Resolve it to an absolute path.
3. evaluate symlinks.
4. Require an existing directory.
5. Store the cleaned canonical path.

Every file path is workspace-relative. `resolve` at `pkg/codingtools/codingtools.go::159` rejects:

- Absolute paths.
- `..` traversal out of the workspace.
- Existing paths whose canonical target escapes through a symlink.
- New paths whose nearest existing parent escapes through a symlink.
- Protected credential paths.

Containment uses `filepath.Rel`, not string-prefix comparison, so sibling names sharing a prefix do not pass accidentally.

### Protected path policy

The following are denied directly:

- `.env`
- `data/.env`
- `data/tokens`
- anything below `data/tokens`

Any path component containing these case-insensitive substrings is denied:

- `credential`
- `token`
- `cookie`
- `secret`
- `private_key`
- `private-key`

A component exactly named `key`, or ending in `.key`, is also denied.

Listing and searching skip protected paths. Direct read, write, and patch access returns an error.

This is a project-specific denylist, not a general secret-detection system. A port must adapt protected paths to its credential layout and should prefer an allowlisted workspace that contains no secrets at all.

## Process safety and portability

### Unix

String commands run as `/bin/sh -c`. Each process starts in a new process group through `Setpgid`. Cancellation and explicit timeout termination send `SIGKILL` to the negative process ID, killing the full process group. See `pkg/codingtools/process_unix.go::12`.

### Windows

String commands run through:

```text
cmd.exe /d /s /c COMMAND
```

Timeout cleanup invokes:

```text
taskkill /T /F /PID PID
```

This targets the process tree. See `pkg/codingtools/process_windows.go::11`.

### Limits

- `context.WithTimeout` bounds command duration.
- A custom buffer bounds combined stdout and stderr while reporting truncation.
- Read, write, patch, and per-file search size use `MaxReadBytes`.
- Tool-loop rounds use `MaxIterations`.

These application limits do not replace OS-level isolation. For untrusted users, run the service under a dedicated low-privilege account and apply container, VM, seccomp, filesystem, network, CPU, memory, process-count, and disk quotas appropriate to the deployment.

## Configuration

Configuration is read from process environment after loading `data/.env`. Process environment values take precedence. See `pkg/models/models.go::117`.

| Variable                        |   Default | Meaning                                                              |
|---------------------------------|----------:|----------------------------------------------------------------------|
| `M365_ENABLE_CODE_TOOLS`        |   `false` | Enables local built-in execution and manager initialization.         |
| `M365_AUTO_EXPOSE_TOOLS`        |   `false` | Adds every built-in schema to effective tool lists.                  |
| `M365_WORKSPACE_DIR`            |       `.` | Existing workspace directory, canonicalized at startup.              |
| `M365_CODE_TOOL_TIMEOUT`        |     `30s` | Per-command Go duration. Must be positive.                           |
| `M365_CODE_TOOL_MAX_OUTPUT`     | `1048576` | Maximum combined command or listing output bytes.                    |
| `M365_CODE_TOOL_MAX_READ_BYTES` | `1048576` | Maximum file read, write input, patch input, and searched-file size. |
| `M365_CODE_TOOL_MAX_ITERATIONS` |      `10` | Maximum model and local-tool loop iterations.                        |

Boolean parsing accepts `true`, `1`, `yes`, or `on`, and their false equivalents, case-insensitively. Invalid or non-positive duration and integer settings fall back to defaults.

Keep application configuration in `data/.env`. Do not duplicate these variables into Compose configuration unless a deployment explicitly requires that ownership model.

## Container and runtime dependencies

The runtime image must contain:

- A POSIX shell for `shell_command` and custom `run_tests` on Linux.
- `git` for `git_status`, `git_diff`, `git_log`, and `apply_patch`.
- Go for the default `run_tests` command.
- CA certificates and timezone data for the base service.

The project Dockerfile installs these runtime dependencies. Removing `git` or Go keeps the API process alive but makes corresponding built-in tools fail at execution time.

A workspace must also be visible inside the runtime environment. This project does not add an implicit Compose workspace mount. Deployment owners must deliberately choose and mount the workspace when local execution is wanted, because a mount broadens the service's filesystem authority.

## Conversation and session continuity

M365 continuity is based on an upstream `ConversationId`, not replaying full history. The server maps a stable client session ID to that conversation ID through `ContextCache` at `pkg/servers/api.go::41`.

Relevant session inputs include:

- `model:session-id` suffix.
- `session_id` request field.
- `user` request field where supported.
- `previous_response_id` for Responses API.
- `X-Session-Id` header.
- A fallback hash derived from the API key and first user message.

The exact priority differs by endpoint. Tool continuations must use a stable session so the model sees the same M365 conversation. Local loops update the in-memory current conversation ID after every M365 call and cache the final value before responding.

## Error model

### Transport errors

M365 connection, payload, or stream errors abort the handler. Non-streaming endpoints return an HTTP error object. Streaming endpoints emit their provider-specific error form when possible.

### Simulation parse failure

No usable JSON payload is not treated as a transport error. The proxy returns raw text and a normal stop reason. This preserves useful model output while avoiding a phantom tool finish state.

### Invented tool name

Calls absent from the effective declaration allowlist are dropped. If all calls are dropped, the result becomes plain content or an empty normal completion according to the parsed payload.

### Local tool failure

Validation failures, command non-zero exits, timeouts, and output truncation are encoded in `codingtools.Result` and sent back to the model. The model can retry with changed arguments, but an identical name and argument payload triggers duplicate-call rejection.

### Local-loop control failure

Duplicate calls and iteration-limit exhaustion fail the HTTP request. They are not silently converted into a successful assistant answer.

### Maximum token handling

Provider handlers apply their normal output truncation after tool resolution. OpenAI uses `length`; Anthropic uses `max_tokens`; Responses marks output incomplete. Tool-call bodies are not token-truncated as assistant text.

## Security analysis and OWASP mapping

Enabling local tools turns an LLM output into a privileged action request. Treat every generated name, argument, path, patch, and command as hostile input.

### Broken Access Control

Risk: users can invoke tools against server files or share an overly broad workspace.

Controls:

- API-key middleware on protected API routes when keys are configured.
- Feature disabled by default.
- Explicit declaration unless auto-exposure is enabled.
- Canonical workspace boundary.
- Absolute path, traversal, and symlink-escape rejection.
- Protected credential-path denial.

Residual concern: when API keys are not configured, API routes accept all callers. Local tools should not be enabled on an untrusted network without authentication and tenant-level authorization.

### Injection

Risk: `shell_command` and custom `run_tests` deliberately execute model-generated shell text. This is command injection by design if exposed to an untrusted principal.

Controls:

- Explicit opt-in.
- Workspace `cwd`.
- Timeout and bounded output.
- Fixed direct argv for Git operations and patch application.

Residual concern: workspace confinement does not confine shell commands. Shell syntax can access absolute filesystem paths, network resources, environment variables, and external programs permitted to the service account. Strong isolation must be enforced outside the Go path resolver.

### Path Traversal

Risk: file tools or patch headers escape the workspace using `..`, absolute paths, or symlinks.

Controls:

- `filepath.IsAbs` rejection.
- cleaned traversal rejection.
- canonical containment checks.
- nearest-existing-parent validation for creation.
- patch-header validation.
- no symlink traversal in listing and search.

### Security Misconfiguration

Risk: enabling auto-exposure, selecting `.` as a broad runtime workspace, mounting host source and credentials together, or running as root expands authority.

Controls and deployment requirements:

- Safe defaults keep execution and auto-exposure off.
- Startup rejects missing workspaces and invalid limits.
- Deployment must use a narrow mount and low-privilege identity.
- Secrets must remain outside the tool workspace.

### Identification and Authentication Failures

Risk: local execution is reachable without effective API authentication.

Control: configure `M365_API_KEYS` or `M365_API_KEY` and isolate tenants. A production port should bind tool permission to an authenticated principal rather than only to process-wide configuration.

### Cryptographic Failures and Sensitive Data Exposure

Risk: tools or command output disclose tokens, cookies, keys, environment files, or secrets to the model and API caller.

Controls:

- Protected path denylist.
- Output limits.
- Runtime credentials stored under `data/` rather than source files.

Residual concern: shell commands bypass path helpers and may print environment variables or secret files. Add environment minimization and output secret redaction for deployments with untrusted prompts.

### Server-Side Request Forgery

Risk: shell commands or installed network clients can reach internal services.

Current in-process path controls do not prevent this. Apply network egress policy, DNS restrictions, metadata-service blocking, and sandbox isolation.

### Software and Data Integrity Failures

Risk: `write_file`, `apply_patch`, and shell commands modify code or configuration; model-generated patches may be malicious.

Controls:

- Workspace boundary.
- patch path validation.
- `git apply --whitespace=error`.
- structured result reporting.

Recommended additions for high-trust workflows include an approval gate, signed change provenance, clean-tree checks, restricted branches, and post-change verification.

### Security Logging and Monitoring Failures

Risk: local side effects occur without auditable attribution.

The current result object captures tool name, success, errors, timeout, exit code, and truncation, but a production port should add structured audit records with authenticated principal, request ID, normalized arguments with secret redaction, duration, and outcome. Never log access tokens, cookies, authorization headers, or complete secret-bearing command output.

### Resource Exhaustion

Risk: recursive scans, large files, commands, process trees, or repeated tool loops consume resources.

Controls:

- File-size, output-size, timeout, and iteration limits.
- process-tree termination.
- 128-candidate parser bound.
- duplicate-call guard.

Residual concern: directory entry count, total scanned bytes, CPU, memory, disk writes, and process count are not comprehensively bounded in process. Add OS or container quotas and consider scan entry and aggregate-byte limits.

## Testing and runtime verification

### Caller-defined protocol scripts

`test_toolcalling_openai.sh` checks:

- `write_file`, `read_file`, and `list_files` or a caller-declared `bash` alternative.
- Correct `finish_reason` and `tool_calls` shape.
- A second request containing assistant call history and a fake tool result.
- A final text response after the result.
- M365 server-side search as a baseline.

`test_toolcalling_anthropic.sh` checks equivalent behavior with `tool_use`, `tool_result`, and `stop_reason`.

These scripts permit a skip when the probabilistic model answers directly instead of selecting a tool. They are integration probes, not deterministic parser unit tests.

### Built-in runtime verification

A complete local-tool verification should exercise the running API and observe real filesystem or process effects inside a disposable workspace:

1. Start with local execution disabled and confirm declared built-in calls are returned, not executed.
2. Enable execution with auto-exposure off and explicitly declare one harmless tool.
3. Invoke `write_file`, confirm the file exists in the workspace, then invoke `read_file` through a new request using the same session.
4. Confirm traversal, absolute paths, symlink escapes, `.env`, `data/tokens`, and key-like paths are rejected.
5. Run a command that exceeds output limits and confirm `truncated:true`.
6. Run a command that exceeds the timeout and confirm its child process tree is terminated.
7. Apply a valid patch and reject a patch that targets outside the workspace.
8. Trigger a non-zero test command and verify exit code and output reach the model.
9. Trigger an identical repeated call and verify duplicate rejection.
10. Trigger the iteration ceiling and verify request failure.
11. Repeat OpenAI Chat, Anthropic Messages, and Responses in streaming and non-streaming modes.
12. Confirm caller-owned calls are returned rather than executed.

Use a configured, running server and disposable data. Never place credentials, tokens, cookies, or production workspaces in test fixtures.

### Documentation-only verification

For changes limited to this document, there is no runtime surface. Validate links and source references, run `git diff --check`, and compare enumerated tools, environment variables, endpoints, parser entry points, and security controls against production source.

## Known limitations

1. Tool selection is probabilistic because it is produced through prompting, not a native M365 function-call protocol.
2. Streaming tool requests buffer text until a complete JSON object is available.
3. OpenAI Completions uses a non-standard tool-call extension and does not execute built-in tools locally.
4. Tool-schema compliance is encouraged in the prompt, but generated arguments are not validated against the declared caller schema before returning them.
5. Built-in argument validation is implemented by tool handlers, not a general JSON Schema validator.
6. The package-level generated call counter is mutable process-global state and is not synchronized. A port should use UUIDs, atomic counters, or request-local ID generation.
7. Name allowlisting prevents undeclared call names, but does not prove argument safety or authorization.
8. `shell_command` and custom `run_tests` can leave the workspace and access anything permitted by the process account.
9. Protected-path matching is denylist-based and project-specific.
10. Search silently skips unreadable and oversized files.
11. `read_file` does not validate UTF-8 despite its schema description.
12. Duplicate detection compares raw JSON bytes, not canonical JSON.
13. Mixed local and caller-owned output returns caller-owned calls and does not execute local calls from that response.
14. Simulation parse failure degrades to text rather than retrying automatically.
15. The integration scripts use probabilistic skip semantics and do not prove every path on every run.
16. Local tools are process-wide configuration, not per-API-key authorization.
17. In-process limits do not provide CPU, memory, disk, network, or syscall isolation.
18. Provider-neutral name normalization is mandatory in allowlist plumbing. OpenAI-only extraction silently disables Anthropic call-name filtering.
19. Responses continuation preserves `function_call_output.call_id` only through formatted plain text because M365 receives the latest message text rather than a native Responses item.

## Rejected and superseded approaches

### Forwarding arbitrary schemas directly to M365

Rejected because the M365 chat payload has no native caller-defined tool contract. The backend would ignore or independently reinterpret such definitions.

### Treating backend tools as caller-defined tools

Rejected because M365 owns their execution and may emit names the caller never declared. Simulated handlers discard backend calls and enforce a declaration allowlist.

### Relying on prompt instructions alone

Rejected because M365 code-interpreter option sets can intercept file and code requests before simulation succeeds. Option-set stripping is required alongside prompting.

### Parsing only fenced code blocks

Superseded by whole-text, fenced-block, and balanced-segment candidate enumeration with shape scoring. Models can omit fences, add prose, or echo request JSON.

### Selecting the first valid JSON object

Rejected because the embedded request is itself valid JSON and may be echoed. Request-like candidates are penalized and response shape is positively scored.

### One provider format for all clients

Rejected at the wire boundary. Anthropic uses native `tool_use`, `tool_result`, stop reasons, and SSE events. Provider-specific adapters share internal calls without exposing an OpenAI wrapper to Anthropic clients.

### Incremental parsing of simulated streaming output

Rejected because JSON and call argument strings can span arbitrary chunks. Buffering is required for deterministic final parsing.

### Global tool-calling mode switches

Removed. Caller-defined simulation now activates whenever effective tools are present. Only dangerous local execution remains explicitly gated.

### Shared mutable request state on the M365 client

Removed in favor of request-local `StreamChunk` state. This preserves concurrent request safety and avoids one request reading another request's calls or conversation ID.

### Sending full `messageHistory` upstream

Rejected because M365 tracks history through `ConversationId`. Only the latest turn is sent, with system instructions merged into it.

### Copilot Studio agent provisioning

Not used. The implementation remains a stateless HTTP adaptation based on simulated provider output and M365 option control.

### Automatically mounting a host workspace

Not used. Filesystem authority is a deployment decision and must be explicit. The default Compose setup should not silently expose host source or credentials.

## Porting guide

### Minimal interfaces

A portable implementation needs these boundaries:

```text
ProviderAdapter
  decodeRequest(body) -> messages, tools, choice, stream, session
  buildSimulationPrompt(fullRequest, effectiveTools, choice)
  parseSimulation(text, allowedNames) -> content, calls, finish
  encodeResponse(content, calls, finish, stream)

UpstreamChat
  complete(messages, conversationID, hasTools)
    -> text, thinking, backendCalls, finish, finalConversationID

ToolRegistry
  schemas(provider) -> tool definitions
  isLocal(name) -> bool
  execute(context, name, arguments) -> structured result

ConversationStore
  get(sessionID) -> conversationID
  set(sessionID, conversationID)
```

Keep provider serialization, upstream transport, local execution, and session storage separate. This prevents OpenAI-specific assumptions from contaminating Anthropic output and keeps dangerous execution out of parser code.

### Recommended implementation order

1. Implement request-local upstream chat results with text, calls, finish reason, and conversation ID.
2. Implement provider-neutral internal `ToolCall` and provider-specific definition decoding.
3. Implement OpenAI prompt construction and candidate extraction.
4. Add request-like penalties, response-shape scoring, and name allowlisting.
5. Implement OpenAI non-streaming output and caller tool-result history conversion.
6. Add buffered OpenAI streaming output.
7. Implement Anthropic-native prompt, parser, history conversion, and SSE.
8. Add Responses input and output conversion if required.
9. Strip upstream tool features that conflict with caller ownership.
10. Add session-to-conversation continuity.
11. Add a local registry behind a disabled-by-default feature gate.
12. Implement canonical workspace and protected-path controls before any file operation.
13. Add bounded command execution and process-tree termination before shell exposure.
14. Add duplicate and iteration guards before enabling multi-round execution.
15. Add runtime dependencies and a deliberately scoped workspace mount.
16. Verify all provider surfaces through the running application.
17. Add structured security audit logging and deployment isolation.

### Porting invariants

A correct port should preserve these invariants:

- No caller-defined call name may survive unless it was declared in the effective request.
- No local operation may execute unless local tools are enabled and that name is classified local.
- No backend-invented call may be represented as a caller-declared call.
- No file helper may escape its canonical workspace through absolute paths, traversal, or symlinks.
- No command may run without a deadline and bounded captured output.
- No local loop may run without duplicate and iteration guards.
- No tool-bearing simulated stream may emit unparsed partial JSON as normal content.
- No provider may receive another provider's wire format.
- No conversation continuation may lose the latest upstream conversation ID.
- No tool failure may be silently reported as success.

## Migration checklist

### Protocol

- [ ] Inventory every API endpoint that accepts tools.
- [ ] Define OpenAI and Anthropic tool schemas separately.
- [ ] Preserve the complete original request for prompt construction.
- [ ] Normalize `tool_choice` without losing provider-specific semantics.
- [ ] Convert caller result history into upstream-readable content.
- [ ] Define finish and stop reason mappings.
- [ ] Decide which legacy endpoints receive non-standard extensions.

### Parser

- [ ] Enumerate whole, fenced, and balanced JSON candidates.
- [ ] Bound candidate count and parser work.
- [ ] Penalize echoed requests.
- [ ] Score provider response shapes.
- [ ] Normalize arguments and missing IDs.
- [ ] Enforce the declared-name allowlist after parsing.
- [ ] Reset finish reasons when no call blocks survive.
- [ ] Add deterministic fixtures for prose, malformed JSON, echoed requests, invented names, missing IDs, object arguments, and multiple candidates.

### Streaming

- [ ] Stream directly when no tools are present.
- [ ] Buffer tool-bearing model text until parsing completes.
- [ ] Emit provider-native call events and final reasons.
- [ ] Preserve reasoning behavior intentionally.
- [ ] Document buffered latency for `stream:true`.

### Local execution

- [ ] Keep local execution disabled by default.
- [ ] Separate explicit declaration from auto-exposure.
- [ ] Distinguish local and caller-owned calls.
- [ ] Define mixed-call behavior.
- [ ] Add structured results.
- [ ] Add duplicate and iteration guards.
- [ ] Propagate request cancellation.

### Filesystem and process security

- [ ] Canonicalize an existing workspace at startup.
- [ ] Reject absolute paths and traversal.
- [ ] Validate symlinks and nearest existing parents.
- [ ] Validate every patch path.
- [ ] Keep secrets outside the workspace and adapt protected paths.
- [ ] Do not follow symlinks during scans.
- [ ] Bound read, write, patch, scan, and output sizes.
- [ ] Kill complete process trees on timeout.
- [ ] Run under a low-privilege identity.
- [ ] Apply network and resource isolation.
- [ ] Add approval for destructive or externally visible operations where required.

### Operations

- [ ] Install every runtime binary required by fixed and default commands.
- [ ] Mount only the intended workspace.
- [ ] Configure API authentication before local execution.
- [ ] Remove secrets from the child-process environment.
- [ ] Redact secrets from logs and model-visible command output.
- [ ] Audit principal, request, tool, normalized arguments, duration, and result.
- [ ] Monitor timeouts, repeated calls, rejected paths, and output truncation.

### Verification

- [ ] Verify caller-executed OpenAI continuation.
- [ ] Verify caller-executed Anthropic continuation.
- [ ] Verify Responses `function_call_output` continuation.
- [ ] Verify streaming and non-streaming shapes.
- [ ] Verify local execution through the real API surface.
- [ ] Verify disabled mode and explicit-declaration mode.
- [ ] Verify traversal, symlink, protected-path, timeout, output, duplicate, and iteration controls.
- [ ] Verify caller-owned calls are never executed locally.
- [ ] Verify backend-invented names are never leaked as declared calls.
- [ ] Verify conversation continuity over multiple tool turns.

## Function and file reference

| Symbol or area                                      | Reference                                |
|-----------------------------------------------------|------------------------------------------|
| `ToolDef`, `ToolCall`, name and history formatters  | `pkg/toolcalling/toolcalling.go::19`     |
| OpenAI prompt                                       | `pkg/toolcalling/simulated.go::21`       |
| Anthropic prompt                                    | `pkg/toolcalling/simulated.go::62`       |
| OpenAI parser, `ParseSimulatedResponse`             | `pkg/toolcalling/simulated.go::110`      |
| Anthropic parser, `ParseSimulatedResponseAnthropic` | `pkg/toolcalling/simulated.go::161`      |
| Anthropic payload extraction                        | `pkg/toolcalling/simulated.go::202`      |
| OpenAI payload extraction                           | `pkg/toolcalling/simulated.go::321`      |
| Argument normalization                              | `pkg/toolcalling/simulated.go::408`      |
| OpenAI candidate scoring                            | `pkg/toolcalling/simulated.go::461`      |
| Request-echo detection                              | `pkg/toolcalling/simulated.go::506`      |
| JSON candidate enumeration                          | `pkg/toolcalling/simulated.go::532`      |
| Built-in manager initialization                     | `pkg/servers/api.go::150`                |
| Built-in schema preparation, `prepareCodingTools`   | `pkg/servers/api.go::557`                |
| Local multi-round loop                              | `pkg/servers/api.go::606`                |
| Chat Completions handler                            | `pkg/servers/api.go::679`                |
| Completions handler                                 | `pkg/servers/api.go::782`                |
| Anthropic Messages handler                          | `pkg/servers/api.go::920`                |
| OpenAI chat streaming                               | `pkg/servers/api.go::1217`               |
| OpenAI chat non-streaming                           | `pkg/servers/api.go::1439`               |
| Anthropic Messages streaming                        | `pkg/servers/api.go::1551`               |
| Anthropic Messages non-streaming                    | `pkg/servers/api.go::1839`               |
| OpenAI Completions streaming                        | `pkg/servers/api.go::1943`               |
| OpenAI Completions non-streaming                    | `pkg/servers/api.go::2107`               |
| Prompt injection helpers                            | `pkg/servers/api.go::2366`               |
| Tool-choice normalization                           | `pkg/servers/api.go::2399`               |
| Responses request and handler                       | `pkg/servers/api.go::2527`               |
| Responses input conversion                          | `pkg/servers/api.go::2639`               |
| Responses output construction                       | `pkg/servers/api.go::2742`               |
| M365 transport call types                           | `pkg/client/client.go::45`               |
| Request-local stream result                         | `pkg/client/client.go::248`              |
| SignalR extraction loop                             | `pkg/client/client.go::295`              |
| M365 message tool extraction                        | `pkg/client/client.go::552`              |
| Tool history decoding                               | `pkg/payload/payload.go::115`            |
| M365 option-set filtering                           | `pkg/payload/payload.go::552`            |
| M365 tool-message map                               | `pkg/models/models.go::93`               |
| Local-tool environment config                       | `pkg/models/models.go::101`              |
| Built-in schemas and dispatch                       | `pkg/codingtools/codingtools.go::77`     |
| Path resolution and policy                          | `pkg/codingtools/codingtools.go::159`    |
| File operations                                     | `pkg/codingtools/codingtools.go::221`    |
| Patch validation                                    | `pkg/codingtools/codingtools.go::355`    |
| Command execution and bounds                        | `pkg/codingtools/codingtools.go::386`    |
| Unix process-tree handling                          | `pkg/codingtools/process_unix.go::12`    |
| Windows process-tree handling                       | `pkg/codingtools/process_windows.go::11` |
| OpenAI protocol script                              | `test_toolcalling_openai.sh::1`          |
| Anthropic protocol script                           | `test_toolcalling_anthropic.sh::1`       |
| Runtime image packages                              | `Dockerfile::16`                         |
