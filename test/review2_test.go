package test

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"testing"
	"time"
)

// Regression tests for the second review round (N1, N2, N3, M3, H1).

// N1: a second spelling of "model" or "previous_response_id" (Go matches
// JSON names case-insensitively, xAI does not) is refused.
func TestCaseVariantFieldsRejected(t *testing.T) {
	e := newEnv(t, "xai-ok")
	var tok struct{ Token string }
	e.api("POST", "/api/v1/tokens", map[string]any{"name": "r", "allowed_models": []string{"grok-4.3"}}, 201, &tok)
	for _, body := range []string{
		`{"model":"grok-4.7","MODEL":"grok-4.3","messages":[]}`,
		`{"Model":"grok-4.3","messages":[]}`,
		`{"model":"grok-4.3","previous_response_id":"a","Previous_Response_ID":"b"}`,
	} {
		if st := e.raw("POST", "/v1/chat/completions", tok.Token, "application/json", []byte(body)); st != 400 {
			t.Errorf("%s: status %d, want 400", body, st)
		}
	}
	if st := e.raw("POST", "/v1/chat/completions", tok.Token, "application/json",
		[]byte(`{"model":"grok-4.3","messages":[]}`)); st != 200 {
		t.Fatalf("plain request: %d", st)
	}
}

// N2: WebSocket sessions apply the same model policy.
func TestWebSocketModelPolicy(t *testing.T) {
	e := newEnv(t, "xai-ok")
	var tok struct{ Token string }
	e.api("POST", "/api/v1/tokens", map[string]any{"name": "r", "allowed_models": []string{"grok-voice-latest"}}, 201, &tok)
	auth := http.Header{"Authorization": {"Bearer " + tok.Token}}
	for path, want := range map[string]int{
		"/v1/realtime": 400, // restricted token must name the model
		"/v1/realtime?model=grok-voice-latest&model=grok-4.7": 400,
		"/v1/realtime?model=grok-4.7":                         403,
		"/v1/realtime?model=grok-voice-latest":                101,
	} {
		c, err := e.dialWS(path, auth)
		if err != nil {
			t.Fatal(err)
		}
		if c.resp.StatusCode != want {
			t.Errorf("%s: status %d, want %d", path, c.resp.StatusCode, want)
		}
		c.conn.Close()
	}
	e.api("PATCH", "/api/v1/models/grok-4.3", map[string]any{"enabled": false}, 200, nil)
	c, _ := e.dialWS("/v1/realtime?model=grok-4.3", http.Header{"Authorization": {"Bearer " + e.client}})
	if c.resp.StatusCode != 403 {
		t.Fatalf("disabled model over WebSocket: %d", c.resp.StatusCode)
	}
	c.conn.Close()
}

// N3: list endpoints of team-scoped resources only show the token's own.
func TestTeamResourceListsFiltered(t *testing.T) {
	e := newEnv(t, "xai-ok")
	var other struct{ Token string }
	e.api("POST", "/api/v1/tokens", map[string]any{"name": "other"}, 201, &other)
	var f struct{ ID string }
	e.proxy("POST", "/v1/files", map[string]any{"purpose": "x"}, 200, &f)
	var b struct {
		BatchID string `json:"batch_id"`
	}
	e.proxy("POST", "/v1/batches", map[string]any{"name": "b"}, 200, &b)
	var files struct{ Data []struct{ ID string } }
	e.proxy("GET", "/v1/files", nil, 200, &files)
	if len(files.Data) != 1 || files.Data[0].ID != f.ID {
		t.Fatalf("owner sees %+v, want only %s", files.Data, f.ID)
	}
	e.do("GET", "/v1/files", other.Token, nil, 200, &files)
	if len(files.Data) != 0 {
		t.Fatalf("other token sees %+v", files.Data)
	}
	var batches struct {
		Batches []struct {
			BatchID string `json:"batch_id"`
		}
	}
	e.proxy("GET", "/v1/batches", nil, 200, &batches)
	if len(batches.Batches) != 1 || batches.Batches[0].BatchID != b.BatchID {
		t.Fatalf("owner batches %+v", batches.Batches)
	}
	// Filtering does not depend on the upstream's field name.
	var other2 struct {
		Data []struct {
			BatchID string `json:"batch_id"`
		}
	}
	e.do("GET", "/v1/batches?shape=data", other.Token, nil, 200, &other2)
	if len(other2.Data) != 0 {
		t.Fatalf("other token sees batches under an unexpected field: %+v", other2.Data)
	}
	// The created resources are private: other tokens get 404.
	e.do("GET", "/v1/files/"+f.ID, other.Token, nil, 404, nil)
	e.do("GET", "/v1/batches/"+b.BatchID, other.Token, nil, 404, nil)
}

