package config

import "testing"

func TestDefaultsValid(t *testing.T) {
	s := DefaultSettings()
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestMerge(t *testing.T) {
	s := DefaultSettings()
	out, err := s.Merge([]byte(`{"max_attempts":3,"retry_keywords":[" Foo ","foo","bar"],"upstream_base_url":"https://us.api.x.ai/"}`))
	if err != nil {
		t.Fatal(err)
	}
	if out.MaxAttempts != 3 || len(out.RetryKeywords) != 2 || out.RetryKeywords[0] != "foo" || out.UpstreamBaseURL != "https://us.api.x.ai" {
		t.Fatalf("merge result: %+v", out)
	}
	if s.MaxAttempts != 5 {
		t.Fatal("merge mutated the original")
	}
	for _, bad := range []string{`{"max_attempts":0}`, `{"unknown_field":1}`, `{"selection_strategy":"x"}`,
		`{"upstream_base_url":"ftp://x"}`, `{"trusted_proxies":["nope"]}`, `{"retry_status_codes":[999]}`} {
		if _, err := s.Merge([]byte(bad)); err == nil {
			t.Errorf("expected error for %s", bad)
		}
	}
}

func TestMatchKeyword(t *testing.T) {
	if MatchKeyword(`{"error":"Rate Limit reached"}`, []string{"quota", "rate limit"}) != "rate limit" {
		t.Fatal("case-insensitive match failed")
	}
	if MatchKeyword("", []string{"x"}) != "" {
		t.Fatal("empty body matched")
	}
}
