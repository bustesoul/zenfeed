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

package block

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/benbjohnson/clock"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"k8s.io/utils/ptr"

	"github.com/glidea/zenfeed/pkg/component"
	"github.com/glidea/zenfeed/pkg/llm"
	"github.com/glidea/zenfeed/pkg/model"
	"github.com/glidea/zenfeed/pkg/storage/feed/block/chunk"
	"github.com/glidea/zenfeed/pkg/storage/feed/block/index"
	"github.com/glidea/zenfeed/pkg/storage/feed/block/index/inverted"
	"github.com/glidea/zenfeed/pkg/storage/feed/block/index/primary"
	"github.com/glidea/zenfeed/pkg/storage/feed/block/index/vector"
	"github.com/glidea/zenfeed/pkg/telemetry"
	"github.com/glidea/zenfeed/pkg/telemetry/log"
	telemetrymodel "github.com/glidea/zenfeed/pkg/telemetry/model"
	"github.com/glidea/zenfeed/pkg/util/heap"
	"github.com/glidea/zenfeed/pkg/util/retry"
	runtimeutil "github.com/glidea/zenfeed/pkg/util/runtime"
	timeutil "github.com/glidea/zenfeed/pkg/util/time"
)

var clk = clock.New()

// --- Interface code block ---
type Block interface {
	component.Component
	Reload(c *Config) error

	Start() time.Time
	End() time.Time
	State() State
	TransformToCold() error
	ClearOnDisk() error

	Append(ctx context.Context, feeds ...*model.Feed) error
	Query(ctx context.Context, query QueryOptions) ([]*FeedVO, error)
	Get(ctx context.Context, id uint64) (*FeedVO, bool, error)
	Exists(ctx context.Context, id uint64) (bool, error)
}

type Config struct {
	Dir           string
	FlushInterval time.Duration
	ForCreate     *ForCreateConfig

	// Copy from ForCreateConfig or load from disk.
	start        time.Time
	duration     time.Duration
	embeddingLLM string
}

type ForCreateConfig struct {
	Start        time.Time
	Duration     time.Duration
	EmbeddingLLM string
}

func (c *Config) Validate() error {
	if c.Dir == "" {
		return errors.New("dir is required")
	}
	if c.FlushInterval == 0 {
		c.FlushInterval = 200 * time.Millisecond
	}

	if c.ForCreate != nil {
		if err := c.validateForCreate(); err != nil {
			return errors.Wrap(err, "validate for create")
		}

	} else { // Load from disk.
		if err := c.validateForLoad(); err != nil {
			return errors.Wrap(err, "validate for load")
		}
	}

	return nil
}

func (c *Config) validateForCreate() error {
	cfc := c.ForCreate
	if cfc.Start.IsZero() {
		return errors.New("start is required")
	}
	if cfc.Duration == 0 {
		cfc.Duration = 25 * time.Hour
	}
	if cfc.Duration < timeutil.Day || cfc.Duration > 15*timeutil.Day {
		return errors.Errorf("duration must be between %s and %s", timeutil.Day, 15*timeutil.Day)
	}
	if cfc.EmbeddingLLM == "" {
		return errors.New("embedding LLM is required")
	}

	c.start = cfc.Start
	c.duration = cfc.Duration
	c.embeddingLLM = cfc.EmbeddingLLM

	return nil
}

func (c *Config) validateForLoad() error {
	b, err := os.ReadFile(filepath.Join(c.Dir, metadataFilename))
	switch {
	case err == nil:
	case os.IsNotExist(err):
		return errors.New("metadata file not found")
	default:
		return errors.Wrap(err, "reading metadata file")
	}

	var m metadata
	if err := json.Unmarshal(b, &m); err != nil {
		return errors.Wrap(err, "unmarshalling metadata")
	}

	c.start = m.Start
	c.duration = m.Duration
	c.embeddingLLM = m.EmbeddingLLM

	return nil
}

func (c *Config) end() time.Time {
	return c.start.Add(c.duration)
}

type Dependencies struct {
	ChunkFactory    chunk.Factory
	PrimaryFactory  primary.Factory
	InvertedFactory inverted.Factory
	VectorFactory   vector.Factory
	LLMFactory      llm.Factory
}

var (
	states    = []string{string(StateHot), string(StateCold)}
	blockInfo = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: model.AppName,
			Subsystem: "block",
			Name:      "info",
			Help:      "Block info.",
		},
		[]string{telemetrymodel.KeyComponent, telemetrymodel.KeyComponentInstance, "state", "dir"},
	)
)

// Directory structure (Interface on disk)
//
// .data
// * cold-block-name-1
// *** archive.json // archive metadata, also indicates that this is a cold block
// *** metadata.json // metadata
// *** index
// ***** primary
// ***** inverted
// ***** vector
// *** chunk
// ***** 1
// ***** 2
// ***** ...
//
// * hot-block-name-1
// *** metadata.json // metadata
// *** chunk // If there is only a chunk directory, it means it is a hot block
// ***** 1
// ***** 2
// ***** ...
const (
	archiveMetaFilename   = "archive.json"
	metadataFilename      = "metadata.json"
	chunkDirname          = "chunk"
	indexDirname          = "index"
	indexPrimaryFilename  = "primary"
	indexInvertedFilename = "inverted"
	indexVectorFilename   = "vector"

	// estimatedChunkFeedsLimit is the estimated maximum number of feeds in a chunk.
	// Due to lock-free concurrency, the actual value may be larger.
	estimatedChunkFeedsLimit = 5000
)

// archiveMetadata is the metadata of the cold block.
type archiveMetadata struct {
	FeedCount uint32 `json:"feed_count"`
}

