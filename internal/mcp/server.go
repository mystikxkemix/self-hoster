// Package mcp exposes self-hoster to MCP clients (Claude) over the Streamable HTTP
// transport, behind the OAuth provider in internal/oauth.
//
// The server is stateless: every POST carries one JSON-RPC message and gets a plain
// JSON response (no SSE stream, no session id), which the transport allows and is
// enough for request/response tools.
package mcp

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"slices"

	"selfhoster/internal/oauth"
	"selfhoster/internal/projects"
	"selfhoster/internal/store"
	"selfhoster/internal/supervisor"
)

const maxBodyBytes = 1 << 20

// supportedVersions are the MCP protocol revisions this server speaks, newest first.
var supportedVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26"}

const instructions = `self-hoster runs personal projects (services and static websites) on a home server.
Each project is cloned from git, built, and run on its own port.
- deploy_project, start_project and restart_project return immediately: a deploy runs in the background. Poll get_project until status.deploying is false, then check status.state, status.lastError and project_logs.
- Services must listen on $PORT. Persistent data belongs in $DATA_DIR, which survives deploys.
- Environment variable values are never shown; use set_env to change them. Saving settings or env restarts a running project.
- Projects cannot be deleted from here; the administrator does that in the web UI.`

// JSON-RPC error codes.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
)

// Server answers MCP requests. It is safe for concurrent use.
type Server struct {
	store    *store.Store
	sup      *supervisor.Supervisor
	projects *projects.Service
	// ports is the allowed project port range ("" = any), shown to the model.
	ports string
	tools []tool
}

// New returns an MCP server operating on the given store and supervisor.
func New(st *store.Store, sup *supervisor.Supervisor, ports string) *Server {
	s := &Server{store: st, sup: sup, projects: projects.New(st, sup), ports: ports}
	s.tools = s.toolset()
	return s
}

// Handler serves the OAuth endpoints and the MCP endpoint they protect.
func Handler(provider *oauth.Provider, s *Server) http.Handler {
	mux := http.NewServeMux()
	provider.Routes(mux)
	mux.Handle(oauth.ResourcePath, provider.Protect(s))
	return mux
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		// No server-initiated stream (GET) and no session to delete (DELETE).
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req rpcRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&req); err != nil {
		writeRPC(w, rpcResponse{ID: json.RawMessage("null"), Error: &rpcError{Code: codeParseError, Message: "invalid JSON-RPC message: " + err.Error()}})
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		writeRPC(w, rpcResponse{ID: idOrNull(req.ID), Error: &rpcError{Code: codeInvalidRequest, Message: "not a JSON-RPC 2.0 request"}})
		return
	}
	// Notifications (no id) and responses get no reply.
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	result, rpcErr := s.dispatch(req)
	writeRPC(w, rpcResponse{ID: req.ID, Result: result, Error: rpcErr})
}

func (s *Server) dispatch(req rpcRequest) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &params)
		version := supportedVersions[0]
		if slices.Contains(supportedVersions, params.ProtocolVersion) {
			version = params.ProtocolVersion
		}
		return map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "self-hoster", "title": "self-hoster", "version": "1.0.0"},
			"instructions":    instructions,
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		list := make([]map[string]any, 0, len(s.tools))
		for _, t := range s.tools {
			list = append(list, t.descriptor())
		}
		return map[string]any{"tools": list}, nil
	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return nil, &rpcError{Code: codeInvalidParams, Message: err.Error()}
		}
		for _, t := range s.tools {
			if t.name == params.Name {
				return s.call(t, params.Arguments), nil
			}
		}
		return nil, &rpcError{Code: codeInvalidParams, Message: "unknown tool " + params.Name}
	default:
		return nil, &rpcError{Code: codeMethodNotFound, Message: "method not found: " + req.Method}
	}
}

// call runs a tool. Failures are tool results with isError, so the model sees them.
func (s *Server) call(t tool, args json.RawMessage) map[string]any {
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage("{}")
	}
	out, err := t.run(args)
	if err != nil {
		log.Printf("mcp: %s: %v", t.name, err)
		return map[string]any{"content": []map[string]any{{"type": "text", "text": err.Error()}}, "isError": true}
	}
	if text, ok := out.(string); ok {
		return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return map[string]any{"content": []map[string]any{{"type": "text", "text": err.Error()}}, "isError": true}
	}
	return map[string]any{
		"content":           []map[string]any{{"type": "text", "text": string(data)}},
		"structuredContent": out,
	}
}

func idOrNull(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return json.RawMessage("null")
	}
	return id
}

func writeRPC(w http.ResponseWriter, resp rpcResponse) {
	resp.JSONRPC = "2.0"
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil && !errors.Is(err, http.ErrHandlerTimeout) {
		log.Printf("mcp: write response: %v", err)
	}
}
