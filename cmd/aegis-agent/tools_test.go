package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReplaceNth(t *testing.T) {
	tests := []struct {
		name     string
		s        string
		old, new string
		n        int
		want     string
	}{
		{"first of two", "foo bar foo baz", "foo", "qux", 1, "qux bar foo baz"},
		{"second of two", "foo bar foo baz", "foo", "qux", 2, "foo bar qux baz"},
		{"first of three", "aaa bbb aaa ccc aaa", "aaa", "xxx", 1, "xxx bbb aaa ccc aaa"},
		{"third of three", "aaa bbb aaa ccc aaa", "aaa", "xxx", 3, "aaa bbb aaa ccc xxx"},
		{"replace with longer", "ab ab ab", "ab", "abcd", 2, "ab abcd ab"},
		{"replace with shorter", "hello hello", "hello", "hi", 1, "hi hello"},
		{"replace with empty", "rm rm rm", "rm", "", 2, "rm  rm"},
		{"single occurrence", "only one", "one", "two", 1, "only two"},
		{"multiline", "line1\nfoo\nline3\nfoo\nline5", "foo", "bar", 2, "line1\nfoo\nline3\nbar\nline5"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := replaceNth(tt.s, tt.old, tt.new, tt.n)
			if got != tt.want {
				t.Errorf("replaceNth(%q, %q, %q, %d) = %q, want %q", tt.s, tt.old, tt.new, tt.n, got, tt.want)
			}
		})
	}
}

func TestComputeDiff(t *testing.T) {
	tests := []struct {
		name        string
		old, new    string
		wantRemoved string
		wantAdded   string
	}{
		{
			"simple replacement",
			"line1\nold\nline3",
			"line1\nnew\nline3",
			"-old",
			"+new",
		},
		{
			"addition",
			"line1\nline2",
			"line1\ninserted\nline2",
			"", // line2 is not removed, just shifted — no "-" lines
			"+inserted",
		},
		{
			"deletion",
			"line1\nremove_me\nline3",
			"line1\nline3",
			"-remove_me",
			"",
		},
		{
			"multi-line change",
			"a\nb\nc\nd\ne",
			"a\nx\ny\nd\ne",
			"-b\n-c",
			"+x\n+y",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			diff := computeDiff(tt.old, tt.new)
			if tt.wantRemoved != "" && !strings.Contains(diff, tt.wantRemoved) {
				t.Errorf("diff missing removed lines %q:\n%s", tt.wantRemoved, diff)
			}
			if tt.wantAdded != "" && !strings.Contains(diff, tt.wantAdded) {
				t.Errorf("diff missing added lines %q:\n%s", tt.wantAdded, diff)
			}
		})
	}
}

func TestComputeDiffContextLines(t *testing.T) {
	old := "line1\nline2\nline3\nline4\nline5\nold\nline7\nline8"
	new := "line1\nline2\nline3\nline4\nline5\nnew\nline7\nline8"
	diff := computeDiff(old, new)

	// Should include up to 3 context lines before the change
	if !strings.Contains(diff, " line4\n") || !strings.Contains(diff, " line5\n") {
		t.Errorf("diff missing context before change:\n%s", diff)
	}
	// Should include context after the change
	if !strings.Contains(diff, " line7\n") {
		t.Errorf("diff missing context after change:\n%s", diff)
	}
}

func TestComputeDiffTruncation(t *testing.T) {
	// Build a diff that exceeds 2000 chars
	var oldLines, newLines []string
	for i := 0; i < 500; i++ {
		oldLines = append(oldLines, fmt.Sprintf("old line %d with some extra text to fill space", i))
		newLines = append(newLines, fmt.Sprintf("new line %d with some extra text to fill space", i))
	}
	diff := computeDiff(strings.Join(oldLines, "\n"), strings.Join(newLines, "\n"))
	if !strings.Contains(diff, "... (truncated)") {
		t.Errorf("expected truncation marker in large diff, got %d chars", len(diff))
	}
}

