package provider

import "testing"

func TestCanonicalBaseURL(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
		ok    bool
	}{
		{name: "root gains v1", input: "https://api.example.com", want: "https://api.example.com/v1", ok: true},
		{name: "existing v1 remains", input: "https://api.example.com/v1/", want: "https://api.example.com/v1", ok: true},
		{name: "custom path remains", input: "http://localhost:9000/openai/v1", want: "http://localhost:9000/openai/v1", ok: true},
		{name: "credentials rejected", input: "https://user:pass@api.example.com", ok: false},
		{name: "query rejected", input: "https://api.example.com?token=value", ok: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := canonicalBaseURL(test.input)
			if ok != test.ok || got != test.want {
				t.Fatalf("canonicalBaseURL(%q) = %q, %v; want %q, %v", test.input, got, ok, test.want, test.ok)
			}
		})
	}
}

func TestBodyPreviewDoesNotEchoHTML(t *testing.T) {
	if got := bodyPreview([]byte("<html><body>upstream login page</body></html>")); got != "HTML response" {
		t.Fatalf("bodyPreview returned %q", got)
	}
}

func TestModelExamplesPreferSameFamily(t *testing.T) {
	got := modelExamples([]string{"claude-sonnet", "gpt-5.6-sol", "gpt-5.5", "gemini-pro"}, "gpt-5.6", 2)
	if got[0] != "gpt-5.5" || got[1] != "gpt-5.6-sol" {
		t.Fatalf("modelExamples returned %v", got)
	}
}