type metadata struct {
	Start        time.Time     `json:"start"`
	Duration     time.Duration `json:"duration"`
	EmbeddingLLM string        `json:"embedding_llm"`
}

var (
	chunkFilename = func(chunk uint32) string {
		return strconv.FormatUint(uint64(chunk), 10)
	}

	parseChunkFilename = func(name string) (chunk uint32, err error) {
		chunk64, err := strconv.ParseUint(name, 10, 32)
		if err != nil {
			return 0, errors.Wrap(err, "invalid chunk filename format")
		}

		return uint32(chunk64), nil
	}
)

// State is the state of the block.
type State string

const (
	// StateHot means the block is writable.
	// ALL data is in memory.
	// It is the head of the block chain.
	StateHot State = "hot"
	// StateCold for read only.
	// It has indexs on disk.
	StateCold State = "cold"
)

// FeedVO is the feed view for query result.
type FeedVO struct {
	*model.Feed        `json:",inline"`
	Vectors            [][]float32 `json:"-"`
	Score              float32     `json:"score,omitempty"`               // Only exists when SemanticFilter is set.
	MatchedPreferences []string    `json:"matched_preferences,omitempty"` // Tags that matched user preferences.
}

type FeedVOs []*FeedVO

func NewFeedVOHeap(feeds []*FeedVO) *heap.Heap[*FeedVO] {
	return heap.New(feeds, func(a, b *FeedVO) bool {
		if a.Score == b.Score {
			return a.Time.Before(b.Time)
		}

		return a.Score < b.Score
	})
}

type QueryOptions struct {
	Query        string
	Threshold    float32
	LabelFilters []string
	labelFilters model.LabelFilters
	Limit        int
	RecallLimit  int // if > Limit, use RecallLimit for heap size then re-rank
	Start, End   time.Time
}

func (q *QueryOptions) Validate() error { //nolint:cyclop,gocognit
	if q.Threshold < 0 || q.Threshold > 1 {
		return errors.New("threshold must be between 0 and 1")
	}
	for _, s := range q.LabelFilters {
		if s == "" {
			return errors.New("label filter is required")
		}
		filter, err := model.NewLabelFilter(s)
		if err != nil {
			return errors.Wrap(err, "parse label filter")
		}
		q.labelFilters = append(q.labelFilters, filter)
	}
	if q.Threshold == 0 {
		q.Threshold = 0.55
	}
	if q.Limit <= 0 {
		q.Limit = 10
	}
	if q.Limit > 500 {
		return errors.New("limit must be less than or equal to 500")
	}
	if q.RecallLimit > 0 && q.RecallLimit < q.Limit {
		q.RecallLimit = q.Limit
	}
	if q.RecallLimit > 2500 {
		q.RecallLimit = 2500
	}
	if q.Start.IsZero() {
		q.Start = time.Now().Add(-time.Hour * 24)
	}
	if q.End.IsZero() {
		q.End = time.Now()
	}
	if q.End.Before(q.Start) {
		return errors.New("end time must be after start time")
	}

	return nil
}

func (q *QueryOptions) HitTimeRangeCondition(b Block) bool {
	bStart, bEnd := b.Start(), b.End()
	qStart, qEnd := q.Start, q.End
	if qStart.IsZero() && qEnd.IsZero() {
		return true
	}
	if qStart.IsZero() {
		return bStart.Before(qEnd)
	}
	if qEnd.IsZero() {
		return !bEnd.Before(qStart)
	}

	in := func(t time.Time, s, e time.Time) bool { // [start, end)
		return !t.Before(s) && t.Before(e)
	}

	queryAsBase := in(bStart, qStart, qEnd) || in(bEnd, qStart, qEnd)
	blockAsBase := in(qStart, bStart, bEnd) || in(qEnd, bStart, bEnd)

	return queryAsBase || blockAsBase
}

// EffectiveLimit returns RecallLimit if it is greater than Limit, otherwise Limit.
func EffectiveLimit(q QueryOptions) int {
	if q.RecallLimit > q.Limit {
		return q.RecallLimit
	}

	return q.Limit
}

// effectiveLimit is the unexported alias used internally.
func effectiveLimit(q QueryOptions) int { return EffectiveLimit(q) }

// --- Factory code block ---
type Factory component.Factory[Block, Config, Dependencies]

func NewFactory(mockOn ...component.MockOption) Factory {
	if len(mockOn) > 0 {
		return component.FactoryFunc[Block, Config, Dependencies](
			func(instance string, config *Config, dependencies Dependencies) (Block, error) {
				m := &mockBlock{}
				component.MockOptions(mockOn).Apply(&m.Mock)

				return m, nil
			},
		)
	}

	return component.FactoryFunc[Block, Config, Dependencies](new)
}

func new(instance string, config *Config, dependencies Dependencies) (Block, error) {
	if err := config.Validate(); err != nil {
		return nil, errors.Wrap(err, "validate config")
	}

	// New block.
	block, err := newBlock(instance, config, dependencies)
	if err != nil {
		return nil, errors.Wrap(err, "creating block")
	}

	// Init block.
	if createMode := config.ForCreate != nil; createMode {
		return initBlock(block)
	}

	// Load mode.
	state, err := block.loadState()
	if err != nil {
		return nil, errors.Wrap(err, "checking hot on disk")
	}
	block.state.Store(state)

	// Load hot block data from disk.
	// For cold block, it will be loaded when the query is called.
	if state == StateHot {
		if err := block.load(context.Background(), nil); err != nil {
			return nil, errors.Wrap(err, "decoding from disk")
		}
	}

	return block, nil
}

