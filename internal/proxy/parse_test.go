package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Kiyliy/xai-router/internal/billing"
	"github.com/Kiyliy/xai-router/internal/config"
	"github.com/Kiyliy/xai-router/internal/store"
)

func TestParseRequestJSON(t *testing.T) {
	cases := []struct {
		name string
		body string
		want reqInfo
	}{
		{"empty", "", reqInfo{}},
		{"whitespace", " \n\t", reqInfo{}},
		{"not an object", `["model"]`, reqInfo{}},
		{"invalid json", `{"model":`, reqInfo{Invalid: "the request body is not valid JSON"}},
		{"basic", `  {"model":"grok-4.5","stream":true,"max_tokens":10,"max_completion_tokens":30,"max_output_tokens":20}`,
			reqInfo{Model: "grok-4.5", Stream: true, MaxTokens: 30}},
		{"responses", `{"model":"m","previous_response_id":"resp_9","service_tier":"priority"}`,
			reqInfo{Model: "m", PreviousResponseID: "resp_9", ServiceTier: "priority"}},
		{"image", `{"model":"img","n":3,"quality":"medium","resolution":"2k","deferred":true}`,
			reqInfo{Model: "img", N: 3, Quality: "medium", Resolution: "2k", Deferred: true}},
		{"resolution not a string", `{"resolution":{"w":1}}`, reqInfo{Invalid: `"resolution" must be a string`}},
		{"duration number", `{"duration":7.2}`, reqInfo{Duration: 8}},
		{"duration string", `{"duration":" 6 "}`, reqInfo{Duration: 6}},
		{"seconds fallback", `{"seconds":"4"}`, reqInfo{Duration: 4}},
		{"duration garbage", `{"duration":"long"}`, reqInfo{Invalid: `"duration" must be a number`}},
		{"wrong-typed field keeps the others", `{"model":"grok-4.3","deferred":true,"text":123}`, reqInfo{Model: "grok-4.3", Deferred: true, Invalid: `"text" must be a string`}},
		{"float max_tokens accepted", `{"max_tokens":16.0}`, reqInfo{MaxTokens: 16}},
		{"tts text runes", `{"text":"héllo wörld"}`, reqInfo{TextChars: 11}},
		{"duplicate model", `{"model":"a","model":"b"}`, reqInfo{Model: "b", Ambiguous: true}},
		{"nested model is fine", `{"model":"a","tools":[{"model":"b"}],"x":{"model":"c"}}`, reqInfo{Model: "a"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseRequest("application/json", []byte(c.body)); got != c.want {
				t.Fatalf("got %+v\nwant %+v", got, c.want)
			}
		})
	}
}

func multipartBody(boundary string, parts ...[2]string) string {
	var b strings.Builder
	for _, p := range parts {
		b.WriteString("--" + boundary + "\r\n")
		b.WriteString(`Content-Disposition: form-data; name="` + p[0] + `"`)
		if p[0] == "file" {
			b.WriteString(`; filename="a.wav"`)
		}
		b.WriteString("\r\n\r\n" + p[1] + "\r\n")
	}
	b.WriteString("--" + boundary + "--\r\n")
	return b.String()
}

func TestParseMultipart(t *testing.T) {
	ct := "multipart/form-data; boundary=XX"
	cases := []struct {
		name, ct, body string
		want           reqInfo
	}{
		{"model field", ct, multipartBody("XX", [2]string{"file", "RIFF...."}, [2]string{"model", " grok-stt "}),
			reqInfo{Model: "grok-stt"}},
		{"no model", ct, multipartBody("XX", [2]string{"prompt", "x"}), reqInfo{}},
		{"two models", ct, multipartBody("XX", [2]string{"model", "a"}, [2]string{"model", "b"}),
			reqInfo{Model: "b", Ambiguous: true}},
		{"missing boundary", "multipart/form-data", "--XX\r\n", reqInfo{Ambiguous: true}},
		{"unterminated last part", ct, "--XX\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\nabc",
			reqInfo{Model: "abc"}},
		{"malformed part header", ct, "--XX\r\nno colon here\r\n\r\nabc\r\n--XX--\r\n", reqInfo{Ambiguous: true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseRequest(c.ct, []byte(c.body)); got != c.want {
				t.Fatalf("got %+v want %+v", got, c.want)
			}
		})
	}
}

func TestCountTopLevelKey(t *testing.T) {
	cases := []struct {
		body string
		want int
	}{
		{`{"model":1}`, 1},
		{`{"model":1,"a":{"model":2},"model":3}`, 2},
		{`[]`, 0},
		{``, 0},
		{`{"model":1,"model":`, 2},
		{`{"model":1,`, 1},
	}
	for _, c := range cases {
		if got := countTopLevelKey([]byte(c.body), "model"); got != c.want {
			t.Errorf("%s: %d want %d", c.body, got, c.want)
		}
	}
}

