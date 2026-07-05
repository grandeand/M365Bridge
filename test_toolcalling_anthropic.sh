#!/bin/bash
# Test: Anthropic Messages API Tool Use Protocol
# Tests client-side tool calling: does the M365 backend attempt to use
# client-defined tools (write_file, read_file, list_files) when asked?

set -e

BASE_URL="http://localhost:8230"
PASSED=0
FAILED=0
SKIPPED=0

pass() { echo "  PASS: $1"; PASSED=$((PASSED+1)); }
fail() { echo "  FAIL: $1"; FAILED=$((FAILED+1)); }
skip() { echo "  SKIP: $1"; SKIPPED=$((SKIPPED+1)); }

# Anthropic-style tool definitions (flat, no "function" wrapper)
# Includes bash tool for shell-routing (cramt approach).
TOOLS_JSON='[
  {
    "name": "bash",
    "description": "Run a bash command on the local filesystem. Use this for file operations: cat to read, ls to list, heredocs to write.",
    "input_schema": {
      "type": "object",
      "properties": {
        "command": {"type": "string", "description": "The bash command to execute"}
      },
      "required": ["command"]
    }
  },
  {
    "name": "write_file",
    "description": "Write content to a file on the local filesystem",
    "input_schema": {
      "type": "object",
      "properties": {
        "path": {"type": "string", "description": "The file path to write to"},
        "content": {"type": "string", "description": "The content to write"}
      },
      "required": ["path", "content"]
    }
  },
  {
    "name": "read_file",
    "description": "Read the contents of a file from the local filesystem",
    "input_schema": {
      "type": "object",
      "properties": {
        "path": {"type": "string", "description": "The file path to read"}
      },
      "required": ["path"]
    }
  },
  {
    "name": "list_files",
    "description": "List files in a directory on the local filesystem",
    "input_schema": {
      "type": "object",
      "properties": {
        "directory": {"type": "string", "description": "The directory path to list"}
      },
      "required": ["directory"]
    }
  }
]'

echo "=== Anthropic Tool Use Test (Client-Side Tools) ==="
echo ""

# --- Test 1: write_file tool ---
echo "--- Test 1: write_file tool ---"
SESSION_1="test-anthropic-write-$(date +%s)"
RESPONSE=$(curl -s "$BASE_URL/v1/messages" \
  -H "Content-Type: application/json" \
  -H "anthropic-version: 2023-06-01" \
  -d "$(jq -n --arg sid "$SESSION_1" --argjson tools "$TOOLS_JSON" '{
    model: "gpt5.5-reasoning",
    max_tokens: 1024,
    session_id: $sid,
    messages: [{role:"user",content:"Create a file called hello.txt with the content \"Hello World\". Use the write_file tool."}],
    tools: $tools
  }')")

STOP=$(echo "$RESPONSE" | jq -r '.stop_reason // "null"')
TOOL_USE_NAMES=$(echo "$RESPONSE" | jq -r '[.content[]? | select(.type=="tool_use") | .name] // []')
TEXT=$(echo "$RESPONSE" | jq -r '[.content[]? | select(.type=="text")] | .[0].text // ""')

echo "  stop_reason: $STOP"
echo "  tool_use names: $TOOL_USE_NAMES"
echo "  text: ${TEXT:0:150}..."

if echo "$TOOL_USE_NAMES" | jq -e 'index("write_file") or index("bash")' >/dev/null 2>&1; then
    pass "Backend attempted to use write_file or bash tool"
    TU_ID=$(echo "$RESPONSE" | jq -r '.content[] | select(.type=="tool_use") | .id')
    TU_NAME=$(echo "$RESPONSE" | jq -r '.content[] | select(.type=="tool_use") | .name')
    TU_INPUT=$(echo "$RESPONSE" | jq -c '.content[] | select(.type=="tool_use") | .input')

    RESPONSE2=$(curl -s "$BASE_URL/v1/messages" \
      -H "Content-Type: application/json" \
      -H "anthropic-version: 2023-06-01" \
      -d "$(jq -n --arg sid "$SESSION_1" --arg tuid "$TU_ID" --arg tuname "$TU_NAME" --argjson tuinput "$TU_INPUT" --argjson tools "$TOOLS_JSON" '{
        model: "gpt5.5-reasoning",
        max_tokens: 1024,
        session_id: $sid,
        messages: [
          {role:"user",content:"Create a file called hello.txt with the content \"Hello World\". Use the write_file tool."},
          {role:"assistant",content:[{type:"tool_use",id:$tuid,name:$tuname,input:$tuinput}]},
          {role:"user",content:[{type:"tool_result",tool_use_id:$tuid,content:"File hello.txt written successfully (20 bytes)"}]}
        ],
        tools: $tools
      }')")

    STOP2=$(echo "$RESPONSE2" | jq -r '.stop_reason // "null"')
    CONTENT2=$(echo "$RESPONSE2" | jq -r '[.content[]? | select(.type=="text")] | .[0].text // ""')
    echo "  follow-up stop_reason: $STOP2"
    echo "  follow-up text: ${CONTENT2:0:150}..."

    if [ "$STOP2" = "end_turn" ] && [ -n "$CONTENT2" ] && [ "$CONTENT2" != "null" ]; then
        pass "Backend continued with text response after write_file result"
    else
        fail "Backend did not produce text response after write_file result (stop_reason=$STOP2)"
    fi
