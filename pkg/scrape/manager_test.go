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

package scrape

import (
	"context"
	"testing"
	"time"

	"github.com/glidea/zenfeed/pkg/component"
	"github.com/glidea/zenfeed/pkg/config"
	"github.com/glidea/zenfeed/pkg/scrape/scraper"
	"github.com/glidea/zenfeed/pkg/stats"
	timeutil "github.com/glidea/zenfeed/pkg/util/time"
)

type panicKV struct{}

func (p *panicKV) Name() string           { return "panicKV" }
func (p *panicKV) Instance() string       { return "panic" }
func (p *panicKV) Run() error             { return nil }
func (p *panicKV) Ready() <-chan struct{} { ch := make(chan struct{}); close(ch); return ch }
func (p *panicKV) Close() error           { return nil }

func (p *panicKV) Get(context.Context, []byte) ([]byte, error) {
	panic("kv Get should not be called during scrape manager construction")
}

func (p *panicKV) Set(context.Context, []byte, []byte, time.Duration) error {
	return nil
}

func (p *panicKV) Delete(context.Context, []byte) error {
	return nil
}

func (p *panicKV) Keys(context.Context, []byte) ([][]byte, error) {
	return nil, nil
}

func TestNewDoesNotLoadStatsDuringConstruction(t *testing.T) {
	t.Parallel()

	app := &config.App{}
	app.Scrape.Interval = timeutil.Duration(time.Hour)
	app.Scrape.Sources = []config.ScrapeSource{
		{
			Name: "test-source",
			RSS: &config.ScrapeSourceRSS{
				URL: "https://example.com/feed.xml",
			},
		},
	}

	store := stats.New(&panicKV{})

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("unexpected panic during scrape manager construction: %v", r)
		}
	}()

	manager, err := NewFactory().New(component.Global, app, Dependencies{
		ScraperFactory: scraper.NewFactory(),
		Stats:          store,
	})
	if err != nil {
		t.Fatalf("new scrape manager: %v", err)
	}
	if manager == nil {
		t.Fatal("manager is nil")
	}
}

// import (
// 	"testing"
// 	"time"

// 	. "github.com/onsi/gomega"
// )

// func TestValidate(t *testing.T) {
// 	RegisterTestingT(t)

// 	tests := []struct {
// 		scenario string
// 		given    string
// 		when     string
// 		then     string
// 		config   *Config
// 		want     *Config
// 		wantErr  string
// 	}{
// 		{
// 			scenario: "Valid Configuration",
// 			given:    "a valid configuration with RSS sources",
// 			when:     "creating a new manager",
// 			then:     "should create manager successfully",
// 			config: &Config{
// 				ScrapeInterval: time.Second,
// 				RSSs: []RSS{
// 					{
// 						Name:           "test",
// 						URL:            "http://example.com",
// 						ScrapeInterval: time.Second,
// 						Labels:         map[string]string{},
// 					},
// 				},
// 			},
// 		},
// 		{
// 			scenario: "Invalid Global Interval",
// 			given:    "a configuration with invalid global scrape interval",
// 			when:     "creating a new manager",
// 			then:     "should return interval validation error",
// 			config: &Config{
// 				ScrapeInterval: time.Millisecond,
// 			},
// 			wantErr: "scrape interval must be at least 1 second",
// 		},
// 		{
// 			scenario: "Invalid RSS Config",
// 			given:    "a configuration with invalid RSS config",
// 			when:     "creating a new manager",
// 			then:     "should return RSS config validation error",
// 			config: &Config{
// 				ScrapeInterval: time.Second,
// 				RSSs: []RSS{
// 					{
// 						Name:           "",
// 						URL:            "",
// 						ScrapeInterval: time.Second,
// 						Labels:         map[string]string{},
// 					},
// 				},
// 			},
// 			wantErr: "invalid RSS config",
// 		},
// 		{
// 			scenario: "Default Global Interval",
// 			given:    "a configuration with zero global interval",
// 			when:     "validating and adjusting the config",
// 			then:     "should set default global interval",
// 			config: &Config{
// 				ScrapeInterval: 0,
// 				RSSs: []RSS{
// 					{
// 						Name:           "test",
// 						URL:            "http://example.com",
// 						ScrapeInterval: time.Second,
// 						Labels:         map[string]string{},
// 					},
// 				},
// 			},
// 			want: &Config{
// 				ScrapeInterval: time.Hour, // default value
// 				RSSs: []RSS{
// 					{
// 						Name:           "test",
// 						URL:            "http://example.com",
// 						ScrapeInterval: time.Hour, // inherited from global
// 						Labels:         map[string]string{},
// 					},
// 				},
// 			},
// 		},
// 		{
// 			scenario: "Default RSS Interval",
// 			given:    "a configuration with zero RSS interval",
// 			when:     "validating and adjusting the config",
// 			then:     "should inherit global interval",
// 			config: &Config{
// 				ScrapeInterval: time.Minute,
// 				RSSs: []RSS{
// 					{
// 						Name:   "test",
// 						URL:    "http://example.com",
// 						Labels: map[string]string{},
// 					},
// 				},
// 			},
// 			want: &Config{
// 				ScrapeInterval: time.Minute,
// 				RSSs: []RSS{
// 					{
// 						Name:           "test",
// 						URL:            "http://example.com",
// 						ScrapeInterval: time.Minute,
// 						Labels:         map[string]string{},
// 					},
// 				},
// 			},
// 		},
// 	}

