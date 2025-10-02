#!/bin/bash

LC_ALL=C  # allows use of ${#REQ} to get number of bytes

# concatenated array members len in bytes
# printf %s "${array[@]}" | wc -c
# and other tricks: https://unix.stackexchange.com/a/702009/11829

# TODO: must be one line in mcp, no content-length.
# TODO: response must be one line too

REQ=$(cat << _EOF
{
  "jsonrpc": "2.0",
  "id": 1,
  "method": "initialize",
  "params": {
    "protocolVersion": "2024-11-05",
    "capabilities": {
      "roots": {
        "listChanged": true
      },
      "sampling": {},
      "elicitation": {}
    },
    "clientInfo": {
      "name": "ExampleClient",
      "title": "Example Client Display Name",
      "version": "1.0.0"
    }
  }
}
_EOF
)

SZ=$(printf %s "$REQ" | wc -c)

# TODO: replace with a nice 'expect' script
(printf "Content-Length: %d\r\n\r\n%s" "$SZ" "$REQ" ; sleep 3) | ${GOPATH}/bin/minimcp
