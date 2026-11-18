package oauth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testUser     = "admin"
	testPassword = "secret-pw"
	testVerifier = "a-pkce-verifier-that-is-long-enough-to-be-valid-0123456789"
	testRedirect = "https://claude.ai/api/mcp/auth_callback"
	// testPublicURL is fixed so tokens stay valid when the fixture restarts on a new port.
	testPublicURL = "https://mcp.example.com"
)

type fakeAuth struct{}

func (fakeAuth) Authenticate(user, password string) bool {
	return user == testUser && password == testPassword
}

// clock is a settable test clock.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type fixture struct {
	t     *testing.T
	p     *Provider
	srv   *httptest.Server
	clock *clock
	path  string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, clock: &clock{now: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)}, path: filepath.Join(t.TempDir(), "oauth.json")}
	f.open()
	return f
}

// open (re)loads the provider from disk and serves it, like a restart.
func (f *fixture) open() {
	f.t.Helper()
	if f.srv != nil {
		f.srv.Close()
	}
	p, err := Open(Config{Path: f.path, Auth: fakeAuth{}, Now: f.clock.Now, PublicURL: testPublicURL})
	if err != nil {
		f.t.Fatal(err)
	}
	mux := http.NewServeMux()
	p.Routes(mux)
	mux.Handle(ResourcePath, p.Protect(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "protected")
	})))
	f.p, f.srv = p, httptest.NewServer(mux)
	f.t.Cleanup(f.srv.Close)
}

// noRedirect returns redirects as responses, so the test can read Location.
var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func (f *fixture) send(req *http.Request) (*http.Response, string) {
	f.t.Helper()
	res, err := noRedirect.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return res, string(body)
}

func (f *fixture) postForm(path string, form url.Values) (*http.Response, string) {
	f.t.Helper()
	req, _ := http.NewRequest("POST", f.srv.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return f.send(req)
}

func (f *fixture) register(body string) map[string]any {
	f.t.Helper()
	req, _ := http.NewRequest("POST", f.srv.URL+"/register", strings.NewReader(body))
	res, data := f.send(req)
	if res.StatusCode != http.StatusCreated {
		f.t.Fatalf("register: %d %s", res.StatusCode, data)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(data), &out); err != nil {
		f.t.Fatal(err)
	}
	return out
}

func (f *fixture) registerPublic() string {
	f.t.Helper()
	out := f.register(`{"client_name":"Claude","redirect_uris":["` + testRedirect + `"],"token_endpoint_auth_method":"none"}`)
	return out["client_id"].(string)
}

func challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func (f *fixture) authForm(clientID string) url.Values {
	return url.Values{
		"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {testRedirect},
		"state": {"xyz"}, "code_challenge": {challenge(testVerifier)}, "code_challenge_method": {"S256"},
		"resource": {testPublicURL + ResourcePath},
	}
}

// authorize signs in on the consent page and returns the authorization code.
func (f *fixture) authorize(clientID string) string {
	f.t.Helper()
	form := f.authForm(clientID)
	form.Set("username", testUser)
	form.Set("password", testPassword)
	form.Set("action", "allow")
	res, body := f.postForm("/authorize", form)
	if res.StatusCode != http.StatusFound {
		f.t.Fatalf("authorize: %d %s", res.StatusCode, body)
	}
	loc, err := url.Parse(res.Header.Get("Location"))
	if err != nil {
		f.t.Fatal(err)
	}
	if loc.Query().Get("state") != "xyz" {
		f.t.Fatalf("state not echoed: %s", loc)
	}
	return loc.Query().Get("code")
}

type tokens struct {
	Access  string `json:"access_token"`
	Refresh string `json:"refresh_token"`
	Error   string `json:"error"`
}

func (f *fixture) token(form url.Values) (int, tokens) {
	f.t.Helper()
	res, body := f.postForm("/token", form)
	var tok tokens
	if err := json.Unmarshal([]byte(body), &tok); err != nil {
		f.t.Fatalf("token response %q: %v", body, err)
	}
	return res.StatusCode, tok
}

func (f *fixture) exchange(clientID, code string) tokens {
	f.t.Helper()
	status, tok := f.token(url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID},
		"redirect_uri": {testRedirect}, "code_verifier": {testVerifier},
	})
	if status != http.StatusOK || tok.Access == "" || tok.Refresh == "" {
		f.t.Fatalf("exchange: %d %+v", status, tok)
	}
	return tok
}

