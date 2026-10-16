// Package billing extracts usage from xAI responses and prices it.
package billing

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// TicksPerUSD is the xAI accounting unit: 1 USD = 1e10 ticks.
const TicksPerUSD = 10_000_000_000

// Tools counts server-side tool invocations reported by the upstream.
type Tools struct {
	WebSearch, XSearch, XPosts, XUsers, CodeInterpreter, FileSearch, DocumentSearch, Attachment int64
}

// Usage is normalised usage for one request.
//
//   - PromptTokens includes cached and image tokens (OpenAI semantics).
//   - CompletionTokens excludes reasoning tokens; ReasoningTokens is separate.
//     Both are billed at the output price.
type Usage struct {
	Found            bool
	PromptTokens     int64
	CompletionTokens int64
	CachedTokens     int64
	ReasoningTokens  int64
	ImageTokens      int64
	TotalTokens      int64
	CostTicks        int64
	HasCost          bool
	Images           int64
	VideoSeconds     float64
	ServiceTier      string
	Tools            Tools
}

// Merge copies the fields set in o into u (later events win).
func (u *Usage) Merge(o Usage) {
	if !o.Found {
		return
	}
	u.Found = true
	set := func(dst *int64, v int64) {
		if v != 0 {
			*dst = v
		}
	}
	set(&u.PromptTokens, o.PromptTokens)
	set(&u.CompletionTokens, o.CompletionTokens)
	set(&u.CachedTokens, o.CachedTokens)
	set(&u.ReasoningTokens, o.ReasoningTokens)
	set(&u.ImageTokens, o.ImageTokens)
	set(&u.TotalTokens, o.TotalTokens)
	set(&u.Images, o.Images)
	if o.HasCost {
		u.CostTicks, u.HasCost = o.CostTicks, true
	}
	if o.VideoSeconds != 0 {
		u.VideoSeconds = o.VideoSeconds
	}
	if o.ServiceTier != "" {
		u.ServiceTier = o.ServiceTier
	}
	if o.Tools != (Tools{}) {
		u.Tools = o.Tools
	}
}

// Finalize fills TotalTokens when the upstream did not report it.
func (u *Usage) Finalize() {
	if u.TotalTokens == 0 {
		u.TotalTokens = u.PromptTokens + u.CompletionTokens + u.ReasoningTokens
	}
}

