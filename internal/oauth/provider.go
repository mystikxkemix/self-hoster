// Package oauth is a minimal OAuth 2.1 authorization server for the MCP endpoint, as
// required by remote MCP clients such as Claude: protected-resource and
// authorization-server metadata (RFC 9728, RFC 8414), dynamic client registration
// (RFC 7591), authorization code with mandatory PKCE S256, and rotating refresh tokens.
//
// The admin username and password are the only credentials: the consent page asks
// for them once per connection. Tokens are opaque random strings, stored hashed in a
// JSON file on the data volume so connections survive restarts.
package oauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"selfhoster/internal/fileutil"
)

const (
	// Scope is the only scope: full control of the MCP tools.
	Scope = "selfhoster"

	accessTokenTTL  = time.Hour
	refreshTokenTTL = 30 * 24 * time.Hour
	codeTTL         = time.Minute
	// maxClients bounds dynamic registration, which is open to anyone who can reach
	// the endpoint. Clients without a connection are evicted first.
	maxClients = 100
)

// Authenticator checks the admin credentials (settings.Manager).
type Authenticator interface {
	Authenticate(user, password string) bool
}

// Config configures a Provider.
type Config struct {
	// Path is the JSON file holding registered clients and connections.
	Path string
	// PublicURL is the base URL clients reach the server at (e.g.
	// https://mcp.example.com). Empty derives it from each request's Host and
	// X-Forwarded-Proto headers.
	PublicURL string
	Auth      Authenticator
	// Now is the clock (tests). Defaults to time.Now.
	Now func() time.Time
}

// client is a dynamically registered OAuth client.
type client struct {
	ID string `json:"id"`
	// SecretHash is set for confidential clients (client_secret_basic/post).
	SecretHash   string    `json:"secretHash,omitempty"`
	AuthMethod   string    `json:"authMethod"`
	Name         string    `json:"name,omitempty"`
	RedirectURIs []string  `json:"redirectUris"`
	CreatedAt    time.Time `json:"createdAt"`
}

// grant is one authorized connection: a refresh token and its current access token.
type grant struct {
	ID             string    `json:"id"`
	ClientID       string    `json:"clientId"`
	Resource       string    `json:"resource,omitempty"`
	RefreshHash    string    `json:"refreshHash"`
	RefreshExpires time.Time `json:"refreshExpires"`
	AccessHash     string    `json:"accessHash"`
	AccessExpires  time.Time `json:"accessExpires"`
	CreatedAt      time.Time `json:"createdAt"`
	LastUsed       time.Time `json:"lastUsed"`
}

// authCode is a pending authorization code (memory only, single use).
type authCode struct {
	clientID      string
	redirectURI   string
	codeChallenge string
	resource      string
	expires       time.Time
}

type fileFormat struct {
	Clients []client `json:"clients"`
	Grants  []grant  `json:"grants"`
}

// Connection is an authorized client, as listed on the settings page.
type Connection struct {
	ID         string    `json:"id"`
	ClientName string    `json:"clientName"`
	CreatedAt  time.Time `json:"createdAt"`
	LastUsed   time.Time `json:"lastUsed"`
}

// tokenPair is what the token endpoint returns.
type tokenPair struct {
	access, refresh string
	expiresIn       time.Duration
}

// Provider is the authorization server and token validator. It is safe for concurrent use.
type Provider struct {
	path      string
	publicURL string
	auth      Authenticator
	now       func() time.Time
	limiter   loginLimiter

	mu      sync.Mutex
	clients map[string]*client
	grants  map[string]*grant
	codes   map[string]authCode
}

// Open loads the provider state from cfg.Path, starting empty if it does not exist.
func Open(cfg Config) (*Provider, error) {
	if cfg.Auth == nil {
		return nil, errors.New("oauth: an authenticator is required")
	}
	p := &Provider{
		path:      cfg.Path,
		publicURL: strings.TrimRight(cfg.PublicURL, "/"),
		auth:      cfg.Auth,
		now:       cfg.Now,
		clients:   map[string]*client{},
		grants:    map[string]*grant{},
		codes:     map[string]authCode{},
	}
	if p.now == nil {
		p.now = time.Now
	}
	data, err := os.ReadFile(cfg.Path)
	if errors.Is(err, os.ErrNotExist) {
		return p, nil
	}
	if err != nil {
		return nil, err
	}
	var f fileFormat
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", cfg.Path, err)
	}
	for i := range f.Clients {
		p.clients[f.Clients[i].ID] = &f.Clients[i]
	}
	for i := range f.Grants {
		p.grants[f.Grants[i].ID] = &f.Grants[i]
	}
	return p, nil
}