func newBlock(instance string, config *Config, dependencies Dependencies) (*block, error) {
	primaryIndex, vectorIndex, invertedIndex, err := newIndexs(instance, dependencies)
	if err != nil {
		return nil, errors.Wrap(err, "creating indexs")
	}

	block := &block{
		Base: component.New(&component.BaseConfig[Config, Dependencies]{
			Name:         "FeedBlock",
			Instance:     instance,
			Config:       config,
			Dependencies: dependencies,
		}),
		primaryIndex:  primaryIndex,
		vectorIndex:   vectorIndex,
		invertedIndex: invertedIndex,
		toWrite:       make(chan []*chunk.Feed, 1024),
		chunks:        make(chunkChain, 0, 2),
	}
	block.lastDataAccess.Store(clk.Now())

	return block, nil
}

func newIndexs(instance string, dependencies Dependencies) (primary.Index, vector.Index, inverted.Index, error) {
	primaryIndex, err := dependencies.PrimaryFactory.New(instance, &primary.Config{}, primary.Dependencies{})
	if err != nil {
		return nil, nil, nil, errors.Wrap(err, "creating primary index")
	}
	vectorIndex, err := dependencies.VectorFactory.New(instance, &vector.Config{}, vector.Dependencies{})
	if err != nil {
		return nil, nil, nil, errors.Wrap(err, "creating vector index")
	}
	invertedIndex, err := dependencies.InvertedFactory.New(instance, &inverted.Config{}, inverted.Dependencies{})
	if err != nil {
		return nil, nil, nil, errors.Wrap(err, "creating inverted index")
	}

	return primaryIndex, vectorIndex, invertedIndex, nil
}

func initBlock(block *block) (Block, error) {
	// Create metadata file.
	if err := os.MkdirAll(block.Config().Dir, 0700); err != nil {
		return nil, errors.Wrap(err, "creating block directory")
	}
	metadata := metadata{
		Start:        block.Config().start,
		Duration:     block.Config().duration,
		EmbeddingLLM: block.Config().embeddingLLM,
	}
	b := runtimeutil.Must1(json.Marshal(metadata))
	p := filepath.Join(block.Config().Dir, metadataFilename)
	if err := os.WriteFile(p, b, 0600); err != nil {
		return nil, errors.Wrap(err, "creating metadata file")
	}

	// Create head chunk.
	if err := os.MkdirAll(filepath.Join(block.Config().Dir, chunkDirname), 0700); err != nil {
		return nil, errors.Wrap(err, "creating chunk directory")
	}

	id := uint32(0)
	chunk, err := block.Dependencies().ChunkFactory.New(
		fmt.Sprintf("%s-%d", block.Instance(), id),
		&chunk.Config{Path: filepath.Join(block.Config().Dir, chunkDirname, chunkFilename(id))},
		chunk.Dependencies{},
	)
	if err != nil {
		return nil, errors.Wrap(err, "creating head chunk file")
	}
	block.chunks = append(block.chunks, chunk)

	// Set state to hot.
	block.state.Store(StateHot)

	return block, nil
}

// --- Implementation code block ---
type block struct {
	*component.Base[Config, Dependencies]

	primaryIndex  primary.Index
	vectorIndex   vector.Index
	invertedIndex inverted.Index

	toWrite chan []*chunk.Feed
	chunks  chunkChain
	mu      sync.RWMutex

	lastDataAccess atomic.Value
	state          atomic.Value
	coldLoaded     bool
}

func (b *block) Run() (err error) {
	ctx := telemetry.StartWith(b.Context(), append(b.TelemetryLabels(), telemetrymodel.KeyOperation, "Run")...)
	defer func() { telemetry.End(ctx, err) }()

	// Maintain metrics.
	go b.maintainMetrics(ctx)

	// Run writing worker.
	go b.runWritingWorker(ctx)

	// Run indexs.
	if err := component.RunUntilReady(ctx, b.primaryIndex, 10*time.Second); err != nil {
		return errors.Wrap(err, "running primary index")
	}
	if err := component.RunUntilReady(ctx, b.vectorIndex, 10*time.Second); err != nil {
		return errors.Wrap(err, "running vector index")
	}
	if err := component.RunUntilReady(ctx, b.invertedIndex, 10*time.Second); err != nil {
		return errors.Wrap(err, "running inverted index")
	}

	// Run chunks.
	for _, c := range b.chunks {
		if err := component.RunUntilReady(ctx, c, 10*time.Second); err != nil {
			return errors.Wrap(err, "running chunk")
		}
	}

	b.MarkReady()

	tick := clk.Ticker(30 * time.Second)
	defer tick.Stop()

	for {
	NEXT_SELECT:
		select {
		case now := <-tick.C:
			if err := b.reconcileState(ctx, now); err != nil {
				log.Error(b.Context(), errors.Wrap(err, "reconciling state"))

				break NEXT_SELECT
			}

		case <-b.Context().Done():
			return nil
		}
	}
}

func (b *block) Close() error {
	if err := b.Base.Close(); err != nil {
		return errors.Wrap(err, "closing base")
	}

	// Remove Metrics.
	blockInfo.DeletePartialMatch(b.TelemetryLabelsID())

	// Close indexs.
	if err := b.primaryIndex.Close(); err != nil {
		return errors.Wrap(err, "closing primary index")
	}
	if err := b.vectorIndex.Close(); err != nil {
		return errors.Wrap(err, "closing vector index")
	}
	if err := b.invertedIndex.Close(); err != nil {
		return errors.Wrap(err, "closing inverted index")
	}

	// Close chunks.
	for _, c := range b.chunks {
		if err := c.Close(); err != nil {
			return errors.Wrap(err, "closing chunk")
		}
	}

	// Reset.
	if err := b.resetMem(); err != nil {
		return errors.Wrap(err, "resetting memory")
	}

	return nil
}

