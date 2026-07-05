#!/bin/bash
# Test: OpenAI Chat Completions Tool Calling Protocol
# Tests client-side tool calling: does the M365 backend attempt to use
# client-defined tools (write_file, read_file, list_files) when asked?
#
# Flow per tool:
# 1. Send request with tool definitions + a prompt that naturally requires the tool
# 2. Check if response contains tool_calls for the client-side tool
# 3. If yes, send follow-up with a fake tool result
# 4. Verify backend continues with a text response
#
# Also tests server-side tool (search) to confirm both paths work.

set -e

BASE_URL="http://localhost:8230"
PASSED=0
FAILED=0
SKIPPED=0

pass() { echo "  PASS: $1"; PASSED=$((PASSED+1)); }
fail() { echo "  FAIL: $1"; FAILED=$((FAILED+1)); }
skip() { echo "  SKIP: $1"; SKIPPED=$((SKIPPED+1)); }

# Tool definitions (harmless client-side tools)
# Includes bash tool for shell-routing: model emits ```bash blocks that get
# routed to the bash tool, enabling agentic loops (cramt approach).
TOOLS_JSON='[
  {
    "type": "function",
    "function": {
      "name": "bash",
      "description": "Run a bash command on the local filesystem. Use this for file operations: cat to read, ls to list, heredocs to write.",
      "parameters": {
        "type": "object",
        "properties": {
          "command": {"type": "string", "description": "The bash command to execute"}
        },
        "required": ["command"]
      }
    }
  },
  {
    "type": "function",
    "function": {
      "name": "write_file",
      "description": "Write content to a file on the local filesystem",
      "parameters": {
        "type": "object",
        "properties": {
          "path": {"type": "string", "description": "The file path to write to"},
          "content": {"type": "string", "description": "The content to write"}
        },
        "required": ["path", "content"]
      }
    }
  },
  {
    "type": "function",
    "function": {
      "name": "read_file",
      "description": "Read the contents of a file from the local filesystem",
      "parameters": {
        "type": "object",
        "properties": {
          "path": {"type": "string", "description": "The file path to read"}
        },
        "required": ["path"]
      }
    }
  },
  {
    "type": "function",
    "function": {
      "name": "list_files",
      "description": "List files in a directory on the local filesystem",
      "parameters": {
        "type": "object",
        "properties": {
          "directory": {"type": "string", "description": "The directory path to list"}
        },
        "required": ["directory"]
      }
    }
  }
]'

echo "=== OpenAI Tool Calling Test (Client-Side Tools) ==="
echo ""

# --- Test 1: write_file tool ---
echo "--- Test 1: write_file tool ---"
SESSION_1="test-openai-write-$(date +%s)"
RESPONSE=$(curl -s "$BASE_URL/v1/chat/completions" \
  -H "Content-Type: application/json" \
  -d "$(jq -n --arg sid "$SESSION_1" --argjson tools "$TOOLS_JSON" '{
    model: "gpt5.5-reasoning",
    stream: false,
    session_id: $sid,
    messages: [{role:"user",content:"Create a file called hello.txt with the content \"Hello World\". Use the write_file tool."}],
    tools: $tools
  }')")

FINISH=$(echo "$RESPONSE" | jq -r '.choices[0].finish_reason // "null"')
TOOL_CALLS=$(echo "$RESPONSE" | jq -r '[.choices[0].message.tool_calls[]?.function.name] // []')
CONTENT=$(echo "$RESPONSE" | jq -r '.choices[0].message.content // ""')

echo "  finish_reason: $FINISH"
echo "  tool_calls: $TOOL_CALLS"
echo "  content: ${CONTENT:0:150}..."

if echo "$TOOL_CALLS" | jq -e 'index("write_file") or index("bash")' >/dev/null 2>&1; then
    pass "Backend attempted to use write_file or bash tool"
    # Send follow-up with fake tool result
    TC_ID=$(echo "$RESPONSE" | jq -r '.choices[0].message.tool_calls[0].id')
    TC_NAME=$(echo "$RESPONSE" | jq -r '.choices[0].message.tool_calls[0].function.name')
    TC_ARGS=$(echo "$RESPONSE" | jq -c '.choices[0].message.tool_calls[0].function.arguments')

    RESPONSE2=$(curl -s "$BASE_URL/v1/chat/completions" \
      -H "Content-Type: application/json" \
      -d "$(jq -n --arg sid "$SESSION_1" --arg tcid "$TC_ID" --arg tcname "$TC_NAME" --argjson tcargs "$TC_ARGS" --argjson tools "$TOOLS_JSON" '{
        model: "gpt5.5-reasoning",
        stream: false,
        session_id: $sid,
        messages: [
          {role:"user",content:"Create a file called hello.txt with the content \"Hello World\". Use the write_file tool."},
          {role:"assistant",content:null,tool_calls:[{id:$tcid,type:"function",function:{name:$tcname,arguments:($tcargs|tostring)}}]},
          {role:"tool",tool_call_id:$tcid,content:"File hello.txt written successfully (20 bytes)"}
        ],
        tools: $tools
      }')")

    FINISH2=$(echo "$RESPONSE2" | jq -r '.choices[0].finish_reason // "null"')
    CONTENT2=$(echo "$RESPONSE2" | jq -r '.choices[0].message.content // ""')
    echo "  follow-up finish_reason: $FINISH2"
    echo "  follow-up content: ${CONTENT2:0:150}..."

    if [ "$FINISH2" = "stop" ] && [ -n "$CONTENT2" ] && [ "$CONTENT2" != "null" ]; then
        pass "Backend continued with text response after write_file result"
    else
        fail "Backend did not produce text response after write_file result (finish_reason=$FINISH2)"
    fi
