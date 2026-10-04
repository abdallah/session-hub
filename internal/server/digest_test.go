package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

func TestPutDigestEndpoint(t *testing.T) {
	e := newEnv(t)
	e.register(e.tokA, api.SessionUpsert{ID: sid1})
	asOf := time.Date(2026, 9, 30, 11, 0, 0, 0, time.UTC)
	d := api.DigestIn{AsOf: asOf, Recap: "Recap text.",
		Links: []api.DigestLink{{Number: "7", URL: "https://github.com/o/r/pull/7"}}}

	code, body, _ := e.do("PUT", "/v1/sessions/"+sid1+"/digest", e.tokA, d)
	if code != http.StatusOK {
		t.Fatalf("PUT digest = %d %s", code, body)
	}
	var s api.Session
	if err := json.Unmarshal(body, &s); err != nil {
		t.Fatal(err)
	}
	if s.Recap != "Recap text." || s.Summary == nil || s.Summary.LatestLink == nil {
		t.Errorf("response session = %+v", s)
	}

	// The list carries the summary, the detail carries the digest.
	code, body, _ = e.do("GET", "/v1/sessions/"+sid1, e.tokA, nil)
	var det api.SessionDetail
	if code != 200 || json.Unmarshal(body, &det) != nil || det.Digest == nil || det.Digest.Links[0].Number != "7" {
		t.Errorf("GET detail = %d %s", code, body)
	}

	// Wrong paths.
	for name, tc := range map[string]struct {
		path string
		body any
		want int
	}{
		"unknown session": {"/v1/sessions/nope/digest", d, 404},
		"javascript link": {"/v1/sessions/" + sid1 + "/digest",
			api.DigestIn{AsOf: asOf, Links: []api.DigestLink{{URL: "javascript:alert(1)"}}}, 400},
		"no as_of": {"/v1/sessions/" + sid1 + "/digest", api.DigestIn{}, 400},
		"not json": {"/v1/sessions/" + sid1 + "/digest", []byte(`{`), 400},
		"oversize": {"/v1/sessions/" + sid1 + "/digest", map[string]string{"recap": strings.Repeat("x", 17<<10)}, 413},
	} {
		code, body, _ := e.do("PUT", tc.path, e.tokA, tc.body)
		if code != tc.want {
			t.Errorf("%s: %d %s, want %d", name, code, body, tc.want)
		}
	}
	// Another machine's token: 409, and the digest is unchanged.
	code, _, _ = e.do("PUT", "/v1/sessions/"+sid1+"/digest", e.tokB, api.DigestIn{AsOf: asOf.Add(time.Minute), Recap: "hijack"})
	if code != http.StatusConflict {
		t.Errorf("other machine = %d, want 409", code)
	}
	_, body, _ = e.do("GET", "/v1/sessions/"+sid1, e.tokA, nil)
	json.Unmarshal(body, &det)
	if det.Recap != "Recap text." {
		t.Errorf("recap after refused writes = %q", det.Recap)
	}
}
