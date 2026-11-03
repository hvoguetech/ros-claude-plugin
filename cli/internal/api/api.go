// Package api is ros-local's client for the ROS backend: the RFC 8628 device flow on `/v1/oauth/*` and the
// local connector surface `/v1/local/*` (spec §17).
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DeviceCodeGrant is the RFC 8628 grant type.
const DeviceCodeGrant = "urn:ietf:params:oauth:grant-type:device_code"

// MaxBundleBytes caps a skills bundle download.
const MaxBundleBytes = 32 << 20

// Client talks to one ROS deployment. Token is the device access token (or a PAT) for `/v1/local/*`.
type Client struct {
	BaseURL   string
	Token     string
	HTTP      *http.Client
	UserAgent string
}

// New returns a client with a per-request timeout.
func New(baseURL, token, userAgent string, timeout time.Duration) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), Token: token, UserAgent: userAgent,
		HTTP: &http.Client{Timeout: timeout}}
}

// Error codes the backend uses for local-agent conditions ros-local reacts to.
const (
	// CodeLocalAgentDisabled: the org or project hasn't turned the local agent on.
	CodeLocalAgentDisabled = "local_agent_disabled"
	// CodeProjectUnavailable: the requested project (a repo's link, or X-ROS-Project) can't be used.
	// Reason: not_found, deleted (404), archived, no_access, local_agent_disabled (403).
	CodeProjectUnavailable = "project_unavailable"
	// CodeNoProject: no project was named and none resolves for the caller (404).
	CodeNoProject = "no_project"
)

// Error is a non-2xx answer. Code is the machine-readable error (an OAuth `error`, or a structured
// `detail.error` / `detail.code`), Reason its sub-reason when the backend gives one.
type Error struct {
	Status int
	Code   string
	Reason string
	Detail string
}

func (e *Error) Error() string {
	switch {
	case e.Code != "" && e.Detail != "":
		return fmt.Sprintf("%s: %s", e.Code, e.Detail)
	case e.Detail != "":
		return fmt.Sprintf("HTTP %d: %s", e.Status, e.Detail)
	case e.Code != "":
		return e.Code
	default:
		return fmt.Sprintf("HTTP %d", e.Status)
	}
}

// IsStatus reports whether err is an *Error with that HTTP status.
func IsStatus(err error, status int) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == status
}

// OAuthCode returns the error code of err (OAuth `error` or structured `detail.error`), or "".
func OAuthCode(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// HasCode reports whether err carries that error code.
func HasCode(err error, code string) bool { return code != "" && OAuthCode(err) == code }

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// decodeError reads the three shapes the backend answers with: an OAuth error
// ({"error", "error_description"}), FastAPI's {"detail": "text"}, and a structured
// {"detail": {"error"|"code", "reason", "message"|"detail"}} (or the same object at the top level).
func decodeError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	e := &Error{Status: resp.StatusCode}
	var body map[string]any
	if json.Unmarshal(b, &body) == nil {
		e.Code = firstString(body, "error", "code")
		e.Reason = firstString(body, "reason")
		e.Detail = firstString(body, "error_description", "message")
		switch d := body["detail"].(type) {
		case string:
			if e.Detail == "" {
				e.Detail = d
			}
		case map[string]any:
			if c := firstString(d, "error", "code"); c != "" {
				e.Code = c
			}
			if r := firstString(d, "reason"); r != "" {
				e.Reason = r
			}
			if m := firstString(d, "message", "detail", "error_description"); m != "" {
				e.Detail = m
			}
		case nil:
		default:
			if j, err := json.Marshal(d); err == nil && e.Detail == "" {
				e.Detail = string(j)
			}
		}
	}
	if e.Detail == "" && e.Code == "" {
		e.Detail = strings.TrimSpace(string(b))
		if len(e.Detail) > 200 {
			e.Detail = e.Detail[:200]
		}
	}
	return e
}