func TestFlexInt(t *testing.T) {
	cases := map[string]int64{``: 0, `5`: 5, `5.1`: 6, `"3"`: 3, `"2.5"`: 3, `"x"`: 0, `null`: 0, `true`: 0}
	for in, want := range cases {
		if got := flexInt(json.RawMessage(in)); got != want {
			t.Errorf("flexInt(%s) = %d want %d", in, got, want)
		}
	}
}

func TestInjectStreamUsageEdgeCases(t *testing.T) {
	for _, in := range []string{`not json`, `{"stream_options":"bad"}`} {
		if got, c := injectStreamUsage([]byte(in)); string(got) != in || c != usageUnset {
			t.Errorf("%s changed to %s", in, got)
		}
	}
	if got, c := injectStreamUsage([]byte(`{"stream_options":null}`)); string(got) != `{"stream_options":{"include_usage":true}}` || c != usageUnset {
		t.Errorf("null options: %s %d", got, c)
	}
}

func TestErrorHelpers(t *testing.T) {
	e := errf(418, "teapot", "short and %s", "stout")
	if e.Error() != "short and stout" || e.status != 418 {
		t.Fatalf("%+v", e)
	}
	var v map[string]map[string]string
	if err := json.Unmarshal(errorJSON("c", "m"), &v); err != nil || v["error"]["code"] != "c" ||
		v["error"]["type"] != "xai_router_error" || v["error"]["message"] != "m" {
		t.Fatalf("errorJSON: %v %v", v, err)
	}
	if firstNonEmpty("", "", "x", "y") != "x" || firstNonEmpty() != "" {
		t.Fatal("firstNonEmpty")
	}
	if ownsResource(1, 2) || !ownsResource(1, 1) || !ownsResource(5, 0) {
		t.Fatal("ownsResource")
	}
}

func TestCreatedResourceKindAndAsyncPoll(t *testing.T) {
	cases := []struct {
		method, path string
		deferred     bool
		want         string
	}{
		{"POST", "/v1/videos/generations", false, KindVideo},
		{"POST", "/v1/videos/edits", false, KindVideo},
		{"POST", "/v1/images/generations", false, ""},
		{"POST", "/v1/images/edits", true, KindImage},
		{"POST", "/v1/chat/completions", false, ""},
		{"POST", "/v1/chat/completions", true, KindDeferred},
		{"POST", "/v1/responses", false, KindResponse},
		{"POST", "/v1/files", false, KindFile},
		{"POST", "/v1/batches", false, KindBatch},
		{"POST", "/v1/collections", false, KindColl},
		{"GET", "/v1/responses", false, ""},
		{"POST", "/v1/embeddings", false, ""},
	}
	for _, c := range cases {
		if got := createdResourceKind(c.method, c.path, c.deferred); got != c.want {
			t.Errorf("%s %s %v: %q want %q", c.method, c.path, c.deferred, got, c.want)
		}
	}
	for _, c := range []struct {
		method, path string
		ok           bool
	}{
		{"GET", "/v1/videos/v1", true},
		{"GET", "/v1/images/i1", true},
		{"GET", "/v1/chat/deferred-completion/d1", true},
		{"GET", "/v1/responses/r1", false},
		{"POST", "/v1/videos/v1", false},
	} {
		if _, _, ok := isAsyncPoll(c.method, c.path); ok != c.ok {
			t.Errorf("isAsyncPoll(%s %s) = %v", c.method, c.path, ok)
		}
	}
	for _, c := range []struct{ path, kind, id string }{
		{"/v1/collections/c1/documents", KindColl, "c1"},
		{"/v1/images/edits", "", ""},
		{"/v1/videos/extensions", "", ""},
		{"/v1/chat/completions", "", ""},
		{"/v2/videos/x", "", ""},
		{"/v1/x", "", ""},
	} {
		if k, id := referencedResource("GET", c.path); k != c.kind || id != c.id {
			t.Errorf("%s: (%q,%q)", c.path, k, id)
		}
	}
}

func TestModelAllowed(t *testing.T) {
	hs := newHarness(t, nil)
	cases := []struct {
		allowed []string
		model   string
		want    bool
	}{
		{nil, "anything", true},
		{[]string{"*"}, "anything", true},
		{[]string{"grok-4*"}, "grok-4.5", true},
		{[]string{"grok-4*"}, "grok-3", false},
		{[]string{" GROK-4.5 "}, "grok-4.5", true},
		{[]string{"grok-4.5"}, "grok-4.5-latest", true}, // alias resolves to the canonical id
		{[]string{"grok-4.5"}, "grok-4.3", false},
	}
	for _, c := range cases {
		tok := &store.Token{AllowedModels: c.allowed}
		if got := hs.h.modelAllowed(tok, c.model); got != c.want {
			t.Errorf("%v / %s: %v", c.allowed, c.model, got)
		}
	}
}