// PublicURL returns the configured base URL ("" when derived from requests).
func (p *Provider) PublicURL() string {
	return p.publicURL
}

// Connections lists the authorized clients, most recently used first.
func (p *Provider) Connections() []Connection {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	out := make([]Connection, 0, len(p.grants))
	for _, g := range p.grants {
		if now.After(g.RefreshExpires) {
			continue
		}
		name := g.ClientID
		if c, ok := p.clients[g.ClientID]; ok && c.Name != "" {
			name = c.Name
		}
		out = append(out, Connection{ID: g.ID, ClientName: name, CreatedAt: g.CreatedAt, LastUsed: g.LastUsed})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastUsed.After(out[j].LastUsed) })
	return out
}

// Revoke deletes one connection: its access and refresh tokens stop working at once.
func (p *Provider) Revoke(id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.grants[id]; !ok {
		return errors.New("connection not found")
	}
	delete(p.grants, id)
	return p.saveLocked()
}

// validateAccess returns the grant an access token belongs to, if it is valid for resource.
func (p *Provider) validateAccess(token, resource string) bool {
	if token == "" {
		return false
	}
	hash := hashToken(token)
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	for _, g := range p.grants {
		if g.AccessHash != hash {
			continue
		}
		if now.After(g.AccessExpires) || (g.Resource != "" && g.Resource != resource) {
			return false
		}
		// Kept in memory; persisted with the next write (refresh, new connection).
		g.LastUsed = now
		return true
	}
	return false
}

// registerClient stores a new client and returns it with its plain secret, if any.
func (p *Provider) registerClient(name, authMethod string, redirectURIs []string) (*client, string, error) {
	c := &client{
		ID:           randomToken(),
		AuthMethod:   authMethod,
		Name:         name,
		RedirectURIs: redirectURIs,
		CreatedAt:    p.now(),
	}
	var secret string
	if authMethod != authNone {
		secret = randomToken()
		c.SecretHash = hashToken(secret)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.clients) >= maxClients && !p.evictClientLocked() {
		return nil, "", errors.New("too many registered clients: revoke unused connections first")
	}
	p.clients[c.ID] = c
	if err := p.saveLocked(); err != nil {
		delete(p.clients, c.ID)
		return nil, "", err
	}
	return c, secret, nil
}

// evictClientLocked removes the oldest client that has no connection.
func (p *Provider) evictClientLocked() bool {
	used := map[string]bool{}
	for _, g := range p.grants {
		used[g.ClientID] = true
	}
	var oldest *client
	for _, c := range p.clients {
		if !used[c.ID] && (oldest == nil || c.CreatedAt.Before(oldest.CreatedAt)) {
			oldest = c
		}
	}
	if oldest == nil {
		return false
	}
	delete(p.clients, oldest.ID)
	return true
}

func (p *Provider) getClient(id string) (client, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.clients[id]
	if !ok {
		return client{}, false
	}
	return *c, true
}

// issueCode creates a single-use authorization code.
func (p *Provider) issueCode(c authCode) string {
	code := randomToken()
	c.expires = p.now().Add(codeTTL)
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, existing := range p.codes {
		if p.now().After(existing.expires) {
			delete(p.codes, k)
		}
	}
	p.codes[hashToken(code)] = c
	return code
}