// 	for _, tt := range tests {
// 		t.Run(tt.scenario, func(t *testing.T) {
// 			err := tt.config.Validate()
// 			if tt.wantErr != "" {
// 				Expect(err).To(HaveOccurred())
// 				Expect(err.Error()).To(ContainSubstring(tt.wantErr))
// 			} else {
// 				Expect(err).NotTo(HaveOccurred())
// 				if tt.want != nil {
// 					Expect(tt.config).To(Equal(tt.want))
// 				}
// 			}
// 		})
// 	}
// }

// // func TestManager_Run(t *testing.T) {
// // 	RegisterTestingT(t)

// // 	tests := []struct {
// // 		scenario  string
// // 		given     string
// // 		when      string
// // 		then      string
// // 		config    *Config
// // 		mockSetup func(*mockScraperFactory)
// // 		wantErr   string
// // 	}{
// // 		{
// // 			scenario: "Basic Run",
// // 			given:    "a valid configuration with one RSS source",
// // 			when:     "running the manager",
// // 			then:     "should start scraper successfully",
// // 			config: &Config{
// // 				ScrapeInterval: time.Second,
// // 				RSSs: []RSS{
// // 					{
// // 						Config: &rss.Config{
// // 							WebsiteName: "Test",
// // 							Name:        "test",
// // 							URL:         "http://example.com",
// // 						},
// // 					},
// // 				},
// // 			},
// // 			mockSetup: func(f *mockScraperFactory) {
// // 				mockScraper := scraper.NewMock()
// // 				mockScraper.On("Config").Return(&scraper.Config{})
// // 				mockScraper.On("Run").Return()
// // 				mockScraper.On("Stop").Return()
// // 				f.On("New", mock.Anything, mock.Anything).Return(mockScraper, nil)
// // 			},
// // 		},
// // 		{
// // 			scenario: "Scraper Creation Failure",
// // 			given:    "a configuration that causes scraper creation to fail",
// // 			when:     "running the manager",
// // 			then:     "should return error",
// // 			config: &Config{
// // 				ScrapeInterval: time.Second,
// // 				RSSs: []RSS{
// // 					{
// // 						Config: &rss.Config{
// // 							WebsiteName: "Test",
// // 							Name:        "test",
// // 							URL:         "http://example.com",
// // 						},
// // 					},
// // 				},
// // 			},
// // 			mockSetup: func(f *mockScraperFactory) {
// // 				f.On("New", mock.Anything, mock.Anything).Return(nil, errors.New("scraper creation failed"))
// // 			},
// // 			wantErr: "creating RSS scraper",
// // 		},
// // 	}

// // 	for _, tt := range tests {
// // 		t.Run(tt.scenario, func(t *testing.T) {
// // 			mockReader := feedreader.NewMock()
// // 			mockWriter := feedwriter.NewMock()
// // 			mockDB := db.New(mockWriter, mockReader)
// // 			m, err := NewManager(tt.config, mockDB)
// // 			Expect(err).NotTo(HaveOccurred())

