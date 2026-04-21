// Copyright (C) 2025 wangyusong
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program. If not, see <https://www.gnu.org/licenses/>.

package feed

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/benbjohnson/clock"
	"github.com/pkg/errors"

	"github.com/glidea/zenfeed/pkg/component"
	"github.com/glidea/zenfeed/pkg/config"
	"github.com/glidea/zenfeed/pkg/llm"
	"github.com/glidea/zenfeed/pkg/model"
	"github.com/glidea/zenfeed/pkg/rewrite"
	"github.com/glidea/zenfeed/pkg/storage/feed/block"
	"github.com/glidea/zenfeed/pkg/storage/feed/block/chunk"
	"github.com/glidea/zenfeed/pkg/storage/feed/block/index/inverted"
	"github.com/glidea/zenfeed/pkg/storage/feed/block/index/primary"
	"github.com/glidea/zenfeed/pkg/storage/feed/block/index/vector"
	"github.com/glidea/zenfeed/pkg/telemetry"
	"github.com/glidea/zenfeed/pkg/telemetry/log"
	telemetrymodel "github.com/glidea/zenfeed/pkg/telemetry/model"
	timeutil "github.com/glidea/zenfeed/pkg/util/time"
)

var clk = clock.New()

// --- Interface code block ---
type Storage interface {
	component.Component
	config.Watcher

	// Append stores some feeds.
	Append(ctx context.Context, feeds ...*model.Feed) error

	// Query retrieves feeds by query options.
	// Results are sorted by score (if vector query) and time.
	Query(ctx context.Context, query block.QueryOptions) ([]*block.FeedVO, error)

	// Exists checks if a feed exists in the storage.
	// If hintTime is zero, it only checks the head block.
	Exists(ctx context.Context, id uint64, hintTime time.Time) (bool, error)
}

type Config struct {
	Dir           string
	Retention     time.Duration
	BlockDuration time.Duration
	EmbeddingLLM  string
	FlushInterval time.Duration
}

const subDir = "feed"

func (c *Config) Validate() error {
	if c.Dir == "" {
		c.Dir = "./data/" + subDir
	}
	if c.Retention <= 0 {
		c.Retention = 8 * timeutil.Day
	}
	if c.Retention < timeutil.Day || c.Retention > 15*timeutil.Day {
		return errors.New("retention must be between 1 day and 15 days")
	}
	if c.BlockDuration <= 0 {
		c.BlockDuration = 25 * time.Hour
	}
	if c.Retention < c.BlockDuration {
		return errors.Errorf("retention must be greater than %s", c.BlockDuration)
	}
	if c.EmbeddingLLM == "" {
		return errors.New("embedding LLM is required")
	}

	return nil
}

func (c *Config) From(app *config.App) {
	*c = Config{
		Dir:           app.Storage.Dir,
		Retention:     time.Duration(app.Storage.Feed.Retention),
		BlockDuration: time.Duration(app.Storage.Feed.BlockDuration),
		FlushInterval: time.Duration(app.Storage.Feed.FlushInterval),
		EmbeddingLLM:  app.Storage.Feed.EmbeddingLLM,
	}
}

type Dependencies struct {
	BlockFactory    block.Factory
	LLMFactory      llm.Factory
	ChunkFactory    chunk.Factory
	PrimaryFactory  primary.Factory
	InvertedFactory inverted.Factory
	VectorFactory   vector.Factory
	Rewriter        rewrite.Rewriter
}

// --- Factory code block ---
type Factory component.Factory[Storage, config.App, Dependencies]

func NewFactory(mockOn ...component.MockOption) Factory {
	if len(mockOn) > 0 {
		return component.FactoryFunc[Storage, config.App, Dependencies](
			func(instance string, app *config.App, dependencies Dependencies) (Storage, error) {
				m := &mockStorage{}
				component.MockOptions(mockOn).Apply(&m.Mock)

				return m, nil
			},
		)
	}

	return component.FactoryFunc[Storage, config.App, Dependencies](new)
}

func new(instance string, app *config.App, dependencies Dependencies) (Storage, error) {
	config := &Config{}
	config.From(app)
	if err := config.Validate(); err != nil {
		return nil, errors.Wrap(err, "validate config")
	}

	s := &storage{
		Base: component.New(&component.BaseConfig[Config, Dependencies]{
			Name:         "FeedStorage",
			Instance:     instance,
			Config:       config,
			Dependencies: dependencies,
		}),
		blocks: &blockChain{blocks: make(map[string]block.Block)},
	}

	if err := os.MkdirAll(config.Dir, 0700); err != nil {
		return nil, errors.Wrap(err, "ensure data dir")
	}
	if err := loadBlocks(config.Dir, s); err != nil {
		return nil, errors.Wrap(err, "load blocks")
	}

	// Ensure head block.
	if len(s.blocks.list(nil)) == 0 {
		if _, err := s.createBlock(clk.Now()); err != nil {
			return nil, errors.Wrap(err, "create head block")
		}
	}

	return s, nil
}