func TestMatchGlob(t *testing.T) {
	tests := []struct {
		pattern string
		path    string
		isDir   bool
		want    bool
	}{
		// Simple glob
		{"*.go", "main.go", false, true},
		{"*.go", "main.py", false, false},
		{"*.go", "src/main.go", false, true},
		{"Makefile", "Makefile", false, true},
		{"Makefile", "src/Makefile", false, true},

		// ** recursive patterns
		{"**/*.go", "main.go", false, true},
		{"**/*.go", "cmd/main.go", false, true},
		{"**/*.go", "cmd/agent/main.go", false, true},
		{"**/*.go", "main.py", false, false},
		{"**/*.ts", "src/components/App.ts", false, true},

		// ** with prefix
		{"src/**/*.go", "src/main.go", false, true},
		{"src/**/*.go", "src/cmd/main.go", false, true},
		{"src/**/*.go", "lib/main.go", false, false},

		// ** alone matches all files, not dirs
		{"**", "any/path/file.txt", false, true},
		{"**", "somedir", true, false},

		// Directories should not match file patterns
		{"*.go", "cmd", true, false},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s_%s", tt.pattern, tt.path), func(t *testing.T) {
			got := matchGlob(tt.pattern, tt.path, tt.isDir)
			if got != tt.want {
				t.Errorf("matchGlob(%q, %q, %v) = %v, want %v", tt.pattern, tt.path, tt.isDir, got, tt.want)
			}
		})
	}
}

