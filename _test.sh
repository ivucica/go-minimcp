#!/bin/bash
echo "ATTEMPT 1:"
echo 'Content-Length: 70\r\n\r\n{"jsonrpc": "2.0", "method": "subtract", "params": [42, 23], "id": 1}' | ${GOPATH}/bin/minimcp
echo "ATTEMPT 2:"
echo '{"jsonrpc": "2.0", "method": "subtract", "params": [42, 23], "id": 1}' | ${GOPATH}/bin/minimcp
echo "ATTEMPT 3:"
printf 'Content-Length: 70\r\n\r\n{"jsonrpc": "2.0", "method": "subtract", "params": [42, 23], "id": 1}' | ${GOPATH}/bin/minimcp