func (b *block) Reload(c *Config) error {
	currentConfig := b.Config()
	if c.ForCreate != nil {
		return errors.New("cannot reload the for create config")
	}
	if c.Dir != "" && c.Dir != currentConfig.Dir {
		return errors.New("cannot reload the dir, MUST pass the same dir, or set it to empty for unchange")
	}
	if c.Dir == "" {
		c.Dir = currentConfig.Dir
	}
	if c.FlushInterval == 0 {
		c.FlushInterval = currentConfig.FlushInterval
	}
	if c.ForCreate == nil {
		c.ForCreate = currentConfig.ForCreate
	}

	// Validate the config.
	if err := c.Validate(); err != nil {
		return errors.Wrap(err, "validate config")
	}

	// Set new config.
	b.SetConfig(c)

	return nil
}

func (b *block) Append(ctx context.Context, feeds ...*model.Feed) (err error) {
	ctx = telemetry.StartWith(ctx, append(b.TelemetryLabels(), telemetrymodel.KeyOperation, "Append")...)
	defer func() { telemetry.End(ctx, err) }()
	if b.State() != StateHot {
		return errors.New("block is not writable")
	}
	b.lastDataAccess.Store(clk.Now())

	embedded, err := b.fillEmbedding(ctx, feeds)
	if err != nil {
		return errors.Wrap(err, "fill embedding")
	}

	b.toWrite <- embedded

	return nil
}

func (b *block) Query(ctx context.Context, query QueryOptions) (feeds []*FeedVO, err error) {
	ctx = telemetry.StartWith(ctx, append(b.TelemetryLabels(), telemetrymodel.KeyOperation, "Query")...)
	defer func() { telemetry.End(ctx, err) }()
	if err := (&query).Validate(); err != nil {
		return nil, errors.Wrap(err, "validate query")
	}
	b.lastDataAccess.Store(clk.Now())

	// Ensure the block is loaded.
	if err := b.ensureLoaded(ctx); err != nil {
		return nil, errors.Wrap(err, "ensuring block loaded")
	}

	// Apply filters.
	filterResult, err := b.applyFilters(ctx, &query)
	if err != nil {
		return nil, errors.Wrap(err, "applying filters")
	}
	if isMatchedNothingFilterResult(filterResult) {
		return []*FeedVO{}, nil
	}

	// Read feeds.
	b.mu.RLock()
	chunks := b.chunks
	b.mu.RUnlock()

	result := NewFeedVOHeap(make(FeedVOs, 0, effectiveLimit(query)))
	if err := filterResult.forEach(ctx, b.primaryIndex, func(ref primary.FeedRef, score float32) error {
		if ref.Time.Before(query.Start) || !ref.Time.Before(query.End) {
			return nil
		}

		ck := chunks[ref.Chunk]
		if ck == nil {
			log.Error(ctx, errors.Errorf("chunk file not found, data may be corrupted: %d", ref.Chunk))

			return nil
		}

		feed, err := ck.Read(ctx, ref.Offset)
		if err != nil {
			log.Error(ctx, errors.Wrapf(err, "reading chunk file: %d", ref.Chunk))

			return nil
		}

		result.TryEvictPush(&FeedVO{
			Feed:    feed.Feed,
			Vectors: feed.Vectors,
			Score:   score,
		})

		return nil

	}); err != nil {
		return nil, errors.Wrap(err, "iterating filter result")
	}

	return result.Slice(), nil
}

func (b *block) Get(ctx context.Context, id uint64) (feedVO *FeedVO, ok bool, err error) {
	ctx = telemetry.StartWith(ctx, append(b.TelemetryLabels(), telemetrymodel.KeyOperation, "Get")...)
	defer func() { telemetry.End(ctx, err) }()
	b.lastDataAccess.Store(clk.Now())

	if err := b.ensureLoaded(ctx); err != nil {
		return nil, false, errors.Wrap(err, "ensuring block loaded")
	}

	ref, ok := b.primaryIndex.Search(ctx, id)
	if !ok {
		return nil, false, nil
	}

	b.mu.RLock()
	ck := b.chunks[ref.Chunk]
	b.mu.RUnlock()
	if ck == nil {
		return nil, false, errors.Errorf("chunk file not found, data may be corrupted: %d", ref.Chunk)
	}

	feed, err := ck.Read(ctx, ref.Offset)
	if err != nil {
		return nil, false, errors.Wrapf(err, "reading chunk file: %d", ref.Chunk)
	}

	return &FeedVO{
		Feed:    feed.Feed,
		Vectors: feed.Vectors,
	}, true, nil
}

func (b *block) Exists(ctx context.Context, id uint64) (exists bool, err error) {
	ctx = telemetry.StartWith(ctx, append(b.TelemetryLabels(), telemetrymodel.KeyOperation, "Exists")...)
	defer func() { telemetry.End(ctx, err) }()

	// Ensure the block is loaded.
	if err := b.ensureLoaded(ctx); err != nil {
		return false, errors.Wrap(err, "ensuring block loaded")
	}

	// Search the primary index.
	_, ok := b.primaryIndex.Search(ctx, id)

	return ok, nil
}

