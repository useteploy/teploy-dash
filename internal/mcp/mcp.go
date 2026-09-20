package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
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

// tokenCtxKey carries the verified token on the context handed to Tool.Run,
// so mutating backends can attribute their operations to the API principal
// that requested them (A27) without widening every Tool signature.
type tokenCtxKey struct{}

// WithToken attaches a verified token to a tool-invocation context.
func WithToken(ctx context.Context, tok Token) context.Context {
	return context.WithValue(ctx, tokenCtxKey{}, tok)
}

// TokenFromContext returns the verified token for the current tool call, if
// any (absent when a tool runs outside the HTTP handler — e.g. tests).
func TokenFromContext(ctx context.Context) (Token, bool) {
	tok, ok := ctx.Value(tokenCtxKey{}).(Token)
	return tok, ok
}

// ServeHTTP implements the /api/mcp endpoint. Auth is enforced here (bearer
// token) — the route is exempt from the dashboard's session gate.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed — MCP endpoint accepts POST only", http.StatusMethodNotAllowed)
		return
	}

	// Origin validation per the MCP Streamable HTTP transport spec: a browser
	// that sends an Origin must be same-origin (DNS-rebinding and cross-site
	// protection). Non-browser clients typically omit the header and pass.
	// R05: the comparison is on the FULL origin (scheme+host+port,
	// normalized) — the previous host-only check accepted http:// against
	// https:// for the same host.
	if origin := r.Header.Get("Origin"); origin != "" {
		if !sameOrigin(r) {
			http.Error(w, "cross-origin MCP requests are not permitted", http.StatusForbidden)
			return
		}
	}

	// A42: a client that pins a protocol version gets it validated. The
	// header is optional for the initial handshake (the version is then
	// negotiated in initialize params), but a PRESENT unknown version is
	// rejected rather than silently answered with our latest.
	if version := r.Header.Get("MCP-Protocol-Version"); version != "" && !supportedProtocols[version] {
		http.Error(w, "unsupported MCP protocol version", http.StatusBadRequest)
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
	// A42: validate the ID shape MCP allows (string or number). A present
	// but null/object/array/bool ID is an invalid request, not a
	// notification; a genuinely absent ID is the notification path.
	if len(req.ID) != 0 {
		if !validRPCID(req.ID) {
			writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{Code: -32600, Message: "invalid request: id must be a string or a number"}})
			return
		}
	} else {
		// Notifications (no id) get a bare 202 per streamable-HTTP.
		w.WriteHeader(http.StatusAccepted)
		return
	}

	resp := h.dispatch(r, req, tok)
	writeRPC(w, resp)
}

// validRPCID reports whether a raw request ID matches JSON-RPC 2.0's string
// or number contract (A42). The raw bytes are preserved verbatim in the
// response — no lossy number conversion.
func validRPCID(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return false
	}
	switch trimmed[0] {
	case '"':
		return json.Valid(trimmed)
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		var n json.Number
		return json.Unmarshal(trimmed, &n) == nil
	}
	return false
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
		// R54: malformed initialize params are an invalid-params error, not
		// a silently successful negotiation over garbage (an unparseable
		// protocolVersion used to fall through to "use latest").
		if len(req.Params) > 0 {
			if err := json.Unmarshal(req.Params, &params); err != nil {
				resp.Error = &rpcError{Code: -32602, Message: "invalid params"}
				return resp
			}
		}
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
		// A41: arguments are kept RAW until they have been validated against
		// the advertised schema's top-level fields — the old map-decode path
		// silently ignored unknown fields (an unrecognized `dry_run: true`
		// beside a real deploy request) and defaulted nulls.
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			resp.Error = &rpcError{Code: -32602, Message: "invalid params"}
			return resp
		}
		if err := validateToolArguments(h.tools, params.Name, params.Arguments); err != nil {
			resp.Error = &rpcError{Code: -32602, Message: err.Error()}
			return resp
		}
		var args map[string]interface{}
		if len(params.Arguments) > 0 {
			if err := json.Unmarshal(params.Arguments, &args); err != nil {
				resp.Error = &rpcError{Code: -32602, Message: "arguments must be an object"}
				return resp
			}
		}
		resp.Result = h.callTool(r, params.Name, args, tok)

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
		out, err := t.Run(WithToken(r.Context(), tok), args)
		if err != nil {
			return toolError(err.Error())
		}
		return map[string]interface{}{
			"content": []map[string]string{{"type": "text", "text": out}},
		}
	}
	return toolError(fmt.Sprintf("unknown tool: %s", name))
}

