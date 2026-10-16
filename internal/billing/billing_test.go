package billing

import (
	"strings"
	"testing"
)

func table() *Table {
	t := NewTable()
	t.Set(BuiltinModels())
	return t
}

// The bodies below are real xAI responses (trimmed) captured on 2026-10-16;
// the expected costs equal the cost_in_usd_ticks xAI reported for them.

const chatBody = `{"id":"e02a2fb0","object":"chat.completion","model":"grok-build-0.1","choices":[{"index":0,
"message":{"role":"assistant","content":"Hi!","reasoning_content":"The user requested a greeting."}}],
"usage":{"prompt_tokens":188,"completion_tokens":9,"total_tokens":370,"prompt_tokens_details":{"text_tokens":188,
"audio_tokens":0,"image_tokens":0,"cached_tokens":128},"completion_tokens_details":{"reasoning_tokens":173},
"num_sources_used":0,"cost_in_usd_ticks":4496000},"service_tier":"default"}`

const responsesBody = `{"id":"a0b092d0","object":"response","model":"grok-build-0.1","output":[
{"type":"reasoning","summary":[{"type":"summary_text","text":"thinking"}]},
{"type":"message","content":[{"type":"output_text","text":"Hi there"}]}],
"usage":{"input_tokens":188,"input_tokens_details":{"cached_tokens":128},"output_tokens":219,
"output_tokens_details":{"reasoning_tokens":209},"total_tokens":407,"cost_in_usd_ticks":5236000}}`

func TestParseChatUsage(t *testing.T) {
	u, env := ParseBody([]byte(chatBody))
	if env.ID != "e02a2fb0" || env.Model != "grok-build-0.1" {
		t.Fatalf("envelope: %+v", env)
	}
	want := Usage{Found: true, PromptTokens: 188, CompletionTokens: 9, CachedTokens: 128, ReasoningTokens: 173,
		TotalTokens: 370, CostTicks: 4496000, HasCost: true, ServiceTier: "default"}
	if u != want {
		t.Fatalf("usage\n got %+v\nwant %+v", u, want)
	}
	// Without the upstream cost, the price table must reproduce it exactly.
	u.HasCost = false
	if got, src := table().Cost("grok-build-0.1", u, Request{}); got != 4496000 || src != "computed" {
		t.Fatalf("computed cost = %d (%s), want 4496000", got, src)
	}
	if got, src := table().Cost("grok-code-fast-1", u, Request{}); got != 4496000 || src != "computed" {
		t.Fatalf("alias cost = %d (%s)", got, src)
	}
}

func TestParseResponsesUsage(t *testing.T) {
	u, _ := ParseBody([]byte(responsesBody))
	if u.PromptTokens != 188 || u.CachedTokens != 128 || u.ReasoningTokens != 209 || u.CompletionTokens != 10 {
		t.Fatalf("usage: %+v", u)
	}
	u.HasCost = false
	if got, _ := table().Cost("grok-build-0.1", u, Request{}); got != 5236000 {
		t.Fatalf("computed cost = %d, want 5236000", got)
	}
	text, reasoning := ExtractText([]byte(responsesBody))
	if text != "Hi there" || reasoning != "thinking" {
		t.Fatalf("text=%q reasoning=%q", text, reasoning)
	}
}

func TestParseMessagesUsage(t *testing.T) {
	body := `{"id":"m1","type":"message","model":"grok-build-0.1","content":[{"type":"thinking","thinking":"hm"},
{"type":"text","text":"Hello"}],"usage":{"input_tokens":60,"cache_creation_input_tokens":0,"cache_read_input_tokens":128,"output_tokens":173}}`
	u, _ := ParseBody([]byte(body))
	if u.PromptTokens != 188 || u.CachedTokens != 128 || u.CompletionTokens != 173 {
		t.Fatalf("usage: %+v", u)
	}
	got, src := table().Cost("grok-build-0.1", u, Request{})
	if want := int64(60*10000 + 128*2000 + 173*20000); got != want || src != "computed" {
		t.Fatalf("cost = %d (%s), want %d", got, src, want)
	}
	text, reasoning := ExtractText([]byte(body))
	if text != "Hello" || reasoning != "hm" {
		t.Fatalf("text=%q reasoning=%q", text, reasoning)
	}
}