func (c *Client) newRequest(ctx context.Context, method, path string, q url.Values, body io.Reader) (*http.Request, error) {
	u := c.BaseURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	return req, nil
}

func (c *Client) doJSON(req *http.Request, out any) error {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return decodeError(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out)
}

func (c *Client) postForm(ctx context.Context, path string, form url.Values, out any) error {
	req, err := c.newRequest(ctx, http.MethodPost, path, nil, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Del("Authorization") // the token endpoint authenticates by grant, never by bearer
	return c.doJSON(req, out)
}

func (c *Client) postJSON(ctx context.Context, path string, in, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := c.newRequest(ctx, http.MethodPost, path, nil, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	return c.doJSON(req, out)
}

// --- device flow --------------------------------------------------------------------------------------

// DeviceAuthorization is the RFC 8628 §3.2 response.
type DeviceAuthorization struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// DeviceRequest is what `ros-local login` sends to start the flow. Scope empty = every local scope.
type DeviceRequest struct {
	Scope, ProjectID, DeviceName, Platform, PluginVersion string
}

// StartDeviceAuthorization calls `POST /v1/oauth/device_authorization`.
func (c *Client) StartDeviceAuthorization(ctx context.Context, clientID string, r DeviceRequest) (*DeviceAuthorization, error) {
	form := url.Values{"client_id": {clientID}}
	for k, v := range map[string]string{"scope": r.Scope, "project_id": r.ProjectID, "device_name": r.DeviceName,
		"platform": r.Platform, "plugin_version": r.PluginVersion} {
		if v != "" {
			form.Set(k, v)
		}
	}
	var out DeviceAuthorization
	if err := c.postForm(ctx, "/v1/oauth/device_authorization", form, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Token is a `/v1/oauth/token` answer. RefreshToken is empty on a refresh (the backend doesn't rotate it).
type Token struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
	DeviceID     string `json:"device_id"`
	ProjectID    string `json:"project_id"`
}

// ExchangeDeviceCode polls the token endpoint once. RFC 8628 errors come back as *Error with Code set
// (authorization_pending, slow_down, access_denied, expired_token).
func (c *Client) ExchangeDeviceCode(ctx context.Context, clientID, deviceCode string) (*Token, error) {
	var out Token
	err := c.postForm(ctx, "/v1/oauth/token", url.Values{
		"grant_type": {DeviceCodeGrant}, "device_code": {deviceCode}, "client_id": {clientID}}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Refresh exchanges the device refresh token for a new access token.
func (c *Client) Refresh(ctx context.Context, clientID, refreshToken string) (*Token, error) {
	var out Token
	err := c.postForm(ctx, "/v1/oauth/token", url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {refreshToken}, "client_id": {clientID}}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// --- /v1/local ----------------------------------------------------------------------------------------

type Project struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type RepoLink struct {
	Repo      string `json:"repo"` // host/owner/name
	ProjectID string `json:"project_id"`
}

// Me is `GET /v1/local/me`.
type Me struct {
	User struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	} `json:"user"`
	Via              string     `json:"via"`
	DeviceID         string     `json:"device_id"`
	Scopes           []string   `json:"scopes"`
	Projects         []Project  `json:"projects"`
	DefaultProjectID string     `json:"default_project_id"`
	Repos            []RepoLink `json:"repos"`
	MinPluginVersion string     `json:"min_plugin_version,omitempty"`
	MinCLIVersion    string     `json:"min_cli_version,omitempty"`
}

func (c *Client) Me(ctx context.Context) (*Me, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/v1/local/me", nil, nil)
	if err != nil {
		return nil, err
	}
	var out Me
	if err := c.doJSON(req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SessionRequest is `POST /v1/local/sessions` (spec §5).
type SessionRequest struct {
	ProjectID              string   `json:"project_id,omitempty"`
	Repo                   string   `json:"repo,omitempty"`
	Branch                 string   `json:"branch,omitempty"`
	Head                   string   `json:"head,omitempty"`
	Harness                string   `json:"harness"`
	HarnessVersion         string   `json:"harness_version,omitempty"`
	PluginVersion          string   `json:"plugin_version,omitempty"`
	HarnessSessionID       string   `json:"harness_session_id"`
	Source                 string   `json:"source"`
	ParentHarnessSessionID string   `json:"parent_harness_session_id,omitempty"`
	CwdName                string   `json:"cwd_name,omitempty"`
	RecentCommits          []string `json:"recent_commits,omitempty"`
}

type Recall struct {
	Profile string         `json:"profile"`
	Counts  map[string]int `json:"counts"`
	Dropped int            `json:"dropped"`
	ItemIDs []string       `json:"item_ids"`
}

type Policy struct {
	DenyPaths       []string `json:"deny_paths"`
	DenyCommands    []string `json:"deny_commands"`
	MCPAllowlist    []string `json:"mcp_allowlist"`
	EgressAllowlist []string `json:"egress_allowlist"`
	Enforcement     string   `json:"enforcement"`
	ETag            string   `json:"etag"`
}

// Session is the `POST /v1/local/sessions` answer. MinPluginVersion / MinCLIVersion (spec §13a) are read
// when present: below them, ROS is off for the session and the banner says what to update.
type Session struct {
	RunID              string  `json:"run_id"`
	ThreadID           string  `json:"thread_id"`
	ProjectID          string  `json:"project_id"`
	ProjectName        string  `json:"project_name"`
	Reopened           bool    `json:"reopened"`
	RunToken           string  `json:"run_token"`
	ExpiresAt          Stamp   `json:"expires_at"`
	Context            string  `json:"context"`
	Recall             *Recall `json:"recall"`
	SkillsManifestETag *string `json:"skills_manifest_etag"`
	Policy             *Policy `json:"policy"`
	TraceURL           string  `json:"trace_url"`
	MinPluginVersion   string  `json:"min_plugin_version,omitempty"`
	MinCLIVersion      string  `json:"min_cli_version,omitempty"`
}

func (c *Client) OpenSession(ctx context.Context, in *SessionRequest) (*Session, error) {
	var out Session
	if err := c.postJSON(ctx, "/v1/local/sessions", in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// EndSession calls `POST /v1/local/sessions/{id}/end`.
func (c *Client) EndSession(ctx context.Context, runID, reason string) error {
	body := map[string]any{}
	if reason != "" {
		if len(reason) > 60 {
			reason = reason[:60]
		}
		body["reason"] = reason
	}
	return c.postJSON(ctx, "/v1/local/sessions/"+url.PathEscape(runID)+"/end", body, nil)
}

// SkillEntry is one manifest row (spec §7).
type SkillEntry struct {
	Name    string  `json:"name"`
	SkillID string  `json:"skill_id"`
	Version int     `json:"version"`
	SHA256  string  `json:"sha256"`
	Label   *string `json:"label"`
}

type Manifest struct {
	ProjectID string       `json:"project_id"`
	ETag      string       `json:"etag"`
	Skills    []SkillEntry `json:"skills"`
}

func scopeQuery(projectID, repo string) url.Values {
	q := url.Values{}
	if projectID != "" {
		q.Set("project_id", projectID)
	} else if repo != "" {
		q.Set("repo", repo)
	}
	return q
}

// SkillsManifest is a conditional GET: (nil, true, nil) when ifNoneMatch still matches (304).
func (c *Client) SkillsManifest(ctx context.Context, projectID, repo, ifNoneMatch string) (*Manifest, bool, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/v1/local/skills/manifest", scopeQuery(projectID, repo), nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Accept", "application/json")
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return nil, true, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, false, decodeError(resp)
	}
	var m Manifest
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&m); err != nil {
		return nil, false, err
	}
	if m.ETag == "" {
		m.ETag = resp.Header.Get("ETag")
	}
	return &m, false, nil
}

// SkillsBundle downloads the zip of `<name>/SKILL.md` + files for names.
func (c *Client) SkillsBundle(ctx context.Context, projectID, repo string, names []string) ([]byte, error) {
	q := scopeQuery(projectID, repo)
	q.Set("names", strings.Join(names, ","))
	req, err := c.newRequest(ctx, http.MethodGet, "/v1/local/skills/bundle", q, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, decodeError(resp)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, MaxBundleBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxBundleBytes {
		return nil, fmt.Errorf("skills bundle is larger than %d MB", MaxBundleBytes>>20)
	}
	return b, nil
}

// --- pick up / send (routers/local_pickup.py) ---------------------------------------------------------

// PickupSnapshot is the conversation's latest workspace snapshot and where to fetch it (a /v1/local path).
type PickupSnapshot struct {
	ArtifactID string `json:"artifact_id"`
	Filename   string `json:"filename"`
	Size       int64  `json:"size"`
	TakenAt    string `json:"taken_at"`
	URL        string `json:"url"`
}

type PickupRepo struct {
	FullName string `json:"full_name"`
	URL      string `json:"url"`
	Branch   string `json:"branch"`
}

type PickupPR struct {
	URL   string `json:"url"`
	Title string `json:"title"`
}

// Pickup is `GET /v1/local/runs/{id}/pickup`: what continuing a cloud run on this machine needs.
type Pickup struct {
	RunID        string          `json:"run_id"`
	ThreadID     string          `json:"thread_id"`
	Status       string          `json:"status"`
	ProjectID    string          `json:"project_id"`
	WorkflowName string          `json:"workflow_name"`
	Snapshot     *PickupSnapshot `json:"snapshot"`
	Repos        []PickupRepo    `json:"repos"`
	PullRequests []PickupPR      `json:"pull_requests"`
	Question     string          `json:"question"`
	Report       string          `json:"report"`
	Folder       string          `json:"folder"`
	Steps        []string        `json:"steps"`
	CLI          string          `json:"cli"`
	Briefing     string          `json:"briefing"`
	PickedUp     map[string]any  `json:"picked_up"`
}

func (c *Client) RunPickup(ctx context.Context, runID string) (*Pickup, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/v1/local/runs/"+url.PathEscape(runID)+"/pickup", nil, nil)
	if err != nil {
		return nil, err
	}
	var out Pickup
	if err := c.doJSON(req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// MarkPickup records on the run that it was picked up here (`POST /v1/local/runs/{id}/pickup`).
func (c *Client) MarkPickup(ctx context.Context, runID, machine, folder string) error {
	body := map[string]any{}
	if machine != "" {
		body["machine"] = machine
	}
	if folder != "" {
		body["folder"] = folder
	}
	return c.postJSON(ctx, "/v1/local/runs/"+url.PathEscape(runID)+"/pickup", body, nil)
}

// Download streams the body of an authenticated GET (an artifact's bytes). The caller closes it.
func (c *Client) Download(ctx context.Context, path string) (io.ReadCloser, int64, error) {
	req, err := c.newRequest(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		return nil, 0, decodeError(resp)
	}
	return resp.Body, resp.ContentLength, nil
}

type TaskLink struct {
	URL   string `json:"url"`
	Title string `json:"title,omitempty"`
}

// TaskRequest is `POST /v1/local/tasks` (`ros-local send`).
type TaskRequest struct {
	ProjectID     string            `json:"project_id,omitempty"`
	Repo          string            `json:"repo,omitempty"`
	Message       string            `json:"message"`
	Title         string            `json:"title,omitempty"`
	RepoFullNames []string          `json:"repo_full_names,omitempty"`
	RepoBranches  map[string]string `json:"repo_branches,omitempty"`
	Links         []TaskLink        `json:"links,omitempty"`
}

type TaskResult struct {
	TaskID    string `json:"task_id"`
	ProjectID string `json:"project_id"`
	Status    string `json:"status"`
	Title     string `json:"title"`
	URL       string `json:"url"`
}

func (c *Client) CreateTask(ctx context.Context, in *TaskRequest) (*TaskResult, error) {
	var out TaskResult
	if err := c.postJSON(ctx, "/v1/local/tasks", in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// --- peers: /v1/mcp/me tool calls and the inbox long-poll (services/peer_messaging/local_peer.py) --------

// MCPCall calls one tool on `/v1/mcp/me` (plain JSON-RPC) and returns its text — JSON for the peer tools.
func (c *Client) MCPCall(ctx context.Context, name string, args map[string]any) (string, error) {
	body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args}}
	var out struct {
		Result *struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := c.postJSON(ctx, "/v1/mcp/me", body, &out); err != nil {
		return "", err
	}
	if out.Error != nil {
		return "", fmt.Errorf("%s: %s", name, out.Error.Message)
	}
	if out.Result == nil {
		return "", fmt.Errorf("%s: empty result", name)
	}
	text := ""
	if len(out.Result.Content) > 0 {
		text = out.Result.Content[0].Text
	}
	if out.Result.IsError {
		return "", fmt.Errorf("%s: %s", name, text)
	}
	return text, nil
}

// PeerMessage is one inbox row (local_peer.inbox / wait_inbox).
type PeerMessage struct {
	MessageID    string `json:"message_id"`
	SenderTaskID string `json:"sender_task_id"`
	Kind         string `json:"kind"` // request | note | reply
	Body         struct {
		Text string         `json:"text"`
		Data map[string]any `json:"data"`
	} `json:"body"`
	Sequence   int    `json:"sequence"`
	ResponseTo string `json:"response_to"`
}

type PeerPage struct {
	Messages   []PeerMessage `json:"messages"`
	NextCursor *int          `json:"next_cursor"`
}

// PeersWait long-polls a session's peer inbox (`GET /v1/local/sessions/{id}/peers/wait`).
func (c *Client) PeersWait(ctx context.Context, runID string, after, timeout int) (*PeerPage, error) {
	q := url.Values{"after": {strconv.Itoa(after)}, "timeout": {strconv.Itoa(timeout)}}
	req, err := c.newRequest(ctx, http.MethodGet, "/v1/local/sessions/"+url.PathEscape(runID)+"/peers/wait", q, nil)
	if err != nil {
		return nil, err
	}
	// A long-poll outlives the client's usual per-request timeout: give this call the poll's own budget plus slack.
	long := *c.HTTP
	long.Timeout = time.Duration(timeout+15) * time.Second
	resp, err := long.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, decodeError(resp)
	}
	var out PeerPage
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PeerSummary is `GET /v1/local/sessions/{id}/peers/summary` (pending is a count, or "5+").
type PeerSummary struct {
	Pending json.RawMessage `json:"pending"`
	Latest  []struct {
		MessageID string `json:"message_id"`
		From      string `json:"from"`
		Kind      string `json:"kind"`
		Preview   string `json:"preview"`
	} `json:"latest"`
}

// PendingText renders the pending count ("0", "3", "5+").
func (s *PeerSummary) PendingText() string {
	return strings.Trim(strings.TrimSpace(string(s.Pending)), `"`)
}

func (c *Client) PeersSummary(ctx context.Context, runID string) (*PeerSummary, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/v1/local/sessions/"+url.PathEscape(runID)+"/peers/summary", nil, nil)
	if err != nil {
		return nil, err
	}
	var out PeerSummary
	if err := c.doJSON(req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Stamp is a time the backend sends as RFC 3339, or — from an older server — as a naive UTC datetime with no
// offset ("2026-11-03T10:59:05.664194"). Either parses; the zero value is an empty string.
type Stamp struct{ time.Time }

func (s *Stamp) UnmarshalJSON(b []byte) error {
	var raw string
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	if raw == "" {
		s.Time = time.Time{}
		return nil
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999", "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, raw); err == nil {
			s.Time = t.UTC()
			return nil
		}
	}
	return fmt.Errorf("unrecognised time %q", raw)
}
