package blob

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestMemStore(t *testing.T) {
	ctx := context.Background()
	m := NewMem()
	body := []byte("hello world, this is the object body")
	if err := m.Put(ctx, "repos/a/pack", bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatal(err)
	}
	if err := m.Put(ctx, "repos/a/idx", bytes.NewReader([]byte("idx")), 3); err != nil {
		t.Fatal(err)
	}
	if err := m.Put(ctx, "other/x", bytes.NewReader([]byte("no")), 2); err != nil {
		t.Fatal(err)
	}

	got, err := m.GetRange(ctx, "repos/a/pack", 6, 5)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "world" {
		t.Fatalf("GetRange %q", got)
	}
	if _, err := m.GetRange(ctx, "repos/a/pack", int64(len(body)-2), 5); err == nil {
		t.Fatal("GetRange past the end returned nil")
	}

	info, err := m.Head(ctx, "repos/a/pack")
	if err != nil {
		t.Fatal(err)
	}
	if info.Key != "repos/a/pack" || info.Size != int64(len(body)) {
		t.Fatalf("Head %+v", info)
	}

	var listed []string
	if err := m.List(ctx, "repos/a/", func(info Info) error {
		listed = append(listed, info.Key)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 || listed[0] != "repos/a/idx" || listed[1] != "repos/a/pack" {
		t.Fatalf("List %v", listed)
	}

	if err := m.Delete(ctx, "repos/a/pack"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.GetRange(ctx, "repos/a/pack", 0, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetRange deleted: %v", err)
	}
	if _, err := m.Head(ctx, "repos/a/pack"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Head deleted: %v", err)
	}
	if err := m.Delete(ctx, "repos/a/pack"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete deleted: %v", err)
	}
}
