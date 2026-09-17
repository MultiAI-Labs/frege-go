// Package frege is a small, dependency-free Go client for the Frege HTTP API.
//
// Frege turns each project's OpenAPI spec into a set of tools and runs them
// against the upstream API with the right credential injected. This client lets
// your own code — a Telegram bot, a cron job, a backend service — call those
// tools directly, with no AI model in the loop.
//
// # Auth in one paragraph
//
// Use a project API key. Mint one in the dashboard under Project → API keys; it
// is shown once and starts with frege_sk_. It is scoped to that one project, it
// does not expire, and it needs no refresh machinery:
//
//	c := frege.New(frege.StaticToken("frege_sk_…"))
//	res, err := c.InvokeTool(ctx, projectID, "get_account_profile", nil)
//
// A Frege USER's access token works too, for code acting as a person rather than
// as a service. Those are short-lived, so sign in once with an email code (see
// the bootstrap example) and let a *RefreshingToken trade the refresh token for
// new access tokens. Refresh tokens ROTATE — every refresh invalidates the old
// one — so persist each new pair via OnRefresh or a restart will present a token
// the server no longer accepts.
//
//	tok := frege.NewRefreshingToken("", storedRefreshToken,
//	    frege.WithOnRefresh(func(access, refresh string) { save(refresh) }))
//	c := frege.New(tok)
//
// # Acting as one customer
//
// On a project whose end customers each sign in with their own upstream account,
// AsClient names WHICH customer's credential to spend. One key, one agent, any
// customer — and both parties land in the audit trail.
//
//	res, err := c.InvokeTool(ctx, projectID, "get_account_profile", nil,
//	    frege.AsClient(customerID))
package frege

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultBaseURL is Frege's production API. Use https://frege.uz for dev.
const DefaultBaseURL = "https://frege.io"

var authHTTP = &http.Client{Timeout: 30 * time.Second}

// TokenSource yields the bearer token to send on each request.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// refresher is a TokenSource that can mint a new token after the current one is
// rejected. The Client calls Refresh once on a 401 and retries the request.
type refresher interface {
	Refresh(ctx context.Context) (string, error)
}

// StaticToken is a TokenSource that always returns the same token. Use it when
// you already hold a valid access token and manage its lifetime yourself.
type StaticToken string

// Token implements TokenSource.
func (s StaticToken) Token(context.Context) (string, error) {
	if s == "" {
		return "", errors.New("frege: empty token")
	}
	return string(s), nil
}

// Client talks to one Frege environment as one user.
// The two environments every project has. A project-scoped call is answered by
// one of them, and the choice is carried in a header.
const (
	StageStaging = "staging"
	StageLive    = "live"
)

// StageHeader carries the environment on every project-scoped request.
const StageHeader = "X-Frege-Stage"

type Client struct {
	baseURL string
	http    *http.Client
	tokens  TokenSource
	stage   string
}

// Option configures a Client.
type Option func(*Client)

// WithBaseURL points the client at a specific environment (default
// DefaultBaseURL).
func WithBaseURL(u string) Option {
	return func(c *Client) { c.baseURL = strings.TrimRight(u, "/") }
}

// WithHTTPClient supplies your own *http.Client (timeouts, proxy, and so on).
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.http = h }
}

// WithStage picks the environment every project-scoped call is answered by:
// frege.StageStaging or frege.StageLive.
//
// OMITTING THIS MEANS LIVE. That is the server's default, not this client's
// choice, and it is the one thing worth knowing before pointing a test at a
// real deployment: a run that means to exercise staging and never sets a stage
// reads production's spec and spends production's credential, and every
// response looks perfectly normal.
//
// An API key is bound to ONE environment when it is issued, so a key and a
// stage that disagree fail rather than crossing over. That is the server
// protecting you, not this option.
func WithStage(stage string) Option {
	return func(c *Client) { c.stage = stage }
}

