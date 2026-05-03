package cli

import (
	"reflect"
	"testing"
)

// TestSanitizeImages_KeepsValidDataURIs asserts the happy path: entries that
// already look like MakeThumbnail output survive untouched, and the original
// slice is returned (no allocation). Locks the "zero-cost on conforming
// producer" property advertised in the godoc. S15 (Round 174).
func TestSanitizeImages_KeepsValidDataURIs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   []string
	}{
		{"nil", nil},
		{"empty", []string{}},
		{"single_jpeg", []string{"data:image/jpeg;base64,AAAA"}},
		{"single_png", []string{"data:image/png;base64,AAAA"}},
		{"multiple_mixed_image_kinds", []string{
			"data:image/jpeg;base64,A",
			"data:image/png;base64,B",
			"data:image/webp;base64,C",
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := sanitizeImages(tc.in)
			if !reflect.DeepEqual(got, tc.in) {
				t.Errorf("sanitizeImages(%v) = %v, want %v (should be unchanged)", tc.in, got, tc.in)
			}
		})
	}
}

// TestSanitizeImages_StripsInvalid asserts the defense: any non-image data URI
// is stripped, and entries with mixed valid+invalid content yield only the
// valid subset. Covers the "future refactor passes through a URL" regression
// scenario called out in the godoc. S15 (Round 174).
func TestSanitizeImages_StripsInvalid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "javascript_uri_stripped",
			in:   []string{"javascript:alert(1)"},
			want: nil,
		},
		{
			name: "http_url_stripped",
			in:   []string{"http://evil.example.com/image.png"},
			want: nil,
		},
		{
			name: "https_url_stripped",
			in:   []string{"https://cdn.example.com/img.jpg"},
			want: nil,
		},
		{
			name: "data_text_html_stripped",
			in:   []string{"data:text/html;base64,PHNjcmlwdD4="},
			want: nil,
		},
		{
			name: "data_application_octet_stream_stripped",
			in:   []string{"data:application/octet-stream;base64,AAAA"},
			want: nil,
		},
		{
			name: "empty_string_stripped",
			in:   []string{""},
			want: nil,
		},
		{
			name: "mixed_valid_and_invalid",
			in: []string{
				"data:image/jpeg;base64,GOOD1",
				"javascript:alert(1)",
				"data:image/png;base64,GOOD2",
				"http://evil/",
				"",
				"data:image/webp;base64,GOOD3",
			},
			want: []string{
				"data:image/jpeg;base64,GOOD1",
				"data:image/png;base64,GOOD2",
				"data:image/webp;base64,GOOD3",
			},
		},
		{
			name: "relative_path_stripped",
			in:   []string{"/static/img.png"},
			want: nil,
		},
		{
			name: "case_sensitive_prefix_data_uppercase_stripped",
			// data: URI scheme is defined as case-insensitive by RFC 2397,
			// but MakeThumbnail always emits lowercase. Accepting uppercase
			// would widen the attack surface for little gain; assert the
			// strict "lowercase data:image/" contract.
			in:   []string{"DATA:image/jpeg;base64,AAAA"},
			want: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := sanitizeImages(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("sanitizeImages(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestEventLog_Append_FiltersInvalidImages verifies that Append enforces the
// contract at the log entry point: even a caller that bypasses MakeThumbnail
// and synthesizes an EventEntry with a javascript: URI cannot pollute the
// stored entry. S15 (Round 174).
func TestEventLog_Append_FiltersInvalidImages(t *testing.T) {
	t.Parallel()

	l := NewEventLog(4)
	l.Append(EventEntry{
		Type: "user",
		Images: []string{
			"data:image/jpeg;base64,OK",
			"javascript:alert(1)",
			"http://evil/",
		},
	})

	entries := l.Entries()
	if len(entries) != 1 {
		t.Fatalf("len = %d, want 1", len(entries))
	}
	want := []string{"data:image/jpeg;base64,OK"}
	if !reflect.DeepEqual(entries[0].Images, want) {
		t.Errorf("Images = %v, want %v", entries[0].Images, want)
	}
}

// TestEventLog_AppendBatch_FiltersInvalidImages mirrors the Append test for
// the batch path. InjectHistory replays historical entries via AppendBatch,
// so stale data must be sanitized on the way in too. S15 (Round 174).
func TestEventLog_AppendBatch_FiltersInvalidImages(t *testing.T) {
	t.Parallel()

	l := NewEventLog(8)
	l.AppendBatch([]EventEntry{
		{
			Type: "user",
			Images: []string{
				"data:image/png;base64,A",
				"javascript:alert(1)",
			},
		},
		{
			Type: "user",
			Images: []string{
				"data:text/html;base64,PHNjcmlwdD4=",
			},
		},
		{
			Type: "user",
			Images: []string{
				"data:image/webp;base64,B",
			},
		},
	})

	entries := l.Entries()
	if len(entries) != 3 {
		t.Fatalf("len = %d, want 3", len(entries))
	}
	if got, want := entries[0].Images, []string{"data:image/png;base64,A"}; !reflect.DeepEqual(got, want) {
		t.Errorf("entries[0].Images = %v, want %v", got, want)
	}
	if entries[1].Images != nil {
		// All entries in this row were invalid; sanitizeImages returns nil
		// (not an empty slice) so downstream JSON marshaling with omitempty
		// produces no field rather than `"images":[]`.
		t.Errorf("entries[1].Images = %v, want nil (all invalid → stripped)", entries[1].Images)
	}
	if got, want := entries[2].Images, []string{"data:image/webp;base64,B"}; !reflect.DeepEqual(got, want) {
		t.Errorf("entries[2].Images = %v, want %v", got, want)
	}
}
