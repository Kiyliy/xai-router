package billing

import "testing"

func TestStreamStateErrors(t *testing.T) {
	type ev struct{ name, data string }
	cases := []struct {
		name   string
		events []ev
		want   *StreamError
		output bool
	}{
		{"chat openai shape", []ev{{"", `{"error":{"message":"at capacity","type":"server_error","code":"resource-exhausted"}}`}},
			&StreamError{Code: "resource-exhausted", Type: "server_error", Message: "at capacity"}, false},
		{"chat xai shape", []ev{{"", `{"code":"resource-exhausted","error":"at capacity"}`}},
			&StreamError{Code: "resource-exhausted", Message: "at capacity"}, false},
		{"numeric code", []ev{{"", `{"error":{"message":"x","code":503}}`}}, &StreamError{Code: "503", Message: "x"}, false},
		{"chat null error is no error", []ev{{"", `{"id":"c","choices":[{"delta":{"content":"hi"}}],"error":null}`}}, nil, true},
		{"chat tool call is output", []ev{{"", `{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{"}}]}}]}`}}, nil, true},
		{"chat role chunk is not output", []ev{{"", `{"choices":[{"delta":{"role":"assistant","content":""}}]}`}}, nil, false},
		{"responses error event", []ev{{"error", `{"type":"error","code":"resource-exhausted","message":"at capacity","param":null}`}},
			&StreamError{Code: "resource-exhausted", Message: "at capacity"}, false},
		{"responses error by event name", []ev{{"error", `{"code":"resource-exhausted","message":"at capacity"}`}},
			&StreamError{Code: "resource-exhausted", Message: "at capacity"}, false},
		{"responses failed", []ev{{"response.failed", `{"type":"response.failed","response":{"id":"r","error":{"code":"server_error","message":"boom"}}}`}},
			&StreamError{Code: "server_error", Message: "boom"}, false},
		{"responses created has null error", []ev{{"response.created", `{"type":"response.created","response":{"id":"r","error":null}}`}}, nil, false},
		{"responses function args are output", []ev{{"", `{"type":"response.function_call_arguments.delta","delta":"{\"a\""}`}}, nil, true},
		{"messages error", []ev{{"error", `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`}},
			&StreamError{Type: "overloaded_error", Message: "Overloaded"}, false},
		{"messages tool input is output", []ev{{"", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{"}}`}}, nil, true},
		{"first error wins", []ev{{"", `{"error":"first"}`}, {"", `{"error":"second"}`}}, &StreamError{Message: "first"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var s StreamState
			for _, e := range c.events {
				s.AddEvent(e.name, []byte(e.data))
			}
			switch {
			case c.want == nil && s.Err != nil:
				t.Fatalf("unexpected error %+v", s.Err)
			case c.want != nil && (s.Err == nil || *s.Err != *c.want):
				t.Fatalf("error %+v want %+v", s.Err, c.want)
			}
			if s.Output != c.output {
				t.Fatalf("output %v want %v", s.Output, c.output)
			}
		})
	}
	// message_start still reads its message object.
	var s StreamState
	s.AddEvent("message_start", []byte(`{"type":"message_start","message":{"id":"m","model":"g","usage":{"input_tokens":4}}}`))
	if s.ID != "m" || s.Model != "g" || s.Usage.PromptTokens != 4 || s.Err != nil {
		t.Fatalf("message_start: %+v", s)
	}
	if got := (&StreamError{Code: "resource-exhausted", Type: "resource-exhausted", Message: "m"}).String(); got != "resource-exhausted: m" {
		t.Fatalf("String() = %q", got)
	}
}