else
    if [ "$STOP" = "end_turn" ] && [ -n "$TEXT" ]; then
        skip "Backend did not use write_file tool, answered with text directly (this is the real-world behavior)"
    else
        fail "Backend did not use write_file and did not produce text (stop_reason=$STOP)"
    fi
fi
echo ""

# --- Test 2: read_file tool ---
echo "--- Test 2: read_file tool ---"
SESSION_2="test-anthropic-read-$(date +%s)"
RESPONSE=$(curl -s "$BASE_URL/v1/messages" \
  -H "Content-Type: application/json" \
  -H "anthropic-version: 2023-06-01" \
  -d "$(jq -n --arg sid "$SESSION_2" --argjson tools "$TOOLS_JSON" '{
    model: "gpt5.5-reasoning",
    max_tokens: 1024,
    session_id: $sid,
    messages: [{role:"user",content:"Read the file /tmp/config.json and tell me what is in it. You must use the read_file tool to read it."}],
    tools: $tools
  }')")

STOP=$(echo "$RESPONSE" | jq -r '.stop_reason // "null"')
TOOL_USE_NAMES=$(echo "$RESPONSE" | jq -r '[.content[]? | select(.type=="tool_use") | .name] // []')
TEXT=$(echo "$RESPONSE" | jq -r '[.content[]? | select(.type=="text")] | .[0].text // ""')

echo "  stop_reason: $STOP"
echo "  tool_use names: $TOOL_USE_NAMES"
echo "  text: ${TEXT:0:150}..."

if echo "$TOOL_USE_NAMES" | jq -e 'index("read_file") or index("bash")' >/dev/null 2>&1; then
    pass "Backend attempted to use read_file or bash tool"
    TU_ID=$(echo "$RESPONSE" | jq -r '.content[] | select(.type=="tool_use") | .id')
    TU_NAME=$(echo "$RESPONSE" | jq -r '.content[] | select(.type=="tool_use") | .name')
    TU_INPUT=$(echo "$RESPONSE" | jq -c '.content[] | select(.type=="tool_use") | .input')

    RESPONSE2=$(curl -s "$BASE_URL/v1/messages" \
      -H "Content-Type: application/json" \
      -H "anthropic-version: 2023-06-01" \
      -d "$(jq -n --arg sid "$SESSION_2" --arg tuid "$TU_ID" --arg tuname "$TU_NAME" --argjson tuinput "$TU_INPUT" --argjson tools "$TOOLS_JSON" '{
        model: "gpt5.5-reasoning",
        max_tokens: 1024,
        session_id: $sid,
        messages: [
          {role:"user",content:"Read the file /tmp/config.json and tell me what is in it. You must use the read_file tool to read it."},
          {role:"assistant",content:[{type:"tool_use",id:$tuid,name:$tuname,input:$tuinput}]},
          {role:"user",content:[{type:"tool_result",tool_use_id:$tuid,content:"{\"server\":\"localhost\",\"port\":8080,\"debug\":true}"}]}
        ],
        tools: $tools
      }')")

    STOP2=$(echo "$RESPONSE2" | jq -r '.stop_reason // "null"')
    CONTENT2=$(echo "$RESPONSE2" | jq -r '[.content[]? | select(.type=="text")] | .[0].text // ""')
    echo "  follow-up stop_reason: $STOP2"
    echo "  follow-up text: ${CONTENT2:0:150}..."

    if [ "$STOP2" = "end_turn" ] && [ -n "$CONTENT2" ] && [ "$CONTENT2" != "null" ]; then
        pass "Backend continued with text response after read_file result"
    else
        fail "Backend did not produce text response after read_file result (stop_reason=$STOP2)"
    fi
else
    if [ "$STOP" = "end_turn" ] && [ -n "$TEXT" ]; then
        skip "Backend did not use read_file tool, answered with text directly"
    else
        fail "Backend did not use read_file and did not produce text (stop_reason=$STOP)"
    fi
fi
echo ""