func TestLongContextAndPriority(t *testing.T) {
	tb := table()
	u := Usage{Found: true, PromptTokens: 250_000, CachedTokens: 50_000, CompletionTokens: 1000}
	// grok-4.3 long context: $2.50 / $0.40 / $5.00 per 1M for the whole request.
	want := int64(200_000*25000 + 50_000*4000 + 1000*50000)
	if got, _ := tb.Cost("grok-4.3", u, Request{}); got != want {
		t.Fatalf("long-context cost = %d, want %d", got, want)
	}
	u = Usage{Found: true, PromptTokens: 1000, CompletionTokens: 100, ServiceTier: "priority"}
	want = 2 * (1000*12500 + 100*25000)
	if got, _ := tb.Cost("grok-4.3", u, Request{}); got != want {
		t.Fatalf("priority cost = %d, want %d", got, want)
	}
}

func TestToolCosts(t *testing.T) {
	u := Usage{Found: true, PromptTokens: 100, CompletionTokens: 10, Tools: Tools{WebSearch: 2, XPosts: 3, XUsers: 1}}
	want := int64(100*20000+10*60000) + 2*50_000_000 + 3*50_000_000 + 100_000_000
	if got, _ := table().Cost("grok-4.7", u, Request{}); got != want {
		t.Fatalf("cost = %d, want %d", got, want)
	}
}

func TestMediaCost(t *testing.T) {
	tb := table()
	u, _ := ParseBody([]byte(`{"data":[{"url":"https://x/1.png"},{"url":"https://x/2.png"}]}`))
	if u.Images != 2 {
		t.Fatalf("images = %d", u.Images)
	}
	if got, _ := tb.Cost("grok-imagine-image", u, Request{}); got != 2*200_000_000 {
		t.Fatalf("image cost = %d", got)
	}
	if got, _ := tb.Cost("grok-imagine-image-2.0", u, Request{Quality: "medium", Resolution: "2k"}); got != 2*800_000_000 {
		t.Fatalf("tiered image cost = %d", got)
	}
	// Defaults: 1k, quality auto = low for generation, medium for edits.
	if got, _ := tb.Cost("grok-imagine-image-2.0", u, Request{}); got != 2*400_000_000 {
		t.Fatalf("default generation cost = %d", got)
	}
	if got, _ := tb.Cost("grok-imagine-image-2.0", u, Request{Edit: true, Quality: "auto"}); got != 2*600_000_000 {
		t.Fatalf("default edit cost = %d", got)
	}
	// Upstream cost wins.
	u, _ = ParseBody([]byte(`{"data":[{"b64_json":"AAAA"}],"usage":{"cost_in_usd_ticks":400000000}}`))
	if got, src := tb.Cost("grok-imagine-image", u, Request{}); got != 400_000_000 || src != "upstream" {
		t.Fatalf("got %d %s", got, src)
	}
	// Video: duration × per-second price.
	u, _ = ParseBody([]byte(`{"status":"done","video":{"url":"https://v/1.mp4","duration":6},"model":"grok-imagine-video"}`))
	if got, _ := tb.Cost("grok-imagine-video", u, Request{}); got != 6*500_000_000 {
		t.Fatalf("video cost = %d", got)
	}
	// Model lists are not images.
	if u, _ := ParseBody([]byte(`{"object":"list","data":[{"id":"grok-4.3","object":"model"}]}`)); u.Images != 0 {
		t.Fatal("model list counted as images")
	}
}

func TestUnknownModel(t *testing.T) {
	if got, src := table().Cost("nope", Usage{Found: true, PromptTokens: 10}, Request{}); got != 0 || src != "none" {
		t.Fatalf("got %d %s", got, src)
	}
}

