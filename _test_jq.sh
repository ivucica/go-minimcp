#!/bin/bash

# Read JSON from stdin or argument
if [ -t 0 ]; then # Check if stdin is a terminal (no pipe)
  JSON_INPUT="$1"
else
  JSON_INPUT=$(cat /dev/stdin)
fi

# Validate JSON using jq
# This example checks for specific fields and values.
# You can extend this with more complex jq expressions for schema validation, etc.
echo "$JSON_INPUT" | jq -e '.id == 1 and .jsonrpc == "2.0" and .result.serverInfo.name == "MiniMCP" and .result.serverInfo.version == "0.0.1"'

if [ $? -eq 0 ]; then
  echo "JSON validation successful by jq."
  exit 0
else
  echo "JSON validation failed by jq."
  exit 1
fi