// // 			mockFactory := newMockScraperFactory()
// // 			defer mockFactory.AssertExpectations(t)
// // 			if tt.mockSetup != nil {
// // 				tt.mockSetup(mockFactory)
// // 			}
// // 			mgr := m.(*manager)
// // 			mgr.rssScraperFactory = mockFactory

// // 			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
// // 			defer cancel()

// // 			err = m.Run(ctx)
// // 			if tt.wantErr != "" {
// // 				Expect(err).To(HaveOccurred())
// // 				Expect(err.Error()).To(ContainSubstring(tt.wantErr))
// // 			} else {
// // 				Expect(err).NotTo(HaveOccurred())
// // 			}
// // 		})
// // 	}
// // }

// // func TestManager_Reload(t *testing.T) {
// // 	RegisterTestingT(t)

// // 	tests := []struct {
// // 		scenario   string
// // 		given      string
// // 		when       string
// // 		then       string
// // 		initConfig *Config
// // 		newConfig  *Config
// // 		mockSetup  func(*mockScraperFactory)
// // 		validate   func(Manager)
// // 		wantErr    string
// // 	}{
// // 		{
// // 			scenario: "Valid Config Update",
// // 			given:    "a running manager and valid new configuration",
// // 			when:     "reloading with new config",
// // 			then:     "should update scrapers successfully",
// // 			initConfig: &Config{
// // 				ScrapeInterval: time.Second,
// // 				RSSs: []RSS{
// // 					{
// // 						Config: &rss.Config{
// // 							WebsiteName: "Test",
// // 							Name:        "test1",
// // 							URL:         "http://example1.com",
// // 						},
// // 					},
// // 				},
// // 			},
// // 			newConfig: &Config{
// // 				ScrapeInterval: time.Second,
// // 				RSSs: []RSS{
// // 					{
// // 						Config: &rss.Config{
// // 							WebsiteName: "Test",
// // 							Name:        "test1",
// // 							URL:         "http://example1.com",
// // 						},
// // 					},
// // 					{
// // 						Config: &rss.Config{
// // 							WebsiteName: "Test",
// // 							Name:        "test2",
// // 							URL:         "http://example2.com",
// // 						},
// // 					},
// // 				},
// // 			},
// // 			mockSetup: func(f *mockScraperFactory) {
// // 				mockScraper1 := scraper.NewMock()
// // 				mockScraper1.On("Config").Return(&scraper.Config{
// // 					Interval:        time.Second,
// // 					RetentionPeriod: 24 * time.Hour,
// // 				})
// // 				mockScraper1.On("Run").Return()
// // 				mockScraper1.On("Stop").Return()

// // 				mockScraper2 := scraper.NewMock()
// // 				mockScraper2.On("Config").Return(&scraper.Config{
// // 					Interval:        time.Second,
// // 					RetentionPeriod: 24 * time.Hour,
// // 				})
// // 				mockScraper2.On("Run").Return()
// // 				mockScraper2.On("Stop").Return()

