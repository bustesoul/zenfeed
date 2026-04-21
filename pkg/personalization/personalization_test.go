package personalization

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/pkg/errors"

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

func TestReplaceFeedbackProfileReplacesSignalsWithoutInflatingCount(t *testing.T) {
	t.Parallel()

	store := NewStore(newMemoryKV())
	ctx := context.Background()

	first := &Feedback{
		FeedID: "1",
		AppliedSignals: []AppliedTagSignal{
			{Tag: "AI/工具", Action: TagActionBoost, Delta: 0.6},
		},
	}
	if err := store.ReplaceFeedbackProfile(ctx, nil, first); err != nil {
		t.Fatalf("apply first feedback: %v", err)
	}

	second := &Feedback{
		FeedID: "1",
		AppliedSignals: []AppliedTagSignal{
			{Tag: "AI/工具", Action: TagActionDemote, Delta: 0.6},
		},
	}
	if err := store.ReplaceFeedbackProfile(ctx, first, second); err != nil {
		t.Fatalf("replace feedback: %v", err)
	}

	profile, err := store.GetProfile(ctx)
	if err != nil {
		t.Fatalf("get profile: %v", err)
	}
	if profile.FeedbackCount != 1 {
		t.Fatalf("feedback_count = %d, want 1", profile.FeedbackCount)
	}
	if len(profile.TagControls) != 1 {
		t.Fatalf("tag_controls len = %d, want 1", len(profile.TagControls))
	}
	if profile.TagControls[0].Action != TagActionDemote {
		t.Fatalf("action = %s, want %s", profile.TagControls[0].Action, TagActionDemote)
	}
	if profile.TagControls[0].Weight != 0.6 {
		t.Fatalf("weight = %.2f, want 0.60", profile.TagControls[0].Weight)
	}
}

func TestSaveArchiveDeduplicatesIndexEntries(t *testing.T) {
	t.Parallel()

	store := NewStore(newMemoryKV())
	ctx := context.Background()

	entry := &ArchiveEntry{
		FeedID:   "1",
		Labels:   map[string]string{"title": "First", "source": "SourceA"},
		FeedTime: time.Now(),
	}
	if err := store.SaveArchive(ctx, entry); err != nil {
		t.Fatalf("save archive first time: %v", err)
	}

	entry.Labels["title"] = "Updated"
	if err := store.SaveArchive(ctx, entry); err != nil {
		t.Fatalf("save archive second time: %v", err)
	}

	profile, err := store.GetProfile(ctx)
	if err != nil {
		t.Fatalf("get profile: %v", err)
	}
	if len(profile.ArchiveIndex) != 1 {
		t.Fatalf("archive_index len = %d, want 1", len(profile.ArchiveIndex))
	}
	if profile.ArchiveIndex[0].Title != "Updated" {
		t.Fatalf("archive title = %q, want Updated", profile.ArchiveIndex[0].Title)
	}
	if profile.ArchiveIndex[0].Source != "SourceA" {
		t.Fatalf("archive source = %q, want SourceA", profile.ArchiveIndex[0].Source)
	}
}

func TestMarkReadBatchDeduplicatesReadIndex(t *testing.T) {
	t.Parallel()

	store := NewStore(newMemoryKV())
	ctx := context.Background()

	if err := store.MarkReadBatch(ctx, []string{"1", "2", "1"}); err != nil {
		t.Fatalf("mark read batch: %v", err)
	}

	profile, err := store.GetProfile(ctx)
	if err != nil {
		t.Fatalf("get profile: %v", err)
	}
	if len(profile.ReadIndex) != 2 {
		t.Fatalf("read_index len = %d, want 2", len(profile.ReadIndex))
	}
	readTimes := make(map[string]time.Time, len(profile.ReadIndex))
	for _, entry := range profile.ReadIndex {
		readTimes[entry.FeedID] = entry.ReadAt
	}
	if len(readTimes) != 2 || readTimes["1"].IsZero() || readTimes["2"].IsZero() {
		t.Fatalf("unexpected read index content: %+v", profile.ReadIndex)
	}
	if readTimes["1"].Before(readTimes["2"]) {
		t.Fatalf("expected repeated feed to carry the latest read_at, got %+v", profile.ReadIndex)
	}
	if !store.IsRead(ctx, "1") || !store.IsRead(ctx, "2") {
		t.Fatalf("expected read entries to be queryable after mark read")
	}
}

func TestResetAdvancesNamespaceAndHidesLegacyHistory(t *testing.T) {
	t.Parallel()

	store := NewStore(newMemoryKV())
	ctx := context.Background()

	if err := store.MarkRead(ctx, "1"); err != nil {
		t.Fatalf("mark read: %v", err)
	}
	if err := store.SaveFeedback(ctx, &Feedback{
		FeedID: "1",
		Tags: []UserTag{
			{Tag: "AI/工具", Action: TagActionBoost},
		},
	}); err != nil {
		t.Fatalf("save feedback: %v", err)
	}
	if err := store.SaveArchive(ctx, &ArchiveEntry{
		FeedID:   "1",
		Labels:   map[string]string{"title": "First"},
		FeedTime: time.Now(),
	}); err != nil {
		t.Fatalf("save archive: %v", err)
	}

	if err := store.Reset(ctx); err != nil {
		t.Fatalf("reset: %v", err)
	}

	profile, err := store.GetProfile(ctx)
	if err != nil {
		t.Fatalf("get profile after reset: %v", err)
	}
	if profile.NamespaceVersion != 1 {
		t.Fatalf("namespace_version = %d, want 1", profile.NamespaceVersion)
	}
	if profile.FeedbackCount != 0 || len(profile.TagControls) != 0 || len(profile.ReadIndex) != 0 || len(profile.ArchiveIndex) != 0 {
		t.Fatalf("profile should be empty after reset: %+v", profile)
	}
	if store.IsRead(ctx, "1") {
		t.Fatal("legacy read state should not leak after reset")
	}
	if _, err := store.GetFeedback(ctx, "1"); !errors.Is(err, kv.ErrNotFound) {
		t.Fatalf("get feedback after reset error = %v, want kv.ErrNotFound", err)
	}
	if _, err := store.GetArchive(ctx, "1"); !errors.Is(err, kv.ErrNotFound) {
		t.Fatalf("get archive after reset error = %v, want kv.ErrNotFound", err)
	}
}