else
    # Backend may not use the tool - this is the key finding
    if [ "$FINISH" = "stop" ] && [ -n "$CONTENT" ]; then
        skip "Backend did not use write_file tool, answered with text directly (this is the real-world behavior)"
    else
        fail "Backend did not use write_file and did not produce text (finish_reason=$FINISH)"
    fi
fi
echo ""

# --- Test 2: read_file tool ---
echo "--- Test 2: read_file tool ---"
SESSION_2="test-openai-read-$(date +%s)"
RESPONSE=$(curl -s "$BASE_URL/v1/chat/completions" \
  -H "Content-Type: application/json" \
  -d "$(jq -n --arg sid "$SESSION_2" --argjson tools "$TOOLS_JSON" '{
    model: "gpt5.5-reasoning",
    stream: false,
    session_id: $sid,
    messages: [{role:"user",content:"Read the file /tmp/config.json and tell me what is in it. You must use the read_file tool to read it."}],
    tools: $tools
  }')")

FINISH=$(echo "$RESPONSE" | jq -r '.choices[0].finish_reason // "null"')
TOOL_CALLS=$(echo "$RESPONSE" | jq -r '[.choices[0].message.tool_calls[]?.function.name] // []')
CONTENT=$(echo "$RESPONSE" | jq -r '.choices[0].message.content // ""')

echo "  finish_reason: $FINISH"
echo "  tool_calls: $TOOL_CALLS"
echo "  content: ${CONTENT:0:150}..."

if echo "$TOOL_CALLS" | jq -e 'index("read_file") or index("bash")' >/dev/null 2>&1; then
    pass "Backend attempted to use read_file or bash tool"
    TC_ID=$(echo "$RESPONSE" | jq -r '.choices[0].message.tool_calls[0].id')
    TC_NAME=$(echo "$RESPONSE" | jq -r '.choices[0].message.tool_calls[0].function.name')
    TC_ARGS=$(echo "$RESPONSE" | jq -c '.choices[0].message.tool_calls[0].function.arguments')

    RESPONSE2=$(curl -s "$BASE_URL/v1/chat/completions" \
      -H "Content-Type: application/json" \
      -d "$(jq -n --arg sid "$SESSION_2" --arg tcid "$TC_ID" --arg tcname "$TC_NAME" --argjson tcargs "$TC_ARGS" --argjson tools "$TOOLS_JSON" '{
        model: "gpt5.5-reasoning",
        stream: false,
        session_id: $sid,
        messages: [
          {role:"user",content:"Read the file /tmp/config.json and tell me what is in it. You must use the read_file tool to read it."},
          {role:"assistant",content:null,tool_calls:[{id:$tcid,type:"function",function:{name:$tcname,arguments:($tcargs|tostring)}}]},
          {role:"tool",tool_call_id:$tcid,content:"{\"server\":\"localhost\",\"port\":8080,\"debug\":true}"}
        ],
        tools: $tools
      }')")

    FINISH2=$(echo "$RESPONSE2" | jq -r '.choices[0].finish_reason // "null"')
    CONTENT2=$(echo "$RESPONSE2" | jq -r '.choices[0].message.content // ""')
    echo "  follow-up finish_reason: $FINISH2"
    echo "  follow-up content: ${CONTENT2:0:150}..."

    if [ "$FINISH2" = "stop" ] && [ -n "$CONTENT2" ] && [ "$CONTENT2" != "null" ]; then
        pass "Backend continued with text response after read_file result"
    else
        fail "Backend did not produce text response after read_file result (finish_reason=$FINISH2)"
    fi
else
    if [ "$FINISH" = "stop" ] && [ -n "$CONTENT" ]; then
        skip "Backend did not use read_file tool, answered with text directly"
    else
        fail "Backend did not use read_file and did not produce text (finish_reason=$FINISH)"
    fi
fi
echo ""

