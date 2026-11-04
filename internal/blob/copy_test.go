package blob

import (
	"bytes"
	"context"
	"testing"
)

func TestCopyStreamsInParts(t *testing.T) {
	size := 3*PartSize + 5
	body := make([]byte, size)
	for i := range body {
		body[i] = byte(i)
	}
	src := NewMem()
	dst := NewMem()
	key := "repos/00000000-0000-4000-8000-000000000001/packs/00000000-0000-4000-8000-000000000002.pack"
	if err := src.Put(context.Background(), key, bytes.NewReader(body), int64(size)); err != nil {
		t.Fatal(err)
	}
	src.ResetCalls()
	if err := Copy(context.Background(), src, dst, key, int64(size)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(dst.Raw(key), body) {
		t.Fatal("bytes differ")
	}
	calls := src.RangeCalls()
	if len(calls) != 4 {
		t.Fatalf("ranges %d", len(calls))
	}
	var got int64
	for i, call := range calls {
		if call.Key != key || call.N > int64(PartSize) || call.N <= 0 {
			t.Fatalf("call %d %+v", i, call)
		}
		if call.Off != got {
			t.Fatalf("call %d off %d want %d", i, call.Off, got)
		}
		got += call.N
	}
	if got != int64(size) {
		t.Fatalf("read %d", got)
	}
	if calls[len(calls)-1].N != 5 {
		t.Fatalf("last range %d", calls[len(calls)-1].N)
	}
}