// // 				f.On("New", mock.Anything, mock.Anything).Return(mockScraper1, nil).Once()
// // 				f.On("New", mock.Anything, mock.Anything).Return(mockScraper2, nil).Once()
// // 			},
// // 			validate: func(m Manager) {
// // 				mgr := m.(*manager)
// // 				Expect(mgr.scrapers).To(HaveLen(2))
// // 				for id := range mgr.scrapers {
// // 					Expect(id).To(BeElementOf([]string{"rss/Test/test1", "rss/Test/test2"}))
// // 				}
// // 			},
// // 		},
// // 		{
// // 			scenario: "Invalid New Config",
// // 			given:    "a running manager and invalid new configuration",
// // 			when:     "reloading with invalid config",
// // 			then:     "should return validation error",
// // 			initConfig: &Config{
// // 				ScrapeInterval: time.Second,
// // 				RSSs: []RSS{
// // 					{
// // 						Config: &rss.Config{
// // 							WebsiteName: "Test",
// // 							Name:        "test",
// // 							URL:         "http://example.com",
// // 						},
// // 					},
// // 				},
// // 			},
// // 			newConfig: &Config{
// // 				ScrapeInterval: time.Millisecond,
// // 			},
// // 			mockSetup: func(f *mockScraperFactory) {
// // 				mockScraper := scraper.NewMock()
// // 				mockScraper.On("Config").Return(&scraper.Config{})
// // 				mockScraper.On("Run").Return()
// // 				mockScraper.On("Stop").Return()
// // 				f.On("New", mock.Anything, mock.Anything).Return(mockScraper, nil).Maybe()
// // 			},
// // 			wantErr: "scrape interval must be at least 1 second",
// // 		},
// // 		{
// // 			scenario: "Keep Unchanged Scraper",
// // 			given:    "a running scraper with unchanged config",
// // 			when:     "reloading with same config",
// // 			then:     "should keep the same scraper instance",
// // 			initConfig: &Config{
// // 				ScrapeInterval: time.Second,
// // 				RSSs: []RSS{
// // 					{
// // 						Config: &rss.Config{
// // 							WebsiteName: "Test",
// // 							Name:        "test",
// // 							URL:         "http://example.com",
// // 						},
// // 					},
// // 				},
// // 			},
// // 			newConfig: &Config{
// // 				ScrapeInterval: time.Second,
// // 				RSSs: []RSS{
// // 					{
// // 						Config: &rss.Config{
// // 							WebsiteName: "Test",
// // 							Name:        "test",
// // 							URL:         "http://example.com",
// // 						},
// // 					},
// // 				},
// // 			},
// // 			mockSetup: func(f *mockScraperFactory) {
// // 				mockScraper := scraper.NewMock()
// // 				mockScraper.On("Config").Return(&scraper.Config{
// // 					Interval:        time.Second,
// // 					RetentionPeriod: 24 * time.Hour,
// // 				})
// // 				mockScraper.On("Run").Return()
// // 				mockScraper.On("Stop").Return()
// // 				f.On("New", mock.Anything, mock.Anything).Return(mockScraper, nil).Once()
// // 			},
// // 			validate: func(m Manager) {
// // 				mgr := m.(*manager)
// // 				Expect(mgr.scrapers).To(HaveLen(1))
// // 				Expect(mgr.scrapers["rss/Test/test"].Config().Interval).To(Equal(time.Second))
// // 			},
// // 		},
// // 		{
// // 			scenario: "Stop Removed Scraper",
// // 			given:    "a running scraper",
// // 			when:     "reloading with empty config",
// // 			then:     "should stop and remove the scraper",
// // 			initConfig: &Config{
// // 				ScrapeInterval: time.Second,
// // 				RSSs: []RSS{
// // 					{
// // 						Config: &rss.Config{
// // 							WebsiteName: "Test",
// // 							Name:        "test",
// // 							URL:         "http://example.com",
// // 						},
// // 					},
// // 				},
// // 			},
// // 			newConfig: &Config{
// // 				ScrapeInterval: time.Second,
// // 				RSSs:           []RSS{},
// // 			},
// // 			mockSetup: func(f *mockScraperFactory) {
// // 				mockScraper := scraper.NewMock()
// // 				mockScraper.On("Config").Return(&scraper.Config{
// // 					Interval:        time.Second,
// // 					RetentionPeriod: 24 * time.Hour,
// // 				})
// // 				mockScraper.On("Run").Return()
// // 				mockScraper.On("Stop").Return().Once()
// // 				f.On("New", mock.Anything, mock.Anything).Return(mockScraper, nil).Once()
// // 			},
// // 			validate: func(m Manager) {
// // 				mgr := m.(*manager)
// // 				Expect(mgr.scrapers).To(BeEmpty())
// // 			},
// // 		},
// // 		{
// // 			scenario: "Restart Changed Scraper",
// // 			given:    "a running scraper",
// // 			when:     "reloading with modified config",
// // 			then:     "should stop old scraper and start new one",
// // 			initConfig: &Config{
// // 				ScrapeInterval: time.Second,
// // 				RSSs: []RSS{
// // 					{
// // 						Config: &rss.Config{
// // 							WebsiteName: "Test",
// // 							Name:        "test",
// // 							URL:         "http://example.com",
// // 						},
// // 						Scrape: &scraper.Config{
// // 							Interval: time.Second,
// // 						},
// // 					},
// // 				},
// // 			},
// // 			newConfig: &Config{
// // 				ScrapeInterval: time.Second,
// // 				RSSs: []RSS{
// // 					{
// // 						Config: &rss.Config{
// // 							WebsiteName: "Test",
// // 							Name:        "test",
// // 							URL:         "http://example.com",
// // 						},
// // 						Scrape: &scraper.Config{
// // 							Interval: time.Minute,
// // 						},
// // 					},
// // 				},
// // 			},
// // 			mockSetup: func(f *mockScraperFactory) {
// // 				mockScraper1 := scraper.NewMock()
// // 				mockScraper1.On("Config").Return(&scraper.Config{
// // 					Interval:        time.Second,
// // 					RetentionPeriod: 24 * time.Hour,
// // 				})
// // 				mockScraper1.On("Run").Return()
// // 				mockScraper1.On("Stop").Return().Once()