func (b *block) TransformToCold() (err error) {
	ctx := telemetry.StartWith(b.Context(), append(b.TelemetryLabels(), telemetrymodel.KeyOperation, "TransformToCold")...)
	defer func() { telemetry.End(ctx, err) }()

	// Dump meta and indexes to disk.
	config := b.Config()
	indexDirpath := filepath.Join(config.Dir, indexDirname)
	if err := b.writeIndexFile(ctx, filepath.Join(indexDirpath, indexPrimaryFilename), b.primaryIndex); err != nil {
		return errors.Wrap(err, "writing primary index")
	}
	if err := b.writeIndexFile(ctx, filepath.Join(indexDirpath, indexInvertedFilename), b.invertedIndex); err != nil {
		return errors.Wrap(err, "writing inverted index")
	}
	if err := b.writeIndexFile(ctx, filepath.Join(indexDirpath, indexVectorFilename), b.vectorIndex); err != nil {
		return errors.Wrap(err, "writing vector index")
	}
	bs := runtimeutil.Must1(json.Marshal(archiveMetadata{
		FeedCount: b.primaryIndex.Count(ctx),
	}))
	if err := os.WriteFile(filepath.Join(config.Dir, archiveMetaFilename), bs, 0600); err != nil {
		return errors.Wrap(err, "writing archive metadata")
	}

	// Reset memory.
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.resetMem(); err != nil {
		return errors.Wrap(err, "resetting memory")
	}

	b.state.Store(StateCold)

	return nil
}

func (b *block) ClearOnDisk() error {
	return os.RemoveAll(b.Config().Dir)
}

func (b *block) Start() time.Time {
	return b.Config().start
}

func (b *block) End() time.Time {
	return b.Config().end()
}

func (b *block) State() State {
	return b.state.Load().(State)
}

func (b *block) ensureLoaded(ctx context.Context) error {
	if b.State() != StateCold {
		return nil
	}

	b.mu.RLock()
	loaded := b.coldLoaded
	b.mu.RUnlock()
	if loaded {
		return nil
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.coldLoaded {
		return nil
	}

	if err := b.load(ctx, func(ck chunk.File) error {
		return component.RunUntilReady(ctx, ck, 10*time.Second)
	}); err != nil {
		return errors.Wrap(err, "decoding from disk")
	}
	b.coldLoaded = true

	log.Info(ctx, "cold block loaded")

	return nil
}

func (b *block) maintainMetrics(ctx context.Context) {
	_ = timeutil.Tick(ctx, 30*time.Second, func() error {
		var (
			dir   = b.Config().Dir
			state = string(b.State())
		)
		blockInfo.WithLabelValues(append(b.TelemetryLabelsIDFields(), state, dir)...)
		for _, s := range states {
			if s == state {
				continue
			}
			blockInfo.DeleteLabelValues(append(b.TelemetryLabelsIDFields(), s, dir)...)
		}

		return nil
	})
}

func (b *block) runWritingWorker(ctx context.Context) {
	var concurrency = runtime.NumCPU() * 4 // I/O bound.
	var wg sync.WaitGroup
	wg.Add(concurrency)
	for i := range concurrency {
		go func(i int) {
			defer wg.Done()
			workerCtx := telemetry.StartWith(ctx,
				append(b.TelemetryLabels(), "worker", i, telemetrymodel.KeyOperation, "Run")...,
			)
			defer func() { telemetry.End(workerCtx, nil) }()

			flushInterval := b.Config().FlushInterval
			tick := time.NewTimer(flushInterval)
			defer tick.Stop()

			buffer := make([]*chunk.Feed, 0)
			for {
				select {
				case <-workerCtx.Done():
					gracefulCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					eof := b.flush(gracefulCtx, buffer)
					for !eof {
						eof = b.flush(gracefulCtx, buffer)
					}

					return
				case <-tick.C:
					_ = b.flush(workerCtx, buffer)

					flushInterval = b.Config().FlushInterval
					tick.Reset(flushInterval)
				}
			}
		}(i)
	}
	wg.Wait()
}

func (b *block) flush(ctx context.Context, buffer []*chunk.Feed) (eof bool) {
	const maxBatch = 1000
	buffer = buffer[:0]

OUTER:
	for {
		select {
		case feeds := <-b.toWrite:
			buffer = append(buffer, feeds...)
			if len(buffer) >= maxBatch {
				eof = false

				break OUTER
			}

		default:
			eof = true

			break OUTER
		}
	}
	if len(buffer) == 0 {
		return eof
	}

	// Append feeds.
	if err := retry.Backoff(ctx, func() error {
		return b.append(ctx, buffer)
	}, &retry.Options{
		MinInterval: 100 * time.Millisecond,
		MaxAttempts: ptr.To(3),
	}); err != nil {
		log.Error(ctx, errors.Wrap(err, "append feeds"))
	}

	return eof
}

func (b *block) append(ctx context.Context, feeds []*chunk.Feed) (err error) {
	ctx = telemetry.StartWith(ctx, append(b.TelemetryLabels(), telemetrymodel.KeyOperation, "append")...)
	defer func() { telemetry.End(ctx, err) }()

	// Ensure the head chunk's space is enough.
	b.mu.RLock()
	headChunkID, headChunk := b.chunks.head()
	needGrow := headChunk.Count(ctx)+uint32(len(feeds)) > estimatedChunkFeedsLimit
	b.mu.RUnlock()
	if needGrow {
		b.mu.Lock()
		headChunkID, headChunk = b.chunks.head()
		needGrow = headChunk.Count(ctx)+uint32(len(feeds)) > estimatedChunkFeedsLimit
		if needGrow {
			if err := b.nextChunk(); err != nil {
				b.mu.Unlock()

				return errors.Wrap(err, "creating new chunk")
			}
			headChunkID, headChunk = b.chunks.head()
		}
		b.mu.Unlock()
	}

	// Write to head chunk.
	if err := headChunk.Append(ctx, feeds, func(feed *chunk.Feed, offset uint64) error {
		b.primaryIndex.Add(ctx, feed.ID, primary.FeedRef{ // Write primary first, for query.
			Chunk:  headChunkID,
			Offset: offset,
			Time:   feed.Time,
		})
		b.invertedIndex.Add(ctx, feed.ID, feed.Labels)
		if len(feed.Vectors) > 0 {
			if err := b.vectorIndex.Add(ctx, feed.ID, feed.Vectors); err != nil {
				return errors.Wrap(err, "adding to vector index")
			}
		}

		return nil
	}); err != nil {
		return errors.Wrap(err, "writing to head chunk")
	}

	return nil
}

const coldingWindow = 30 * time.Minute

func (b *block) reconcileState(ctx context.Context, now time.Time) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.coldLoaded && now.Add(-coldingWindow).After(b.lastDataAccess.Load().(time.Time)) {
		if err := b.resetMem(); err != nil {
			return errors.Wrap(err, "resetting memory")
		}

		b.coldLoaded = false
		log.Info(ctx, "block is archived")
	}

	return nil
}

