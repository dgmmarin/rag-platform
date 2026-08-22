package provision

import (
	"context"
	"errors"
	"testing"
)

// TestLifecycleRequiresPrivilegedURL proves every lifecycle operation fails
// closed when no privileged connection is configured, before touching a DB.
func TestLifecycleRequiresPrivilegedURL(t *testing.T) {
	l := &Lifecycle{}
	ops := map[string]func() error{
		"Suspend":        func() error { _, err := l.Suspend(context.Background(), "acme"); return err },
		"Resume":         func() error { _, err := l.Resume(context.Background(), "acme"); return err },
		"ScheduleDelete": func() error { _, err := l.ScheduleDelete(context.Background(), "acme", 0); return err },
		"CancelDelete":   func() error { _, err := l.CancelDelete(context.Background(), "acme"); return err },
		"RunDelete":      func() error { _, err := l.RunDelete(context.Background(), "acme"); return err },
		"Move":           func() error { _, err := l.Move(context.Background(), "acme", MoveParams{Host: "pg-2"}); return err },
	}
	for name, op := range ops {
		if err := op(); err == nil {
			t.Errorf("%s with no privileged URL should fail closed", name)
		} else if !errors.Is(err, errValidation) {
			t.Errorf("%s: want errValidation, got %v", name, err)
		}
	}
}

// TestLifecycleRejectsEmptySlug proves a blank slug is rejected up front.
func TestLifecycleRejectsEmptySlug(t *testing.T) {
	l := &Lifecycle{PrivilegedURL: "postgres://x"}
	if _, err := l.Suspend(context.Background(), "   "); !errors.Is(err, errValidation) {
		t.Fatalf("blank slug: want errValidation, got %v", err)
	}
}

// TestScheduleDeleteRejectsNegativeGrace proves a negative grace is rejected
// rather than silently coerced (fail closed on obviously-wrong input).
func TestScheduleDeleteRejectsNegativeGrace(t *testing.T) {
	l := &Lifecycle{PrivilegedURL: "postgres://x"}
	if _, err := l.ScheduleDelete(context.Background(), "acme", -1); !errors.Is(err, errValidation) {
		t.Fatalf("negative grace: want errValidation, got %v", err)
	}
}
