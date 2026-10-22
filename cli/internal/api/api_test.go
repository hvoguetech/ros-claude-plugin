package api

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func errFrom(status int, body string) *Error {
	return decodeError(&http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}).(*Error)
}

// The three shapes the backend answers errors with.
func TestDecodeError(t *testing.T) {
	cases := []struct {
		body                 string
		code, reason, detail string
	}{
		{`{"error":"invalid_grant","error_description":"refresh token reuse detected"}`, "invalid_grant", "", "refresh token reuse detected"},
		{`{"detail":"project not found"}`, "", "", "project not found"},
		{`{"detail":{"error":"project_unavailable","reason":"deleted","message":"project p1 is unavailable"}}`, "project_unavailable", "deleted", "project p1 is unavailable"},
		{`{"detail":{"code":"local_agent_disabled"}}`, "local_agent_disabled", "", ""},
		{`{"error":"local_agent_disabled","reason":"org","message":"off"}`, "local_agent_disabled", "org", "off"},
		// backend #440: top-level code + reason, human text in `detail`
		{`{"error":"project_unavailable","reason":"deleted","project":"p1","detail":"Project p1 was deleted. Run ` + "`ros-local link`" + `."}`, "project_unavailable", "deleted", "Project p1 was deleted. Run `ros-local link`."},
		{`{"error":"local_agent_disabled","reason":"server_disabled","detail":"The local agent is not enabled."}`, "local_agent_disabled", "server_disabled", "The local agent is not enabled."},
		{`{"error":"local_agent_disabled","error_description":"off","reason":"server_disabled"}`, "local_agent_disabled", "server_disabled", "off"},
		{`{"error":"no_project","detail":"No project could be resolved."}`, "no_project", "", "No project could be resolved."},
		{`<html>bad gateway</html>`, "", "", "<html>bad gateway</html>"},
	}
	for _, c := range cases {
		e := errFrom(403, c.body)
		if e.Code != c.code || e.Reason != c.reason || e.Detail != c.detail {
			t.Errorf("%s → code=%q reason=%q detail=%q", c.body, e.Code, e.Reason, e.Detail)
		}
	}
	if !HasCode(errFrom(403, `{"detail":{"error":"project_unavailable"}}`), CodeProjectUnavailable) {
		t.Fatal("HasCode")
	}
}
