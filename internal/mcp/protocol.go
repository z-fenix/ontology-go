// Package mcp exposes the ontology to AI agents over the Model Context Protocol, revision
// 2026-07-28 (stateless Streamable HTTP: no initialize handshake, no sessions; every request
// carries its protocol version and client capabilities in params._meta).
package mcp

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

// ProtocolVersion is the only MCP revision this server implements.
const ProtocolVersion = "2026-07-28"

// _meta keys defined by the protocol.
const (
	MetaProtocolVersion    = "io.modelcontextprotocol/protocolVersion"
	MetaClientInfo         = "io.modelcontextprotocol/clientInfo"
	MetaClientCapabilities = "io.modelcontextprotocol/clientCapabilities"
	MetaServerInfo         = "io.modelcontextprotocol/serverInfo"
)

// JSON-RPC and MCP error codes.
const (
	CodeParseError                 = -32700
	CodeInvalidRequest             = -32600
	CodeMethodNotFound             = -32601
	CodeInvalidParams              = -32602
	CodeInternal                   = -32603
	CodeHeaderMismatch             = -32020
	CodeMissingClientCapability    = -32021
	CodeUnsupportedProtocolVersion = -32022
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func (r *rpcRequest) isNotification() bool { return len(r.ID) == 0 || string(r.ID) == "null" }

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// requestMeta is the per-request metadata common to every modern request.
type requestMeta struct {
	ProtocolVersion    string                     `json:"io.modelcontextprotocol/protocolVersion"`
	ClientCapabilities map[string]json.RawMessage `json:"io.modelcontextprotocol/clientCapabilities"`
}

type baseParams struct {
	Meta *requestMeta `json:"_meta"`
	Name string       `json:"name"`
	URI  string       `json:"uri"`
}

// decodeHeaderValue undoes the "=?base64?...?=" sentinel used for non-ASCII header values.
func decodeHeaderValue(v string) (string, bool) {
	if strings.HasPrefix(v, "=?base64?") && strings.HasSuffix(v, "?=") && len(v) >= len("=?base64??=") {
		b, err := base64.StdEncoding.DecodeString(v[len("=?base64?") : len(v)-2])
		if err != nil {
			return "", false
		}
		return string(b), true
	}
	for _, r := range v {
		if r < 0x20 || r > 0x7e {
			return "", false
		}
	}
	return v, true
}

// Tool is a tool definition as returned by tools/list.
type Tool struct {
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations,omitempty"`
}

// textResult is a completed tool result: human/LLM-readable text plus structured content.
func textResult(text string, structured any, isError bool) map[string]any {
	r := map[string]any{
		"resultType": "complete",
		"content":    []map[string]any{{"type": "text", "text": text}},
		"isError":    isError,
	}
	if structured != nil {
		r["structuredContent"] = structured
	}
	return r
}