// redeemCode consumes an authorization code and returns a new connection's tokens.
func (p *Provider) redeemCode(code, clientID, redirectURI, verifier, resource string) (tokenPair, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := hashToken(code)
	c, ok := p.codes[key]
	// Single use, even when the exchange fails below.
	delete(p.codes, key)
	switch {
	case !ok || p.now().After(c.expires):
		return tokenPair{}, errInvalidGrant("authorization code is invalid or expired")
	case c.clientID != clientID:
		return tokenPair{}, errInvalidGrant("authorization code was issued to another client")
	case c.redirectURI != redirectURI:
		return tokenPair{}, errInvalidGrant("redirect_uri does not match the authorization request")
	case !verifyPKCE(verifier, c.codeChallenge):
		return tokenPair{}, errInvalidGrant("code_verifier does not match the code challenge")
	case resource != "" && c.resource != "" && resource != c.resource:
		return tokenPair{}, &tokenError{Code: "invalid_target", Description: "resource does not match the authorization request"}
	}
	now := p.now()
	g := &grant{ID: randomToken()[:16], ClientID: clientID, Resource: c.resource, CreatedAt: now}
	pair := p.rotateLocked(g)
	p.grants[g.ID] = g
	if err := p.saveLocked(); err != nil {
		delete(p.grants, g.ID)
		return tokenPair{}, err
	}
	return pair, nil
}

// refresh exchanges a refresh token for new tokens. The old refresh token stops working.
func (p *Provider) refresh(refreshToken, clientID, resource string) (tokenPair, error) {
	hash := hashToken(refreshToken)
	p.mu.Lock()
	defer p.mu.Unlock()
	var g *grant
	for _, candidate := range p.grants {
		if candidate.RefreshHash == hash {
			g = candidate
			break
		}
	}
	switch {
	case g == nil || p.now().After(g.RefreshExpires):
		return tokenPair{}, errInvalidGrant("refresh token is invalid or expired")
	case g.ClientID != clientID:
		return tokenPair{}, errInvalidGrant("refresh token was issued to another client")
	case resource != "" && g.Resource != "" && resource != g.Resource:
		return tokenPair{}, &tokenError{Code: "invalid_target", Description: "resource does not match the connection"}
	}
	previous := *g
	pair := p.rotateLocked(g)
	if err := p.saveLocked(); err != nil {
		*g = previous
		return tokenPair{}, err
	}
	return pair, nil
}

// rotateLocked gives g a new access and refresh token.
func (p *Provider) rotateLocked(g *grant) tokenPair {
	now := p.now()
	pair := tokenPair{access: randomToken(), refresh: randomToken(), expiresIn: accessTokenTTL}
	g.AccessHash, g.AccessExpires = hashToken(pair.access), now.Add(accessTokenTTL)
	g.RefreshHash, g.RefreshExpires = hashToken(pair.refresh), now.Add(refreshTokenTTL)
	g.LastUsed = now
	return pair
}

// saveLocked drops expired connections and writes the state. The caller holds p.mu.
func (p *Provider) saveLocked() error {
	now := p.now()
	f := fileFormat{Clients: make([]client, 0, len(p.clients)), Grants: make([]grant, 0, len(p.grants))}
	for id, g := range p.grants {
		if now.After(g.RefreshExpires) {
			delete(p.grants, id)
			continue
		}
		f.Grants = append(f.Grants, *g)
	}
	for _, c := range p.clients {
		f.Clients = append(f.Clients, *c)
	}
	sort.Slice(f.Clients, func(i, j int) bool { return f.Clients[i].CreatedAt.Before(f.Clients[j].CreatedAt) })
	sort.Slice(f.Grants, func(i, j int) bool { return f.Grants[i].CreatedAt.Before(f.Grants[j].CreatedAt) })
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return fileutil.WriteFileAtomic(p.path, data)
}

// verifyPKCE checks an S256 code verifier (RFC 7636: 43-128 unreserved characters).
func verifyPKCE(verifier, challenge string) bool {
	if len(verifier) < 43 || len(verifier) > 128 {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:]) == challenge
}

// randomToken returns 256 random bits, URL-safe.
func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // never fails (crypto/rand panics on failure since Go 1.24)
	return base64.RawURLEncoding.EncodeToString(b)
}

// hashToken stores tokens hashed: a leaked state file does not leak usable tokens.
// Tokens are 256-bit random, so a plain SHA-256 is enough (no password stretching).
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// clientAuthMethods are the token endpoint authentication methods we accept.
var clientAuthMethods = []string{authNone, authSecretPost, authSecretBasic}

const (
	authNone        = "none"
	authSecretPost  = "client_secret_post"
	authSecretBasic = "client_secret_basic"
)

func supportedAuthMethod(method string) bool {
	return slices.Contains(clientAuthMethods, method)
}