func (b *block) nextChunk() (err error) {
	ctx := telemetry.StartWith(b.Context(), append(b.TelemetryLabels(), telemetrymodel.KeyOperation, "nextChunk")...)
	defer func() { telemetry.End(ctx, err) }()
	oldHeadID, oldHead := b.chunks.head()

	id := oldHeadID + 1
	chunk, err := b.Dependencies().ChunkFactory.New(
		fmt.Sprintf("%s-%d", b.Instance(), id),
		&chunk.Config{
			Path: filepath.Join(b.Config().Dir, chunkDirname, chunkFilename(id)),
		},
		chunk.Dependencies{},
	)
	if err != nil {
		return errors.Wrap(err, "creating new chunk")
	}
	if err := component.RunUntilReady(ctx, chunk, 10*time.Second); err != nil {
		return errors.Wrap(err, "running chunk")
	}
	b.chunks = append(b.chunks, chunk)

	if err := oldHead.EnsureReadonly(ctx); err != nil {
		return errors.Wrap(err, "ensuring old chunk readonly")
	}

	log.Info(ctx, "new chunk created", "chunk", id)

	return nil
}

func (b *block) loadState() (State, error) {
	_, err := os.Stat(filepath.Join(b.Config().Dir, archiveMetaFilename))
	switch {
	case err == nil:
		return StateCold, nil
	case os.IsNotExist(err):
		return StateHot, nil
	default:
		return StateCold, errors.Wrap(err, "checking meta file")
	}
}

func (b *block) load(ctx context.Context, callback func(chunk chunk.File) error) (err error) {
	ctx = telemetry.StartWith(ctx, append(b.TelemetryLabels(), telemetrymodel.KeyOperation, "load", "state", b.State())...)
	defer func() { telemetry.End(ctx, err) }()
	cold := b.State() == StateCold

	// List all chunks.
	if err := b.loadChunks(cold, callback); err != nil {
		return errors.Wrap(err, "loading chunks")
	}

	// Load indexs.
	b.primaryIndex, b.vectorIndex, b.invertedIndex, err = newIndexs(b.Instance(), b.Dependencies())
	if err != nil {
		return errors.Wrap(err, "creating indexs")
	}
	if cold {
		if err := b.loadIndexs(ctx); err != nil {
			return errors.Wrap(err, "loading index files")
		}
	} else {
		for id, ck := range b.chunks {
			if err := b.replayIndex(ctx, uint32(id), ck); err != nil {
				return errors.Wrapf(err, "replaying index for chunk %d", id)
			}
		}
	}

	return nil
}

func (b *block) loadChunks(cold bool, callback func(chunk chunk.File) error) error {
	// List all chunk ids.
	ids, err := b.loadChunkIDs()
	if err != nil {
		return errors.Wrap(err, "loading chunk IDs")
	}

	// Decode each chunk file.
	for _, id := range ids {
		p := filepath.Join(b.Config().Dir, chunkDirname, chunkFilename(id))
		ck, err := b.Dependencies().ChunkFactory.New(
			fmt.Sprintf("%s-%d", b.Instance(), id),
			&chunk.Config{
				Path:            p,
				ReadonlyAtFirst: cold,
			},
			chunk.Dependencies{},
		)
		if err != nil {
			return errors.Wrapf(err, "creating chunk file %s", p)
		}

		if callback != nil {
			if err := callback(ck); err != nil {
				return errors.Wrapf(err, "running callback for chunk %d", id)
			}
		}

		b.chunks = append(b.chunks, ck)
	}

	return nil
}

func (b *block) loadChunkIDs() ([]uint32, error) {
	chunkInfos, err := os.ReadDir(filepath.Join(b.Config().Dir, chunkDirname))
	if err != nil {
		return nil, errors.Wrap(err, "reading chunk directory")
	}
	if len(chunkInfos) == 0 {
		return nil, nil
	}
	chunkIDs := make([]uint32, 0, len(chunkInfos))
	for _, info := range chunkInfos {
		if info.IsDir() {
			continue
		}
		chunkID, err := parseChunkFilename(info.Name())
		if err != nil {
			return nil, errors.Wrap(err, "converting chunk file name to int")
		}
		chunkIDs = append(chunkIDs, chunkID)
	}

	hasGap := false

	slices.Sort(chunkIDs)
	prevID := chunkIDs[0]
	for _, id := range chunkIDs[1:] {
		if id != prevID+1 {
			hasGap = true

			break
		}
		prevID = id
	}

	if hasGap { // TODO: may be tolerant and fix it.
		return nil, errors.New("chunk IDs are not continuous, data may be corrupted")
	}

	return chunkIDs, nil
}