func TestAdmit(t *testing.T) {
	hs := newHarness(t, nil)
	open := &store.Token{}
	limited := &store.Token{AllowedModels: []string{"grok-4.5"}}
	quotaTok := &store.Token{QuotaTicks: 100, Limited: true}
	allow := testSettings()
	allow.UnpricedModels = "allow"
	cases := []struct {
		name         string
		tok          *store.Token
		s            *config.Settings
		method, path string
		info         reqInfo
		code         string
	}{
		{"ambiguous", open, nil, "POST", "/v1/chat/completions", reqInfo{Model: "a", Ambiguous: true}, "ambiguous_model"},
		{"restricted without model", limited, nil, "POST", "/v1/chat/completions", reqInfo{}, "model_required"},
		{"restricted tts without model", limited, nil, "POST", "/v1/tts", reqInfo{}, ""},
		{"restricted GET without model", limited, nil, "GET", "/v1/models", reqInfo{}, ""},
		{"open without model", open, nil, "POST", "/v1/chat/completions", reqInfo{}, ""},
		{"not allowed", limited, nil, "POST", "/v1/chat/completions", reqInfo{Model: "grok-4.3"}, "model_not_allowed"},
		{"allowed", limited, nil, "POST", "/v1/chat/completions", reqInfo{Model: "grok-4.5"}, ""},
		{"disabled model", open, nil, "POST", "/v1/chat/completions", reqInfo{Model: "off-model"}, "model_disabled"},
		{"unpriced with quota", quotaTok, nil, "POST", "/v1/chat/completions", reqInfo{Model: "mystery"}, "model_not_priced"},
		{"unpriced allowed by settings", quotaTok, &allow, "POST", "/v1/chat/completions", reqInfo{Model: "mystery"}, ""},
		{"unpriced without quota", open, nil, "POST", "/v1/chat/completions", reqInfo{Model: "mystery"}, ""},
		{"unpriced on non-inference path", quotaTok, nil, "POST", "/v1/files", reqInfo{Model: "mystery"}, ""},
		{"unpriced tts", quotaTok, nil, "POST", "/v1/tts", reqInfo{Model: "mystery"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := c.s
			if s == nil {
				s = hs.set.Get()
			}
			st := &state{h: hs.h, s: s, token: c.tok, info: c.info}
			e := hs.h.admit(st, httptest.NewRequest(c.method, c.path, nil))
			switch {
			case c.code == "" && e != nil:
				t.Fatalf("unexpected %s: %s", e.code, e.message)
			case c.code != "" && (e == nil || e.code != c.code):
				t.Fatalf("got %v, want %s", e, c.code)
			}
		})
	}
}

func TestEstimate(t *testing.T) {
	hs := newHarness(t, nil)
	m := func(id string) *store.Model { return hs.h.Prices.Lookup(id) }
	g45 := m("grok-4.5")
	vid := m("grok-imagine-video-1.5")
	body := []byte(`{"model":"grok-4.5","messages":[]}`)
	tools := []byte(`{"model":"grok-4.5","tools":[{"type":"web_search"}]}`)
	cases := []struct {
		name, method, path string
		info               reqInfo
		body               []byte
		want               int64
	}{
		{"GET is free", "GET", "/v1/chat/completions", reqInfo{Model: "grok-4.5"}, body, 0},
		{"tts", "POST", "/v1/tts", reqInfo{TextChars: 10}, nil, 10 * billing.TTSTicksPerChar},
		{"stt", "POST", "/v1/stt", reqInfo{}, nil, billing.STTRestTicksPerHour},
		{"unknown model", "POST", "/v1/chat/completions", reqInfo{Model: "nope"}, body, 0},
		{"embedding model", "POST", "/v1/embeddings", reqInfo{Model: "test-embed"}, body, 0},
		{"images default n", "POST", "/v1/images/generations", reqInfo{Model: "grok-imagine-image-2.0"}, nil, 800_000_000},
		{"images n", "POST", "/v1/images/edits", reqInfo{Model: "grok-imagine-image-2.0", N: 3}, nil, 3 * 800_000_000},
		{"images capped", "POST", "/v1/images/generations", reqInfo{Model: "grok-imagine-image-2.0", N: 99}, nil,
			maxImagesPerRequest * 800_000_000},
		{"video default", "POST", "/v1/videos/generations", reqInfo{Model: "grok-imagine-video-1.5"}, nil,
			defaultVideoSeconds * vid.VideoTicksPerSec},
		{"video capped", "POST", "/v1/videos/extensions", reqInfo{Model: "grok-imagine-video-1.5", Duration: 60}, nil,
			maxVideoSeconds * vid.VideoTicksPerSec},
		{"language", "POST", "/v1/chat/completions", reqInfo{Model: "grok-4.5"}, body,
			languageEstimate(g45, int64(len(body)), 0, 0, "")},
		{"language with tools", "POST", "/v1/responses", reqInfo{Model: "grok-4.5"}, tools,
			languageEstimate(g45, int64(len(tools)), 0, 0, "") + 20*100_000_000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := &state{h: hs.h, info: c.info, reqBody: c.body}
			if got := st.estimate(c.method, c.path); got != c.want {
				t.Fatalf("got %d want %d", got, c.want)
			}
		})
	}
}

