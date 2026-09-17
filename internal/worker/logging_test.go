package worker

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/riverqueue/river/rivertype"
)

func TestLogMiddlewareLogsCancelledDistinctFromFailed(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	m := logMiddleware{log: log}
	job := &rivertype.JobRow{ID: 84, Kind: "sync_source", Attempt: 1, MaxAttempts: 3, EncodedArgs: []byte(`{}`)}

	_ = m.Work(context.Background(), job, func(context.Context) error { return context.Canceled })

	out := buf.String()
	if !strings.Contains(out, `"event":"cancelled"`) {
		t.Fatalf("ctx.Canceled must log event=cancelled, got:\n%s", out)
	}
	if strings.Contains(out, `"event":"failed"`) {
		t.Fatalf("a cancel must not be logged as failed:\n%s", out)
	}
}