func (b *block) loadIndexs(ctx context.Context) error {
	indexDirpath := filepath.Join(b.Config().Dir, indexDirname)
	if err := b.decodeIndexFile(ctx, filepath.Join(indexDirpath, indexPrimaryFilename), b.primaryIndex); err != nil {
		return errors.Wrap(err, "decoding primary index")
	}
	if err := b.decodeIndexFile(ctx, filepath.Join(indexDirpath, indexInvertedFilename), b.invertedIndex); err != nil {
		return errors.Wrap(err, "decoding inverted index")
	}
	if err := b.decodeIndexFile(ctx, filepath.Join(indexDirpath, indexVectorFilename), b.vectorIndex); err != nil {
		return errors.Wrap(err, "decoding vector index")
	}
	if err := component.RunUntilReady(ctx, b.primaryIndex, 10*time.Second); err != nil {
		return errors.Wrap(err, "running primary index")
	}
	if err := component.RunUntilReady(ctx, b.invertedIndex, 10*time.Second); err != nil {
		return errors.Wrap(err, "running inverted index")
	}
	if err := component.RunUntilReady(ctx, b.vectorIndex, 10*time.Second); err != nil {
		return errors.Wrap(err, "running vector index")
	}

	return nil
}

func (b *block) decodeIndexFile(ctx context.Context, path string, idx index.Codec) error {
	f, err := os.Open(path)
	if err != nil {
		return errors.Wrap(err, "opening index file")
	}
	defer func() { _ = f.Close() }()

	buf := bufio.NewReaderSize(f, 1*1024*1024)
	defer buf.Reset(nil)
	if err := idx.DecodeFrom(ctx, buf); err != nil {
		return errors.Wrap(err, "decoding index")
	}

	return nil
}

func (b *block) replayIndex(ctx context.Context, id uint32, ck chunk.File) error {
	if err := ck.Range(ctx, func(feed *chunk.Feed, offset uint64) error {
		b.primaryIndex.Add(ctx, feed.ID, primary.FeedRef{
			Chunk:  id,
			Offset: offset,
			Time:   feed.Time,
		})
		b.invertedIndex.Add(ctx, feed.ID, feed.Labels)
		if len(feed.Vectors) > 0 {
			if err := b.vectorIndex.Add(ctx, feed.ID, feed.Vectors); err != nil {
				return errors.Wrap(err, "adding to vector index")
			}
		}

		return nil
	}); err != nil {
		return errors.Wrapf(err, "replaying index for chunk %d", id)
	}

	return nil
}

func (b *block) resetMem() error {
	if err := b.closeFiles(); err != nil {
		return errors.Wrap(err, "closing files")
	}

	b.primaryIndex, b.vectorIndex, b.invertedIndex = nil, nil, nil
	b.chunks = nil

	return nil
}

func (b *block) closeFiles() error {
	if err := b.primaryIndex.Close(); err != nil {
		return errors.Wrap(err, "closing primary index")
	}
	if err := b.vectorIndex.Close(); err != nil {
		return errors.Wrap(err, "closing vector index")
	}
	if err := b.invertedIndex.Close(); err != nil {
		return errors.Wrap(err, "closing inverted index")
	}
	for _, c := range b.chunks {
		if err := c.Close(); err != nil {
			return errors.Wrap(err, "closing chunk")
		}
	}

	return nil
}

func (b *block) applyFilters(ctx context.Context, query *QueryOptions) (res filterResult, err error) {
	// Apply label filters.
	labelsResult := b.applyLabelFilters(ctx, query.labelFilters)
	if isMatchedNothingFilterResult(labelsResult) {
		return matchedNothingFilterResult, nil
	}

	// Apply vector filter.
	vectorsResult, err := b.applyVectorFilter(ctx, query.Query, query.Threshold, effectiveLimit(*query))
	if err != nil {
		return nil, errors.Wrap(err, "applying vector filter")
	}
	if isMatchedNothingFilterResult(vectorsResult) {
		return matchedNothingFilterResult, nil
	}

	// Merge filter results and prepare for reading.
	return b.mergeFilterResults(labelsResult, vectorsResult), nil
}

func (b *block) applyLabelFilters(ctx context.Context, filters model.LabelFilters) filterResult {
	if len(filters) == 0 {
		return matchedAllFilterResult
	}

	var allIDs map[uint64]struct{}
	for _, filter := range filters {
		ids := b.invertedIndex.Search(ctx, filter)
		if len(ids) == 0 {
			return matchedNothingFilterResult
		}

		// initialize allIDs
		if allIDs == nil {
			allIDs = ids

			continue
		}

		// merge AND results
		for id := range allIDs {
			if _, ok := ids[id]; !ok {
				delete(allIDs, id)
			}
		}
		if len(allIDs) == 0 {
			return matchedNothingFilterResult
		}
	}

	// convert to filterResult
	result := make(filterResult, len(allIDs))
	for id := range allIDs {
		result[id] = 0
	}

	return result
}

func (b *block) applyVectorFilter(
	ctx context.Context,
	query string,
	threshold float32,
	limit int,
) (filterResult, error) {
	if query == "" {
		return matchedAllFilterResult, nil
	}
	llm := b.Dependencies().LLMFactory.Get(b.Config().embeddingLLM)

	queryVector, err := llm.Embedding(ctx, query)
	if err != nil {
		return nil, errors.Wrap(err, "embed query")
	}

	ids, err := b.vectorIndex.Search(ctx, queryVector, threshold, limit)
	if err != nil {
		return nil, errors.Wrap(err, "applying vector filter")
	}

	return ids, nil
}

