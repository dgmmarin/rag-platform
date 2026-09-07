package querylog

import (
	"encoding/base64"
	"encoding/json"
)

// encodeCursor serialises a Cursor to an opaque base64url token (mirrors
// internal/documents' cursor helpers).
func encodeCursor(c Cursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeCursor parses a base64url query-log cursor token.
func decodeCursor(s string) (*Cursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	var c Cursor
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return &c, nil
}