# --- Test 3: list_files tool ---
echo "--- Test 3: list_files tool ---"
SESSION_3="test-openai-list-$(date +%s)"
RESPONSE=$(curl -s "$BASE_URL/v1/chat/completions" \
  -H "Content-Type: application/json" \
  -d "$(jq -n --arg sid "$SESSION_3" --argjson tools "$TOOLS_JSON" '{
    model: "gpt5.5-reasoning",
    stream: false,
    session_id: $sid,
    messages: [{role:"user",content:"List all files in the /home/user/project directory. Use the list_files tool."}],
    tools: $tools
  }')")

FINISH=$(echo "$RESPONSE" | jq -r '.choices[0].finish_reason // "null"')
TOOL_CALLS=$(echo "$RESPONSE" | jq -r '[.choices[0].message.tool_calls[]?.function.name] // []')
CONTENT=$(echo "$RESPONSE" | jq -r '.choices[0].message.content // ""')

echo "  finish_reason: $FINISH"
echo "  tool_calls: $TOOL_CALLS"
echo "  content: ${CONTENT:0:150}..."

if echo "$TOOL_CALLS" | jq -e 'index("list_files") or index("bash")' >/dev/null 2>&1; then
    pass "Backend attempted to use list_files or bash tool"
    TC_ID=$(echo "$RESPONSE" | jq -r '.choices[0].message.tool_calls[0].id')
    TC_NAME=$(echo "$RESPONSE" | jq -r '.choices[0].message.tool_calls[0].function.name')
    TC_ARGS=$(echo "$RESPONSE" | jq -c '.choices[0].message.tool_calls[0].function.arguments')

    RESPONSE2=$(curl -s "$BASE_URL/v1/chat/completions" \
      -H "Content-Type: application/json" \
      -d "$(jq -n --arg sid "$SESSION_3" --arg tcid "$TC_ID" --arg tcname "$TC_NAME" --argjson tcargs "$TC_ARGS" --argjson tools "$TOOLS_JSON" '{
        model: "gpt5.5-reasoning",
        stream: false,
        session_id: $sid,
        messages: [
          {role:"user",content:"List all files in the /home/user/project directory. Use the list_files tool."},
          {role:"assistant",content:null,tool_calls:[{id:$tcid,type:"function",function:{name:$tcname,arguments:($tcargs|tostring)}}]},
          {role:"tool",tool_call_id:$tcid,content:"main.go\ngo.mod\nREADME.md\ndocker-compose.yml"}
        ],
        tools: $tools
      }')")

    FINISH2=$(echo "$RESPONSE2" | jq -r '.choices[0].finish_reason // "null"')
    CONTENT2=$(echo "$RESPONSE2" | jq -r '.choices[0].message.content // ""')
    echo "  follow-up finish_reason: $FINISH2"
    echo "  follow-up content: ${CONTENT2:0:150}..."

    if [ "$FINISH2" = "stop" ] && [ -n "$CONTENT2" ] && [ "$CONTENT2" != "null" ]; then
        pass "Backend continued with text response after list_files result"
    else
        fail "Backend did not produce text response after list_files result (finish_reason=$FINISH2)"
    fi
else
    if [ "$FINISH" = "stop" ] && [ -n "$CONTENT" ]; then
        skip "Backend did not use list_files tool, answered with text directly"
    else
        fail "Backend did not use list_files and did not produce text (finish_reason=$FINISH)"
    fi
fi
echo ""

# --- Test 4: Server-side search tool (baseline) ---
echo "--- Test 4: Server-side search tool (baseline) ---"
SESSION_4="test-openai-search-$(date +%s)"
RESPONSE=$(curl -s "$BASE_URL/v1/chat/completions" \
  -H "Content-Type: application/json" \
  -d "$(jq -n --arg sid "$SESSION_4" --argjson tools "$TOOLS_JSON" '{
    model: "gpt5.5-reasoning",
    stream: false,
    session_id: $sid,
    messages: [{role:"user",content:"Search the web for the latest news about AI. Use the available tools."}],
    tools: $tools
  }')")

FINISH=$(echo "$RESPONSE" | jq -r '.choices[0].finish_reason // "null"')
TOOL_CALLS=$(echo "$RESPONSE" | jq -r '[.choices[0].message.tool_calls[]?.function.name] // []')
CONTENT=$(echo "$RESPONSE" | jq -r '.choices[0].message.content // ""')

echo "  finish_reason: $FINISH"
echo "  tool_calls: $TOOL_CALLS"
echo "  content: ${CONTENT:0:150}..."

if echo "$TOOL_CALLS" | jq -e 'index("search")' >/dev/null 2>&1; then
    pass "Backend used server-side search tool (expected)"
else
    skip "Backend did not use search tool this time"
fi
echo ""

echo "=== Results: $PASSED passed, $FAILED failed, $SKIPPED skipped ==="
exit $FAILED
