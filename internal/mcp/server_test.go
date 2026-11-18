package mcp

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"selfhoster/internal/store"
	"selfhoster/internal/supervisor"
	"selfhoster/internal/testutil"
)

type fixture struct {
	t     *testing.T
	store *store.Store
	sup   *supervisor.Supervisor
	srv   *httptest.Server
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "config.json"), store.Options{ReservedPort: 8080, MinPort: 4000, MaxPort: 4019})
	if err != nil {
		t.Fatal(err)
	}
	sup := supervisor.New(dir, filepath.Join(dir, "apps"))
	// The tools are tested without OAuth: the flow is covered in internal/oauth and the
	// wiring in internal/api (TestMCPRoutes).
	srv := httptest.NewServer(New(st, sup, "4000-4019"))
	t.Cleanup(func() {
		srv.Close()
		sup.Shutdown()
	})
	return &fixture{t: t, store: st, sup: sup, srv: srv}
}

type rpcResult struct {
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

// rpc sends one JSON-RPC message and returns the HTTP status and decoded reply.
func (f *fixture) rpc(body string) (int, rpcResult) {
	f.t.Helper()
	res, err := http.Post(f.srv.URL, "application/json", strings.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	var out rpcResult
	if len(data) > 0 {
		if err := json.Unmarshal(data, &out); err != nil {
			f.t.Fatalf("decode %q: %v", data, err)
		}
	}
	return res.StatusCode, out
}

type toolResult struct {
	Content []struct {
		Text string `json:"text"`
	} `json:"content"`
	IsError bool `json:"isError"`
}

// call runs a tool and returns its text output and whether it failed.
func (f *fixture) call(name, args string) (string, bool) {
	f.t.Helper()
	_, out := f.rpc(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + name + `","arguments":` + args + `}}`)
	if out.Error != nil {
		f.t.Fatalf("%s: rpc error %+v", name, out.Error)
	}
	var tr toolResult
	if err := json.Unmarshal(out.Result, &tr); err != nil || len(tr.Content) == 0 {
		f.t.Fatalf("%s: bad result %s", name, out.Result)
	}
	return tr.Content[0].Text, tr.IsError
}

func (f *fixture) mustCall(name, args string) string {
	f.t.Helper()
	text, isErr := f.call(name, args)
	if isErr {
		f.t.Fatalf("%s(%s) failed: %s", name, args, text)
	}
	return text
}

func TestInitializeNegotiatesVersion(t *testing.T) {
	f := newFixture(t)
	for requested, want := range map[string]string{"2025-06-18": "2025-06-18", "1999-01-01": supportedVersions[0]} {
		_, out := f.rpc(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"` + requested + `","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
		if !strings.Contains(string(out.Result), `"protocolVersion":"`+want+`"`) || !strings.Contains(string(out.Result), `"tools"`) {
			t.Fatalf("initialize(%s) = %s", requested, out.Result)
		}
	}
}

func TestProtocolEdges(t *testing.T) {
	f := newFixture(t)
	if status, _ := f.rpc(`{"jsonrpc":"2.0","method":"notifications/initialized"}`); status != http.StatusAccepted {
		t.Fatalf("notification status = %d, want 202", status)
	}
	if _, out := f.rpc(`{"jsonrpc":"2.0","id":2,"method":"resources/list"}`); out.Error == nil || out.Error.Code != codeMethodNotFound {
		t.Fatalf("unknown method = %+v", out.Error)
	}
	if _, out := f.rpc(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"delete_project","arguments":{}}}`); out.Error == nil || out.Error.Code != codeInvalidParams {
		t.Fatalf("unknown tool = %+v", out.Error)
	}
	if _, out := f.rpc(`not json`); out.Error == nil || out.Error.Code != codeParseError {
		t.Fatalf("parse error = %+v", out.Error)
	}
	res, err := http.Get(f.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET = %d, want 405", res.StatusCode)
	}
}

func TestToolsListHasNoDelete(t *testing.T) {
	f := newFixture(t)
	_, out := f.rpc(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	var list struct {
		Tools []struct {
			Name        string         `json:"name"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(out.Result, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Tools) != 10 {
		t.Fatalf("got %d tools", len(list.Tools))
	}
	for _, tl := range list.Tools {
		if strings.Contains(tl.Name, "delete") || tl.InputSchema["type"] != "object" {
			t.Fatalf("unexpected tool %+v", tl)
		}
	}
}

func TestCreateEditAndHideSecrets(t *testing.T) {
	f := newFixture(t)
	f.mustCall("create_project", `{"name":"api","repo":"https://ghp_TOKEN@github.com/me/api.git","startCmd":"node index.js","port":4001,"env":{"MONGO_URL":"mongodb://u:pw@db"}}`)

	got := f.mustCall("get_project", `{"name":"api"}`)
	for _, leak := range []string{"ghp_TOKEN", "u:pw"} {
		if strings.Contains(got, leak) {
			t.Fatalf("get_project leaks %q: %s", leak, got)
		}
	}
	for _, want := range []string{`"envKeys": [`, `"MONGO_URL"`, `"autoDeploy": true`, `"enabled": false`, "https://***@github.com"} {
		if !strings.Contains(got, want) {
			t.Fatalf("get_project lacks %s: %s", want, got)
		}
	}

	// Patch semantics: only the given fields change; the real repo token is kept.
	f.mustCall("update_project", `{"name":"api","branch":"dev","autoDeploy":false}`)
	p, _ := f.store.Get("api")
	if p.Branch != "dev" || p.AutoDeploy || p.StartCmd != "node index.js" || p.Repo != "https://ghp_TOKEN@github.com/me/api.git" {
		t.Fatalf("after update: %+v", p)
	}

	f.mustCall("set_env", `{"name":"api","set":{"API_KEY":"k"},"unset":["MONGO_URL"]}`)
	p, _ = f.store.Get("api")
	if len(p.Env) != 1 || p.Env["API_KEY"] != "k" {
		t.Fatalf("env after set_env: %v", p.Env)
	}

	failures := []struct{ tool, args, want string }{
		{"update_project", `{"name":"api","repo":"https://***@github.com/me/api.git"}`, "redacted"},
		{"update_project", `{"name":"api","command":"x"}`, "unknown field"},
		{"update_project", `{"name":"api","port":8080}`, "reserved for the admin UI"},
		{"set_env", `{"name":"api","set":{"PORT":"1"}}`, "PORT"},
		{"set_env", `{"name":"api"}`, "nothing to change"},
		{"create_project", `{"name":"api","repo":"r","startCmd":"s","port":4002}`, "already exists"},
		{"get_project", `{"name":"nope"}`, "not found"},
	}
	for _, tc := range failures {
		text, isErr := f.call(tc.tool, tc.args)
		if !isErr || !strings.Contains(text, tc.want) {
			t.Fatalf("%s(%s) = %q (isError %v), want error containing %q", tc.tool, tc.args, text, isErr, tc.want)
		}
	}
}

func TestDeployStopAndLogs(t *testing.T) {
	f := newFixture(t)
	repo := testutil.NewRepo(t, map[string]string{"README": "x"})
	f.mustCall("create_project", `{"name":"svc","repo":"`+repo+`","buildCmd":"echo building","startCmd":"exec sleep 60","port":4003}`)
	f.mustCall("deploy_project", `{"name":"svc"}`)

	deadline := time.Now().Add(15 * time.Second)
	for f.sup.Status("svc").Deploying || f.sup.Status("svc").State != supervisor.StateRunning {
		if time.Now().After(deadline) {
			t.Fatalf("not running: %+v", f.sup.Status("svc"))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if p, _ := f.store.Get("svc"); !p.Enabled {
		t.Fatal("deploy did not enable the project")
	}
	if logs := f.mustCall("project_logs", `{"name":"svc","lines":50}`); !strings.Contains(logs, "building") {
		t.Fatalf("logs = %s", logs)
	}
	if list := f.mustCall("list_projects", `{}`); !strings.Contains(list, `"state": "running"`) || !strings.Contains(list, `"allowedPorts": "4000-4019"`) {
		t.Fatalf("list = %s", list)
	}
	f.mustCall("stop_project", `{"name":"svc"}`)
	if p, _ := f.store.Get("svc"); p.Enabled || f.sup.Status("svc").State != supervisor.StateStopped {
		t.Fatal("stop did not stop and disable the project")
	}
}
