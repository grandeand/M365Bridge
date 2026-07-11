# M365Bridge

[![CI](https://github.com/KilimcininKorOglu/M365Bridge/actions/workflows/ci.yml/badge.svg)](https://github.com/KilimcininKorOglu/M365Bridge/actions/workflows/ci.yml)
[![Release](https://github.com/KilimcininKorOglu/M365Bridge/actions/workflows/release.yml/badge.svg)](https://github.com/KilimcininKorOglu/M365Bridge/actions/workflows/release.yml)
[![Version](https://img.shields.io/github/v/release/KilimcininKorOglu/M365Bridge)](https://github.com/KilimcininKorOglu/M365Bridge/releases)
[![Docker](https://img.shields.io/badge/docker-ghcr.io-blue)](https://github.com/KilimcininKorOglu/M365Bridge/pkgs/container/m365bridge)

**English** | **[Türkçe](README.tr.md)**

A Go implementation that converts Microsoft 365 Copilot's WebSocket interface to OpenAI/Anthropic compatible HTTP API.

## Architecture

Your App -> M365Bridge -> substrate.office.com (SignalR) -> M365 Copilot Backend

## Prerequisites

- **Go 1.22+** installed ([download](https://go.dev/dl/))
- **git** for cloning this repository
- A **Microsoft 365 Copilot license** (business or enterprise account with Copilot access) tested a copilot chat (basic) account
- A browser logged into [https://m365.cloud.microsoft](https://m365.cloud.microsoft) (for setup wizard token extraction)

## Features

- Text chat with streaming/non-streaming output
- Multimodal image input (OpenAI `image_url` and Anthropic `image` content blocks; PNG, JPEG, GIF, WebP)
- Image generation via  (`/v1/images/generations`, `/v1/images/edits`) with `url` and `b64_json` response formats
- Multi-turn conversation support via ConversationId tracking
- Session isolation (per-session M365 conversations)
- Thinking/reasoning content extraction (`reasoning_content` for OpenAI, `thinking` blocks for Anthropic)
- Simulated tool calling (client-defined tools work on both OpenAI and Anthropic endpoints, streaming and non-streaming)
- OpenAI-compatible API endpoints
- Anthropic-compatible API endpoints (dedicated SSE handlers)
- API key authentication (`M365_API_KEYS` / `M365_API_KEY`)
- max_tokens enforcement across all endpoints (tiktoken BPE)
- CLI interface for interactive use
- Single binary with subcommand routing

## Installation

```bash
git clone https://github.com/KilimcininKorOglu/M365Bridge
cd M365Bridge
go mod download
go build -o bin/m365-bridge ./cmd/cli
```

### Pre-built Binaries

Download the latest binary for your platform from [GitHub Releases](https://github.com/KilimcininKorOglu/M365Bridge/releases):

| Platform                    | File                            |
|-----------------------------|---------------------------------|
| Linux amd64                 | `m365-bridge-linux-amd64`       |
| Linux arm64                 | `m365-bridge-linux-arm64`       |
| macOS amd64 (Intel)         | `m365-bridge-darwin-amd64`      |
| macOS arm64 (Apple Silicon) | `m365-bridge-darwin-arm64`      |
| Windows amd64               | `m365-bridge-windows-amd64.exe` |
| Windows arm64               | `m365-bridge-windows-arm64.exe` |

```bash
# Example: Linux amd64
wget https://github.com/KilimcininKorOglu/M365Bridge/releases/latest/download/m365-bridge-linux-amd64
chmod +x m365-bridge-linux-amd64
./m365-bridge-linux-amd64 serve --port 8000
```

### Docker

The easiest way to run M365Bridge is with Docker. The pre-built image is available on GitHub Container Registry.

#### Step 1: Create docker-compose.yml

Create a `docker-compose.yml` file in your project directory:

```yaml
services:
  m365bridge:
    image: ghcr.io/kilimcininkoroglu/m365bridge:latest
    container_name: m365bridge
    ports:
      - "8230:8000"
    volumes:
      - ./data:/app/data
    restart: unless-stopped
```

#### Step 2: Start the container

```bash
docker compose up -d
```

The API will be available at `http://localhost:8230`.

#### Step 3: Get your authentication token from the browser

The server needs a refresh token from your Microsoft 365 Copilot session. Extract it as follows:

1. Open [https://m365.cloud.microsoft](https://m365.cloud.microsoft) in your browser and log in
2. Press **F12** to open DevTools, go to **Console**
3. Paste and run the following JavaScript code:

<details>
<summary>Click to expand the JavaScript extraction snippet</summary>

```javascript
(async () => {
// 1. Get oid/tenant
let oid, tenant;
for (const key of Object.keys(localStorage)) {
  if (!key.includes('active-account-filters')) continue;
  try {
    const val = JSON.parse(localStorage.getItem(key));
    if (val?.homeAccountId?.includes('.')) { [oid, tenant] = val.homeAccountId.split('.'); break; }
  } catch(e) {}
}
if (!oid) {
  const mk = Object.keys(localStorage).find(k => k.startsWith('msal.') && k.includes('|'));
  if (mk) { const p = mk.split('|')[1]; if (p?.includes('.')) [oid, tenant] = p.split('.'); }
}
if (!oid || !tenant) return 'ERROR: No MSAL account found. Make sure you are logged in.';

// 2. Install fetch interceptor to capture token response for the target client ID
const targetClientID = '4765445b-32c6-49b0-83e6-1d93765276ca';
const origFetch = window.fetch;
let captured = false;
window.fetch = async function(...args) {
  const resp = await origFetch.apply(this, args);
  const url = typeof args[0] === 'string' ? args[0] : args[0]?.url || '';
  if (url.includes('oauth2/v2.0/token') && !captured) {
    try {
      // Verify this request is for our target client ID
      let bodyStr = '';
      const init = args[1];
      if (typeof init?.body === 'string') {
        bodyStr = init.body;
      } else if (init?.body instanceof URLSearchParams) {
        bodyStr = init.body.toString();
      } else if (init?.body instanceof ArrayBuffer || ArrayBuffer.isView(init?.body)) {
        bodyStr = new TextDecoder().decode(init.body);
      } else if (args[0] instanceof Request) {
        bodyStr = await args[0].clone().text();
      }
      const isTarget = new URLSearchParams(bodyStr).get('client_id') === targetClientID;
      if (isTarget) {
        const clone = resp.clone();
        const data = await clone.json();
        if (data.refresh_token) {
          captured = true;
          const result = {oid, tenant, refresh_token: data.refresh_token};
          try {
            if (window.cookieStore) {
              const cookies = await cookieStore.getAll();
              const sso = cookies.filter(c => c.name === 'ESTSAUTH' || c.name === 'ESTSAUTHPERSISTENT');
              if (sso.length > 0) result.sso_cookies = sso.map(c => ({name: c.name, value: c.value}));
            }
          } catch(e) {}
          console.log('===== COPY THE COMPLETE JSON BELOW =====');
          console.log(JSON.stringify(result, null, 2));
        }
      }
    } catch(e) {}
  }
  return resp;
};

// 3. Find MSAL instance and force token refresh
let msal = null;
const checked = new WeakSet();
function findMsal(obj, depth) {
  if (!obj || depth > 3 || typeof obj !== 'object' || checked.has(obj)) return null;
  checked.add(obj);
  try {
    if (typeof obj.acquireTokenSilent === 'function' && typeof obj.getAllAccounts === 'function') return obj;
    if (depth < 3) for (const k of Object.keys(obj)) {
      try { const r = findMsal(obj[k], depth + 1); if (r) return r; } catch(e) {}
    }
  } catch(e) {}
  return null;
}
for (const k of Object.getOwnPropertyNames(window)) {
  try { msal = findMsal(window[k], 0); if (msal) break; } catch(e) {}
}

if (msal) {
  const accounts = msal.getAllAccounts();
  if (accounts.length > 0) {
    try {
      await msal.acquireTokenSilent({
        account: accounts[0],
        scopes: ['https://substrate.office.com/.default'],
        forceRefresh: true
      });
    } catch(e) {}
  }
  return 'Token refresh triggered. Copy the JSON output above.';
}
return 'Interceptor installed but MSAL instance not found. Navigate within m365.cloud.microsoft to trigger a token refresh, then copy the JSON output.';
})()
```

</details>

4. The console will output: `===== COPY THE COMPLETE JSON BELOW =====`
5. Copy the JSON output. It will look like this:

```json
{
  "oid": "your-oid",
  "tenant": "your-tenant",
  "refresh_token": "your-refresh-token",
  "sso_cookies": [
    {"name": "ESTSAUTH", "value": "..."},
    {"name": "ESTSAUTHPERSISTENT", "value": "..."}
  ]
}
```

> **Note:** SSO cookies are captured automatically if the `cookieStore` API is available (Chrome, Edge). If `sso_cookies` is missing from the output, see Step 4 below.

#### Step 4 (Optional): Get SSO cookies manually

If the script above did not capture SSO cookies automatically (e.g. Firefox, or third-party cookie restrictions), capture them manually:

Microsoft SPA refresh tokens expire after **24 hours**. Without SSO cookies, you must repeat Step 3 every 24 hours. SSO cookies enable automatic renewal and last weeks/months.

To capture SSO cookies:

1. Open [https://login.microsoftonline.com](https://login.microsoftonline.com) in your browser (this is where the cookies live, not m365.cloud.microsoft)
2. Press **F12** to open DevTools, go to **Application** > **Cookies** > `https://login.microsoftonline.com`
3. Find and copy the values of these two cookies:
   - `ESTSAUTH`
   - `ESTSAUTHPERSISTENT`

#### Step 5: Create setup.json

Create a file at `data/setup.json` with the JSON from Step 3. If you captured SSO cookies manually in Step 4, add them to the `sso_cookies` array:

**Without SSO cookies (must re-run setup every 24 hours):**

```json
{"oid":"your-oid","tenant":"your-tenant","refresh_token":"your-refresh-token"}
```

**With SSO cookies (automatic renewal, recommended):**

```json
{
  "oid": "your-oid",
  "tenant": "your-tenant",
  "refresh_token": "your-refresh-token",
  "sso_cookies": [
    {"name": "ESTSAUTH", "value": "paste-estsauth-value-here"},
    {"name": "ESTSAUTHPERSISTENT", "value": "paste-estsauthpersistent-value-here"}
  ]
}
```

#### Step 6: Run the setup wizard

Run the setup wizard inside the container to encrypt and save your credentials:

```bash
docker exec -it m365bridge ./bin/m365-bridge setup-wizard
```


The wizard will:
- Read `data/setup.json`
- Encrypt the refresh token and SSO cookies with AES-256-GCM
- Save environment variables to `data/.env`
- Verify the token by exchanging it for an access token

On success, the server is ready. The API is available at `http://localhost:8230`.

> **Note:** If you did not capture SSO cookies, the refresh token will expire after 24 hours and the server will stop working. Re-run Steps 3, 5, and 6 to get a new token. With SSO cookies, the server automatically renews tokens when they expire.

#### Alternative: docker run

If you prefer `docker run` instead of Docker Compose:

```bash
docker run -d \
  --name m365bridge \
  -p 8230:8000 \
  -v $(pwd)/data:/app/data \
  --restart unless-stopped \
  ghcr.io/kilimcininkoroglu/m365bridge:latest
```

Then follow Steps 3-6 above.

#### Notes

- The `data/` directory stores tokens, cache, and configuration. It is created automatically on first run.
- Port `8230` (host) maps to port `8000` (container). Change the host port in `docker-compose.yml` or the `-p` flag if needed.
- The container starts with `serve --port 8000` by default.
- To build the image from source instead of using the pre-built one: `docker compose up --build -d`

## Usage

### CLI Flags

| Flag            | Type   | Default | Description                                                              |
|-----------------|--------|---------|--------------------------------------------------------------------------|
| `-i`            | bool   | false   | Interactive mode (multi-turn conversation)                               |
| `--model`       | string | `auto`  | Model to use: `auto`, `quick`, `reasoning`, `gpt5.5`, `gpt5.5-reasoning` |
| `--reasoning`   | bool   | false   | Use reasoning mode                                                       |
| `--no-stream`   | bool   | false   | Disable streaming, print full response at once                           |
| `--list-models` | bool   | false   | List all available models and exit                                       |
| `--version`     | bool   | false   | Show version and exit                                                    |

Positional argument (if no flag consumes it): the query text for single query mode.

### Subcommand: serve

Starts the HTTP API server.

| Flag        | Type | Default | Description           |
|-------------|------|---------|-----------------------|
| `--port`    | int  | 8000    | Port to listen on     |
| `--version` | bool | false   | Show version and exit |

### Subcommand: setup-wizard

Runs the browser-based setup wizard. Reads JSON from file containing `oid`, `tenant`, and `refresh_token`.

| Flag     | Type   | Default           | Description             |
|----------|--------|-------------------|-------------------------|
| `--file` | string | `data/setup.json` | Path to setup JSON file |

### Examples

```bash
# Single query
./bin/m365-bridge "your question"

# Interactive mode
./bin/m365-bridge -i

# Specify model with reasoning
./bin/m365-bridge --model gpt5.5-reasoning "your question"

# Non-streaming
./bin/m365-bridge --no-stream "your question"

# List models
./bin/m365-bridge --list-models

# Start API server
./bin/m365-bridge serve --port 8000

# Run setup wizard with custom file
./bin/m365-bridge setup-wizard --file /path/to/setup.json
```

### API Server

```bash
# Start API server on port 8000
./bin/m365-bridge serve --port 8000

# Test with curl (no auth)
curl http://127.0.0.1:8000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"auto","messages":[{"role":"user","content":"Hello"}]}'

# Test with curl (with API key)
curl http://127.0.0.1:8000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer your-api-key" \
  -d '{"model":"gpt5.5","messages":[{"role":"user","content":"Hello"}]}'

# Streaming with session isolation
curl http://127.0.0.1:8000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer your-api-key" \
  -H "X-Session-Id: my-session-1" \
  -d '{"model":"gpt5.5","stream":true,"messages":[{"role":"user","content":"Hello"}]}'
```

### First Run

When you start the server for the first time:

1. The server reads `data/.env` from the current working directory
2. It loads the encrypted refresh token from `data/tokens/rt_90day.txt`
3. It performs a token refresh (exchanges refresh token for an access token). This takes 1-2 seconds
4. On success, you will see: `Starting API server on port 8000`
5. The first request may take slightly longer as it opens a WebSocket connection to `substrate.office.com`

If the refresh token is missing or expired, the server will attempt SSO cookie re-authentication if `data/tokens/sso_cookies.json` exists. If SSO cookies are also missing or expired, the server will fail to start with a token refresh error. Re-run `./bin/m365-bridge setup-wizard` to extract fresh tokens and cookies.

### Session Isolation

Each session maps to a unique M365 conversation. Session ID is resolved in priority order:

1. `session_id` field in request body
2. `user` field in request body
3. `X-Session-Id` header
4. `hash(api_key + first_user_message)` (when auth is on) or `hash(first_user_message)` (when auth is off)

The hash fallback allows standard OpenAI clients (like Claude Code) that cannot send custom headers to have separate conversations automatically, as long as their first user message differs.

### Python Client (OpenAI SDK)

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://127.0.0.1:8000/v1",
    api_key="your-api-key",  # required if M365_API_KEYS is set
)
resp = client.chat.completions.create(
    model="gpt5.5",
    messages=[{"role": "user", "content": "Hello"}]
)
print(resp.choices[0].message.content)
```

### Python Client (Anthropic SDK)

```python
from anthropic import Anthropic

client = Anthropic(
    base_url="http://127.0.0.1:8000/v1",
    api_key="your-api-key",  # required if M365_API_KEYS is set
)
resp = client.messages.create(
    model="gpt5.5",
    max_tokens=1024,
    messages=[{"role": "user", "content": "Hello"}]
)
print(resp.content[0].text)
```

### Image Input Example

```python
from openai import OpenAI
import base64

client = OpenAI(
    base_url="http://127.0.0.1:8000/v1",
    api_key="your-api-key",
)

with open("image.png", "rb") as f:
    img_b64 = base64.b64encode(f.read()).decode()

resp = client.chat.completions.create(
    model="gpt5.5",
    messages=[{
        "role": "user",
        "content": [
            {"type": "text", "text": "What is in this image?"},
            {"type": "image_url", "image_url": {"url": f"data:image/png;base64,{img_b64}"}},
        ],
    }],
)
print(resp.choices[0].message.content)
```

## API Endpoints

| Endpoint                      | Description                                         |
|-------------------------------|-----------------------------------------------------|
| `POST /v1/chat/completions`   | OpenAI Chat Completions (streaming + non-streaming) |
| `POST /v1/completions`        | OpenAI text completion (streaming + non-streaming)  |
| `POST /v1/responses`          | OpenAI Responses API (streaming + non-streaming)    |
| `POST /v1/responses/compact`  | OpenAI Responses Compact API (Codex remote compaction) |
| `POST /v1/messages`           | Anthropic Messages format (dedicated SSE handlers)  |
| `POST /v1/messages/count_tokens` | Anthropic input token counting                   |
| `POST /v1/complete`           | Anthropic Complete (FIM)                            |
| `POST /v1/images/generations` | OpenAI Images API: generate from text (JSON body)   |
| `POST /v1/images/edits`       | OpenAI Images API: edit existing image (multipart)  |
| `GET /v1/conversations`       | List M365 conversations (requires M365 web cookies) |
| `POST /v1/conversations`      | Create a conversation with an initial message       |
| `PATCH /v1/conversations/{id}` | Rename a conversation with `{ "name": "..." }`  |
| `DELETE /v1/conversations/{id}` | Permanently delete a conversation                  |
| `GET /v1/models`              | Model list                                          |
| `GET /health`                 | Health check (no auth required)                     |

## Models

All model selection is via the `tone` field sent to the M365 backend. The `Override` field is empty for all models. GPT-5.x models route to the GPT-5 backend; Claude models route to real Anthropic Claude models (verified via tone test).

| Key                        | Tone              | OpenAI ID         | Thinking? | Backend |
|----------------------------|-------------------|-------------------|-----------|---------|
| `auto`                     | Magic             | gpt-4-auto        | No        | GPT-5   |
| `quick`                    | Chat              | gpt-4-quick       | No        | GPT-5   |
| `reasoning`                | Magic             | gpt-4-reasoning   | No        | GPT-5   |
| `gpt5.5`                   | Gpt_5_5_Chat      | gpt-5.5           | No        | GPT-5   |
| `gpt5.5-reasoning`         | Gpt_5_5_Reasoning | gpt-5.5-reasoning | Yes       | GPT-5   |
| `claude`                   | Claude_Sonnet     | claude-sonnet-4.6 | No        | Claude  |
| `claude-sonnet`            | Claude_Sonnet     | claude-sonnet-4.6 | No        | Claude  |
| `claude-opus`              | Claude_Opus       | claude-opus-4.6   | No        | Claude  |
| `claude-sonnet-4-20250514` | Claude_Sonnet     | claude-sonnet-4.6 | No        | Claude  |

### Which model should I use?

| Use case                                     | Model              |
|----------------------------------------------|--------------------|
| General purpose, let backend decide          | `auto`             |
| Fast responses, simple questions             | `quick`            |
| Complex reasoning, multi-step problems       | `reasoning`        |
| GPT-5.5 chat (latest conversational model)   | `gpt5.5`           |
| GPT-5.5 with deep thinking (shows reasoning) | `gpt5.5-reasoning` |
| Claude Sonnet 4.6 (Anthropic)                | `claude-sonnet`    |
| Claude Opus 4.6 (Anthropic, most capable)    | `claude-opus`      |

`gpt5.5-reasoning` produces `reasoning_content` output containing the model's thinking process. OpenAI endpoints expose this as `reasoning_content`; Anthropic endpoints expose it as a `thinking` content block before the `text` block. Claude models do not produce reasoning content.

### Session ID in Model Name

You can embed a session ID directly in the model name using the `:` separator. This is useful for clients (like Claude Code, Codex) that cannot send custom headers:

```
model: "gpt5.5-reasoning:my-session-001"
```

This is equivalent to setting `X-Session-Id: my-session-001` header or `session_id: "my-session-001"` in the request body. The model key is extracted before the `:` and the session ID is extracted after it.

### External Model Names

Clients that send model names not in the registry (e.g. `claude-sonnet-4-20250514`, `gpt-4o`, `o1`) will fall back to the `auto` model. The proxy accepts any model string — unknown names do not cause errors, they just use the default model.

## Tool Calling

M365Bridge supports **simulated tool calling** — client-defined tools (Claude Code's Read/Bash/Write, Codex tools, etc.) work without M365 backend natively supporting them.

### How It Works

1. Client sends a request with `tools` array (OpenAI function definitions or Anthropic tool schemas)
2. M365Bridge embeds the entire request JSON into the prompt sent to M365 Copilot
3. M365 Copilot returns a full response JSON in a ```` ```json ```` block
4. M365Bridge parses the response and extracts tool calls into OpenAI `tool_calls` or Anthropic `tool_use` content blocks
5. Client executes the tool and sends the result back in the next message

This works on both OpenAI (`/v1/chat/completions`) and Anthropic (`/v1/messages`) endpoints, in both streaming and non-streaming modes.

### Example (OpenAI)

```bash
curl http://127.0.0.1:8000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer your-api-key" \
  -d '{
    "model": "gpt5.5-reasoning",
    "messages": [{"role": "user", "content": "Run: echo hello"}],
    "tools": [{
      "type": "function",
      "function": {
        "name": "bash",
        "description": "Run a shell command",
        "parameters": {
          "type": "object",
          "properties": {"command": {"type": "string"}},
          "required": ["command"]
        }
      }
    }],
    "tool_choice": "required"
  }'
```

Response:

```json
{
  "choices": [{
    "finish_reason": "tool_calls",
    "message": {
      "role": "assistant",
      "tool_calls": [{
        "id": "call_001",
        "type": "function",
        "function": {
          "name": "bash",
          "arguments": "{\"command\": \"echo hello\"}"
        }
      }]
    }
  }]
}
```

### Example (Anthropic)

```bash
curl http://127.0.0.1:8000/v1/messages \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer your-api-key" \
  -d '{
    "model": "gpt5.5-reasoning",
    "max_tokens": 1024,
    "messages": [{"role": "user", "content": "Run: echo hello"}],
    "tools": [{
      "name": "bash",
      "description": "Run a shell command",
      "input_schema": {
        "type": "object",
        "properties": {"command": {"type": "string"}},
        "required": ["command"]
      }
    }],
    "tool_choice": {"type": "any"}
  }'
```

Response:

```json
{
  "content": [{
    "type": "tool_use",
    "id": "toolu_001",
    "name": "bash",
    "input": {"command": "echo hello"}
  }],
  "stop_reason": "tool_use"
}
```

### Notes

- Tool calling is always enabled — no configuration needed. Requests without `tools` are unaffected.
- When M365 Copilot runs its own server-side tools (web search, code interpreter) and returns plain text instead of a simulated JSON payload, the response is returned as a normal text completion with `finish_reason: "stop"`.
- `tool_result` messages (OpenAI) and `tool_use`/`tool_result` content blocks (Anthropic) in conversation history are converted to plain text before being sent to M365, since the M365 backend does not understand tool roles.
- Streaming endpoints buffer the full response before parsing tool calls (tool call JSON may span multiple chunks).

## Built-in Coding Tools (Opt-in)

M365Bridge can execute a restricted set of local coding operations on the server. This feature is **disabled by default** and its main gate is `M365_ENABLE_CODE_TOOLS=1`. It is available on OpenAI Chat Completions (`/v1/chat/completions`), Anthropic Messages (`/v1/messages`), and OpenAI Responses (`/v1/responses`).

When enabled, tools explicitly included in a request are recognized and executed locally. `M365_AUTO_EXPOSE_TOOLS=1` also adds all built-in tools to requests automatically; leave it at `0` when clients should select tools explicitly. The server sends local results back to the model and continues until the model returns a final answer, emits a caller-defined tool call, or reaches the iteration limit. Because tool calls and intermediate results must be collected first, requests using built-in tools buffer the complete model response even when `stream: true`, then emit the provider-compatible streaming response.

### Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `M365_ENABLE_CODE_TOOLS` | `0` | Main gate. Set to `1` to enable local tool execution. |
| `M365_AUTO_EXPOSE_TOOLS` | `0` | Set to `1` to inject all built-in tool schemas when the client does not provide them. |
| `M365_WORKSPACE_DIR` | `.` | Existing directory that confines file and Git operations. |
| `M365_CODE_TOOL_TIMEOUT` | `30s` | Timeout for each command or test execution. Accepts Go duration syntax, such as `10s` or `2m`. |
| `M365_CODE_TOOL_MAX_OUTPUT` | `1048576` | Maximum captured command output in bytes. Longer output is truncated. |
| `M365_CODE_TOOL_MAX_READ_BYTES` | `1048576` | Maximum number of bytes returned by a file read. |
| `M365_CODE_TOOL_MAX_ITERATIONS` | `10` | Maximum model/tool loop iterations per request. |

Set these variables in `data/.env`. For Docker, `M365_WORKSPACE_DIR` must refer to a directory that already exists inside the container. The provided Compose file mounts only `./data` at `/app/data`; it does not expose a host source workspace.

### Available Tools

| Tool | Operation |
|------|-----------|
| `list_files` | List files and directories under a workspace path. |
| `read_file` | Read a file, subject to the configured byte limit. |
| `write_file` | Create or replace a file inside the workspace. |
| `search_files` | Search workspace file contents. |
| `git_status` | Show workspace Git status. |
| `git_diff` | Show workspace Git changes. |
| `git_log` | Show recent workspace Git history. |
| `shell_command` | Run a shell command with the workspace as its working directory. |
| `apply_patch` | Apply a unified patch inside the workspace. |
| `run_tests` | Run a test command with the configured timeout and output limit. |

### Security Requirements

Enabling these tools turns the API into a remote code and file access surface. **Configure `M365_API_KEYS` or `M365_API_KEY` before enabling them; API key authentication is mandatory for every deployment with coding tools enabled.** Do not expose such a deployment directly to the public internet. Use a least-privilege service account, a dedicated workspace, strict filesystem permissions, network isolation, and container resource limits.

- **OWASP Broken Access Control:** a missing, leaked, or shared API key can let unauthorized callers read, modify, or execute within the mounted workspace. Use unique, rotated keys and enforce authorization at a trusted reverse proxy as well.
- **Command Injection:** `shell_command` and `run_tests` execute model-selected command strings. Treat prompts, repository content, patches, and tool arguments as untrusted input; isolate the process and never provide production credentials.
- **Path Traversal:** file tools confine resolved paths to `M365_WORKSPACE_DIR`, but an overly broad workspace or unsafe mount still exposes sensitive files. Mount only the required project directory and review symlinks and permissions.
- **Sensitive Data Exposure:** tool output and file contents can be returned to the caller and sent to the M365 backend. Keep secrets, tokens, `.env` files, SSH keys, cloud credentials, and customer data outside the workspace.
- **Resource exhaustion:** commands, recursive searches, large files, output, and repeated tool loops can consume CPU, memory, disk, and process capacity. Keep timeout, output, read, and iteration limits conservative and enforce container or OS quotas.

## Responses API

The `/v1/responses` endpoint implements the OpenAI Responses API format. It accepts `input` (string or array of typed items), `instructions`, `max_output_tokens`, `tools`, and `previous_response_id` for conversation continuity.

### Example (non-streaming)

```bash
curl http://127.0.0.1:8000/v1/responses \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer your-api-key" \
  -d '{
    "model": "gpt5.5",
    "input": "What is 2+2?",
    "session_id": "my-session"
  }'
```

Response:

```json
{
  "id": "resp_...",
  "object": "response",
  "created_at": 1234567890,
  "status": "completed",
  "model": "gpt-5.5",
  "output": [{
    "id": "msg_...",
    "type": "message",
    "status": "completed",
    "role": "assistant",
    "content": [{"type": "output_text", "text": "2+2 equals 4.", "annotations": []}]
  }],
  "output_text": "2+2 equals 4.",
  "usage": {"input_tokens": 5, "output_tokens": 8, "total_tokens": 13}
}
```

### Example (with instructions and input items)

```bash
curl http://127.0.0.1:8000/v1/responses \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer your-api-key" \
  -d '{
    "model": "gpt5.5-reasoning",
    "instructions": "You are a concise assistant.",
    "input": [{"role": "user", "content": [{"type": "input_text", "text": "Explain recursion"}]}],
    "stream": true
  }'
```

### Streaming Events

The streaming endpoint emits typed SSE events:

| Event | Description |
|-------|-------------|
| `response.created` | Response object created (status: in_progress) |
| `response.in_progress` | Response is being generated |
| `response.output_item.added` | New output item added (message, reasoning, or function_call) |
| `response.content_part.added` | Content part added to message item |
| `response.output_text.delta` | Text delta |
| `response.output_text.done` | Text complete |
| `response.content_part.done` | Content part complete |
| `response.output_item.done` | Output item complete |
| `response.reasoning_summary_text.delta` | Reasoning/thinking delta |
| `response.reasoning_summary_text.done` | Reasoning complete |
| `response.function_call_arguments.delta` | Tool call arguments delta |
| `response.function_call_arguments.done` | Tool call arguments complete |
| `response.completed` | Full response object (status: completed) |
| `response.failed` | Error occurred (status: failed) |

## Responses Compact API

The `/v1/responses/compact` endpoint implements the OpenAI Responses Compact API for Codex remote compaction. It accepts the same request body as `/v1/responses` (model, input, instructions, tools, stream) and returns a compacted response containing exactly one `compaction` output item.

### How It Works

1. The conversation history (input items) is flattened into a single user message with a compaction prompt
2. The message is sent to M365 Copilot to generate a concise summary
3. The summary is returned wrapped in a `compaction` output item with `encrypted_content` field

### Example (non-streaming)

```bash
curl http://127.0.0.1:8000/v1/responses/compact \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer your-api-key" \
  -d '{
    "model": "gpt5.5-reasoning",
    "input": [
      {"role": "user", "content": "Fix the auth bug in sso.go"},
      {"role": "assistant", "content": "I added the missing sso_reload parameter."},
      {"role": "user", "content": "Now add logging to the refresh path"}
    ]
  }'
```

Response:

```json
{
  "id": "resp_...",
  "object": "response",
  "status": "completed",
  "output": [{
    "id": "cmp_...",
    "type": "compaction",
    "encrypted_content": "The conversation focused on fixing an SSO auth bug..."
  }]
}
```

### Streaming

Streaming mode emits the same SSE event sequence as `/v1/responses` (`response.created`, `response.in_progress`, `response.output_item.added`, `response.output_item.done`, `response.completed`, `[DONE]`), but the output item has `type: "compaction"` instead of `type: "message"`.

### Notes

- Custom `instructions` in the request body override the default compaction prompt
- The compaction request should use a new session ID (not reuse an existing conversation) for best results

## Project Structure

```
cmd/cli/main.go          # Single entry point, subcommand router
pkg/
  auth/auth.go           # TokenManager, token refresh, AES-encrypted refresh token storage
  auth/sso.go            # SSO cookie-based re-authentication (fallback for 24h token expiry)
  client/client.go       # M365Client, WebSocket (SignalR) communication
  crypto/crypto.go       # AES-256-GCM encryption for refresh tokens
  models/models.go       # Version, ModelRegistry, Config, LoadConfig, LookupModel
  payload/payload.go     # Request payload builders, URL builder, locale/timezone helpers
  servers/
    api.go               # HTTP API server, all endpoints, max_tokens, token counting, session isolation
    cli.go               # CLI server, interactive mode
  setup/wizard.go        # Browser-based setup wizard (JS snippet, token verify, data/.env save)
go.mod                   # Module: github.com/KilimcininKorOglu/M365Bridge, Go 1.22
data/                    # Runtime data (gitignored): tokens/, setup.json, cache/
```

## Dependencies

| Dependency                      | Purpose                                                               |
|---------------------------------|-----------------------------------------------------------------------|
| `github.com/google/uuid`        | UUID generation for SIDs and request IDs                              |
| `github.com/gorilla/websocket`  | WebSocket client for SignalR                                          |
| `github.com/pkoukk/tiktoken-go` | BPE token counting (cl100k_base) for usage and max_tokens enforcement |
| `golang.org/x/net`              | publicsuffix list for SSO cookie jar                                  |

## Security

- Refresh tokens encrypted with AES-256-GCM before storage
- SSO cookies encrypted with AES-256-GCM before storage (`data/tokens/sso_cookies.json`)
- Encryption key stored in `data/tokens/encryption.key`
- Access tokens cached in `data/tokens/token_cache.json` (disk-persisted, ~1h expiry with 60s buffer)
- Background token refresher proactively refreshes access token every 30 minutes in `serve` mode
- SSO cookie auto-renewal silently re-authenticates when refresh token expires (24h SPA limit)
- No credentials stored in code or repository
- `data/` directory is gitignored (contains tokens, cache, setup.json)
- API key authentication protects all `/v1/*` endpoints when configured

## Image Input Support

The proxy supports multimodal image input via OpenAI and Anthropic API formats:

- **OpenAI**: `content` array with `{"type": "image_url", "image_url": {"url": "data:image/png;base64,..."}}` blocks
- **Anthropic**: `content` array with `{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "..."}}` blocks

Images are uploaded to the M365 backend via `POST https://substrate.office.com/m365Copilot/UploadFile` and attached to the WebSocket message as `messageAnnotations`. Supported formats: PNG, JPEG, GIF, WebP.

## Image Generation

The proxy exposes M365 Copilot's  image generation as OpenAI Images API endpoints:

- `POST /v1/images/generations` (JSON body): Generate images from a text prompt (no file upload)
- `POST /v1/images/edits` (multipart/form-data): Edit existing image(s) with a text prompt; supports up to 16 images via repeated `image` form fields

Both endpoints accept the following parameters:

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `prompt` | string | (required) | The text prompt for image generation/editing |
| `n` | int | 1 | Number of images to generate (M365 generates one per request) |
| `size` | string | `1024x1024` | Image size hint (appended to prompt as natural language) |
| `quality` | string | `standard` | Quality hint (appended to prompt; `standard` is skipped) |
| `style` | string | `natural` | Style hint (appended to prompt; `natural` is skipped) |
| `response_format` | string | `url` | Response format: `url` returns a data URL (base64), `b64_json` returns base64 in a separate field |
| `session_id` | string | (optional) | Session ID for conversation continuity |

### Response Format

- `response_format=url` (default): Downloads the image server-side and returns a `data:image/png;base64,...` data URL. Falls back to the raw `designerapp.officeapps.live.com` URL if the download fails.
- `response_format=b64_json`: Downloads the image server-side using a broker token and returns the image as base64-encoded PNG data in the `b64_json` field.

### Image Download Token Flow

When images are generated, the proxy acquires a JWE access token for `designerappservice.officeapps.live.com` via the MSAL.js broker token flow to download the image (used for both `url` and `b64_json` response formats):

1. The broker app (`c0ab8ce9`) acquires a token on behalf of the M365 web app (`4765445b`) with the `designerappservice.officeapps.live.com/.default` scope
2. A broker-compatible refresh token is stored at `data/tokens/rt_broker.txt` (encrypted), rotated automatically by the background token refresher
3. If no broker refresh token exists, one is acquired via SSO cookie broker authorize flow (PKCE + `brk-multihub://outlook.office.com` redirect URI)
4. The JWE token and `fileToken` header are used to download the image from `designerapp.officeapps.live.com`
5. The downloaded image is base64-encoded and returned in the `b64_json` field

### Example

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:8230/v1",
    api_key="your-api-key",  # omit if no API key configured
)

resp = client.images.generate(
    model="gpt5.5-reasoning",
    prompt="a serene mountain landscape at sunset",
    n=1,
    response_format="b64_json",
)

# resp.data[0].b64_json contains the base64-encoded PNG
import base64
with open("output.png", "wb") as f:
    f.write(base64.b64decode(resp.data[0].b64_json))
```

## Unimplemented Features

- File upload
- Code interpreter

## Disclaimer

This project is for learning and research purposes only. It explores publicly observable network communication protocols.

By using this project, you confirm that:
- You have legitimate Microsoft 365 Copilot authorization
- It is for personal learning and research, not commercial use
- You understand the risks of using unofficial interfaces
- You accept all consequences

This project does not:
- Crack encryption or bypass authentication
- Access or leak others' data
- Interfere with Microsoft services
- Have any association with Microsoft Corporation

## License

Research Only
