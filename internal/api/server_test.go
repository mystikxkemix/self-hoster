package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"selfhoster/internal/settings"
	"selfhoster/internal/store"
	"selfhoster/internal/supervisor"
	"selfhoster/internal/testutil"
)

const (
	testUser     = "admin"
	testPassword = "pw"
)

type fixture struct {
	t        *testing.T
	store    *store.Store
	sup      *supervisor.Supervisor
	settings *settings.Manager
	srv      *httptest.Server
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dataDir := t.TempDir()
	st, err := store.Open(filepath.Join(dataDir, "config.json"), store.Options{ReservedPort: 8080, MinPort: 4000, MaxPort: 4019})
	if err != nil {
		t.Fatal(err)
	}
	sup := supervisor.New(dataDir, filepath.Join(dataDir, "apps"))
	set, err := settings.Open(filepath.Join(dataDir, "settings.json"), settings.Defaults{
		AdminUser: testUser, AdminPassword: testPassword, PollInterval: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	info := Info{DataDir: dataDir, AppsDir: filepath.Join(dataDir, "apps"), Ports: "4000-4019"}
	srv := httptest.NewServer(New(st, sup, set, info))
	t.Cleanup(func() {
		srv.Close()
		sup.Shutdown()
	})
	return &fixture{t: t, store: st, sup: sup, settings: set, srv: srv}
}

// do sends an authenticated request and returns the status code and raw body.
func (f *fixture) do(method, path, body string) (int, string) {
	f.t.Helper()
	return f.doAs(testUser, testPassword, method, path, body)
}

func (f *fixture) doAs(user, password, method, path, body string) (int, string) {
	f.t.Helper()
	req, err := http.NewRequest(method, f.srv.URL+path, strings.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	if user != "" || password != "" {
		req.SetBasicAuth(user, password)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(data)
}

// create registers a valid service project and fails the test otherwise.
func (f *fixture) create(name, repo, extra string) {
	f.t.Helper()
	body := `{"name":"` + name + `","repo":"` + repo + `","startCmd":"exec sleep 60","port":4001` + extra + `}`
	if code, resp := f.do("POST", "/api/projects", body); code != http.StatusCreated {
		f.t.Fatalf("create: %d %s", code, resp)
	}
}

func decodeJSON[T any](t *testing.T, body string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	return v
}

func expect(t *testing.T, gotCode int, body string, wantCode int, wantInBody string) {
	t.Helper()
	if gotCode != wantCode {
		t.Fatalf("status = %d, want %d (body %s)", gotCode, wantCode, body)
	}
	if !strings.Contains(body, wantInBody) {
		t.Fatalf("body = %s, want it to contain %q", body, wantInBody)
	}
}

func waitDeployed(t *testing.T, sup *supervisor.Supervisor, name string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for sup.Status(name).Deploying {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for deploy")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestBasicAuth(t *testing.T) {
	f := newFixture(t)
	tests := []struct {
		name, user, password string
		want                 int
	}{
		{"no credentials", "", "", http.StatusUnauthorized},
		{"wrong password", testUser, "nope", http.StatusUnauthorized},
		{"wrong user", "root", testPassword, http.StatusUnauthorized},
		{"valid", testUser, testPassword, http.StatusOK},
	}
	for _, tc := range tests {
		for _, path := range []string{"/", "/api/projects"} {
			t.Run(tc.name+" "+path, func(t *testing.T) {
				code, _ := f.doAs(tc.user, tc.password, "GET", path, "")
				if code != tc.want {
					t.Fatalf("status = %d, want %d", code, tc.want)
				}
			})
		}
	}

	req, _ := http.NewRequest("GET", f.srv.URL+"/api/projects", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if got := res.Header.Get("WWW-Authenticate"); !strings.HasPrefix(got, "Basic") {
		t.Fatalf("WWW-Authenticate = %q, want a Basic challenge so browsers prompt", got)
	}
}

// Unknown /api paths answer JSON, whether or not the web app was built, so API
// clients never receive the UI page.
func TestUnknownAPIRouteIsJSON404(t *testing.T) {
	f := newFixture(t)
	for _, path := range []string{"/api/", "/api/nope", "/api/projects/x/y/z", "/api/v2/projects"} {
		for _, method := range []string{"GET", "POST", "DELETE"} {
			code, body := f.do(method, path, "")
			expect(t, code, body, http.StatusNotFound, `"error":"unknown API route"`)
		}
	}
	code, _ := f.doAs("", "", "GET", "/api/nope", "")
	if code != http.StatusUnauthorized {
		t.Fatalf("unknown route without auth = %d, want 401 (auth first)", code)
	}
}

func TestUIIsServed(t *testing.T) {
	f := newFixture(t)
	code, body := f.do("GET", "/", "")
	expect(t, code, body, http.StatusOK, "<title>self-hoster</title>")
}

func TestCreateProject(t *testing.T) {
	f := newFixture(t)
	valid := `{"name":"api","repo":"https://example.com/r.git","startCmd":"node index.js","port":4001}`
	tests := []struct {
		name       string
		body       string
		wantCode   int
		wantInBody string
	}{
		{"valid", valid, http.StatusCreated, `"enabled":false`},
		{"duplicate name", valid, http.StatusConflict, "already exists"},
		{"unknown field", `{"name":"x","repo":"r","startCmd":"s","port":4002,"command":"oops"}`, http.StatusBadRequest, "unknown field"},
		{"status is not writable", `{"name":"x","repo":"r","startCmd":"s","port":4002,"status":{}}`, http.StatusBadRequest, "unknown field"},
		{"malformed json", `{"name":`, http.StatusBadRequest, "error"},
		{"validation error", `{"name":"Bad Name","repo":"r","startCmd":"s","port":4002}`, http.StatusBadRequest, "name must be"},
		{"port outside range", `{"name":"x","repo":"r","startCmd":"s","port":9000}`, http.StatusBadRequest, "published range"},
		{"port already used", `{"name":"x","repo":"r","startCmd":"s","port":4001}`, http.StatusBadRequest, "already used"},
		{"body too large", `{"name":"x","repo":"` + strings.Repeat("a", maxBodyBytes) + `"}`, http.StatusBadRequest, "too large"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, body := f.do("POST", "/api/projects", tc.body)
			expect(t, code, body, tc.wantCode, tc.wantInBody)
		})
	}
}

func TestCreateCannotEnableProject(t *testing.T) {
	f := newFixture(t)
	code, body := f.do("POST", "/api/projects", `{"name":"api","repo":"r","startCmd":"s","port":4001,"enabled":true}`)
	expect(t, code, body, http.StatusCreated, "")
	if p, _ := f.store.Get("api"); p.Enabled {
		t.Fatal("a new project must start disabled until it is deployed")
	}
}

func TestGetAndListProjects(t *testing.T) {
	f := newFixture(t)
	f.create("api", "r", "")

	code, body := f.do("GET", "/api/projects", "")
	expect(t, code, body, http.StatusOK, "")
	list := decodeJSON[[]map[string]any](t, body)
	if len(list) != 1 || list[0]["name"] != "api" || list[0]["status"] == nil {
		t.Fatalf("list = %v, want one project with its status", list)
	}

	code, body = f.do("GET", "/api/projects/api", "")
	expect(t, code, body, http.StatusOK, `"state":"stopped"`)
	if view := decodeJSON[map[string]any](t, body); view["dataDir"] != f.sup.AppDir("api") {
		t.Fatalf("dataDir = %v, want %s", view["dataDir"], f.sup.AppDir("api"))
	}

	code, body = f.do("GET", "/api/projects/missing", "")
	expect(t, code, body, http.StatusNotFound, "not found")
}

func TestUpdateProject(t *testing.T) {
	f := newFixture(t)
	f.create("api", "r", "")
	if err := f.store.Put(func() store.Project { p, _ := f.store.Get("api"); p.Enabled = true; return p }()); err != nil {
		t.Fatal(err)
	}

	// The name comes from the path and enabled is preserved, whatever the body says.
	code, body := f.do("PUT", "/api/projects/api",
		`{"name":"renamed","repo":"r2","startCmd":"s","port":4002,"env":{"A":"1"},"enabled":false}`)
	expect(t, code, body, http.StatusOK, `"repo":"r2"`)
	p, err := f.store.Get("api")
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "api" || !p.Enabled || p.Port != 4002 || p.Env["A"] != "1" {
		t.Fatalf("stored = %+v", p)
	}
	if _, err := f.store.Get("renamed"); err == nil {
		t.Fatal("PUT must not rename a project")
	}

	code, body = f.do("PUT", "/api/projects/api", `{"repo":"r","startCmd":"s","port":4002,"oops":1}`)
	expect(t, code, body, http.StatusBadRequest, "unknown field")

	code, body = f.do("PUT", "/api/projects/api", `{"repo":"","startCmd":"s","port":4002}`)
	expect(t, code, body, http.StatusBadRequest, "repo is required")

	code, body = f.do("PUT", "/api/projects/missing", `{"repo":"r","startCmd":"s","port":4003}`)
	expect(t, code, body, http.StatusNotFound, "not found")
}

func TestLifecycleActions(t *testing.T) {
	f := newFixture(t)
	f.create("api", testutil.NewRepo(t, map[string]string{"README": "x"}), `,"buildCmd":"sleep 1"`)

	code, body := f.do("POST", "/api/projects/api/start", "")
	expect(t, code, body, http.StatusBadRequest, "not been deployed")

	code, body = f.do("POST", "/api/projects/api/deploy", "")
	expect(t, code, body, http.StatusAccepted, `"enabled":true`)

	code, body = f.do("POST", "/api/projects/api/deploy", "")
	expect(t, code, body, http.StatusConflict, "already in progress")

	code, body = f.do("DELETE", "/api/projects/api", "")
	expect(t, code, body, http.StatusConflict, "already in progress")

	waitDeployed(t, f.sup, "api")
	code, body = f.do("GET", "/api/projects/api", "")
	expect(t, code, body, http.StatusOK, `"state":"running"`)

	code, body = f.do("POST", "/api/projects/api/restart", "")
	expect(t, code, body, http.StatusOK, `"state":"running"`)

	code, body = f.do("POST", "/api/projects/api/stop", "")
	expect(t, code, body, http.StatusOK, `"state":"stopped"`)
	if p, _ := f.store.Get("api"); p.Enabled {
		t.Fatal("stop must persist enabled=false so the project stays down after a reboot")
	}

	code, body = f.do("POST", "/api/projects/api/start", "")
	expect(t, code, body, http.StatusOK, `"state":"running"`)
	if p, _ := f.store.Get("api"); !p.Enabled {
		t.Fatal("start must persist enabled=true")
	}

	code, body = f.do("GET", "/api/projects/api/logs", "")
	expect(t, code, body, http.StatusOK, "deploy finished")

	code, body = f.do("DELETE", "/api/projects/api", "")
	expect(t, code, body, http.StatusNoContent, "")
	code, body = f.do("GET", "/api/projects/api", "")
	expect(t, code, body, http.StatusNotFound, "")
}

func TestActionsOnUnknownProject(t *testing.T) {
	f := newFixture(t)
	for _, route := range []string{"deploy", "start", "stop", "restart"} {
		code, body := f.do("POST", "/api/projects/missing/"+route, "")
		expect(t, code, body, http.StatusNotFound, "not found")
	}
	for _, method := range []string{"GET", "DELETE"} {
		path := "/api/projects/missing"
		if method == "GET" {
			path += "/logs"
		}
		code, body := f.do(method, path, "")
		expect(t, code, body, http.StatusNotFound, "not found")
	}
}

// Actions are POST-only, so following a link (GET) can never trigger a deploy.
func TestActionsIgnoreGet(t *testing.T) {
	f := newFixture(t)
	f.create("api", "r", "")
	for _, route := range []string{"deploy", "start", "stop", "restart"} {
		code, body := f.do("GET", "/api/projects/api/"+route, "")
		expect(t, code, body, http.StatusNotFound, `"error":"unknown API route"`)
	}
	if st := f.sup.Status("api"); st.Deploying || st.State != supervisor.StateStopped {
		t.Fatalf("status = %+v, a GET must not change anything", st)
	}
	if p, _ := f.store.Get("api"); p.Enabled {
		t.Fatal("a GET must not enable the project")
	}
}

func TestSettingsRoutes(t *testing.T) {
	f := newFixture(t)

	code, body := f.do("GET", "/api/settings", "")
	expect(t, code, body, http.StatusOK, `"adminUser":"admin"`)
	expect(t, code, body, http.StatusOK, `"ports":"4000-4019"`)
	if strings.Contains(body, "assword\":\"") || strings.Contains(body, testPassword) {
		t.Fatalf("settings leak the password: %s", body)
	}

	code, body = f.do("PUT", "/api/settings", `{"adminUser":"admin","pollInterval":"1s","linkHost":""}`)
	expect(t, code, body, http.StatusBadRequest, "at least 5s")
	code, body = f.do("PUT", "/api/settings", `{"adminUser":"admin","oops":true}`)
	expect(t, code, body, http.StatusBadRequest, "unknown field")

	code, body = f.do("PUT", "/api/settings", `{"adminUser":"admin","pollInterval":"30s","linkHost":"zima.local"}`)
	expect(t, code, body, http.StatusOK, `"linkHost":"zima.local"`)
	if f.settings.PollInterval() != 30*time.Second {
		t.Fatalf("interval not applied: %s", f.settings.PollInterval())
	}

	code, body = f.do("POST", "/api/settings/password", `{"current":"nope","new":"new-password-1"}`)
	expect(t, code, body, http.StatusForbidden, "incorrect")
	code, body = f.do("POST", "/api/settings/password", `{"current":"pw","new":"short"}`)
	expect(t, code, body, http.StatusBadRequest, "at least 8")
	code, body = f.do("POST", "/api/settings/password", `{"current":"pw","new":"new-password-1"}`)
	expect(t, code, body, http.StatusNoContent, "")

	// The old password stops working immediately, the new one works.
	if code, _ := f.do("GET", "/api/settings", ""); code != http.StatusUnauthorized {
		t.Fatalf("old password still accepted: %d", code)
	}
	code, body = f.doAs(testUser, "new-password-1", "GET", "/api/settings", "")
	expect(t, code, body, http.StatusOK, `"passwordSource":"settings"`)
}

func TestSettingsUsernameChangeRequiresNewLogin(t *testing.T) {
	f := newFixture(t)
	code, body := f.do("PUT", "/api/settings", `{"adminUser":"kemix","pollInterval":"","linkHost":""}`)
	expect(t, code, body, http.StatusOK, `"adminUser":"kemix"`)
	if code, _ := f.do("GET", "/api/projects", ""); code != http.StatusUnauthorized {
		t.Fatalf("old username still accepted: %d", code)
	}
	code, body = f.doAs("kemix", testPassword, "GET", "/api/projects", "")
	expect(t, code, body, http.StatusOK, "")
}

func TestProjectPublicURLValidation(t *testing.T) {
	f := newFixture(t)
	code, body := f.do("POST", "/api/projects", `{"name":"a","repo":"r","startCmd":"s","port":4001,"url":"ftp://x"}`)
	expect(t, code, body, http.StatusBadRequest, "public URL")
	code, body = f.do("POST", "/api/projects", `{"name":"a","repo":"r","startCmd":"s","port":4001,"url":"https://budget.example.com"}`)
	expect(t, code, body, http.StatusCreated, `"url":"https://budget.example.com"`)
}