func loadBlocks(path string, s *storage) error {
	// Scan path.
	ls, err := os.ReadDir(path)
	if err != nil {
		return errors.Wrap(err, "read dir")
	}

	// Load blocks.
	for _, info := range ls {
		if !info.IsDir() {
			continue
		}
		if _, err := s.loadBlock(info.Name()); err != nil {
			return errors.Wrapf(err, "load block %s", info.Name())
		}
	}

	return nil
}

type blockChain struct {
	blocks map[string]block.Block
	mu     sync.RWMutex
}

func (c *blockChain) isHead(b block.Block) bool {
	return timeutil.InRange(clk.Now(), b.Start(), b.End())
}
func (c *blockChain) head() block.Block {
	b, ok := c.get(clk.Now())
	if !ok {
		return nil
	}

	return b
}
func (c *blockChain) list(filter func(block block.Block) bool) []block.Block {
	c.mu.RLock()
	defer c.mu.RUnlock()
	blocks := make([]block.Block, 0, len(c.blocks))
	for _, b := range c.blocks {
		if filter != nil && !filter(b) {
			continue
		}
		blocks = append(blocks, b)
	}

	return blocks
}
func (c *blockChain) endTime() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.blocks) == 0 {
		return time.Time{}
	}
	var maxEnd time.Time
	for _, b := range c.blocks {
		if !b.End().After(maxEnd) {
			continue
		}
		maxEnd = b.End()
	}

	return maxEnd
}
func (c *blockChain) get(time time.Time) (block.Block, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, b := range c.blocks {
		if timeutil.InRange(time, b.Start(), b.End()) {
			return b, true
		}
	}

	return nil, false
}
func (c *blockChain) add(block block.Block) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.blocks[blockName(block.Start())] = block
}
func (c *blockChain) remove(before time.Time, callback func(block block.Block)) {
	c.mu.Lock()
	defer c.mu.Unlock()

	keys := make([]string, 0)
	for key, b := range c.blocks {
		if b.End().After(before) {
			continue
		}
		keys = append(keys, key)
	}

	for _, key := range keys {
		b := c.blocks[key]
		delete(c.blocks, key)
		callback(b)
	}
}

// --- Implementation code block ---

type storage struct {
	*component.Base[Config, Dependencies]
	blocks *blockChain
}

func (s *storage) Run() (err error) {
	ctx := telemetry.StartWith(s.Context(), append(s.TelemetryLabels(), telemetrymodel.KeyOperation, "Run")...)
	defer func() { telemetry.End(ctx, err) }()

	// Run blocks.
	for _, b := range s.blocks.list(nil) {
		if err := component.RunUntilReady(ctx, b, 10*time.Second); err != nil {
			return errors.Wrap(err, "run block")
		}
	}

	// Maintain blocks.
	s.MarkReady()

	ticker := clk.Timer(0)
	defer ticker.Stop()

	for {
		select {
		case now := <-ticker.C:
			if err := s.reconcileBlocks(ctx, now); err != nil {
				log.Error(ctx, errors.Wrap(err, "reconcile blocks"))

				continue
			}

			log.Debug(ctx, "reconcile blocks success")
			ticker.Reset(30 * time.Second)

		case <-ctx.Done():
			return nil
		}
	}
}

func (s *storage) Close() error {
	if err := s.Base.Close(); err != nil {
		return errors.Wrap(err, "close base")
	}
	for _, b := range s.blocks.list(nil) {
		if err := b.Close(); err != nil {
			return errors.Wrap(err, "close block")
		}
	}

	return nil
}

func (s *storage) Reload(app *config.App) error {
	// Validate new config.
	newConfig := &Config{}
	newConfig.From(app)
	if err := newConfig.Validate(); err != nil {
		return errors.Wrap(err, "validate config")
	}
	if reflect.DeepEqual(s.Config(), newConfig) {
		log.Debug(s.Context(), "no changes in feed storage config")

		return nil
	}

	// Check immutable fields.
	curConfig := s.Config()
	if newConfig.Dir != curConfig.Dir {
		return errors.New("cannot reload the dir, MUST pass the same dir, or set it to empty for unchange")
	}

	// Reload blocks.
	for _, b := range s.blocks.list(nil) {
		if err := b.Reload(&block.Config{
			FlushInterval: newConfig.FlushInterval,
		}); err != nil {
			return errors.Wrapf(err, "reload block %s", blockName(b.Start()))
		}
	}

	// Set config.
	s.SetConfig(newConfig)

	return nil
}