// rawUsage covers every usage shape xAI returns: Chat Completions, Responses,
// Anthropic Messages and media (image/video) usage.
type rawUsage struct {
	// chat completions
	PromptTokens        *int64 `json:"prompt_tokens"`
	CompletionTokens    *int64 `json:"completion_tokens"`
	PromptTokensDetails *struct {
		TextTokens   int64 `json:"text_tokens"`
		ImageTokens  int64 `json:"image_tokens"`
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
	// responses / messages / media
	InputTokens        *int64 `json:"input_tokens"`
	OutputTokens       *int64 `json:"output_tokens"`
	InputTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
		ImageTokens  int64 `json:"image_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails *struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
	CacheReadInputTokens *int64 `json:"cache_read_input_tokens"`
	// common
	TotalTokens int64  `json:"total_tokens"`
	CostTicks   *int64 `json:"cost_in_usd_ticks"`
	ImageCost   *int64 `json:"image_cost"` // deferred image results
	Tools       *struct {
		WebSearch       int64 `json:"web_search_calls"`
		XSearch         int64 `json:"x_search_calls"`
		XPosts          int64 `json:"x_posts_fetched"`
		XUsers          int64 `json:"x_users_fetched"`
		CodeInterpreter int64 `json:"code_interpreter_calls"`
		FileSearch      int64 `json:"file_search_calls"`
		DocumentSearch  int64 `json:"document_search_calls"`
	} `json:"server_side_tool_usage_details"`
}

func (r *rawUsage) normalize() Usage {
	u := Usage{Found: true, TotalTokens: r.TotalTokens}
	switch {
	case r.PromptTokens != nil || r.CompletionTokens != nil: // chat completions
		u.PromptTokens = deref(r.PromptTokens)
		u.CompletionTokens = deref(r.CompletionTokens)
		if d := r.PromptTokensDetails; d != nil {
			u.CachedTokens, u.ImageTokens = d.CachedTokens, d.ImageTokens
		}
		if d := r.CompletionTokensDetails; d != nil {
			u.ReasoningTokens = d.ReasoningTokens
		}
	case r.CacheReadInputTokens != nil: // anthropic messages: input excludes cache reads
		u.CachedTokens = *r.CacheReadInputTokens
		u.PromptTokens = deref(r.InputTokens) + u.CachedTokens
		u.CompletionTokens = deref(r.OutputTokens)
	case r.InputTokens != nil || r.OutputTokens != nil: // responses: output includes reasoning
		u.PromptTokens = deref(r.InputTokens)
		out := deref(r.OutputTokens)
		if d := r.InputTokensDetails; d != nil {
			u.CachedTokens, u.ImageTokens = d.CachedTokens, d.ImageTokens
		}
		if d := r.OutputTokensDetails; d != nil {
			u.ReasoningTokens = d.ReasoningTokens
		}
		u.CompletionTokens = max(out-u.ReasoningTokens, 0)
	}
	if r.CostTicks != nil {
		u.CostTicks, u.HasCost = *r.CostTicks, true
	} else if r.ImageCost != nil {
		u.CostTicks, u.HasCost = *r.ImageCost, true
	}
	if t := r.Tools; t != nil {
		u.Tools = Tools{WebSearch: t.WebSearch, XSearch: t.XSearch, XPosts: t.XPosts, XUsers: t.XUsers,
			CodeInterpreter: t.CodeInterpreter, FileSearch: t.FileSearch, DocumentSearch: t.DocumentSearch}
	}
	return u
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// Envelope is the subset of a JSON response body the gateway inspects.
type Envelope struct {
	ID          string          `json:"id"`
	RequestID   string          `json:"request_id"`
	Object      string          `json:"object"`
	Model       string          `json:"model"`
	Status      string          `json:"status"`
	ServiceTier string          `json:"service_tier"`
	Usage       json.RawMessage `json:"usage"`
	Data        []json.RawMessage
	Video       *struct {
		Duration float64 `json:"duration"`
	} `json:"video"`
}

// ParseBody extracts usage and identifiers from a complete JSON response body.
func ParseBody(body []byte) (Usage, Envelope) {
	var env struct {
		Envelope
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return Usage{}, Envelope{}
	}
	e := env.Envelope
	if len(env.Data) > 0 && env.Data[0] == '[' {
		_ = json.Unmarshal(env.Data, &e.Data)
	}
	u := parseUsage(e.Usage)
	u.ServiceTier = e.ServiceTier
	if n := int64(len(e.Data)); n > 0 && isImageList(e) {
		u.Images, u.Found = n, true
	}
	if e.Video != nil && e.Video.Duration > 0 {
		u.VideoSeconds, u.Found = e.Video.Duration, true
	}
	return u, e
}

func isImageList(e Envelope) bool {
	// Image generation responses are {"data":[{"url"|"b64_json":…}]}; model
	// lists also use "data" but carry "object":"list".
	if e.Object == "list" {
		return false
	}
	var first map[string]json.RawMessage
	if json.Unmarshal(e.Data[0], &first) != nil {
		return false
	}
	_, u := first["url"]
	_, b := first["b64_json"]
	return u || b
}

func parseUsage(raw json.RawMessage) Usage {
	if len(raw) == 0 || raw[0] != '{' {
		return Usage{}
	}
	var r rawUsage
	if json.Unmarshal(raw, &r) != nil {
		return Usage{}
	}
	return r.normalize()
}

// StreamState accumulates usage and text from SSE events.
type StreamState struct {
	Usage     Usage
	ID        string
	Model     string
	Text      strings.Builder
	Reasoning strings.Builder
	MaxText   int // cap for Text/Reasoning (bytes); 0 = unlimited
	Truncated bool
}

// streamEvent covers chat chunks, Responses events and Messages events.
type streamEvent struct {
	Type        string          `json:"type"`
	ID          string          `json:"id"`
	Model       string          `json:"model"`
	ServiceTier string          `json:"service_tier"`
	Usage       json.RawMessage `json:"usage"`
	Choices     []struct {
		Delta struct {
			Content          *string `json:"content"`
			ReasoningContent *string `json:"reasoning_content"`
		} `json:"delta"`
	} `json:"choices"`
	Delta    json.RawMessage `json:"delta"`
	Response *struct {
		ID          string          `json:"id"`
		Model       string          `json:"model"`
		ServiceTier string          `json:"service_tier"`
		Usage       json.RawMessage `json:"usage"`
	} `json:"response"`
	Message *struct {
		ID    string          `json:"id"`
		Model string          `json:"model"`
		Usage json.RawMessage `json:"usage"`
	} `json:"message"`
}

// AddEvent processes one SSE "data:" payload.
func (s *StreamState) AddEvent(eventName string, data []byte) {
	if len(data) == 0 || data[0] != '{' {
		return
	}
	var ev streamEvent
	if json.Unmarshal(data, &ev) != nil {
		return
	}
	typ := ev.Type
	if typ == "" {
		typ = eventName
	}
	switch {
	case len(ev.Choices) > 0 || ev.Type == "": // chat completion chunk (no "type" field)
		if s.ID == "" {
			s.ID = ev.ID
		}
		if ev.Model != "" {
			s.Model = ev.Model
		}
		for _, c := range ev.Choices {
			if c.Delta.Content != nil {
				s.appendText(&s.Text, *c.Delta.Content)
			}
			if c.Delta.ReasoningContent != nil {
				s.appendText(&s.Reasoning, *c.Delta.ReasoningContent)
			}
		}
		if u := parseUsage(ev.Usage); u.Found {
			s.Usage.Merge(u)
		}
		if ev.ServiceTier != "" {
			s.Usage.ServiceTier = ev.ServiceTier
		}
	case strings.HasPrefix(typ, "response."):
		if ev.Response != nil {
			if ev.Response.ID != "" {
				s.ID = ev.Response.ID
			}
			if ev.Response.Model != "" {
				s.Model = ev.Response.Model
			}
			if u := parseUsage(ev.Response.Usage); u.Found {
				s.Usage.Merge(u)
			}
			if ev.Response.ServiceTier != "" {
				s.Usage.ServiceTier = ev.Response.ServiceTier
			}
		}
		switch typ {
		case "response.output_text.delta":
			s.appendText(&s.Text, deltaString(ev.Delta))
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			s.appendText(&s.Reasoning, deltaString(ev.Delta))
		}
	case typ == "message_start" && ev.Message != nil:
		s.ID, s.Model = ev.Message.ID, ev.Message.Model
		if u := parseUsage(ev.Message.Usage); u.Found {
			s.Usage.Merge(u)
		}
	case typ == "content_block_delta":
		var d struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
		}
		if json.Unmarshal(ev.Delta, &d) == nil {
			switch d.Type {
			case "text_delta":
				s.appendText(&s.Text, d.Text)
			case "thinking_delta":
				s.appendText(&s.Reasoning, d.Thinking)
			}
		}
	case typ == "message_delta":
		// Only output_tokens is reported here; keep prompt numbers from message_start.
		var r struct {
			OutputTokens *int64 `json:"output_tokens"`
			CostTicks    *int64 `json:"cost_in_usd_ticks"`
		}
		if json.Unmarshal(ev.Usage, &r) == nil {
			if r.OutputTokens != nil {
				s.Usage.Found = true
				s.Usage.CompletionTokens = *r.OutputTokens
			}
			if r.CostTicks != nil {
				s.Usage.CostTicks, s.Usage.HasCost = *r.CostTicks, true
			}
		}
	}
}

func deltaString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

func (s *StreamState) appendText(b *strings.Builder, t string) {
	if s.MaxText > 0 && b.Len()+len(t) > s.MaxText {
		if room := s.MaxText - b.Len(); room > 0 {
			for room > 0 && !utf8.RuneStart(t[room]) {
				room--
			}
			b.WriteString(t[:room])
		}
		s.Truncated = true
		return
	}
	b.WriteString(t)
}

// ExtractText returns the assistant text and reasoning of a non-streaming
// Chat Completions, Responses or Messages body (for log display).
func ExtractText(body []byte) (text, reasoning string) {
	var v struct {
		Choices []struct {
			Message struct {
				Content          any    `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Summary []struct {
				Text string `json:"text"`
			} `json:"summary"`
		} `json:"output"`
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
		} `json:"content"`
	}
	if json.Unmarshal(body, &v) != nil {
		return "", ""
	}
	var t, r strings.Builder
	for _, c := range v.Choices {
		if s, ok := c.Message.Content.(string); ok {
			t.WriteString(s)
		}
		r.WriteString(c.Message.ReasoningContent)
	}
	for _, o := range v.Output {
		switch o.Type {
		case "message":
			for _, c := range o.Content {
				if c.Type == "output_text" {
					t.WriteString(c.Text)
				}
			}
		case "reasoning":
			for _, s := range o.Summary {
				r.WriteString(s.Text)
			}
		}
	}
	for _, c := range v.Content {
		switch c.Type {
		case "text":
			t.WriteString(c.Text)
		case "thinking":
			r.WriteString(c.Thinking)
		}
	}
	return t.String(), r.String()
}
