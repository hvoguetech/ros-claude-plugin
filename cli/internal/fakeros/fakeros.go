// Package fakeros is an in-memory fake of the ROS backend surface ros-local uses (`/v1/oauth/*`,
// `/v1/local/*`), for tests. Its wire format follows the backend's routers/local.py, routers/local_auth.py
// and services/device_auth.py.
package fakeros

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"time"
)

// Skill is one server-side skill: files keyed `<name>/<path>`, in order.
type Skill struct {
	Name    string
	ID      string
	Version int
	Files   [][2]string // (path relative to the skill dir, content)
}

// Server is the fake. Fields may be changed between requests (under Mu).
type Server struct {
	*httptest.Server
	Mu sync.Mutex

	// device flow
	PendingPolls  int    // authorization_pending answers before approval
	SlowDownPolls int    // slow_down answers (sent first)
	DenyLogin     bool   // answer access_denied
	ProjectID     string // the project approved for the device
	DeviceID      string
	AccessTTL     int // seconds

	// refresh
	RefreshError string // OAuth error to answer refreshes with ("" = succeed)
	RefreshCalls int

	// /v1/local
	Projects       map[string]string // id → name
	Repos          map[string]string // repo key → project id
	Skills         []Skill
	Context        string
	SessionStatus  int // non-zero: answer POST /sessions with this status
	SessionDetail  string
	Sessions       []map[string]any // bodies received
	Ended          []string         // run ids ended
	ManifestCalls  int
	BundleCalls    [][]string
	LastAuth       string // Authorization header of the last /v1/local request
	issued         int
	validTokens    map[string]bool
	RefreshTokenID string
}

