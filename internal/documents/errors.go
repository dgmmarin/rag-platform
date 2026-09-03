package documents

import (
	"errors"
	"strings"
)

// Package sentinels. They are internal decision signals the handler maps to the
// SPEC-07 §1 error envelope; callers match with errors.Is / errors.As.
var (
	// ErrNotFound is an unknown document (or a non-UUID id).
	ErrNotFound = errors.New("documents: not found")

	// ErrTenantUnavailable wraps every resolver outcome that means "the tenant is
	// not ready to serve" (provisioning/deleting/unknown/schema-behind, and a
	// suspended tenant on a write). The handler returns tenant_unavailable.
	ErrTenantUnavailable = errors.New("documents: tenant unavailable")

	// ErrStorageUnavailable is the EPIC-06 object-storage seam: file upload cannot
	// be served until the storage port is wired (STORY-06.x). The handler returns
	// the not_found seam envelope (mirroring STORY-04.1/04.3), failing closed.
	ErrStorageUnavailable = errors.New("documents: object storage not available")
)

// ValidationError is a 400 with a client-safe message (bad limit/cursor, missing
// file, disallowed type, oversize upload).
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

// allowedUploadExts is the FR-SRC-02 upload allowlist by lower-case extension:
// PDF, DOCX, Markdown, HTML, TXT and CSV. The connector (EPIC-06) may broaden
// this per its own validation; this is the API-level gate that fails closed.
var allowedUploadExts = map[string]string{
	".pdf":  "application/pdf",
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".md":   "text/markdown",
	".html": "text/html",
	".htm":  "text/html",
	".txt":  "text/plain",
	".csv":  "text/csv",
}

// uploadContentType returns the canonical content type for an allowed filename, or
// ok=false when the extension is not on the FR-SRC-02 allowlist.
func uploadContentType(filename string) (string, bool) {
	ext := strings.ToLower(filepathExt(filename))
	ct, ok := allowedUploadExts[ext]
	return ct, ok
}

// filepathExt is filepath.Ext without importing path/filepath for one call; it
// returns the extension including the dot, or "" when there is none.
func filepathExt(name string) string {
	for i := len(name) - 1; i >= 0 && name[i] != '/'; i-- {
		if name[i] == '.' {
			return name[i:]
		}
	}
	return ""
}
