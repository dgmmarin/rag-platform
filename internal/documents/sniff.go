package documents

import (
	"net/http"
	"strings"
)

// sniffUpload derives the canonical content type of an uploaded file from BOTH
// its extension (the FR-SRC-02 allowlist) AND a sniff of its leading bytes — it
// never trusts the client-supplied Content-Type header (SPEC-04 §5, security).
//
// The extension selects the canonical type the parse pipeline dispatches on; the
// byte sniff (http.DetectContentType, which keys on magic numbers) must be
// consistent with it, so a hostile or mislabelled file — an executable renamed
// .txt, a non-PDF served as .pdf — is rejected before it can reach a parser.
// head is the first bytes of the file (http.DetectContentType reads at most 512).
func sniffUpload(filename string, head []byte) (string, error) {
	canonical, ok := uploadContentType(filename)
	if !ok {
		return "", invalid("unsupported file type; allowed: pdf, docx, md, html, txt, csv")
	}
	detected := http.DetectContentType(head)
	if !sniffCompatible(canonical, detected) {
		return "", invalid("file content does not match its declared type (%s)", canonical)
	}
	return canonical, nil
}

// sniffCompatible reports whether a sniffed content type is consistent with the
// canonical type chosen from the extension. http.DetectContentType is coarse
// (it cannot tell markdown from CSV, and reports a docx as application/zip), so
// the check verifies the broad class rather than an exact string:
//   - PDF must sniff as application/pdf,
//   - DOCX (an OOXML zip) must sniff as application/zip,
//   - text formats (markdown/plain/csv) must sniff as some text/* type — binary
//     content (application/octet-stream, an executable, an image) is rejected,
//   - HTML may sniff as text/html or, when it lacks a doctype/tag prefix, text/plain.
func sniffCompatible(canonical, detected string) bool {
	base := detected
	if i := strings.IndexByte(base, ';'); i >= 0 { // strip "; charset=utf-8"
		base = strings.TrimSpace(base[:i])
	}
	switch canonical {
	case "application/pdf":
		return base == "application/pdf"
	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		return base == "application/zip"
	case "text/html":
		return base == "text/html" || base == "text/plain"
	case "text/markdown", "text/plain", "text/csv":
		return strings.HasPrefix(base, "text/")
	default:
		return false
	}
}