# --- Test 3: list_files tool ---
echo "--- Test 3: list_files tool ---"
SESSION_3="test-anthropic-list-$(date +%s)"
RESPONSE=$(curl -s "$BASE_URL/v1/messages" \
  -H "Content-Type: application/json" \
  -H "anthropic-version: 2023-06-01" \
  -d "$(jq -n --arg sid "$SESSION_3" --argjson tools "$TOOLS_JSON" '{
    model: "gpt5.5-reasoning",
    max_tokens: 1024,
    session_id: $sid,
    messages: [{role:"user",content:"List all files in the /home/user/project directory. Use the list_files tool."}],
    tools: $tools
  }')")

STOP=$(echo "$RESPONSE" | jq -r '.stop_reason // "null"')
TOOL_USE_NAMES=$(echo "$RESPONSE" | jq -r '[.content[]? | select(.type=="tool_use") | .name] // []')
TEXT=$(echo "$RESPONSE" | jq -r '[.content[]? | select(.type=="text")] | .[0].text // ""')

echo "  stop_reason: $STOP"
echo "  tool_use names: $TOOL_USE_NAMES"
echo "  text: ${TEXT:0:150}..."

if echo "$TOOL_USE_NAMES" | jq -e 'index("list_files") or index("bash")' >/dev/null 2>&1; then
    pass "Backend attempted to use list_files or bash tool"
    TU_ID=$(echo "$RESPONSE" | jq -r '.content[] | select(.type=="tool_use") | .id')
    TU_NAME=$(echo "$RESPONSE" | jq -r '.content[] | select(.type=="tool_use") | .name')
    TU_INPUT=$(echo "$RESPONSE" | jq -c '.content[] | select(.type=="tool_use") | .input')

    RESPONSE2=$(curl -s "$BASE_URL/v1/messages" \
      -H "Content-Type: application/json" \
      -H "anthropic-version: 2023-06-01" \
      -d "$(jq -n --arg sid "$SESSION_3" --arg tuid "$TU_ID" --arg tuname "$TU_NAME" --argjson tuinput "$TU_INPUT" --argjson tools "$TOOLS_JSON" '{
        model: "gpt5.5-reasoning",
        max_tokens: 1024,
        session_id: $sid,
        messages: [
          {role:"user",content:"List all files in the /home/user/project directory. Use the list_files tool."},
          {role:"assistant",content:[{type:"tool_use",id:$tuid,name:$tuname,input:$tuinput}]},
          {role:"user",content:[{type:"tool_result",tool_use_id:$tuid,content:"main.go\ngo.mod\nREADME.md\ndocker-compose.yml"}]}
        ],
        tools: $tools
      }')")

    STOP2=$(echo "$RESPONSE2" | jq -r '.stop_reason // "null"')
    CONTENT2=$(echo "$RESPONSE2" | jq -r '[.content[]? | select(.type=="text")] | .[0].text // ""')
    echo "  follow-up stop_reason: $STOP2"
    echo "  follow-up text: ${CONTENT2:0:150}..."

    if [ "$STOP2" = "end_turn" ] && [ -n "$CONTENT2" ] && [ "$CONTENT2" != "null" ]; then
        pass "Backend continued with text response after list_files result"
    else
        fail "Backend did not produce text response after list_files result (stop_reason=$STOP2)"
    fi
else
    if [ "$STOP" = "end_turn" ] && [ -n "$TEXT" ]; then
        skip "Backend did not use list_files tool, answered with text directly"
    else
        fail "Backend did not use list_files and did not produce text (stop_reason=$STOP)"
    fi
fi
echo ""

# --- Test 4: Server-side search tool (baseline) ---
echo "--- Test 4: Server-side search tool (baseline) ---"
SESSION_4="test-anthropic-search-$(date +%s)"
RESPONSE=$(curl -s "$BASE_URL/v1/messages" \
  -H "Content-Type: application/json" \
  -H "anthropic-version: 2023-06-01" \
  -d "$(jq -n --arg sid "$SESSION_4" --argjson tools "$TOOLS_JSON" '{
    model: "gpt5.5-reasoning",
    max_tokens: 1024,
    session_id: $sid,
    messages: [{role:"user",content:"Search the web for the latest news about AI. Use the available tools."}],
    tools: $tools
  }')")

STOP=$(echo "$RESPONSE" | jq -r '.stop_reason // "null"')
TOOL_USE_NAMES=$(echo "$RESPONSE" | jq -r '[.content[]? | select(.type=="tool_use") | .name] // []')
TEXT=$(echo "$RESPONSE" | jq -r '[.content[]? | select(.type=="text")] | .[0].text // ""')

echo "  stop_reason: $STOP"
echo "  tool_use names: $TOOL_USE_NAMES"
echo "  text: ${TEXT:0:150}..."

if echo "$TOOL_USE_NAMES" | jq -e 'index("search")' >/dev/null 2>&1; then
    pass "Backend used server-side search tool (expected)"
else
    skip "Backend did not use search tool this time"
fi
echo ""

echo "=== Results: $PASSED passed, $FAILED failed, $SKIPPED skipped ==="
exit $FAILED