// New starts a fake with one project and an empty skill set.
func New() *Server {
	s := &Server{
		ProjectID: "p1", DeviceID: "dev1", AccessTTL: 3600,
		Projects:    map[string]string{"p1": "Payments"},
		Repos:       map[string]string{},
		validTokens: map[string]bool{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/oauth/device_authorization", s.deviceAuthorization)
	mux.HandleFunc("POST /v1/oauth/token", s.token)
	mux.HandleFunc("GET /v1/local/me", s.auth(s.me))
	mux.HandleFunc("POST /v1/local/sessions", s.auth(s.openSession))
	mux.HandleFunc("POST /v1/local/sessions/{id}/end", s.auth(s.endSession))
	mux.HandleFunc("GET /v1/local/skills/manifest", s.auth(s.manifest))
	mux.HandleFunc("GET /v1/local/skills/bundle", s.auth(s.bundle))
	s.Server = httptest.NewServer(mux)
	return s
}

// IssueToken mints a valid access token (as if from a previous login).
func (s *Server) IssueToken() string {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	return s.issueLocked()
}

func (s *Server) issueLocked() string {
	s.issued++
	t := fmt.Sprintf("access-%d", s.issued)
	s.validTokens[t] = true
	return t
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func oauthError(w http.ResponseWriter, code, desc string) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": code, "error_description": desc})
}

func (s *Server) deviceAuthorization(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	if r.PostForm.Get("client_id") == "" {
		oauthError(w, "invalid_client", "client_id is required")
		return
	}
	writeJSON(w, 200, map[string]any{
		"device_code": "dc-1", "user_code": "BCDF-GHJK", "verification_uri": s.URL + "/device",
		"verification_uri_complete": s.URL + "/device?user_code=BCDF-GHJK", "expires_in": 600, "interval": 5,
	})
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	s.Mu.Lock()
	defer s.Mu.Unlock()
	switch r.PostForm.Get("grant_type") {
	case "urn:ietf:params:oauth:grant-type:device_code":
		if r.PostForm.Get("device_code") != "dc-1" {
			oauthError(w, "invalid_grant", "unknown device_code")
			return
		}
		if s.SlowDownPolls > 0 {
			s.SlowDownPolls--
			oauthError(w, "slow_down", "poll slower")
			return
		}
		if s.PendingPolls > 0 {
			s.PendingPolls--
			oauthError(w, "authorization_pending", "waiting")
			return
		}
		if s.DenyLogin {
			oauthError(w, "access_denied", "denied")
			return
		}
		s.RefreshTokenID = "refresh-1"
		writeJSON(w, 200, map[string]any{
			"access_token": s.issueLocked(), "token_type": "Bearer", "expires_in": s.AccessTTL,
			"refresh_token": s.RefreshTokenID, "scope": "local:session memory:read skills:read",
			"device_id": s.DeviceID, "project_id": s.ProjectID,
		})
	case "refresh_token":
		s.RefreshCalls++
		if s.RefreshError != "" {
			if s.RefreshError == "server_error" {
				writeJSON(w, 503, map[string]string{"detail": "unavailable"})
				return
			}
			oauthError(w, s.RefreshError, "refused")
			return
		}
		writeJSON(w, 200, map[string]any{"access_token": s.issueLocked(), "token_type": "Bearer",
			"expires_in": s.AccessTTL, "scope": "local:session", "device_id": s.DeviceID})
	default:
		oauthError(w, "unsupported_grant_type", "")
	}
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.Mu.Lock()
		h := r.Header.Get("Authorization")
		s.LastAuth = h
		ok := strings.HasPrefix(h, "Bearer ") && s.validTokens[strings.TrimPrefix(h, "Bearer ")]
		s.Mu.Unlock()
		if !ok {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeJSON(w, 401, map[string]string{"detail": "invalid token"})
			return
		}
		next(w, r)
	}
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	var projects []map[string]string
	for _, id := range sortedKeys(s.Projects) {
		projects = append(projects, map[string]string{"id": id, "name": s.Projects[id]})
	}
	var repos []map[string]string
	for _, k := range sortedKeys(s.Repos) {
		repos = append(repos, map[string]string{"repo": k, "project_id": s.Repos[k]})
	}
	def := ""
	if _, ok := s.Projects[s.ProjectID]; ok {
		def = s.ProjectID
	}
	writeJSON(w, 200, map[string]any{
		"user": map[string]string{"id": "u1", "email": "dev@example.com"}, "via": "device", "device_id": s.DeviceID,
		"scopes": []string{"local:session"}, "projects": projects, "default_project_id": def, "repos": repos,
	})
}

func (s *Server) openSession(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	s.Mu.Lock()
	defer s.Mu.Unlock()
	s.Sessions = append(s.Sessions, body)
	if s.SessionStatus != 0 {
		writeJSON(w, s.SessionStatus, map[string]string{"detail": s.SessionDetail})
		return
	}
	pid, _ := body["project_id"].(string)
	if pid == "" {
		pid = s.ProjectID
	}
	etag := s.etagLocked()
	writeJSON(w, 201, map[string]any{
		"run_id": fmt.Sprintf("run-%d", len(s.Sessions)), "thread_id": "t1", "project_id": pid,
		"project_name": s.Projects[pid], "reopened": false, "run_token": "rt", "expires_at": time.Now().Add(time.Hour),
		"context": s.Context, "recall": map[string]any{"profile": "personal_agent", "counts": map[string]int{},
			"dropped": 0, "item_ids": []string{"m1", "m2"}},
		"skills_manifest_etag": etag,
		"policy": map[string]any{"deny_paths": []string{}, "deny_commands": []string{}, "mcp_allowlist": []string{},
			"egress_allowlist": []string{}, "enforcement": "audit", "etag": `"p"`},
		"trace_url": s.URL + "/traces/t1",
	})
}

func (s *Server) endSession(w http.ResponseWriter, r *http.Request) {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	s.Ended = append(s.Ended, r.PathValue("id"))
	writeJSON(w, 200, map[string]any{"run_id": r.PathValue("id"), "status": "ended", "ended_at": time.Now()})
}

// SkillSHA is the backend's files_sha256 for one skill.
func SkillSHA(sk Skill) string {
	h := sha256.New()
	for _, f := range sk.Files {
		h.Write([]byte(sk.Name + "/" + f[0]))
		h.Write([]byte{0})
		h.Write([]byte(f[1]))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (s *Server) entriesLocked() []map[string]any {
	var out []map[string]any
	for _, sk := range s.Skills {
		out = append(out, map[string]any{"name": sk.Name, "skill_id": sk.ID, "version": sk.Version,
			"sha256": SkillSHA(sk), "label": nil})
	}
	return out
}

func (s *Server) etagLocked() string {
	b, _ := json.Marshal(s.entriesLocked())
	sum := sha256.Sum256(b)
	return `"` + hex.EncodeToString(sum[:])[:32] + `"`
}

func (s *Server) manifest(w http.ResponseWriter, r *http.Request) {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	s.ManifestCalls++
	etag := s.etagLocked()
	if r.Header.Get("If-None-Match") == etag {
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("ETag", etag)
	writeJSON(w, 200, map[string]any{"project_id": r.URL.Query().Get("project_id"), "etag": etag,
		"skills": s.entriesLocked()})
}

func (s *Server) bundle(w http.ResponseWriter, r *http.Request) {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	names := strings.Split(r.URL.Query().Get("names"), ",")
	s.BundleCalls = append(s.BundleCalls, names)
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, sk := range s.Skills {
		if !want[sk.Name] {
			continue
		}
		for _, f := range sk.Files {
			fw, _ := zw.Create(sk.Name + "/" + f[0])
			_, _ = fw.Write([]byte(f[1]))
		}
	}
	_ = zw.Close()
	w.Header().Set("Content-Type", "application/zip")
	_, _ = w.Write(buf.Bytes())
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