// // 				mockScraper2 := scraper.NewMock()
// // 				mockScraper2.On("Config").Return(&scraper.Config{
// // 					Interval: time.Minute,
// // 				})
// // 				mockScraper2.On("Run").Return()
// // 				mockScraper2.On("Stop").Return()

// // 				f.On("New", mock.Anything, mock.Anything).Return(mockScraper1, nil).Once()
// // 				f.On("New", mock.Anything, mock.Anything).Return(mockScraper2, nil).Once()
// // 			},
// // 			validate: func(m Manager) {
// // 				mgr := m.(*manager)
// // 				Expect(mgr.scrapers).To(HaveLen(1))
// // 				Expect(mgr.scrapers["rss/Test/test"].Config().Interval).To(Equal(time.Minute))
// // 			},
// // 		},
// // 	}

// // 	for _, tt := range tests {
// // 		t.Run(tt.scenario, func(t *testing.T) {
// // 			mockReader := feedreader.NewMock()
// // 			mockWriter := feedwriter.NewMock()
// // 			mockDB := db.New(mockWriter, mockReader)
// // 			m, err := NewManager(tt.initConfig, mockDB)
// // 			Expect(err).NotTo(HaveOccurred())

// // 			mgr := m.(*manager)
// // 			mockFactory := newMockScraperFactory()
// // 			defer mockFactory.AssertExpectations(t)
// // 			if tt.mockSetup != nil {
// // 				tt.mockSetup(mockFactory)
// // 			}
// // 			mgr.rssScraperFactory = mockFactory

// // 			ctx, cancel := context.WithCancel(context.Background())
// // 			defer cancel()
// // 			go func() {
// // 				_ = m.Run(ctx)
// // 			}()
// // 			time.Sleep(50 * time.Millisecond)

// // 			err = m.Reload(tt.newConfig)
// // 			if tt.wantErr != "" {
// // 				Expect(err).To(HaveOccurred())
// // 				Expect(err.Error()).To(ContainSubstring(tt.wantErr))
// // 			} else {
// // 				Expect(err).NotTo(HaveOccurred())
// // 			}
// // 			if tt.validate != nil {
// // 				tt.validate(m)
// // 			}
// // 		})
// // 	}
// // }

// // 			mgr := m.(*manager)
// // 			mockFactory := newMockScraperFactory()
// // 			defer mockFactory.AssertExpectations(t)
// // 			if tt.mockSetup != nil {
// // 				tt.mockSetup(mockFactory)
// // 			}
// // 			mgr.rssScraperFactory = mockFactory

// // 			ctx, cancel := context.WithCancel(context.Background())
// // 			defer cancel()
// // 			go func() {
// // 				_ = m.Run(ctx)
// // 			}()
// // 			time.Sleep(50 * time.Millisecond)

// // 			err = m.Reload(tt.newConfig)
// // 			if tt.wantErr != "" {
// // 				Expect(err).To(HaveOccurred())
// // 				Expect(err.Error()).To(ContainSubstring(tt.wantErr))
// // 			} else {
// // 				Expect(err).NotTo(HaveOccurred())
// // 			}
// // 			if tt.validate != nil {
// // 				tt.validate(m)
// // 			}
// // 		})
// // 	}
// // }