func TestLanguageEstimate(t *testing.T) {
	m := &store.Model{ID: "grok-x", InputTicks: 10, ImageTokenTicks: 15, OutputTicks: 100,
		LongThreshold: 1000, LongInputTicks: 20, LongOutputTicks: 200}
	nr := &store.Model{ID: "grok-x-non-reasoning", InputTicks: 10, OutputTicks: 100}
	cases := []struct {
		name      string
		m         *store.Model
		bytes     int64
		maxTokens int64
		tier      string
		want      int64
	}{
		{"defaults", m, 100, 0, "", (100/2+16)*15 + (defaultOutputTokens+reasoningAllowance)*100},
		{"max tokens", m, 100, 10, "", (100/2+16)*15 + (10+reasoningAllowance)*100},
		{"non-reasoning", nr, 100, 10, "", (100/2+16)*10 + 10*100},
		{"priority", nr, 100, 10, "priority", 2 * ((100/2+16)*10 + 10*100)},
		{"long context", m, 4000, 10, "", (4000/2+16)*20 + (10+reasoningAllowance)*200},
	}
	for _, c := range cases {
		if got := languageEstimate(c.m, c.bytes, 0, c.maxTokens, c.tier); got != c.want {
			t.Errorf("%s: %d want %d", c.name, got, c.want)
		}
	}
	for body, want := range map[string]int64{
		`{"tools":[{"type":"x_search"}]}`:           20 * 100_000_000,
		`{"tools":[{"type":"collections_search"}]}`: 20 * 100_000_000,
		`{"tools":[{"type":"function"}]}`:           0,
	} {
		if got := toolAllowance([]byte(body)); got != want {
			t.Errorf("toolAllowance(%s) = %d", body, got)
		}
	}
}

func TestApplyVoiceRESTSkips(t *testing.T) {
	hs := newHarness(t, nil)
	cases := []struct {
		method, path string
		status       int
		source       string
	}{
		{"GET", "/v1/tts", 200, ""},
		{"POST", "/v1/tts", 500, ""},
		{"POST", "/v1/tts", 200, "upstream"},
		{"POST", "/v1/chat/completions", 200, ""},
	}
	for _, c := range cases {
		st := &state{h: hs.h, s: hs.set.Get(), info: reqInfo{TextChars: 4},
			log: &store.Log{Status: c.status, CostSource: c.source, CostTicks: 7}}
		st.applyVoiceREST(httptest.NewRequest(c.method, c.path, nil), []byte(`{}`))
		if st.log.CostTicks != 7 || st.log.CostSource != c.source {
			t.Errorf("%s %s %d: changed to %d %s", c.method, c.path, c.status, st.log.CostTicks, st.log.CostSource)
		}
	}
}

func TestWriteUpstreamError(t *testing.T) {
	hs := newHarness(t, nil)
	st := &state{h: hs.h, s: hs.set.Get(), log: &store.Log{}}
	rec := httptest.NewRecorder()
	hdr := http.Header{"X-Up": {"1"}, "Content-Length": {"999"}, "Content-Type": {"text/plain"}}
	st.writeUpstreamError(rec, &upstreamError{status: 418, header: hdr, body: []byte("teapot"), err: "HTTP 418"})
	if rec.Code != 418 || rec.Body.String() != "teapot" || rec.Header().Get("X-Up") != "1" ||
		rec.Header().Get("Content-Length") != "6" {
		t.Fatalf("got %d %q %v", rec.Code, rec.Body.String(), rec.Header())
	}
	if st.log.Status != 418 || st.log.Error != "HTTP 418" || st.log.RespBody != "teapot" || !strings.Contains(st.log.RespHeaders, "X-Up") {
		t.Fatalf("log %+v", st.log)
	}
	st = &state{h: hs.h, s: hs.set.Get(), log: &store.Log{}}
	rec = httptest.NewRecorder()
	st.writeUpstreamError(rec, &upstreamError{status: 502, err: "network error: refused"})
	if rec.Code != 502 || errCode(t, rec.Body.String()) != "upstream_error" || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
}