// validateToolArguments enforces the top-level argument contract a tool's
// schema advertises (additionalProperties: false) BEFORE any side effect
// (A41): unknown fields and null values are rejected instead of ignored.
// R54: each PRESENT field must also match its advertised type — the old
// check let a boolean/object "domain" through to a discarded string
// assertion, silently queuing a deploy without the operator's value.
func validateToolArguments(tools []Tool, name string, raw json.RawMessage) error {
	var tool *Tool
	for i := range tools {
		if tools[i].Name == name {
			tool = &tools[i]
			break
		}
	}
	if tool == nil {
		return nil // unknown tool: callTool reports it as a tool error
	}
	if len(raw) == 0 {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return fmt.Errorf("arguments must be an object")
	}
	allowed := map[string]bool{}
	types := map[string]string{}
	if props, ok := tool.InputSchema["properties"].(map[string]interface{}); ok {
		for key, def := range props {
			allowed[key] = true
			if prop, ok := def.(map[string]interface{}); ok {
				if t, ok := prop["type"].(string); ok {
					types[key] = t
				}
			}
		}
	}
	for key, value := range fields {
		if !allowed[key] {
			return fmt.Errorf("unknown argument %q for tool %s", key, name)
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("argument %q must not be null", key)
		}
		if expected := types[key]; expected != "" {
			if err := validateScalarType(value, expected); err != nil {
				return fmt.Errorf("argument %q for tool %s %s", key, name, err)
			}
		}
	}
	return nil
}

// validateScalarType checks a raw JSON value against an advertised schema
// type (R54). Only the scalar types the tool schemas use are enforced;
// anything else is left to the tool's own arg parsing.
func validateScalarType(raw json.RawMessage, expected string) error {
	trimmed := bytes.TrimSpace(raw)
	switch expected {
	case "string":
		var value string
		if err := json.Unmarshal(trimmed, &value); err != nil {
			return errors.New("must be a string")
		}
	case "integer":
		var value int64
		if err := json.Unmarshal(trimmed, &value); err != nil {
			return errors.New("must be an integer")
		}
	case "number":
		var value float64
		if err := json.Unmarshal(trimmed, &value); err != nil {
			return errors.New("must be a number")
		}
	case "boolean":
		var value bool
		if err := json.Unmarshal(trimmed, &value); err != nil {
			return errors.New("must be a boolean")
		}
	}
	return nil
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

// sameOrigin compares the request's Origin against its own scheme+host with
// default ports normalized (R05). Self-contained twin of the server
// package's check — the MCP handler must not depend on the session stack.
func sameOrigin(r *http.Request) bool {
	u, err := url.Parse(r.Header.Get("Origin"))
	if err != nil {
		return false
	}
	got, err := originKey(u)
	if err != nil {
		return false
	}
	expected, err := originKey(&url.URL{Scheme: requestScheme(r), Host: r.Host})
	if err != nil {
		return false
	}
	return got == expected
}

func originKey(u *url.URL) (string, error) {
	scheme := strings.ToLower(u.Scheme)
	if (scheme != "http" && scheme != "https") || u.Hostname() == "" ||
		u.User != nil || u.Opaque != "" || (u.Path != "" && u.Path != "/") ||
		u.RawQuery != "" || u.Fragment != "" || u.RawFragment != "" {
		return "", fmt.Errorf("invalid origin")
	}
	port := u.Port()
	if port == "" {
		if scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("invalid origin port")
	}
	return scheme + "://" + net.JoinHostPort(strings.ToLower(u.Hostname()), strconv.Itoa(n)), nil
}

func requestScheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	return "http"
}