func TestStripHTML(t *testing.T) {
	tests := []struct {
		name string
		html string
		want string
	}{
		{
			"basic tags",
			"<p>Hello <b>world</b></p>",
			"Hello world",
		},
		{
			"script removal",
			"<p>Before</p><script>alert('xss')</script><p>After</p>",
			"BeforeAfter",
		},
		{
			"style removal",
			"<style>.foo { color: red; }</style><p>Content</p>",
			"Content",
		},
		{
			"nav removal",
			"<nav><a href='/'>Home</a></nav><main>Content</main>",
			"Content",
		},
		{
			"footer removal",
			"<main>Content</main><footer>Copyright 2024</footer>",
			"Content",
		},
		{
			"entity decoding",
			"&amp; &lt;tag&gt; &quot;quoted&quot; &#39;apos&#39; &nbsp;space",
			"& <tag> \"quoted\" 'apos' space",
		},
		{
			"whitespace normalization",
			"<p>  lots   of    spaces  </p>\n\n\n\n\nand\n\n\n\nnewlines",
			"lots of spaces \n\nand\n\nnewlines",
		},
		{
			"nested tags",
			"<div><ul><li>item 1</li><li>item 2</li></ul></div>",
			"item 1item 2",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripHTML(tt.html)
			if got != tt.want {
				t.Errorf("stripHTML() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStripHTMLMultilineScript(t *testing.T) {
	html := `<html><head>
<script type="text/javascript">
function foo() {
  return "bar";
}
</script>
</head><body><p>Hello</p></body></html>`
	got := stripHTML(html)
	if strings.Contains(got, "function") {
		t.Errorf("script content not removed: %q", got)
	}
	if !strings.Contains(got, "Hello") {
		t.Errorf("body content missing: %q", got)
	}
}

func TestWebFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/text":
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, "Hello, World!")
		case "/html":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "<html><body><p>Hello</p><script>evil()</script></body></html>")
		case "/json":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"key":"value"}`)
		case "/large":
			w.Header().Set("Content-Type", "text/plain")
			// Write >10KB of text
			for i := 0; i < 2000; i++ {
				fmt.Fprintf(w, "line %04d: some text to fill the buffer\n", i)
			}
		case "/error":
			w.WriteHeader(500)
			fmt.Fprint(w, "internal error")
		}
	}))
	defer srv.Close()

	t.Run("plain text", func(t *testing.T) {
		input, _ := json.Marshal(map[string]string{"url": srv.URL + "/text"})
		result := toolWebFetch(input)
		var resp map[string]interface{}
		json.Unmarshal([]byte(result), &resp)

		if resp["status"] != float64(200) {
			t.Errorf("status = %v, want 200", resp["status"])
		}
		if !strings.Contains(resp["text"].(string), "Hello, World!") {
			t.Errorf("text = %q, want 'Hello, World!'", resp["text"])
		}
		if resp["truncated"] != false {
			t.Errorf("truncated = %v, want false", resp["truncated"])
		}
	})

	t.Run("html stripping", func(t *testing.T) {
		input, _ := json.Marshal(map[string]string{"url": srv.URL + "/html"})
		result := toolWebFetch(input)
		var resp map[string]interface{}
		json.Unmarshal([]byte(result), &resp)

		text := resp["text"].(string)
		if !strings.Contains(text, "Hello") {
			t.Errorf("text missing 'Hello': %q", text)
		}
		if strings.Contains(text, "evil") {
			t.Errorf("script not stripped: %q", text)
		}
		if strings.Contains(text, "<") {
			t.Errorf("HTML tags not stripped: %q", text)
		}
	})

	t.Run("json passthrough", func(t *testing.T) {
		input, _ := json.Marshal(map[string]string{"url": srv.URL + "/json"})
		result := toolWebFetch(input)
		var resp map[string]interface{}
		json.Unmarshal([]byte(result), &resp)

		if !strings.Contains(resp["text"].(string), `"key":"value"`) {
			t.Errorf("JSON not passed through: %q", resp["text"])
		}
	})

	t.Run("truncation", func(t *testing.T) {
		input, _ := json.Marshal(map[string]string{"url": srv.URL + "/large"})
		result := toolWebFetch(input)
		var resp map[string]interface{}
		json.Unmarshal([]byte(result), &resp)

		if resp["truncated"] != true {
			t.Errorf("truncated = %v, want true", resp["truncated"])
		}
		text := resp["text"].(string)
		if len(text) > 10001 {
			t.Errorf("text not truncated: %d bytes", len(text))
		}
	})

	t.Run("error status", func(t *testing.T) {
		input, _ := json.Marshal(map[string]string{"url": srv.URL + "/error"})
		result := toolWebFetch(input)
		var resp map[string]interface{}
		json.Unmarshal([]byte(result), &resp)

		if resp["status"] != float64(500) {
			t.Errorf("status = %v, want 500", resp["status"])
		}
	})

	t.Run("missing url", func(t *testing.T) {
		result := toolWebFetch(json.RawMessage(`{}`))
		if !strings.Contains(result, "error") {
			t.Errorf("expected error for missing url: %s", result)
		}
	})

	t.Run("invalid scheme", func(t *testing.T) {
		input, _ := json.Marshal(map[string]string{"url": "ftp://example.com"})
		result := toolWebFetch(input)
		if !strings.Contains(result, "error") {
			t.Errorf("expected error for ftp url: %s", result)
		}
	})
}

func TestJsonResult(t *testing.T) {
	result := jsonResult(map[string]interface{}{"ok": true, "count": 5})
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("jsonResult not valid JSON: %v", err)
	}
	if parsed["ok"] != true {
		t.Errorf("ok = %v, want true", parsed["ok"])
	}
	if parsed["count"] != float64(5) {
		t.Errorf("count = %v, want 5", parsed["count"])
	}
}

func TestJsonError(t *testing.T) {
	result := jsonError("something failed")
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("jsonError not valid JSON: %v", err)
	}
	if parsed["ok"] != false {
		t.Errorf("ok = %v, want false", parsed["ok"])
	}
	if parsed["error"] != "something failed" {
		t.Errorf("error = %v, want 'something failed'", parsed["error"])
	}
}
