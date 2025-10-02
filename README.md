# MiniMCP: a minimal MCP

This only tests what a minimal implementation of an MCP would be like in Go.

Given the MCP spec even talks about no newlines on `stdio` transport, it would likely be possible to do the `stdio` transport without further libraries. However, this is good enough for a small test of whether an MCP client will be willing to talk to this service.
