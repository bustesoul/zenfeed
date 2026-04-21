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

package stats

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/pkg/errors"

	"github.com/glidea/zenfeed/pkg/storage/kv"
)

// Tracker is the interface that components use to report stats.
type Tracker interface {
	RecordScrapeStart(name, url string)
	RecordScrapeEnd(name, url string, fetched int, err error)
	RecordTokens(promptTokens, completionTokens int64)
}

// SourceStat holds per-source scraping statistics.
type SourceStat struct {
	Name              string    `json:"name"`
	URL               string    `json:"url"`
	Scraping          bool      `json:"scraping"`
	LastScrapeAt      time.Time `json:"last_scrape_at,omitempty"`
	LastScrapeOK      bool      `json:"last_scrape_ok"`
	LastScrapeError   string    `json:"last_scrape_error,omitempty"`
	LastScrapeFetched int       `json:"last_scrape_fetched"`
	TotalFetched      int64     `json:"total_fetched"`
	TotalErrors       int64     `json:"total_errors"`
	// PersistSeq increments on each RecordScrapeEnd call. The async persist
	// goroutine checks this before writing to KV to discard stale snapshots.
	PersistSeq uint64 `json:"-"`
}

// LLMStat holds LLM token usage since last restart.
type LLMStat struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

// Snapshot is the full dashboard data returned by /get_stats.
type Snapshot struct {
	Sources []*SourceStat `json:"sources"`
	LLM     LLMStat       `json:"llm"`
}

const kvKeyPrefix = "stats:source:"

// Store is the central stats tracker. It keeps in-memory state and
// persists per-source stats to KV so history survives restarts.
type Store struct {
	mu        sync.Mutex
	persistMu sync.Mutex
	sources   map[string]*SourceStat // keyed by source name
	llm       LLMStat
	kv        kv.Storage
}

// New creates a new Store and loads existing source stats from KV.
func New(kvStorage kv.Storage) *Store {
	return &Store{
		sources: make(map[string]*SourceStat),
		kv:      kvStorage,
	}
}

// RecordScrapeStart marks a source as currently scraping (in-memory only).
func (s *Store) RecordScrapeStart(name, url string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	src := s.getOrCreate(name, url)
	src.Scraping = true
}

// RecordScrapeEnd records the result of a scrape cycle and persists to KV.
func (s *Store) RecordScrapeEnd(name, url string, fetched int, err error) {
	s.mu.Lock()
	src := s.getOrCreate(name, url)
	src.Scraping = false
	src.LastScrapeAt = time.Now()
	src.LastScrapeFetched = fetched
	if err != nil {
		src.LastScrapeOK = false
		src.LastScrapeError = err.Error()
		src.TotalErrors++
	} else {
		src.LastScrapeOK = true
		src.LastScrapeError = ""
		src.TotalFetched += int64(fetched)
	}
	src.PersistSeq++
	snapshot := *src // copy before releasing lock
	s.mu.Unlock()

	// Persist asynchronously to avoid blocking the scraper goroutine.
	go s.persist(name, &snapshot)
}

// RecordTokens adds LLM token counts (in-memory only, resets on restart).
func (s *Store) RecordTokens(promptTokens, completionTokens int64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.llm.PromptTokens += promptTokens
	s.llm.CompletionTokens += completionTokens
	s.llm.TotalTokens += promptTokens + completionTokens
}

// Snapshot returns a consistent read of all current stats.
func (s *Store) Snapshot(ctx context.Context) (*Snapshot, error) {
	s.mu.Lock()

	llm := s.llm
	sources := make([]*SourceStat, 0, len(s.sources))
	for _, src := range s.sources {
		cp := *src
		sources = append(sources, &cp)
	}
	s.mu.Unlock()

	sort.SliceStable(sources, func(i, j int) bool {
		if sources[i].Name == sources[j].Name {
			return sources[i].URL < sources[j].URL
		}
		return sources[i].Name < sources[j].Name
	})

	return &Snapshot{Sources: sources, LLM: llm}, nil
}

// LoadSource loads a previously persisted source stat into memory.
// Call this during startup for each configured source.
func (s *Store) LoadSource(ctx context.Context, name, url string) {
	data, err := s.kv.Get(ctx, []byte(kvKeyPrefix+name))
	if err != nil {
		// Not found or error — start fresh.
		s.mu.Lock()
		s.getOrCreate(name, url)
		s.mu.Unlock()

		return
	}

	var src SourceStat
	if err := json.Unmarshal(data, &src); err != nil {
		s.mu.Lock()
		s.getOrCreate(name, url)
		s.mu.Unlock()

		return
	}
	src.Scraping = false // never persist in-flight state
	src.URL = url        // always use current URL from config

	s.mu.Lock()
	s.sources[name] = &src
	s.mu.Unlock()
}

// getOrCreate returns or creates a SourceStat. Caller must hold s.mu.
func (s *Store) getOrCreate(name, url string) *SourceStat {
	if src, ok := s.sources[name]; ok {
		if url != "" {
			src.URL = url
		}

		return src
	}
	src := &SourceStat{Name: name, URL: url}
	s.sources[name] = src

	return src
}

func (s *Store) persist(name string, src *SourceStat) {
	s.persistMu.Lock()
	defer s.persistMu.Unlock()

	s.mu.Lock()
	current, ok := s.sources[name]
	if !ok || current.PersistSeq != src.PersistSeq {
		s.mu.Unlock()

		return
	}
	s.mu.Unlock()

	data, err := json.Marshal(src)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = errors.Wrap(s.kv.Set(ctx, []byte(kvKeyPrefix+name), data, 0), "persist source stats")
}
