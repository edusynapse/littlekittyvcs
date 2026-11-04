package blob

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

type memObj struct {
	data     []byte
	modified time.Time
}

// Mem is an in-memory Store. RangeCalls records every GetRange, in order,
// including calls that return an error.
type Mem struct {
	mu    sync.Mutex
	objs  map[string]memObj
	calls []RangeCall
}

// RangeCall is one GetRange invocation.
type RangeCall struct {
	Key string
	Off int64
	N   int64
}

// NewMem returns an empty in-memory store.
func NewMem() *Mem {
	return &Mem{objs: map[string]memObj{}}
}

// RangeCalls returns a copy of every GetRange call so far, in order.
func (m *Mem) RangeCalls() []RangeCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]RangeCall, len(m.calls))
	copy(out, m.calls)
	return out
}

// ResetCalls forgets the recorded GetRange calls.
func (m *Mem) ResetCalls() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = nil
}

// Raw returns a copy of the stored bytes. It returns nil when the key is absent.
func (m *Mem) Raw(key string) []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	obj, ok := m.objs[key]
	if !ok {
		return nil
	}
	out := make([]byte, len(obj.data))
	copy(out, obj.data)
	return out
}

// SetRaw replaces the stored bytes. Tamper tests use it.
func (m *Mem) SetRaw(key string, b []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]byte, len(b))
	copy(cp, b)
	m.objs[key] = memObj{data: cp, modified: time.Now()}
}

func (m *Mem) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	buf, err := readExactly(r, size)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objs[key] = memObj{data: buf, modified: time.Now()}
	return nil
}

func (m *Mem) GetRange(ctx context.Context, key string, off, n int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, RangeCall{Key: key, Off: off, N: n})
	obj, ok := m.objs[key]
	if !ok {
		return nil, fmt.Errorf("blob: %s: %w", key, ErrNotFound)
	}
	if off < 0 || n <= 0 || off > int64(len(obj.data)) || n > int64(len(obj.data))-off {
		return nil, fmt.Errorf("blob: range %d+%d outside object of %d bytes", off, n, len(obj.data))
	}
	out := make([]byte, n)
	copy(out, obj.data[off:off+n])
	return out, nil
}

func (m *Mem) Head(ctx context.Context, key string) (Info, error) {
	if err := ctx.Err(); err != nil {
		return Info{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	obj, ok := m.objs[key]
	if !ok {
		return Info{}, fmt.Errorf("blob: %s: %w", key, ErrNotFound)
	}
	return Info{Key: key, Size: int64(len(obj.data)), LastModified: obj.modified}, nil
}

func (m *Mem) List(ctx context.Context, prefix string, fn func(Info) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	infos := make([]Info, 0)
	for key, obj := range m.objs {
		if strings.HasPrefix(key, prefix) {
			infos = append(infos, Info{Key: key, Size: int64(len(obj.data)), LastModified: obj.modified})
		}
	}
	m.mu.Unlock()
	sort.Slice(infos, func(i, j int) bool { return infos[i].Key < infos[j].Key })
	for _, info := range infos {
		if err := fn(info); err != nil {
			return err
		}
	}
	return nil
}

func (m *Mem) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.objs[key]; !ok {
		return fmt.Errorf("blob: %s: %w", key, ErrNotFound)
	}
	delete(m.objs, key)
	return nil
}