func TestStreamStateChat(t *testing.T) {
	var s StreamState
	s.AddEvent("", []byte(`{"id":"c1","object":"chat.completion.chunk","model":"grok-4.3","choices":[{"delta":{"reasoning_content":"think "}}]}`))
	s.AddEvent("", []byte(`{"id":"c1","object":"chat.completion.chunk","choices":[{"delta":{"content":"Hel"}}]}`))
	s.AddEvent("", []byte(`{"id":"c1","object":"chat.completion.chunk","choices":[{"delta":{"content":"lo"}}]}`))
	s.AddEvent("", []byte(`{"id":"c1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":188,"completion_tokens":11,"total_tokens":335,"prompt_tokens_details":{"cached_tokens":128},"completion_tokens_details":{"reasoning_tokens":136},"cost_in_usd_ticks":3796000}}`))
	s.AddEvent("", []byte(`[DONE]`))
	if s.Text.String() != "Hello" || s.Reasoning.String() != "think " || s.ID != "c1" || s.Model != "grok-4.3" {
		t.Fatalf("state: text=%q reasoning=%q id=%q", s.Text.String(), s.Reasoning.String(), s.ID)
	}
	if !s.Usage.HasCost || s.Usage.CostTicks != 3796000 || s.Usage.ReasoningTokens != 136 {
		t.Fatalf("usage: %+v", s.Usage)
	}
}

func TestStreamStateResponses(t *testing.T) {
	var s StreamState
	s.AddEvent("response.created", []byte(`{"type":"response.created","response":{"id":"r1","model":"grok-4.3"}}`))
	s.AddEvent("response.reasoning_summary_text.delta", []byte(`{"type":"response.reasoning_summary_text.delta","delta":"hmm"}`))
	s.AddEvent("response.output_text.delta", []byte(`{"type":"response.output_text.delta","delta":"Hi"}`))
	s.AddEvent("response.completed", []byte(`{"type":"response.completed","response":{"id":"r1","usage":{"input_tokens":10,"output_tokens":5,"output_tokens_details":{"reasoning_tokens":3},"cost_in_usd_ticks":99}}}`))
	if s.ID != "r1" || s.Text.String() != "Hi" || s.Reasoning.String() != "hmm" || s.Usage.CostTicks != 99 ||
		s.Usage.CompletionTokens != 2 || s.Usage.ReasoningTokens != 3 {
		t.Fatalf("state: %+v text=%q", s.Usage, s.Text.String())
	}
}

func TestStreamStateMessages(t *testing.T) {
	var s StreamState
	s.AddEvent("message_start", []byte(`{"type":"message_start","message":{"id":"m1","model":"grok-build-0.1","usage":{"input_tokens":60,"cache_read_input_tokens":128,"output_tokens":0}}}`))
	s.AddEvent("content_block_delta", []byte(`{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"x"}}`))
	s.AddEvent("content_block_delta", []byte(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"Hi"}}`))
	s.AddEvent("message_delta", []byte(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":173}}`))
	if s.ID != "m1" || s.Text.String() != "Hi" || s.Reasoning.String() != "x" {
		t.Fatalf("text=%q", s.Text.String())
	}
	if s.Usage.PromptTokens != 188 || s.Usage.CachedTokens != 128 || s.Usage.CompletionTokens != 173 {
		t.Fatalf("usage: %+v", s.Usage)
	}
}

func TestStreamTextCap(t *testing.T) {
	s := StreamState{MaxText: 5}
	s.AddEvent("", []byte(`{"choices":[{"delta":{"content":"héllo world"}}]}`))
	if !s.Truncated || len(s.Text.String()) > 5 || !strings.HasPrefix("héllo", s.Text.String()) {
		t.Fatalf("text=%q truncated=%v", s.Text.String(), s.Truncated)
	}
}

func TestCheapest(t *testing.T) {
	if got := table().Cheapest(); got != "grok-build-0.1" {
		t.Fatalf("cheapest = %q", got)
	}
}

func TestConversions(t *testing.T) {
	if USDPerMillionToTicks(1.25) != 12500 || TicksToUSDPerMillion(12500) != 1.25 {
		t.Fatal("per-million conversion")
	}
	if USDToTicks(0.02) != 200_000_000 || TicksToUSD(4496000) != 0.0004496 {
		t.Fatal("usd conversion")
	}
}
