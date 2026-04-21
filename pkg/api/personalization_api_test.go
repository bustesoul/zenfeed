package api

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/glidea/zenfeed/pkg/component"
	"github.com/glidea/zenfeed/pkg/config"
	"github.com/glidea/zenfeed/pkg/model"
	"github.com/glidea/zenfeed/pkg/personalization"
	"github.com/glidea/zenfeed/pkg/storage/feed"
	"github.com/glidea/zenfeed/pkg/storage/feed/block"
	"github.com/glidea/zenfeed/pkg/storage/kv"
)

type testKV struct {
	mu   sync.RWMutex
	data map[string][]byte
}

func newTestKV() *testKV {
	return &testKV{data: make(map[string][]byte)}
}

func (m *testKV) Name() string           { return "testKV" }
func (m *testKV) Instance() string       { return "test" }
func (m *testKV) Run() error             { return nil }
func (m *testKV) Ready() <-chan struct{} { ch := make(chan struct{}); close(ch); return ch }
func (m *testKV) Close() error           { return nil }

func (m *testKV) Get(_ context.Context, key []byte) ([]byte, error) {
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

func (m *testKV) Set(_ context.Context, key []byte, value []byte, _ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	copied := make([]byte, len(value))
	copy(copied, value)
	m.data[string(key)] = copied

	return nil
}

type stubFeedStorage struct {
	base    *component.Base[feed.Config, feed.Dependencies]
	queryVO []*block.FeedVO
	byID    map[uint64]*block.FeedVO
}

func newStubFeedStorage(feeds ...*block.FeedVO) *stubFeedStorage {
	byID := make(map[uint64]*block.FeedVO, len(feeds))
	for _, current := range feeds {
		byID[current.ID] = current
	}

	return &stubFeedStorage{
		base: component.New(&component.BaseConfig[feed.Config, feed.Dependencies]{
			Name:         "StubFeedStorage",
			Instance:     "test",
			Config:       &feed.Config{},
			Dependencies: feed.Dependencies{},
		}),
		queryVO: feeds,
		byID:    byID,
	}
}

func (s *stubFeedStorage) Name() string           { return s.base.Name() }
func (s *stubFeedStorage) Instance() string       { return s.base.Instance() }
func (s *stubFeedStorage) Run() error             { return nil }
func (s *stubFeedStorage) Ready() <-chan struct{} { ch := make(chan struct{}); close(ch); return ch }
func (s *stubFeedStorage) Close() error           { return nil }
func (s *stubFeedStorage) Reload(_ *config.App) error {
	return nil
}
func (s *stubFeedStorage) Append(_ context.Context, _ ...*model.Feed) error { return nil }
func (s *stubFeedStorage) Query(_ context.Context, _ block.QueryOptions) ([]*block.FeedVO, error) {
	return append([]*block.FeedVO(nil), s.queryVO...), nil
}
func (s *stubFeedStorage) Get(_ context.Context, id uint64, _ time.Time) (*block.FeedVO, bool, error) {
	feedVO, ok := s.byID[id]
	return feedVO, ok, nil
}
func (s *stubFeedStorage) Exists(_ context.Context, id uint64, _ time.Time) (bool, error) {
	_, ok := s.byID[id]
	return ok, nil
}

func newTestAPI(t *testing.T, storage feed.Storage, kvs kv.Storage) *api {
	t.Helper()

	return &api{
		Base: component.New(&component.BaseConfig[Config, Dependencies]{
			Name:     "API",
			Instance: "test",
			Config:   &Config{},
			Dependencies: Dependencies{
				FeedStorage: storage,
				KVStorage:   kvs,
			},
		}),
	}
}

func newFeedVO(id uint64, title, source, analysis string, ts time.Time) *block.FeedVO {
	return &block.FeedVO{
		Feed: &model.Feed{
			ID: id,
			Labels: model.Labels{
				{Key: model.LabelTitle, Value: title},
				{Key: model.LabelSource, Value: source},
				{Key: "article_analysis", Value: analysis},
			},
			Time: ts,
		},
	}
}

func TestFeedbackArchivesResolvedFeedSnapshot(t *testing.T) {
	t.Parallel()

	kvStore := newTestKV()
	storage := newStubFeedStorage(newFeedVO(
		1,
		"GPT-5 工具链实战",
		"Github 热榜",
		`{"base_quality_score":8,"primary_topic":"AI/工具","format":"深度分析","topics_json":["AI/工具"]}`,
		time.Now(),
	))
	apiImpl := newTestAPI(t, storage, kvStore)

	resp, err := apiImpl.Feedback(context.Background(), &FeedbackRequest{
		FeedID:      "1",
		Score:       9,
		ScoreReason: "信息密度高",
		Tags: []personalization.UserTag{
			{Tag: "AI/工具", Action: personalization.TagActionBoost},
		},
		Archive: true,
	})
	if err != nil {
		t.Fatalf("feedback failed: %v", err)
	}
	if resp.Message == "" {
		t.Fatal("feedback response message should not be empty")
	}

	store := personalization.NewStore(kvStore)
	archive, err := store.GetArchive(context.Background(), "1")
	if err != nil {
		t.Fatalf("get archive: %v", err)
	}
	if archive.Labels[model.LabelTitle] != "GPT-5 工具链实战" {
		t.Fatalf("archive title = %q", archive.Labels[model.LabelTitle])
	}
	if archive.Labels[model.LabelSource] != "Github 热榜" {
		t.Fatalf("archive source = %q", archive.Labels[model.LabelSource])
	}
	if archive.Feedback == nil || archive.Feedback.Score != 9 {
		t.Fatalf("archive feedback score mismatch: %+v", archive.Feedback)
	}
}

func TestQueryFiltersReadAndBlockedFeeds(t *testing.T) {
	t.Parallel()

	now := time.Now()
	blocked := newFeedVO(
		1,
		"Should Block",
		"SourceA",
		`{"base_quality_score":9,"primary_topic":"AI/工具","format":"深度分析","topics_json":["AI/工具"]}`,
		now,
	)
	unread := newFeedVO(
		2,
		"Should Keep",
		"SourceB",
		`{"base_quality_score":7,"primary_topic":"Rust","format":"教程","topics_json":["Rust"]}`,
		now.Add(-time.Hour),
	)
	readFeed := newFeedVO(
		3,
		"Already Read",
		"SourceC",
		`{"base_quality_score":10,"primary_topic":"数据库","format":"深度分析","topics_json":["数据库"]}`,
		now.Add(-2*time.Hour),
	)

	kvStore := newTestKV()
	store := personalization.NewStore(kvStore)
	if err := store.MarkRead(context.Background(), "3"); err != nil {
		t.Fatalf("mark read: %v", err)
	}
	if err := store.SaveProfile(context.Background(), &personalization.ProfileGlobal{
		FeedbackCount: 5,
		TagControls: []personalization.TagControl{
			{Tag: "AI/工具", Action: personalization.TagActionBlock, Weight: 1},
		},
	}); err != nil {
		t.Fatalf("save profile: %v", err)
	}

	apiImpl := newTestAPI(t, newStubFeedStorage(blocked, unread, readFeed), kvStore)
	resp, err := apiImpl.Query(context.Background(), &QueryRequest{
		Limit: 10,
		Start: now.Add(-24 * time.Hour),
		End:   now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if len(resp.Feeds) != 1 {
		t.Fatalf("feeds len = %d, want 1", len(resp.Feeds))
	}
	if resp.Feeds[0].ID != 2 {
		t.Fatalf("remaining feed id = %d, want 2", resp.Feeds[0].ID)
	}
}

func TestListReadsReturnsCurrentNamespaceEntries(t *testing.T) {
	t.Parallel()

	kvStore := newTestKV()
	apiImpl := newTestAPI(t, newStubFeedStorage(), kvStore)

	if _, err := apiImpl.MarkRead(context.Background(), &MarkReadRequest{
		FeedIDs: []string{"1", "2"},
	}); err != nil {
		t.Fatalf("mark read failed: %v", err)
	}

	resp, err := apiImpl.ListReads(context.Background(), &ListReadsRequest{})
	if err != nil {
		t.Fatalf("list reads failed: %v", err)
	}
	if resp.Total != 2 || len(resp.Reads) != 2 {
		t.Fatalf("unexpected list reads response: %+v", resp)
	}
	if resp.Reads[0].FeedID != "1" || resp.Reads[1].FeedID != "2" {
		t.Fatalf("unexpected read ids: %+v", resp.Reads)
	}
}

func TestResetProfileDropsReadHistoryFromCurrentNamespace(t *testing.T) {
	t.Parallel()

	kvStore := newTestKV()
	apiImpl := newTestAPI(t, newStubFeedStorage(), kvStore)

	if _, err := apiImpl.MarkRead(context.Background(), &MarkReadRequest{
		FeedIDs: []string{"1"},
	}); err != nil {
		t.Fatalf("mark read failed: %v", err)
	}
	if _, err := apiImpl.ResetProfile(context.Background(), &ResetProfileRequest{}); err != nil {
		t.Fatalf("reset profile failed: %v", err)
	}

	resp, err := apiImpl.ListReads(context.Background(), &ListReadsRequest{})
	if err != nil {
		t.Fatalf("list reads after reset failed: %v", err)
	}
	if resp.Total != 0 || len(resp.Reads) != 0 {
		t.Fatalf("expected empty reads after reset, got %+v", resp)
	}
}