// callMCP calls the protected endpoint with a bearer token.
func (f *fixture) callMCP(access string) (*http.Response, string) {
	f.t.Helper()
	req, _ := http.NewRequest("POST", f.srv.URL+ResourcePath, nil)
	if access != "" {
		req.Header.Set("Authorization", "Bearer "+access)
	}
	return f.send(req)
}

func TestFullFlowAndRefreshRotation(t *testing.T) {
	f := newFixture(t)
	clientID := f.registerPublic()
	tok := f.exchange(clientID, f.authorize(clientID))

	if res, body := f.callMCP(tok.Access); res.StatusCode != http.StatusOK || body != "protected" {
		t.Fatalf("access token rejected: %d %s", res.StatusCode, body)
	}

	status, next := f.token(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok.Refresh}, "client_id": {clientID}})
	if status != http.StatusOK {
		t.Fatalf("refresh: %d %+v", status, next)
	}
	if res, _ := f.callMCP(tok.Access); res.StatusCode != http.StatusUnauthorized {
		t.Fatal("old access token still works after refresh")
	}
	if status, _ := f.token(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok.Refresh}, "client_id": {clientID}}); status != http.StatusBadRequest {
		t.Fatal("old refresh token still works after rotation")
	}
	if res, _ := f.callMCP(next.Access); res.StatusCode != http.StatusOK {
		t.Fatal("new access token rejected")
	}
}

