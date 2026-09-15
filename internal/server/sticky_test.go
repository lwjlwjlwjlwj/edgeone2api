package server

import "testing"

func TestResolveSessionKey(t *testing.T) {
	body := []byte(`{"metadata":{"conversation_id":"c1"},"conversationId":"c2"}`)
	cases := []struct {
		name      string
		headerKey string
		body      []byte
		want      string
	}{
		{"header wins over body", "hk", body, "hk"},
		{"no header falls back to body key", "", body, "c1"},
		{"no header no body key -> empty", "", []byte(`{"model":"x"}`), ""},
		{"header empty string treated as absent", "", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveSessionKey(c.headerKey, c.body); got != c.want {
				t.Errorf("resolveSessionKey(%q, %s) = %q, want %q", c.headerKey, c.body, got, c.want)
			}
		})
	}
}