func (s *storage) Append(ctx context.Context, feeds ...*model.Feed) (err error) {
	ctx = telemetry.StartWith(ctx, append(s.TelemetryLabels(), telemetrymodel.KeyOperation, "Append")...)
	defer func() { telemetry.End(ctx, err) }()
	for _, f := range feeds {
		if err := f.Validate(); err != nil {
			return errors.Wrap(err, "validate feed")
		}
	}

	// Rewrite feeds.
	rewritten, err := s.rewrite(ctx, feeds)
	if err != nil {
		return errors.Wrap(err, "rewrite feeds")
	}
	if len(rewritten) == 0 {
		log.Debug(ctx, "no feeds to write after rewrites")

		return nil
	}

	// Append feeds to head block.
	log.Debug(ctx, "append feeds", "count", len(rewritten))
	if err := s.blocks.head().Append(ctx, rewritten...); err != nil {
		return errors.Wrap(err, "append feeds")
	}

	return nil
}

func (s *storage) Query(ctx context.Context, query block.QueryOptions) (feeds []*block.FeedVO, err error) {
	ctx = telemetry.StartWith(ctx, append(s.TelemetryLabels(), telemetrymodel.KeyOperation, "Query")...)
	defer func() { telemetry.End(ctx, err) }()
	if err := (&query).Validate(); err != nil {
		return nil, errors.Wrap(err, "validate query")
	}

	// Parallel read.
	blocks := s.blocks.list(nil)
	feedHeap := block.NewFeedVOHeap(make(block.FeedVOs, 0, block.EffectiveLimit(query)))
	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		errs []error
	)

	for _, b := range blocks {
		if !query.HitTimeRangeCondition(b) {
			continue
		}

		wg.Add(1)
		go func(b block.Block) {
			defer wg.Done()
			fs, err := b.Query(ctx, query)
			if err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()

				return
			}

			mu.Lock()
			for _, f := range fs {
				feedHeap.TryEvictPush(f)
			}
			mu.Unlock()
		}(b)
	}
	wg.Wait()
	if len(errs) > 0 {
		return nil, errs[0]
	}

	feedHeap.DESCSort()

	return feedHeap.Slice(), nil
}

func (s *storage) Exists(ctx context.Context, id uint64, hintTime time.Time) (bool, error) {
	// Normal path.
	if !hintTime.IsZero() {
		b, ok := s.blocks.get(hintTime)
		if ok {
			return b.Exists(ctx, id)
		}
	}

	// Fallback to head block.
	return s.blocks.head().Exists(ctx, id)
}

const headBlockCreateBuffer = 30 * time.Minute

func (s *storage) reconcileBlocks(ctx context.Context, now time.Time) error {
	// Create new head block if needed.
	if err := s.ensureHeadBlock(ctx, now); err != nil {
		return errors.Wrap(err, "ensure head block")
	}

	// Transform non-head hot blocks to cold.
	if err := s.ensureColdBlocks(ctx); err != nil {
		return errors.Wrap(err, "ensure cold blocks")
	}

	// Remove expired blocks.
	s.ensureRemovedExpiredBlocks(ctx, now)

	return nil
}

func (s *storage) ensureHeadBlock(ctx context.Context, now time.Time) error {
	if maxEnd := s.blocks.endTime(); now.After(maxEnd.Add(-headBlockCreateBuffer)) {
		nextStart := maxEnd
		if now.After(maxEnd) {
			nextStart = now
		}
		b, err := s.createBlock(nextStart)
		if err != nil {
			return errors.Wrap(err, "create new hot block")
		}
		if err := component.RunUntilReady(ctx, b, 10*time.Second); err != nil {
			return errors.Wrap(err, "run new hot block")
		}
		s.blocks.add(b)
		log.Info(ctx, "block created", "name", blockName(b.Start()))
	}

	return nil
}

func (s *storage) ensureColdBlocks(ctx context.Context) error {
	for _, b := range s.blocks.list(func(b block.Block) bool {
		return b.State() == block.StateHot &&
			!s.blocks.isHead(b) &&
			clk.Now().After(b.End().Add(s.Config().BlockDuration)) // For recent queries.
	}) {
		if err := b.TransformToCold(); err != nil {
			return errors.Wrap(err, "transform to cold")
		}
		log.Info(ctx, "block transformed to cold", "name", blockName(b.Start()))
	}

	return nil
}