func TestUnauthorizedPointsToMetadata(t *testing.T) {
	f := newFixture(t)
	for _, access := range []string{"", "bogus"} {
		res, _ := f.callMCP(access)
		if res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", res.StatusCode)
		}
		want := `Bearer resource_metadata="` + testPublicURL + `/.well-known/oauth-protected-resource"`
		if got := res.Header.Get("WWW-Authenticate"); !strings.HasPrefix(got, want) {
			t.Fatalf("WWW-Authenticate = %q, want prefix %q", got, want)
		}
	}

	req, _ := http.NewRequest("GET", f.srv.URL+"/.well-known/oauth-protected-resource", nil)
	_, body := f.send(req)
	if !strings.Contains(body, `"resource":"`+testPublicURL+`/mcp"`) {
		t.Fatalf("resource metadata = %s", body)
	}
	req, _ = http.NewRequest("GET", f.srv.URL+"/.well-known/oauth-authorization-server", nil)
	_, body = f.send(req)
	for _, want := range []string{`"issuer":"` + testPublicURL + `"`, `"code_challenge_methods_supported":["S256"]`, `/register"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("server metadata %s lacks %s", body, want)
		}
	}
}

func TestPublicURLOverridesRequestHost(t *testing.T) {
	p, err := Open(Config{Path: filepath.Join(t.TempDir(), "o.json"), Auth: fakeAuth{}, PublicURL: "https://mcp.example.com/"})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "http://10.0.0.2:8091/.well-known/oauth-protected-resource", nil)
	if got := p.resource(r); got != "https://mcp.example.com/mcp" {
		t.Fatalf("resource = %q", got)
	}
	r.Header.Set("X-Forwarded-Proto", "https")
	p.publicURL = ""
	if got := p.base(r); got != "https://10.0.0.2:8091" {
		t.Fatalf("derived base = %q", got)
	}
}

func TestTokenExchangeFailures(t *testing.T) {
	f := newFixture(t)
	clientID := f.registerPublic()
	other := f.registerPublic()
	tests := []struct {
		name   string
		mutate func(url.Values)
	}{
		{"wrong verifier", func(v url.Values) { v.Set("code_verifier", strings.Repeat("x", 50)) }},
		{"wrong redirect", func(v url.Values) { v.Set("redirect_uri", "https://evil.example/cb") }},
		{"other client", func(v url.Values) { v.Set("client_id", other) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			form := url.Values{
				"grant_type": {"authorization_code"}, "code": {f.authorize(clientID)}, "client_id": {clientID},
				"redirect_uri": {testRedirect}, "code_verifier": {testVerifier},
			}
			tc.mutate(form)
			status, tok := f.token(form)
			if status != http.StatusBadRequest || tok.Error != "invalid_grant" {
				t.Fatalf("got %d %+v, want 400 invalid_grant", status, tok)
			}
		})
	}

	t.Run("code is single use", func(t *testing.T) {
		code := f.authorize(clientID)
		f.exchange(clientID, code)
		form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID}, "redirect_uri": {testRedirect}, "code_verifier": {testVerifier}}
		if status, _ := f.token(form); status != http.StatusBadRequest {
			t.Fatal("code accepted twice")
		}
	})
	t.Run("code expires", func(t *testing.T) {
		code := f.authorize(clientID)
		f.clock.Advance(2 * codeTTL)
		form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID}, "redirect_uri": {testRedirect}, "code_verifier": {testVerifier}}
		if status, _ := f.token(form); status != http.StatusBadRequest {
			t.Fatal("expired code accepted")
		}
	})
	t.Run("unknown client", func(t *testing.T) {
		status, tok := f.token(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {"x"}, "client_id": {"nope"}})
		if status != http.StatusUnauthorized || tok.Error != "invalid_client" {
			t.Fatalf("got %d %+v", status, tok)
		}
	})
}

func TestConfidentialClientNeedsSecret(t *testing.T) {
	f := newFixture(t)
	out := f.register(`{"client_name":"C","redirect_uris":["` + testRedirect + `"],"token_endpoint_auth_method":"client_secret_post"}`)
	clientID, secret := out["client_id"].(string), out["client_secret"].(string)
	form := url.Values{"grant_type": {"authorization_code"}, "code": {f.authorize(clientID)}, "client_id": {clientID}, "redirect_uri": {testRedirect}, "code_verifier": {testVerifier}}
	if status, _ := f.token(form); status != http.StatusUnauthorized {
		t.Fatal("token issued without the client secret")
	}
	form.Set("code", f.authorize(clientID))
	form.Set("client_secret", secret)
	if status, tok := f.token(form); status != http.StatusOK {
		t.Fatalf("with secret: %d %+v", status, tok)
	}
}

func TestAuthorizeRejections(t *testing.T) {
	f := newFixture(t)
	clientID := f.registerPublic()

	t.Run("wrong password shows the form again", func(t *testing.T) {
		form := f.authForm(clientID)
		form.Set("username", testUser)
		form.Set("password", "nope")
		res, body := f.postForm("/authorize", form)
		if res.StatusCode != http.StatusUnauthorized || !strings.Contains(body, "Wrong username or password") {
			t.Fatalf("got %d", res.StatusCode)
		}
		if res.Header.Get("X-Frame-Options") != "DENY" {
			t.Fatal("consent page can be framed")
		}
		// Browsers check form-action on the redirect after the post: the client's
		// origin must be allowed, or clicking Allow is silently blocked.
		if csp := res.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "form-action 'self' https://claude.ai") {
			t.Fatalf("CSP = %q, want the redirect origin in form-action", csp)
		}
	})
	t.Run("unregistered redirect is never followed", func(t *testing.T) {
		form := f.authForm(clientID)
		form.Set("redirect_uri", "https://evil.example/cb")
		req, _ := http.NewRequest("GET", f.srv.URL+"/authorize?"+form.Encode(), nil)
		res, _ := f.send(req)
		if res.StatusCode != http.StatusBadRequest || res.Header.Get("Location") != "" {
			t.Fatalf("got %d, Location %q", res.StatusCode, res.Header.Get("Location"))
		}
	})
	t.Run("PKCE is required", func(t *testing.T) {
		form := f.authForm(clientID)
		form.Del("code_challenge")
		req, _ := http.NewRequest("GET", f.srv.URL+"/authorize?"+form.Encode(), nil)
		res, _ := f.send(req)
		if res.StatusCode != http.StatusFound || !strings.Contains(res.Header.Get("Location"), "error=invalid_request") {
			t.Fatalf("got %d, Location %q", res.StatusCode, res.Header.Get("Location"))
		}
	})
	t.Run("deny redirects with access_denied", func(t *testing.T) {
		form := f.authForm(clientID)
		form.Set("action", "deny")
		res, _ := f.postForm("/authorize", form)
		if !strings.Contains(res.Header.Get("Location"), "error=access_denied") {
			t.Fatalf("Location = %q", res.Header.Get("Location"))
		}
	})
	t.Run("form renders the client name escaped", func(t *testing.T) {
		evil := f.register(`{"client_name":"<script>x</script>","redirect_uris":["` + testRedirect + `"],"token_endpoint_auth_method":"none"}`)
		req, _ := http.NewRequest("GET", f.srv.URL+"/authorize?"+f.authForm(evil["client_id"].(string)).Encode(), nil)
		_, body := f.send(req)
		if strings.Contains(body, "<script>x") {
			t.Fatal("client name is not escaped")
		}
	})
}

func TestLoginLockout(t *testing.T) {
	f := newFixture(t)
	clientID := f.registerPublic()
	attempt := func(password string) int {
		form := f.authForm(clientID)
		form.Set("username", testUser)
		form.Set("password", password)
		res, _ := f.postForm("/authorize", form)
		return res.StatusCode
	}
	for range freeAttempts {
		attempt("nope")
	}
	if got := attempt(testPassword); got != http.StatusTooManyRequests {
		t.Fatalf("right password during lockout = %d, want 429", got)
	}
	f.clock.Advance(maxLockout)
	if got := attempt(testPassword); got != http.StatusFound {
		t.Fatalf("after lockout = %d, want 302", got)
	}
}

func TestRegisterValidation(t *testing.T) {
	f := newFixture(t)
	for _, body := range []string{
		`{"redirect_uris":["http://evil.example/cb"]}`,
		`{"redirect_uris":["https://x.example/cb#frag"]}`,
		`{"redirect_uris":[]}`,
		`{"redirect_uris":["https://x.example/cb"],"grant_types":["client_credentials"]}`,
		`{"redirect_uris":["https://x.example/cb"],"token_endpoint_auth_method":"private_key_jwt"}`,
	} {
		req, _ := http.NewRequest("POST", f.srv.URL+"/register", strings.NewReader(body))
		if res, data := f.send(req); res.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s accepted: %d %s", body, res.StatusCode, data)
		}
	}
	// Native clients (Claude Code) listen on a loopback port that may change.
	out := f.register(`{"redirect_uris":["http://localhost:33418/callback"],"token_endpoint_auth_method":"none"}`)
	c, _ := f.p.getClient(out["client_id"].(string))
	if !redirectAllowed(c, "http://localhost:50000/callback") || redirectAllowed(c, "http://localhost:50000/other") {
		t.Fatal("loopback port flexibility is wrong")
	}
}

func TestConnectionsPersistAndRevoke(t *testing.T) {
	f := newFixture(t)
	clientID := f.registerPublic()
	tok := f.exchange(clientID, f.authorize(clientID))

	f.open() // restart
	if res, _ := f.callMCP(tok.Access); res.StatusCode != http.StatusOK {
		t.Fatal("access token lost on restart")
	}
	conns := f.p.Connections()
	if len(conns) != 1 || conns[0].ClientName != "Claude" {
		t.Fatalf("connections = %+v", conns)
	}
	if err := f.p.Revoke(conns[0].ID); err != nil {
		t.Fatal(err)
	}
	if res, _ := f.callMCP(tok.Access); res.StatusCode != http.StatusUnauthorized {
		t.Fatal("revoked access token still works")
	}
	if status, _ := f.token(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok.Refresh}, "client_id": {clientID}}); status != http.StatusBadRequest {
		t.Fatal("revoked refresh token still works")
	}
}

func TestAccessTokenExpires(t *testing.T) {
	f := newFixture(t)
	clientID := f.registerPublic()
	tok := f.exchange(clientID, f.authorize(clientID))
	f.clock.Advance(accessTokenTTL + time.Second)
	if res, _ := f.callMCP(tok.Access); res.StatusCode != http.StatusUnauthorized {
		t.Fatal("expired access token accepted")
	}
	if status, _ := f.token(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok.Refresh}, "client_id": {clientID}}); status != http.StatusOK {
		t.Fatal("refresh after access expiry failed")
	}
}
