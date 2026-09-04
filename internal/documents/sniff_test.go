package documents

import "testing"

// pdfHead/zipHead/textHead/htmlHead are minimal byte prefixes with the magic
// numbers http.DetectContentType keys on.
var (
	pdfHead  = []byte("%PDF-1.7\n1 0 obj\n")
	zipHead  = []byte("PK\x03\x04\x14\x00\x00\x00")
	textHead = []byte("the quick brown fox\n")
	htmlHead = []byte("<!DOCTYPE html>\n<html><body>hi</body></html>")
	elfHead  = []byte{0x7f, 'E', 'L', 'F', 2, 1, 1, 0} // a binary executable
)

func TestSniffUploadAcceptsMatchingContent(t *testing.T) {
	cases := []struct {
		name     string
		filename string
		head     []byte
		want     string
	}{
		{"pdf", "report.pdf", pdfHead, "application/pdf"},
		{"docx-is-zip", "memo.docx", zipHead, "application/vnd.openxmlformats-officedocument.wordprocessingml.document"},
		{"markdown", "notes.md", textHead, "text/markdown"},
		{"txt", "notes.txt", textHead, "text/plain"},
		{"csv", "rows.csv", textHead, "text/csv"},
		{"html", "page.html", htmlHead, "text/html"},
		{"html-without-doctype-sniffs-text", "page.html", textHead, "text/html"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := sniffUpload(tc.filename, tc.head)
			if err != nil {
				t.Fatalf("sniffUpload(%q) error: %v", tc.filename, err)
			}
			if got != tc.want {
				t.Fatalf("sniffUpload(%q) = %q, want %q", tc.filename, got, tc.want)
			}
		})
	}
}

func TestSniffUploadRejectsUnknownExtension(t *testing.T) {
	if _, err := sniffUpload("evil.exe", elfHead); err == nil {
		t.Fatal("expected an error for a disallowed extension")
	}
}

func TestSniffUploadRejectsContentExtensionMismatch(t *testing.T) {
	// A binary executable renamed .txt must be rejected: the sniffed bytes are not
	// textual, so it is not trusted just because the extension is on the allowlist
	// (defence against a hostile file mislabelled to reach a parser).
	if _, err := sniffUpload("payload.txt", elfHead); err == nil {
		t.Fatal("expected a mismatch error for a binary file with a .txt extension")
	}
	// A .pdf whose bytes are not a PDF (here plain text) is rejected: the client
	// Content-Type / extension is never trusted over the actual bytes.
	if _, err := sniffUpload("notreally.pdf", textHead); err == nil {
		t.Fatal("expected a mismatch error for a non-PDF file with a .pdf extension")
	}
	// A .docx that is not a zip container is rejected.
	if _, err := sniffUpload("fake.docx", textHead); err == nil {
		t.Fatal("expected a mismatch error for a non-zip file with a .docx extension")
	}
}
