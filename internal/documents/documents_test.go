package documents

import (
	"testing"
	"time"
)

func TestCursorRoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	c := Cursor{FirstSeenAt: now, ID: "11111111-1111-1111-1111-111111111111"}
	got, err := decodeCursor(encodeCursor(c))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.FirstSeenAt.Equal(c.FirstSeenAt) || got.ID != c.ID {
		t.Fatalf("round trip = %+v, want %+v", got, c)
	}
	if _, err := decodeCursor("not-base64-!!!"); err == nil {
		t.Fatal("decode of garbage should fail")
	}
}

func TestChunkCursorRoundTrip(t *testing.T) {
	got, err := decodeChunkCursor(encodeChunkCursor(ChunkCursor{Position: 42}))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Position != 42 {
		t.Fatalf("position = %d, want 42", got.Position)
	}
}

func TestClampLimit(t *testing.T) {
	cases := map[int]int{0: defaultListLimit, -5: defaultListLimit, 10: 10, 1000: maxListLimit}
	for in, want := range cases {
		if got := clampLimit(in); got != want {
			t.Fatalf("clampLimit(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestUploadContentTypeAllowlist(t *testing.T) {
	allowed := map[string]string{
		"a.pdf":     "application/pdf",
		"A.PDF":     "application/pdf",
		"b.docx":    "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"c.md":      "text/markdown",
		"d.html":    "text/html",
		"e.htm":     "text/html",
		"f.txt":     "text/plain",
		"g.csv":     "text/csv",
		"dir/h.csv": "text/csv",
	}
	for name, want := range allowed {
		ct, ok := uploadContentType(name)
		if !ok || ct != want {
			t.Fatalf("uploadContentType(%q) = %q,%v; want %q,true", name, ct, ok, want)
		}
	}
	for _, name := range []string{"evil.exe", "noext", "archive.zip", "img.png", "s.docx.exe"} {
		if _, ok := uploadContentType(name); ok {
			t.Fatalf("uploadContentType(%q) allowed, want rejected", name)
		}
	}
}

func TestValidUUID(t *testing.T) {
	if !validUUID("00000000-0000-0000-0000-000000000000") {
		t.Fatal("zero uuid should be valid")
	}
	for _, bad := range []string{"", "abc", "zzzzzzzz-0000-0000-0000-000000000000", "00000000000000000000000000000000000z"} {
		if validUUID(bad) {
			t.Fatalf("validUUID(%q) = true, want false", bad)
		}
	}
}