func (s *storage) ensureRemovedExpiredBlocks(ctx context.Context, now time.Time) {
	s.blocks.remove(now.Add(-s.Config().Retention), func(b block.Block) {
		var err error
		if err = b.Close(); err != nil {
			log.Error(ctx, errors.Wrap(err, "close block"))
		}
		if err = b.ClearOnDisk(); err != nil {
			log.Error(ctx, errors.Wrap(err, "clear on disk"))
		}
		if err == nil {
			log.Info(ctx, "block deleted", "name", blockName(b.Start()))
		}
	})
}

var blockName = func(start time.Time) string {
	return strconv.FormatInt(start.Unix(), 10)
}

func (s *storage) createBlock(start time.Time) (block.Block, error) {
	config := s.Config()
	blockName := blockName(start)
	dir := filepath.Join(config.Dir, blockName)

	b, err := s.Dependencies().BlockFactory.New(
		blockName,
		&block.Config{
			Dir:           dir,
			FlushInterval: config.FlushInterval,
			ForCreate: &block.ForCreateConfig{
				Start:        start,
				Duration:     config.BlockDuration,
				EmbeddingLLM: config.EmbeddingLLM,
			},
		},
		s.blockDependencies(),
	)
	if err != nil {
		return nil, errors.Wrap(err, "create block")
	}

	s.blocks.add(b)

	return b, nil
}

func (s *storage) loadBlock(name string) (block.Block, error) {
	dir := filepath.Join(s.Config().Dir, name)

	b, err := s.Dependencies().BlockFactory.New(
		name,
		&block.Config{Dir: dir},
		s.blockDependencies(),
	)
	if err != nil {
		return nil, errors.Wrap(err, "create block")
	}

	s.blocks.add(b)

	return b, nil
}

func (s *storage) blockDependencies() block.Dependencies {
	deps := s.Dependencies()

	return block.Dependencies{
		ChunkFactory:    deps.ChunkFactory,
		PrimaryFactory:  deps.PrimaryFactory,
		InvertedFactory: deps.InvertedFactory,
		VectorFactory:   deps.VectorFactory,
		LLMFactory:      deps.LLMFactory,
	}
}

func (s *storage) rewrite(ctx context.Context, feeds []*model.Feed) ([]*model.Feed, error) {
	var (
		rewritten = make([]*model.Feed, 0, len(feeds))
		wg        sync.WaitGroup
		mu        sync.Mutex
		errs      []error
		dropped   atomic.Int32
	)

	for _, item := range feeds { // TODO: Limit the concurrency & goroutine number.
		wg.Add(1)
		go func(item *model.Feed) {
			defer wg.Done()
			labels, err := s.Dependencies().Rewriter.Labels(ctx, item.Labels)
			if err != nil {
				mu.Lock()
				errs = append(errs, errors.Wrap(err, "rewrite item"))
				mu.Unlock()

				return
			}
			if len(labels) == 0 {
				log.Debug(ctx, "drop feed", "id", item.ID)
				dropped.Add(1)

				return // Drop empty labels.
			}

			item.Labels = labels
			mu.Lock()
			rewritten = append(rewritten, item)
			mu.Unlock()
		}(item)
	}
	wg.Wait()

	switch len(errs) {
	case 0:
	case len(feeds) - int(dropped.Load()):
		return nil, errs[0] // All failed.
	default:
		log.Error(ctx, errors.Wrap(errs[0], "rewrite feeds"), "error_count", len(errs))
	}

	return rewritten, nil
}

type mockStorage struct {
	component.Mock
}

func (m *mockStorage) Reload(app *config.App) error {
	args := m.Called(app)

	return args.Error(0)
}

func (m *mockStorage) Append(ctx context.Context, feeds ...*model.Feed) error {
	args := m.Called(ctx, feeds)

	return args.Error(0)
}

func (m *mockStorage) Query(ctx context.Context, query block.QueryOptions) ([]*block.FeedVO, error) {
	args := m.Called(ctx, query)

	return args.Get(0).([]*block.FeedVO), args.Error(1)
}

func (m *mockStorage) Exists(ctx context.Context, id uint64, hintTime time.Time) (bool, error) {
	args := m.Called(ctx, id, hintTime)

	return args.Get(0).(bool), args.Error(1)
}