// M3: a finished job whose binding has expired is not charged again.
func TestExpiredBindingNotBilledAgain(t *testing.T) {
	e := newEnv(t, "xai-ok")
	var sub struct {
		RequestID string `json:"request_id"`
	}
	e.proxy("POST", "/v1/videos/generations", map[string]any{"model": "grok-imagine-video", "prompt": "x"}, 200, &sub)
	e.proxy("GET", "/v1/videos/"+sub.RequestID, nil, 202, nil)
	e.proxy("GET", "/v1/videos/"+sub.RequestID, nil, 200, nil) // billed once
	if _, err := e.app.DB.W.ExecContext(context.Background(), `UPDATE bindings SET expires_at = 1`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		e.proxy("GET", "/v1/videos/"+sub.RequestID, nil, 200, nil)
	}
	time.Sleep(700 * time.Millisecond)
	var tok struct {
		Data []struct {
			UsedUSD float64 `json:"used_usd"`
		}
	}
	e.api("GET", "/api/v1/tokens", nil, 200, &tok)
	if tok.Data[0].UsedUSD != 0.3 {
		t.Fatalf("token charged $%v, want $0.3 (one job)", tok.Data[0].UsedUSD)
	}
}

// H1: a 403 whose text echoes the user's message (containing dead-key
// words) never disables keys: it is verified with the key first.
func TestEchoed403DoesNotDisableKeys(t *testing.T) {
	e := newEnv(t, "xai-a-ok", "xai-b-ok")
	for i := 0; i < 3; i++ {
		e.proxy("POST", "/v1/chat/completions", map[string]any{"model": "echo-403", "messages": []map[string]string{
			{"role": "user", "content": "no credits, team is blocked, incorrect api key, spending limit"}}}, 403, nil)
	}
	time.Sleep(300 * time.Millisecond) // let verifications run
	var keys struct {
		Data []struct{ Name, Status string }
	}
	e.api("GET", "/api/v1/keys", nil, 200, &keys)
	for _, k := range keys.Data {
		if k.Status != "active" {
			t.Fatalf("key %s is %s after echoed 403s", k.Name, k.Status)
		}
	}
}

// R1: every field the gateway reads must have a single, exact spelling.
func TestCaseVariantBillingFieldsRejected(t *testing.T) {
	e := newEnv(t, "xai-ok")
	for _, body := range []string{
		`{"model":"grok-4.3","deferred":true,"DEFERRED":false,"messages":[]}`,
		`{"model":"grok-4.3","Deferred":true,"messages":[]}`,
		`{"model":"grok-4.3","max_tokens":10,"MAX_TOKENS":100000,"messages":[]}`,
		`{"model":"grok-imagine-image","prompt":"p","n":1,"N":10}`,
		`{"model":"grok-imagine-video","prompt":"p","duration":1,"Duration":15}`,
		`{"model":"grok-4.3","service_tier":"default","Service_Tier":"priority","messages":[]}`,
	} {
		if st := e.raw("POST", "/v1/chat/completions", e.client, "application/json", []byte(body)); st != 400 {
			t.Errorf("%s: status %d, want 400", body, st)
		}
	}
	var mp bytes.Buffer
	w := multipart.NewWriter(&mp)
	_ = w.WriteField("model", "grok-imagine-image")
	_ = w.WriteField("Deferred", "true")
	w.Close()
	if st := e.raw("POST", "/v1/images/edits", e.client, w.FormDataContentType(), mp.Bytes()); st != 400 {
		t.Errorf("multipart Deferred: status %d, want 400", st)
	}
}
