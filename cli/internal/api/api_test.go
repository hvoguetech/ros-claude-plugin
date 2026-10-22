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
