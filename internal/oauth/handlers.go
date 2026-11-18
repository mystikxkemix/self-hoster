package oauth

import (
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"html/template"
	"log"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	// ResourcePath is the protected MCP endpoint.
	ResourcePath = "/mcp"

	maxBodyBytes     = 64 << 10
	maxRedirectURIs  = 10
	maxClientNameLen = 200
)

//go:embed authorize.html
var authorizePage string

var authorizeTemplate = template.Must(template.New("authorize").Parse(authorizePage))

// Routes registers the OAuth endpoints on mux.
func (p *Provider) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", p.resourceMetadata)
	// RFC 9728 path-suffixed form, for the /mcp resource.
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", p.resourceMetadata)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", p.serverMetadata)
	mux.HandleFunc("POST /register", p.register)
	mux.HandleFunc("GET /authorize", p.authorizeForm)
	mux.HandleFunc("POST /authorize", p.authorizeSubmit)
	mux.HandleFunc("POST /token", p.token)
}

// Protect requires a valid access token. Without one it answers 401 with a pointer
// to the resource metadata, which is how MCP clients discover the OAuth flow.
func (p *Provider) Protect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, hasToken := bearerToken(r)
		if !p.validateAccess(token, p.resource(r)) {
			challenge := `Bearer resource_metadata="` + p.base(r) + `/.well-known/oauth-protected-resource"`
			if hasToken {
				challenge += `, error="invalid_token"`
			}
			w.Header().Set("WWW-Authenticate", challenge)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_token"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// base is the server's public base URL, without a trailing slash.
func (p *Provider) base(r *http.Request) string {
	if p.publicURL != "" {
		return p.publicURL
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	// Set by the nginx in front (proxy_set_header X-Forwarded-Proto $scheme).
	if proto := r.Header.Get("X-Forwarded-Proto"); proto == "https" || proto == "http" {
		scheme = proto
	}
	return scheme + "://" + r.Host
}

func (p *Provider) resource(r *http.Request) string {
	return p.base(r) + ResourcePath
}

func (p *Provider) resourceMetadata(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":                 p.resource(r),
		"authorization_servers":    []string{p.base(r)},
		"scopes_supported":         []string{Scope},
		"bearer_methods_supported": []string{"header"},
		"resource_name":            "self-hoster",
	})
}

func (p *Provider) serverMetadata(w http.ResponseWriter, r *http.Request) {
	base := p.base(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                base,
		"authorization_endpoint":                base + "/authorize",
		"token_endpoint":                        base + "/token",
		"registration_endpoint":                 base + "/register",
		"scopes_supported":                      []string{Scope},
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": clientAuthMethods,
	})
}

