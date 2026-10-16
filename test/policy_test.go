package test

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"testing"
)

func (e *env) raw(method, path, token, contentType string, body []byte) int {
	req, _ := http.NewRequest(method, e.srv.URL+path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

// The model allow-list cannot be bypassed with other content types,
// whitespace, multipart bodies or duplicate keys.
func TestAllowListCannotBeBypassed(t *testing.T) {
	e := newEnv(t, "xai-ok")
	var tok struct{ Token string }
	e.api("POST", "/api/v1/tokens", map[string]any{"name": "r", "allowed_models": []string{"grok-4.3"}}, 201, &tok)
	cases := []struct {
		name, ct string
		body     string
		want     int
	}{
		{"text/plain + whitespace", "text/plain", "  \n{\"model\":\"grok-4.7\",\"messages\":[]}", 403},
		{"no content type", "", "{\"model\":\"grok-4.7\"}", 403},
		{"duplicate model keys", "application/json", "{\"model\":\"grok-4.3\",\"model\":\"grok-4.7\"}", 400},
		{"missing model", "application/json", "{\"messages\":[]}", 400},
		{"allowed", "application/json", "{\"model\":\"grok-4.3\",\"messages\":[]}", 200},
	}
	for _, c := range cases {
		if got := e.raw("POST", "/v1/chat/completions", tok.Token, c.ct, []byte(c.body)); got != c.want {
			t.Errorf("%s: status %d, want %d", c.name, got, c.want)
		}
	}
	var mp bytes.Buffer
	w := multipart.NewWriter(&mp)
	_ = w.WriteField("prompt", "a cat")
	_ = w.WriteField("model", "grok-imagine-image-quality")
	fw, _ := w.CreateFormFile("image", "x.png")
	_, _ = fw.Write([]byte("png"))
	w.Close()
	if got := e.raw("POST", "/v1/images/edits", tok.Token, w.FormDataContentType(), mp.Bytes()); got != 403 {
		t.Errorf("multipart model: status %d, want 403", got)
	}
}

// Stored responses and async jobs belong to the token that created them.
func TestResourcesIsolatedBetweenTokens(t *testing.T) {
	e := newEnv(t, "xai-a-ok", "xai-b-ok")
	var r struct{ ID string }
	e.proxy("POST", "/v1/responses", map[string]any{"model": "grok-4.3", "input": "hi"}, 200, &r)
	var sub struct {
		RequestID string `json:"request_id"`
	}
	e.proxy("POST", "/v1/videos/generations", map[string]any{"model": "grok-imagine-video", "prompt": "x"}, 200, &sub)
	var other struct{ Token string }
	e.api("POST", "/api/v1/tokens", map[string]any{"name": "other"}, 201, &other)
	e.do("GET", "/v1/responses/"+r.ID, other.Token, nil, 404, nil)
	e.do("POST", "/v1/responses", other.Token, map[string]any{"model": "grok-4.3", "input": "x", "previous_response_id": r.ID}, 404, nil)
	e.do("GET", "/v1/videos/"+sub.RequestID, other.Token, nil, 404, nil)
	// The owner still can.
	e.proxy("GET", "/v1/responses/"+r.ID, nil, 200, nil)
	e.proxy("GET", "/v1/videos/"+sub.RequestID, nil, 202, nil)
}

func TestDisabledAndUnpricedModels(t *testing.T) {
	e := newEnv(t, "xai-ok")
	e.api("PATCH", "/api/v1/models/grok-4.3", map[string]any{"enabled": false}, 200, nil)
	e.proxy("POST", "/v1/chat/completions", map[string]any{"model": "grok-4.3-latest", "messages": []any{}}, 403, nil)
	var q struct{ Token string }
	e.api("POST", "/api/v1/tokens", map[string]any{"name": "q", "quota_usd": 5}, 201, &q)
	// No price known: quota tokens are refused, unlimited tokens pass.
	e.do("POST", "/v1/chat/completions", q.Token, map[string]any{"model": "grok-future-9", "messages": []any{}}, 403, nil)
	e.proxy("POST", "/v1/chat/completions", map[string]any{"model": "grok-future-9", "messages": []any{}}, 200, nil)
	e.api("PUT", "/api/v1/settings", map[string]any{"unpriced_models": "allow"}, 200, nil)
	e.do("POST", "/v1/chat/completions", q.Token, map[string]any{"model": "grok-future-9", "messages": []any{}}, 200, nil)
}

func TestTextToSpeechBilledPerCharacter(t *testing.T) {
	e := newEnv(t, "xai-ok")
	e.proxy("POST", "/v1/tts", map[string]any{"text": "Hello, wörld", "language": "en"}, 200, nil)
	l := e.waitLogs(1)[0]
	if want := 12 * 0.000015; l.CostSource != "computed" || fmt.Sprintf("%.9f", l.CostUSD) != fmt.Sprintf("%.9f", want) {
		t.Fatalf("tts cost %v (%s), want %v", l.CostUSD, l.CostSource, want)
	}
}