func (b *block) mergeFilterResults(x, y filterResult) filterResult {
	switch {
	case len(x) > 0 && len(y) > 0:
		// estimate capacity as the smaller of the two maps
		result := make(filterResult, int(math.Min(float64(len(x)), float64(len(y)))))
		for id := range y {
			if _, ok := x[id]; ok {
				result[id] = y[id]
			}
		}

		return result
	case len(x) > 0:
		return x
	case len(y) > 0:
		return y
	default:
		if isMatchedNothingFilterResult(x) || isMatchedNothingFilterResult(y) {
			return matchedNothingFilterResult
		}

		return matchedAllFilterResult
	}
}

func (b *block) fillEmbedding(ctx context.Context, feeds []*model.Feed) ([]*chunk.Feed, error) {
	embedded := make([]*chunk.Feed, 0, len(feeds))
	llm := b.Dependencies().LLMFactory.Get(b.Config().embeddingLLM)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error
	for i, feed := range feeds {
		wg.Add(1)
		go func(i int, feed *model.Feed) { // TODO: limit go routines.
			defer wg.Done()
			vectors, err := llm.EmbeddingLabels(ctx, feed.Labels)
			if err != nil {
				mu.Lock()
				errs = append(errs, errors.Wrap(err, "fill embedding"))
				mu.Unlock()

				return
			}

			mu.Lock()
			embedded = append(embedded, &chunk.Feed{
				Feed:    feed,
				Vectors: vectors,
			})
			mu.Unlock()
		}(i, feed)
	}
	wg.Wait()

	switch len(errs) {
	case 0:
	case len(feeds):
		return nil, errs[0] // All failed.
	default:
		log.Error(ctx, errors.Wrap(errs[0], "fill embedding"), "error_count", len(errs))
	}

	return embedded, nil
}

func (b *block) writeIndexFile(ctx context.Context, path string, idx index.Codec) error {
	// Ensure index directory.
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return errors.Wrap(err, "ensure index directory")
	}

	// Create index file.
	f, err := os.Create(path)
	if err != nil {
		return errors.Wrap(err, "creating index file")
	}
	defer func() { _ = f.Close() }()

	// Encode index.
	w := bufio.NewWriterSize(f, 4*1024)
	defer func() { _ = w.Flush() }()
	if err := idx.EncodeTo(ctx, w); err != nil {
		return errors.Wrap(err, "encoding index")
	}

	return nil
}

type chunkChain []chunk.File

func (c chunkChain) head() (uint32, chunk.File) {
	return uint32(len(c) - 1), c[len(c)-1]
}

// filterResult is the result of a filter, id -> score.
// If the filter is not a vector filter, the score is 0.
type filterResult map[uint64]float32

var (
	matchedAllFilterResult   filterResult = nil
	isMatchedAllFilterResult              = func(r filterResult) bool {
		return r == nil
	}
	matchedNothingFilterResult   filterResult = map[uint64]float32{}
	isMatchedNothingFilterResult              = func(r filterResult) bool {
		return r != nil && len(r) == 0
	}
)

func (r filterResult) forEach(
	ctx context.Context,
	primaryIndex primary.Index,
	iter func(ref primary.FeedRef, score float32) error,
) error {
	realFilterResult := r
	if isMatchedAllFilterResult(r) {
		ids := primaryIndex.IDs(ctx)
		realFilterResult = make(filterResult, len(ids))
		for id := range ids {
			realFilterResult[id] = 0
		}
	}

	for id, score := range realFilterResult {
		ref, ok := primaryIndex.Search(ctx, id)
		if !ok {
			log.Error(ctx, errors.Errorf("feed: %d not found in primary index via other index, may be BUG", id))

			continue
		}
		if err := iter(ref, score); err != nil {
			return errors.Wrap(err, "iterating filter result")
		}
	}

	return nil
}

type mockBlock struct {
	component.Mock
}

func (m *mockBlock) Reload(c *Config) error {
	args := m.Called(c)

	return args.Error(0)
}

func (m *mockBlock) TransformToCold() error {
	args := m.Called()

	return args.Error(0)
}

func (m *mockBlock) ClearOnDisk() error {
	args := m.Called()

	return args.Error(0)
}

func (m *mockBlock) Append(ctx context.Context, feeds ...*model.Feed) error {
	args := m.Called(ctx, feeds)

	return args.Error(0)
}

func (m *mockBlock) Query(ctx context.Context, query QueryOptions) ([]*FeedVO, error) {
	args := m.Called(ctx, query)

	return args.Get(0).([]*FeedVO), args.Error(1)
}

func (m *mockBlock) Get(ctx context.Context, id uint64) (*FeedVO, bool, error) {
	args := m.Called(ctx, id)

	var feedVO *FeedVO
	if value := args.Get(0); value != nil {
		feedVO = value.(*FeedVO)
	}

	return feedVO, args.Bool(1), args.Error(2)
}

func (m *mockBlock) Exists(ctx context.Context, id uint64) (bool, error) {
	args := m.Called(ctx, id)

	return args.Bool(0), args.Error(1)
}

func (m *mockBlock) Start() time.Time {
	args := m.Called()

	return args.Get(0).(time.Time)
}

func (m *mockBlock) End() time.Time {
	args := m.Called()

	return args.Get(0).(time.Time)
}

func (m *mockBlock) State() State {
	args := m.Called()

	return args.Get(0).(State)
}