// registrationRequest is the subset of RFC 7591 metadata we use. Unknown fields are ignored.
type registrationRequest struct {
	RedirectURIs            []string `json:"redirect_uris"`
	ClientName              string   `json:"client_name"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
}

func (p *Provider) register(w http.ResponseWriter, r *http.Request) {
	var req registrationRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&req); err != nil {
		writeRegistrationError(w, "invalid_client_metadata", "malformed JSON body")
		return
	}
	if req.TokenEndpointAuthMethod == "" {
		req.TokenEndpointAuthMethod = authSecretBasic // RFC 7591 default
	}
	if !supportedAuthMethod(req.TokenEndpointAuthMethod) {
		writeRegistrationError(w, "invalid_client_metadata", "unsupported token_endpoint_auth_method")
		return
	}
	for _, gt := range req.GrantTypes {
		if gt != "authorization_code" && gt != "refresh_token" {
			writeRegistrationError(w, "invalid_client_metadata", "unsupported grant type "+gt)
			return
		}
	}
	for _, rt := range req.ResponseTypes {
		if rt != "code" {
			writeRegistrationError(w, "invalid_client_metadata", "unsupported response type "+rt)
			return
		}
	}
	if len(req.RedirectURIs) == 0 || len(req.RedirectURIs) > maxRedirectURIs {
		writeRegistrationError(w, "invalid_redirect_uri", "between 1 and 10 redirect_uris are required")
		return
	}
	for _, uri := range req.RedirectURIs {
		if err := validateRedirectURI(uri); err != nil {
			writeRegistrationError(w, "invalid_redirect_uri", err.Error())
			return
		}
	}
	name := strings.TrimSpace(req.ClientName)
	if len(name) > maxClientNameLen {
		name = name[:maxClientNameLen]
	}
	c, secret, err := p.registerClient(name, req.TokenEndpointAuthMethod, req.RedirectURIs)
	if err != nil {
		log.Printf("mcp oauth: register client: %v", err)
		writeRegistrationError(w, "invalid_client_metadata", err.Error())
		return
	}
	resp := map[string]any{
		"client_id":                  c.ID,
		"client_id_issued_at":        c.CreatedAt.Unix(),
		"client_name":                c.Name,
		"redirect_uris":              c.RedirectURIs,
		"token_endpoint_auth_method": c.AuthMethod,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"scope":                      Scope,
	}
	if secret != "" {
		resp["client_secret"] = secret
		resp["client_secret_expires_at"] = 0
	}
	writeJSON(w, http.StatusCreated, resp)
}

// validateRedirectURI accepts https URLs, and plain http only on loopback (native
// clients such as Claude Code listen on localhost, RFC 8252).
func validateRedirectURI(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return errors.New("redirect_uri must be an absolute URL")
	}
	if u.Fragment != "" {
		return errors.New("redirect_uri must not contain a fragment")
	}
	switch {
	case u.Scheme == "https":
		return nil
	case u.Scheme == "http" && isLoopback(u.Hostname()):
		return nil
	default:
		return errors.New("redirect_uri must use https (or http on localhost)")
	}
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// redirectAllowed matches a requested redirect_uri against the registered ones.
// Loopback URIs may use any port (RFC 8252 §7.3).
func redirectAllowed(c client, requested string) bool {
	if slices.Contains(c.RedirectURIs, requested) {
		return true
	}
	req, err := url.Parse(requested)
	if err != nil || req.Scheme != "http" || !isLoopback(req.Hostname()) {
		return false
	}
	for _, registered := range c.RedirectURIs {
		reg, err := url.Parse(registered)
		if err == nil && reg.Scheme == "http" && reg.Hostname() == req.Hostname() &&
			reg.Path == req.Path && reg.RawQuery == req.RawQuery {
			return true
		}
	}
	return false
}

// authRequest holds the parameters of an authorization request, echoed by the form.
type authRequest struct {
	ResponseType        string
	ClientID            string
	RedirectURI         string
	State               string
	Scope               string
	CodeChallenge       string
	CodeChallengeMethod string
	Resource            string
}

func parseAuthRequest(form url.Values) authRequest {
	return authRequest{
		ResponseType:        form.Get("response_type"),
		ClientID:            form.Get("client_id"),
		RedirectURI:         form.Get("redirect_uri"),
		State:               form.Get("state"),
		Scope:               form.Get("scope"),
		CodeChallenge:       form.Get("code_challenge"),
		CodeChallengeMethod: form.Get("code_challenge_method"),
		Resource:            form.Get("resource"),
	}
}

// checkAuthRequest validates a request. A non-nil client error must be shown on the
// page (the redirect URI cannot be trusted); a non-empty oauthErr is sent back to it.
func (p *Provider) checkAuthRequest(r *http.Request, req authRequest) (c client, clientErr error, oauthErr, oauthDesc string) {
	c, ok := p.getClient(req.ClientID)
	if !ok {
		return c, errors.New("unknown client: register it again (remove and re-add the connector)"), "", ""
	}
	if !redirectAllowed(c, req.RedirectURI) {
		return c, errors.New("redirect_uri is not registered for this client"), "", ""
	}
	switch {
	case req.ResponseType != "code":
		return c, nil, "unsupported_response_type", "only response_type=code is supported"
	case req.CodeChallenge == "" || req.CodeChallengeMethod != "S256":
		return c, nil, "invalid_request", "PKCE with code_challenge_method=S256 is required"
	case req.Resource != "" && strings.TrimRight(req.Resource, "/") != p.resource(r):
		return c, nil, "invalid_target", "unknown resource " + req.Resource
	}
	return c, nil, "", ""
}

type authorizeView struct {
	authRequest
	ClientName   string
	RedirectHost string
	Error        string
}

func (p *Provider) authorizeForm(w http.ResponseWriter, r *http.Request) {
	req := parseAuthRequest(r.URL.Query())
	c, clientErr, oauthErr, desc := p.checkAuthRequest(r, req)
	if clientErr != nil {
		http.Error(w, clientErr.Error(), http.StatusBadRequest)
		return
	}
	if oauthErr != "" {
		redirectWithError(w, r, req, oauthErr, desc)
		return
	}
	p.renderAuthorize(w, http.StatusOK, req, c, "")
}

func (p *Provider) authorizeSubmit(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed form", http.StatusBadRequest)
		return
	}
	req := parseAuthRequest(r.PostForm)
	c, clientErr, oauthErr, desc := p.checkAuthRequest(r, req)
	if clientErr != nil {
		http.Error(w, clientErr.Error(), http.StatusBadRequest)
		return
	}
	if oauthErr != "" {
		redirectWithError(w, r, req, oauthErr, desc)
		return
	}
	if r.PostForm.Get("action") == "deny" {
		redirectWithError(w, r, req, "access_denied", "the administrator denied access")
		return
	}
	if wait := p.limiter.wait(p.now()); wait > 0 {
		p.renderAuthorize(w, http.StatusTooManyRequests, req, c,
			"Too many failed attempts. Try again in "+wait.Round(time.Second).String()+".")
		return
	}
	if !p.auth.Authenticate(r.PostForm.Get("username"), r.PostForm.Get("password")) {
		p.limiter.fail(p.now())
		log.Printf("mcp oauth: failed sign-in for client %q", c.Name)
		p.renderAuthorize(w, http.StatusUnauthorized, req, c, "Wrong username or password.")
		return
	}
	p.limiter.reset()

	resource := strings.TrimRight(req.Resource, "/")
	if resource == "" {
		resource = p.resource(r)
	}
	code := p.issueCode(authCode{
		clientID: c.ID, redirectURI: req.RedirectURI, codeChallenge: req.CodeChallenge, resource: resource,
	})
	log.Printf("mcp oauth: authorized client %q", c.Name)
	redirect(w, r, req, url.Values{"code": {code}})
}

func (p *Provider) renderAuthorize(w http.ResponseWriter, status int, req authRequest, c client, errText string) {
	name := c.Name
	if name == "" {
		name = "An MCP client"
	}
	host, formAction := req.RedirectURI, "'self'"
	if u, err := url.Parse(req.RedirectURI); err == nil {
		host = u.Host
		// Browsers apply form-action to the redirect that follows the form post, so
		// the client's origin must be allowed or "Allow" does nothing. The redirect
		// URI was checked against the registered client before this page is shown.
		if origin := u.Scheme + "://" + u.Host; !strings.ContainsAny(origin, " ;,'\"") {
			formAction += " " + origin
		}
	}
	// The page asks for the admin password: never render it inside another site.
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'; default-src 'none'; style-src 'unsafe-inline'; form-action "+formAction)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	view := authorizeView{authRequest: req, ClientName: name, RedirectHost: host, Error: errText}
	if err := authorizeTemplate.Execute(w, view); err != nil {
		log.Printf("mcp oauth: render authorize page: %v", err)
	}
}

func redirectWithError(w http.ResponseWriter, r *http.Request, req authRequest, code, desc string) {
	redirect(w, r, req, url.Values{"error": {code}, "error_description": {desc}})
}

// redirect sends the browser back to the client with params (and the client's state).
func redirect(w http.ResponseWriter, r *http.Request, req authRequest, params url.Values) {
	u, err := url.Parse(req.RedirectURI)
	if err != nil {
		http.Error(w, "invalid redirect_uri", http.StatusBadRequest)
		return
	}
	q := u.Query()
	for k, v := range params {
		q[k] = v
	}
	if req.State != "" {
		q.Set("state", req.State)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

// tokenError is an RFC 6749 §5.2 error response.
type tokenError struct {
	Code        string `json:"error"`
	Description string `json:"error_description,omitempty"`
}

func (e *tokenError) Error() string { return e.Code + ": " + e.Description }

func errInvalidGrant(desc string) error {
	return &tokenError{Code: "invalid_grant", Description: desc}
}

func (p *Provider) token(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := r.ParseForm(); err != nil {
		writeTokenError(w, &tokenError{Code: "invalid_request", Description: "malformed form body"})
		return
	}
	form := r.PostForm
	clientID, ok := p.authenticateClient(r)
	if !ok {
		writeTokenError(w, &tokenError{Code: "invalid_client", Description: "unknown client or bad client credentials"})
		return
	}
	resource := strings.TrimRight(form.Get("resource"), "/")
	var (
		pair tokenPair
		err  error
	)
	switch form.Get("grant_type") {
	case "authorization_code":
		pair, err = p.redeemCode(form.Get("code"), clientID, form.Get("redirect_uri"), form.Get("code_verifier"), resource)
	case "refresh_token":
		pair, err = p.refresh(form.Get("refresh_token"), clientID, resource)
	default:
		err = &tokenError{Code: "unsupported_grant_type", Description: "use authorization_code or refresh_token"}
	}
	if err != nil {
		writeTokenError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  pair.access,
		"token_type":    "Bearer",
		"expires_in":    int(pair.expiresIn / time.Second),
		"refresh_token": pair.refresh,
		"scope":         Scope,
	})
}

// authenticateClient identifies the client of a token request (RFC 6749 §2.3).
func (p *Provider) authenticateClient(r *http.Request) (string, bool) {
	id, secret, basic := r.BasicAuth()
	if basic {
		// client_secret_basic: both are form-urlencoded (RFC 6749 §2.3.1).
		id, _ = url.QueryUnescape(id)
		secret, _ = url.QueryUnescape(secret)
	} else {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	c, ok := p.getClient(id)
	if !ok {
		return "", false
	}
	if c.SecretHash == "" {
		return c.ID, true
	}
	return c.ID, secret != "" && subtle.ConstantTimeCompare([]byte(hashToken(secret)), []byte(c.SecretHash)) == 1
}

func writeTokenError(w http.ResponseWriter, err error) {
	var te *tokenError
	if !errors.As(err, &te) {
		log.Printf("mcp oauth: token: %v", err)
		te = &tokenError{Code: "server_error", Description: "could not issue tokens"}
		writeJSON(w, http.StatusInternalServerError, te)
		return
	}
	status := http.StatusBadRequest
	if te.Code == "invalid_client" {
		status = http.StatusUnauthorized
	}
	writeJSON(w, status, te)
}

func writeRegistrationError(w http.ResponseWriter, code, desc string) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": code, "error_description": desc})
}

func bearerToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", header != ""
	}
	return strings.TrimSpace(token), true
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf("mcp oauth: write response: %v", err)
	}
}

// loginLimiter slows down password guessing on the public consent page. It is global
// (not per IP: behind nginx every request comes from the proxy), so an attacker can
// at worst delay new connections; existing ones and the LAN admin UI are unaffected.
type loginLimiter struct {
	mu          sync.Mutex
	failures    int
	lockedUntil time.Time
}

const (
	freeAttempts = 5
	maxLockout   = 15 * time.Minute
)

func (l *loginLimiter) wait(now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Before(l.lockedUntil) {
		return l.lockedUntil.Sub(now)
	}
	return 0
}

// fail records a failed attempt: after freeAttempts, each failure locks the page for
// 30s, doubling up to maxLockout.
func (l *loginLimiter) fail(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.failures++
	if l.failures < freeAttempts {
		return
	}
	lockout := 30 * time.Second << min(l.failures-freeAttempts, 5)
	l.lockedUntil = now.Add(min(lockout, maxLockout))
}

func (l *loginLimiter) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.failures, l.lockedUntil = 0, time.Time{}
}
