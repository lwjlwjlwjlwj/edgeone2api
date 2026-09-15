package auth

import "testing"

func TestExtractKey(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"empty body", "", ""},
		{"invalid json", "{not json", ""},
		{"no keys", `{"model":"x","messages":[]}`, ""},
		{"metadata snake", `{"metadata":{"conversation_id":"c1"}}`, "c1"},
		{"metadata camel", `{"metadata":{"conversationId":"c2"}}`, "c2"},
		{"top-level snake", `{"conversation_id":"c3"}`, "c3"},
		{"top-level camel", `{"conversationId":"c4"}`, "c4"},
		{"metadata user_id fallback", `{"metadata":{"user_id":"u1"}}`, "u1"},
		{"snake beats camel in metadata", `{"metadata":{"conversation_id":"c1","conversationId":"c2"}}`, "c1"},
		{"metadata beats top-level", `{"metadata":{"conversation_id":"c1"},"conversationId":"c4"}`, "c1"},
		{"top-level snake beats camel", `{"conversation_id":"c3","conversationId":"c4"}`, "c3"},
		{"user_id only when conversation absent", `{"metadata":{"user_id":"u1","conversationId":"c2"}}`, "c2"},
		{"non-string values ignored", `{"metadata":{"conversation_id":123},"conversationId":true}`, ""},
		{"empty string values skipped", `{"metadata":{"conversation_id":"","conversationId":""},"conversation_id":"","conversationId":""}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ExtractKey([]byte(c.body)); got != c.want {
				t.Errorf("ExtractKey(%q) = %q, want %q", c.body, got, c.want)
			}
		})
	}
}
