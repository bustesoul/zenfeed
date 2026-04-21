package stats

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glidea/zenfeed/pkg/storage/kv"
)

type memoryKV struct {
	mu   sync.RWMutex
	data map[string][]byte
}

func newMemoryKV() *memoryKV {
	return &memoryKV{data: make(map[string][]byte)}
}

func (m *memoryKV) Name() string           { return "memoryKV" }
func (m *memoryKV) Instance() string       { return "test" }
func (m *memoryKV) Run() error             { return nil }
func (m *memoryKV) Ready() <-chan struct{} { ch := make(chan struct{}); close(ch); return ch }
func (m *memoryKV) Close() error           { return nil }

func (m *memoryKV) Get(_ context.Context, key []byte) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	value, ok := m.data[string(key)]
	if !ok {
		return nil, kv.ErrNotFound
	}

	copied := make([]byte, len(value))
	copy(copied, value)

	return copied, nil
}

func (m *memoryKV) Set(_ context.Context, key []byte, value []byte, _ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	copied := make([]byte, len(value))
	copy(copied, value)
	m.data[string(key)] = copied

	return nil
}

func (m *memoryKV) Delete(_ context.Context, key []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.data, string(key))

	return nil
}

func (m *memoryKV) Keys(_ context.Context, prefix []byte) ([][]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	keys := make([][]byte, 0, len(m.data))
	for key := range m.data {
		if len(prefix) > 0 && !strings.HasPrefix(key, string(prefix)) {
			continue
		}
		keys = append(keys, []byte(key))
	}

	return keys, nil
}

func TestPersistIgnoresStaleSnapshot(t *testing.T) {
	t.Parallel()

	store := New(newMemoryKV())

	store.mu.Lock()
	store.sources["Source B"] = &SourceStat{
		Name:         "Source B",
		URL:          "https://example.com/b",
		TotalFetched: 7,
		PersistSeq:   2,
	}
	store.mu.Unlock()

	latest := &SourceStat{
		Name:         "Source B",
		URL:          "https://example.com/b",
		TotalFetched: 7,
		PersistSeq:   2,
	}
	stale := &SourceStat{
		Name:         "Source B",
		URL:          "https://example.com/b",
		TotalFetched: 3,
		PersistSeq:   1,
	}

	store.persist("Source B", latest)
	store.persist("Source B", stale)

	data, err := store.kv.Get(context.Background(), []byte(kvKeyPrefix+"Source B"))
	if err != nil {
		t.Fatalf("get persisted source: %v", err)
	}

	var persisted SourceStat
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatalf("unmarshal persisted source: %v", err)
	}
	if persisted.TotalFetched != 7 {
		t.Fatalf("total_fetched = %d, want 7", persisted.TotalFetched)
	}
}

func TestSnapshotReturnsSourcesInStableOrder(t *testing.T) {
	t.Parallel()

	store := New(newMemoryKV())
	store.RecordScrapeStart("zeta", "https://example.com/z")
	store.RecordScrapeStart("alpha", "https://example.com/a")
	store.RecordScrapeStart("beta", "https://example.com/b")

	snapshot, err := store.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	if len(snapshot.Sources) != 3 {
		t.Fatalf("sources len = %d, want 3", len(snapshot.Sources))
	}
	got := []string{
		snapshot.Sources[0].Name,
		snapshot.Sources[1].Name,
		snapshot.Sources[2].Name,
	}
	want := []string{"alpha", "beta", "zeta"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sources[%d] = %q, want %q (full order: %v)", i, got[i], want[i], got)
		}
	}
}