// New builds a Client. tokens supplies the bearer for each call — usually a
// *RefreshingToken built from a stored refresh token.
func New(tokens TokenSource, opts ...Option) *Client {
	c := &Client{
		baseURL: DefaultBaseURL,
		http:    &http.Client{Timeout: 60 * time.Second},
		tokens:  tokens,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// ---- token refreshing ------------------------------------------------------

// RefreshingToken keeps a short-lived access token alive by trading a refresh
// token for a new one whenever the access token is rejected. It is safe for
// concurrent use.
//
// Refresh tokens ROTATE: every refresh returns a new refresh token and the old
// one stops working. Set OnRefresh to persist the new pair, or a restart will
// fall back to a refresh token the server no longer accepts.
type RefreshingToken struct {
	mu      sync.Mutex
	baseURL string
	http    *http.Client
	access  string
	refresh string

	// OnRefresh, if set, is called with each new (access, refresh) pair right
	// after a successful refresh. Persist the refresh token here.
	OnRefresh func(access, refresh string)
}

// RTOption configures a RefreshingToken.
type RTOption func(*RefreshingToken)

// WithRefreshBaseURL sets the environment the refresh call is made against
// (default DefaultBaseURL). Use the same base URL as the Client.
func WithRefreshBaseURL(u string) RTOption {
	return func(rt *RefreshingToken) { rt.baseURL = strings.TrimRight(u, "/") }
}

// WithRefreshHTTPClient supplies the *http.Client used for refresh calls.
func WithRefreshHTTPClient(h *http.Client) RTOption {
	return func(rt *RefreshingToken) { rt.http = h }
}

// WithOnRefresh registers a callback invoked with each new (access, refresh)
// pair. Use it to persist the rotating refresh token.
func WithOnRefresh(fn func(access, refresh string)) RTOption {
	return func(rt *RefreshingToken) { rt.OnRefresh = fn }
}

// NewRefreshingToken builds a token source from a stored refresh token. Pass an
// empty accessToken to force a refresh on first use.
func NewRefreshingToken(accessToken, refreshToken string, opts ...RTOption) *RefreshingToken {
	rt := &RefreshingToken{
		baseURL: DefaultBaseURL,
		http:    authHTTP,
		access:  accessToken,
		refresh: refreshToken,
	}
	for _, opt := range opts {
		opt(rt)
	}
	return rt
}

// Token returns the current access token, refreshing first if none is held.
func (rt *RefreshingToken) Token(ctx context.Context) (string, error) {
	rt.mu.Lock()
	access := rt.access
	rt.mu.Unlock()
	if access != "" {
		return access, nil
	}
	return rt.Refresh(ctx)
}

// Refresh trades the refresh token for a fresh access token, rotating the
// refresh token and firing OnRefresh.
func (rt *RefreshingToken) Refresh(ctx context.Context) (string, error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.refresh == "" {
		return "", errors.New("frege: no refresh token available; sign in again")
	}
	var s Session
	err := roundTrip(ctx, rt.http, http.MethodPost, baseURLOr(rt.baseURL)+"/v1/auth/refresh", "", "",
		map[string]string{"refresh_token": rt.refresh}, &s)
	if err != nil {
		return "", err
	}
	rt.access = s.Token
	rt.refresh = s.RefreshToken
	if rt.OnRefresh != nil {
		rt.OnRefresh(s.Token, s.RefreshToken)
	}
	return rt.access, nil
}

// ---- sign-in (unauthenticated) ---------------------------------------------

// SendMagicCode emails a six-digit sign-in code to the address, creating the
// account on first use. Follow with VerifyMagicCode. baseURL may be "" for
// DefaultBaseURL.
func SendMagicCode(ctx context.Context, baseURL, email string) error {
	return roundTrip(ctx, authHTTP, http.MethodPost, baseURLOr(baseURL)+"/v1/auth/magic", "", "",
		map[string]string{"email": email}, nil)
}

// VerifyMagicCode exchanges the emailed code for a Session. Store the refresh
// token; it is what keeps your service signed in. baseURL may be "" for
// DefaultBaseURL.
func VerifyMagicCode(ctx context.Context, baseURL, email, code string) (*Session, error) {
	var s Session
	err := roundTrip(ctx, authHTTP, http.MethodPost, baseURLOr(baseURL)+"/v1/auth/magic/verify", "", "",
		map[string]string{"email": email, "code": code}, &s)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// ---- API methods -----------------------------------------------------------

// ListProjects returns the projects the signed-in user can reach in one
// organization. org_id is required by the API; find it in the dashboard URL.
func (c *Client) ListProjects(ctx context.Context, orgID int64) ([]Project, error) {
	var ps []Project
	path := "/v1/projects?org_id=" + strconv.FormatInt(orgID, 10)
	if err := c.do(ctx, http.MethodGet, path, nil, &ps); err != nil {
		return nil, err
	}
	return ps, nil
}

// ListOperations returns the tools generated from the project's active spec:
// their names, HTTP method/path, summary, and JSON input schema.
func (c *Client) ListOperations(ctx context.Context, projectID int64) ([]Operation, error) {
	var ops []Operation
	path := "/v1/projects/" + strconv.FormatInt(projectID, 10) + "/operations"
	if err := c.do(ctx, http.MethodGet, path, nil, &ops); err != nil {
		return nil, err
	}
	return ops, nil
}

// InvokeTool runs one tool and returns the raw upstream response. args are
// keyed by the tool's input schema; pass nil for a tool that takes none.
//
// The returned ToolResult.StatusCode and Body are the UPSTREAM's own: a 404
// there means the upstream answered 404, not that the tool is missing. A
// missing tool, a bad argument, or an auth problem is returned as an *APIError
// instead.
func (c *Client) InvokeTool(ctx context.Context, projectID int64, toolName string, args map[string]any, opts ...InvokeOption) (*ToolResult, error) {
	body := invokeBody{Arguments: args}
	if body.Arguments == nil {
		body.Arguments = map[string]any{}
	}
	for _, opt := range opts {
		opt(&body)
	}
	// Caught here rather than at the server, because the mistake is in the
	// calling code and this is where the line number is. Naming a customer two
	// ways means believing something untrue about your own state, and the
	// server refuses it for the same reason.
	if body.ClientID != nil && body.ClientRef != "" {
		return nil, errors.New("frege: pass AsClient or AsCustomer, not both")
	}
	path := "/v1/projects/" + strconv.FormatInt(projectID, 10) + "/tools/" + url.PathEscape(toolName) + "/invoke"
	var res ToolResult
	if err := c.do(ctx, http.MethodPost, path, body, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

type invokeBody struct {
	Arguments map[string]any `json:"arguments"`
	ClientID  *int64         `json:"client_id,omitempty"`
	ClientRef string         `json:"client_ref,omitempty"`
}

// InvokeOption tunes a single InvokeTool call.
type InvokeOption func(*invokeBody)

// AsClient runs the tool with one connected customer's stored credential
// instead of the project's own. Required wherever the project has no shared
// project-level credential.
//
// The id comes back from enrolling the customer. If you did not keep it, use
// AsCustomer instead — it is almost always the better call.
func AsClient(clientID int64) InvokeOption {
	return func(b *invokeBody) { b.ClientID = &clientID }
}

// AsCustomer runs the tool as the customer you enrolled under this reference.
//
// The reference is the external_ref you supplied when connecting them: the
// phone number, the chat id, whatever your channel already knows them by. That
// is the point — a bot holds a chat id, not a row id, and keeping a table that
// maps one to the other means running a database whose only job is to remember
// something Frege already knows.
//
// It is a LABEL and not a secret. It authorises nothing on its own: your API
// key is what says you may spend this project's customers' credentials, and it
// could spend this one by id regardless. What the customer's own sign-in
// proved, it proved at enrolment.
//
// One caveat worth knowing before you rely on it. The same person enrolled
// twice — once in a browser, once through your bot — is two customers holding
// two different upstream credentials, because linking them needs a verified
// claim common to both and Frege will not guess. A reference that names both is
// REFUSED rather than resolved, since picking one would spend an account you
// did not name. Use AsClient with the id when that happens.
func AsCustomer(reference string) InvokeOption {
	return func(b *invokeBody) { b.ClientRef = reference }
}

// ---- types -----------------------------------------------------------------

// Session is what a sign-in returns: a short-lived access token, a rotating
// refresh token, and the signed-in user.
type Session struct {
	Token        string `json:"token"`
	RefreshToken string `json:"refresh_token"`
	User         User   `json:"user"`
}

// User is the signed-in Frege user.
type User struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Email     string    `json:"email"`
	Locale    string    `json:"locale"`
	Timezone  string    `json:"timezone"`
	CreatedAt time.Time `json:"created_at"`
}

// Operation is one tool generated from the project's spec.
type Operation struct {
	ID          string         `json:"id"`
	ToolName    string         `json:"tool_name"`
	Method      string         `json:"method"`
	Path        string         `json:"path"`
	Summary     string         `json:"summary"`
	Description string         `json:"description"`
	Tags        []string       `json:"tags"`
	InputSchema map[string]any `json:"input_schema"`
}

// Project is one Frege project the user can reach.
type Project struct {
	ID              int64  `json:"id"`
	OrgID           int64  `json:"org_id"`
	Role            string `json:"role"`
	Slug            string `json:"slug"`
	Name            string `json:"name"`
	Description     string `json:"description"`
	UpstreamBaseURL string `json:"upstream_base_url"`
	MCPURL          string `json:"mcp_url"`
	WritePolicy     string `json:"write_policy"`
}

// ToolResult is the raw upstream response from running a tool.
type ToolResult struct {
	ToolName   string `json:"tool_name"`
	Method     string `json:"method"`
	URL        string `json:"url"`
	StatusCode int    `json:"status_code"`
	Body       string `json:"body"`
}

// APIError is a non-2xx response from Frege itself (not from the upstream API).
type APIError struct {
	StatusCode int
	Code       string
	Message    string
	Fields     map[string]string
	RequestID  string
}

// Error implements error.
func (e *APIError) Error() string {
	msg := fmt.Sprintf("frege: %d %s: %s", e.StatusCode, e.Code, e.Message)
	if len(e.Fields) > 0 {
		parts := make([]string, 0, len(e.Fields))
		for k, v := range e.Fields {
			parts = append(parts, k+": "+v)
		}
		sort.Strings(parts)
		msg += " (" + strings.Join(parts, "; ") + ")"
	}
	return msg
}

// IsAuth reports whether the error is an authentication failure (401).
func (e *APIError) IsAuth() bool { return e.StatusCode == http.StatusUnauthorized }

// ---- transport -------------------------------------------------------------

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	token, err := c.tokens.Token(ctx)
	if err != nil {
		return err
	}
	err = roundTrip(ctx, c.http, method, c.baseURL+path, token, c.stage, body, out)

	// A rejected token gets exactly one refresh-and-retry, and only if the token
	// source knows how to refresh. This is where a short-lived access token
	// silently renews mid-flight.
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusUnauthorized {
		if rf, ok := c.tokens.(refresher); ok {
			newTok, rerr := rf.Refresh(ctx)
			if rerr != nil {
				return rerr
			}
			return roundTrip(ctx, c.http, method, c.baseURL+path, newTok, c.stage, body, out)
		}
	}
	return err
}

// roundTrip performs one request and decodes the envelope. A 2xx unwraps the
// "data" field into out (nil out is fine, e.g. a 204). Anything else becomes an
// *APIError from the "error" envelope.
func roundTrip(ctx context.Context, hc *http.Client, method, fullURL, token, stage string, body, out any) error {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, fullURL, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	// Sent only when chosen. An empty header is not the same as no header, and
	// the server reads absence as live.
	if stage != "" {
		req.Header.Set(StageHeader, stage)
	}

	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if out == nil || len(bytes.TrimSpace(data)) == 0 {
			return nil
		}
		var env struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(data, &env); err != nil {
			return fmt.Errorf("frege: decoding response: %w", err)
		}
		if err := json.Unmarshal(env.Data, out); err != nil {
			return fmt.Errorf("frege: decoding response data: %w", err)
		}
		return nil
	}

	apiErr := &APIError{StatusCode: resp.StatusCode}
	var env struct {
		Error struct {
			Code      string            `json:"code"`
			Message   string            `json:"message"`
			Fields    map[string]string `json:"fields"`
			RequestID string            `json:"request_id"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &env) == nil {
		apiErr.Code = env.Error.Code
		apiErr.Message = env.Error.Message
		apiErr.Fields = env.Error.Fields
		apiErr.RequestID = env.Error.RequestID
	}
	if apiErr.Message == "" {
		apiErr.Message = strings.TrimSpace(string(data))
	}
	return apiErr
}

func baseURLOr(u string) string {
	if u == "" {
		return DefaultBaseURL
	}
	return strings.TrimRight(u, "/")
}
