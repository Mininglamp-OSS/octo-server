package file

import (
	"mime"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestEnsureTextCharset(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"text/plain gets charset", "text/plain", "text/plain; charset=utf-8"},
		{"text/markdown gets charset", "text/markdown", "text/markdown; charset=utf-8"},
		{"text/html gets charset", "text/html", "text/html; charset=utf-8"},
		{"text/csv gets charset", "text/csv", "text/csv; charset=utf-8"},
		{"image/jpeg unchanged", "image/jpeg", "image/jpeg"},
		{"application/json unchanged", "application/json", "application/json"},
		{"application/octet-stream unchanged", "application/octet-stream", "application/octet-stream"},
		{"already has charset", "text/plain; charset=utf-8", "text/plain; charset=utf-8"},
		{"already has charset uppercase", "text/plain; Charset=UTF-8", "text/plain; Charset=UTF-8"},
		{"empty string unchanged", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ensureTextCharset(tt.input)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestContentTypeInference(t *testing.T) {
	// Simulate the logic in uploadFile: if contentType is octet-stream,
	// infer from extension (with fallback), then apply ensureTextCharset.
	inferContentType := func(formValue string, ext string) string {
		contentType := formValue
		if contentType == "application/octet-stream" {
			if detected := mime.TypeByExtension(ext); detected != "" {
				contentType = detected
			} else if fallback, ok := extMIMEFallback[ext]; ok {
				contentType = fallback
			}
		}
		return ensureTextCharset(contentType)
	}

	tests := []struct {
		name      string
		formValue string
		ext       string
		wantType  string
	}{
		{
			name:      "md file inferred from extension with charset",
			formValue: "application/octet-stream",
			ext:       ".md",
			wantType:  "text/markdown; charset=utf-8",
		},
		{
			name:      "txt file inferred from extension with charset",
			formValue: "application/octet-stream",
			ext:       ".txt",
			wantType:  "text/plain; charset=utf-8",
		},
		{
			name:      "html file inferred from extension with charset",
			formValue: "application/octet-stream",
			ext:       ".html",
			wantType:  "text/html; charset=utf-8",
		},
		{
			name:      "jpg file inferred from extension no charset",
			formValue: "application/octet-stream",
			ext:       ".jpg",
			wantType:  "image/jpeg",
		},
		{
			name:      "explicit text/plain gets charset",
			formValue: "text/plain",
			ext:       ".txt",
			wantType:  "text/plain; charset=utf-8",
		},
		{
			name:      "already correct text/plain charset not doubled",
			formValue: "text/plain; charset=utf-8",
			ext:       ".txt",
			wantType:  "text/plain; charset=utf-8",
		},
		{
			name:      "unknown extension stays octet-stream",
			formValue: "application/octet-stream",
			ext:       ".xyz123",
			wantType:  "application/octet-stream",
		},
		{
			name:      "css file inferred with charset",
			formValue: "application/octet-stream",
			ext:       ".css",
			wantType:  "text/css; charset=utf-8",
		},
		{
			name:      "xml file inferred from extension",
			formValue: "application/octet-stream",
			ext:       ".xml",
			// mime.TypeByExtension(".xml") returns "text/xml; charset=utf-8" on most systems
			// but the exact value depends on the OS mime database; just verify it's not octet-stream
			wantType:  "", // checked separately below
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := inferContentType(tt.formValue, tt.ext)
			if tt.wantType == "" {
				// For OS-dependent types, just verify the inference happened
				assert.NotEqual(t, "application/octet-stream", got, "should infer from extension")
			} else {
				assert.Equal(t, tt.wantType, got)
			}
		})
	}
}
