package mcp

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

// Hand-rolled JSON-RPC 2.0 over streamable HTTP (request/response only — GET
// returns 405, no server push). Stateless: no sessions, every POST carries a
// bearer token. Kept dependency-free on purpose; the protocol surface needed
// (initialize, ping, tools/list, tools/call) is small.

const latestProtocol = "2025-06-18"

var supportedProtocols = map[string]bool{
	"2025-06-18": true,
	"2025-03-26": true,
	"2024-11-05": true,
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  interface{}     `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// Handler serves the MCP endpoint.
type Handler struct {
	tokens  *TokenStore
	tools   []Tool
	version string // dash version for serverInfo
}

// NewHandler builds the MCP handler over a token store and tool set.
func NewHandler(tokens *TokenStore, tools []Tool, version string) *Handler {
	return &Handler{tokens: tokens, tools: tools, version: version}
}

// ServeHTTP implements the /api/mcp endpoint. Auth is enforced here (bearer
// token) — the route is exempt from the dashboard's session gate.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed — MCP endpoint accepts POST only", http.StatusMethodNotAllowed)
		return
	}

	tok, ok := h.authenticate(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="teploy-dash MCP"`)
		http.Error(w, "unauthorized: create an MCP token in dash Settings and send it as a Bearer token", http.StatusUnauthorized)
		return
	}

	// DASH-012: unknown top-level fields are tolerated (JSON-RPC extensions
	// legitimately add them), but the body must decode to exactly one JSON
	// value and declare jsonrpc 2.0 — a second concatenated object or a
	// missing/wrong version previously passed straight through to dispatch.
	dec := json.NewDecoder(r.Body)
	var req rpcRequest
	if err := dec.Decode(&req); err != nil {
		writeRPC(w, rpcResponse{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: "parse error"}})
		return
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32700, Message: "parse error: request body must contain exactly one JSON value"}})
		return
	}
	if req.JSONRPC != "2.0" {
		writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32600, Message: `invalid request: "jsonrpc" must be "2.0"`}})
		return
	}

	// Notifications (no id) get a bare 202 per streamable-HTTP.
	if len(req.ID) == 0 || string(req.ID) == "null" {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	resp := h.dispatch(r, req, tok)
	writeRPC(w, resp)
}

func (h *Handler) authenticate(r *http.Request) (Token, bool) {
	auth := r.Header.Get("Authorization")
	const scheme = "Bearer "
	if !strings.HasPrefix(auth, scheme) {
		return Token{}, false
	}
	return h.tokens.Verify(strings.TrimSpace(strings.TrimPrefix(auth, scheme)))
}

func (h *Handler) dispatch(r *http.Request, req rpcRequest, tok Token) rpcResponse {
	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}

	switch req.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &params)
		version := latestProtocol
		if supportedProtocols[params.ProtocolVersion] {
			version = params.ProtocolVersion
		}
		resp.Result = map[string]interface{}{
			"protocolVersion": version,
			"capabilities":    map[string]interface{}{"tools": map[string]interface{}{}},
			"serverInfo":      map[string]string{"name": "teploy-dash", "version": h.version},
			"instructions": "Teploy deployment dashboard. Reads come from the server state files the " +
				"teploy CLI writes; actions run through the same CLI the dashboard uses, so there is " +
				"no second source of truth to drift. Destructive tools are marked; read-only tokens " +
				"only see read tools.",
		}

	case "ping":
		resp.Result = map[string]interface{}{}

	case "tools/list":
		visible := make([]map[string]interface{}, 0, len(h.tools))
		for _, t := range h.tools {
			if tok.ReadOnly && !t.ReadOnly {
				continue
			}
			visible = append(visible, map[string]interface{}{
				"name":        t.Name,
				"description": t.Description,
				"inputSchema": t.InputSchema,
				"annotations": map[string]bool{
					"readOnlyHint":    t.ReadOnly,
					"destructiveHint": t.Destructive,
				},
			})
		}
		resp.Result = map[string]interface{}{"tools": visible}

	case "tools/call":
		var params struct {
			Name      string                 `json:"name"`
			Arguments map[string]interface{} `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			resp.Error = &rpcError{Code: -32602, Message: "invalid params"}
			return resp
		}
		resp.Result = h.callTool(r, params.Name, params.Arguments, tok)

	default:
		resp.Error = &rpcError{Code: -32601, Message: fmt.Sprintf("method not found: %s", req.Method)}
	}
	return resp
}

// callTool runs one tool and always returns an MCP tool result (tool-level
// failures are isError results, not protocol errors).
func (h *Handler) callTool(r *http.Request, name string, args map[string]interface{}, tok Token) map[string]interface{} {
	for _, t := range h.tools {
		if t.Name != name {
			continue
		}
		// Enforcement mirrors listing: a read-only token cannot call a
		// mutating tool even if it guesses the name.
		if tok.ReadOnly && !t.ReadOnly {
			return toolError(fmt.Sprintf("token %q is read-only; %s is not permitted", tok.Name, name))
		}
		log.Printf("[mcp] token=%q tool=%s", tok.Name, name)
		out, err := t.Run(r.Context(), args)
		if err != nil {
			return toolError(err.Error())
		}
		return map[string]interface{}{
			"content": []map[string]string{{"type": "text", "text": out}},
		}
	}
	return toolError(fmt.Sprintf("unknown tool: %s", name))
}

func toolError(msg string) map[string]interface{} {
	return map[string]interface{}{
		"content": []map[string]string{{"type": "text", "text": msg}},
		"isError": true,
	}
}

func writeRPC(w http.ResponseWriter, resp rpcResponse) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
