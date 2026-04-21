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

// Package personalization provides types and KV helpers for the user
// personalization system (feedback, archive, read state, preference profile).
package personalization

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/pkg/errors"

	"github.com/glidea/zenfeed/pkg/storage/kv"
)

// KV key patterns (single-user self-hosted, no user: prefix).
func FeedbackKey(feedID string) []byte { return []byte("feedback:" + feedID) }
func ArchiveKey(feedID string) []byte  { return []byte("archive:" + feedID) }
func ReadKey(feedID string) []byte     { return []byte("read:" + feedID) }

const ProfileGlobalKey = "profile:global"

// TagAction represents the semantic direction of a user tag.
type TagAction string

const (
	TagActionBoost  TagAction = "boost"
	TagActionDemote TagAction = "demote"
	TagActionBlock  TagAction = "block"
	TagActionFlag   TagAction = "flag"
)

// UserTag is a tag with action semantics from user feedback.
type UserTag struct {
	Tag    string    `json:"tag"`
	Action TagAction `json:"action"`
}

// Feedback is per-article raw user feedback stored at feedback:{feed_id}.
type Feedback struct {
	FeedID      string    `json:"feed_id"`
	Score       int       `json:"score,omitempty"`
	ScoreReason string    `json:"score_reason,omitempty"`
	Tags        []UserTag `json:"tags,omitempty"`
	Archive     bool      `json:"archive,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// TagControl is an aggregated tag preference in the profile.
type TagControl struct {
	Tag      string    `json:"tag"`
	Action   TagAction `json:"action"`
	Weight   float64   `json:"weight"`
	LastSeen time.Time `json:"last_seen,omitempty"` // updated on each feedback interaction
}

// ArchiveIndexEntry is a lightweight record of a saved article stored inside profile:global.
type ArchiveIndexEntry struct {
	FeedID     string    `json:"feed_id"`
	Title      string    `json:"title,omitempty"`
	Source     string    `json:"source,omitempty"`
	ArchivedAt time.Time `json:"archived_at"`
}

// ProfileGlobal is the aggregated user preference profile stored at profile:global.
type ProfileGlobal struct {
	TagControls      []TagControl        `json:"tag_controls"`
	FeedbackCount    int                 `json:"feedback_count"`
	LastUpdated      time.Time           `json:"last_updated"`
	WeeklySnapshot   []TagControl        `json:"weekly_snapshot,omitempty"`
	WeeklySnapshotAt time.Time           `json:"weekly_snapshot_at,omitempty"`
	ArchiveIndex     []ArchiveIndexEntry `json:"archive_index,omitempty"`
}

// ArchiveEntry is the archived article stored at archive:{feed_id}.
type ArchiveEntry struct {
	FeedID     string            `json:"feed_id"`
	Labels     map[string]string `json:"labels"`
	FeedTime   time.Time         `json:"feed_time"`
	Feedback   *Feedback         `json:"feedback,omitempty"`
	ArchivedAt time.Time         `json:"archived_at"`
}

// ReadEntry marks an article as read, stored at read:{feed_id}.
type ReadEntry struct {
	FeedID string    `json:"feed_id"`
	ReadAt time.Time `json:"read_at"`
}

// Store provides high-level read/write operations on the personalization KV namespace.
type Store struct {
	kv kv.Storage
}

func NewStore(kv kv.Storage) *Store {
	return &Store{kv: kv}
}

func (s *Store) SaveFeedback(ctx context.Context, fb *Feedback) error {
	fb.CreatedAt = time.Now()
	data, err := json.Marshal(fb)
	if err != nil {
		return errors.Wrap(err, "marshal feedback")
	}

	if err := s.kv.Set(ctx, FeedbackKey(fb.FeedID), data, 0); err != nil {
		return errors.Wrap(err, "set feedback")
	}

	return nil
}

func (s *Store) GetFeedback(ctx context.Context, feedID string) (*Feedback, error) {
	data, err := s.kv.Get(ctx, FeedbackKey(feedID))
	if err != nil {
		return nil, errors.Wrap(err, "get feedback")
	}

	var fb Feedback
	if err := json.Unmarshal(data, &fb); err != nil {
		return nil, errors.Wrap(err, "unmarshal feedback")
	}

	return &fb, nil
}

func (s *Store) SaveArchive(ctx context.Context, entry *ArchiveEntry) error {
	entry.ArchivedAt = time.Now()
	data, err := json.Marshal(entry)
	if err != nil {
		return errors.Wrap(err, "marshal archive entry")
	}

	if err := s.kv.Set(ctx, ArchiveKey(entry.FeedID), data, 0); err != nil {
		return errors.Wrap(err, "set archive entry")
	}

	// Maintain the lightweight archive index inside the global profile.
	profile, err := s.GetProfile(ctx)
	if err != nil {
		return errors.Wrap(err, "get profile for archive index")
	}

	indexEntry := ArchiveIndexEntry{
		FeedID:     entry.FeedID,
		Title:      entry.Labels["title"],
		Source:     entry.Labels["source"],
		ArchivedAt: entry.ArchivedAt,
	}
	profile.ArchiveIndex = append(profile.ArchiveIndex, indexEntry)

	const maxArchiveIndex = 200
	if len(profile.ArchiveIndex) > maxArchiveIndex {
		profile.ArchiveIndex = profile.ArchiveIndex[len(profile.ArchiveIndex)-maxArchiveIndex:]
	}

	if err := s.SaveProfile(ctx, profile); err != nil {
		return errors.Wrap(err, "save profile after archive")
	}

	return nil
}

func (s *Store) GetArchive(ctx context.Context, feedID string) (*ArchiveEntry, error) {
	data, err := s.kv.Get(ctx, ArchiveKey(feedID))
	if err != nil {
		return nil, errors.Wrap(err, "get archive entry")
	}

	var entry ArchiveEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return nil, errors.Wrap(err, "unmarshal archive entry")
	}

	return &entry, nil
}

func (s *Store) IsArchived(ctx context.Context, feedID string) bool {
	_, err := s.kv.Get(ctx, ArchiveKey(feedID))

	return err == nil
}

func (s *Store) MarkRead(ctx context.Context, feedID string) error {
	entry := &ReadEntry{FeedID: feedID, ReadAt: time.Now()}
	data, err := json.Marshal(entry)
	if err != nil {
		return errors.Wrap(err, "marshal read entry")
	}

	if err := s.kv.Set(ctx, ReadKey(feedID), data, 0); err != nil {
		return errors.Wrap(err, "set read entry")
	}

	return nil
}

func (s *Store) IsRead(ctx context.Context, feedID string) bool {
	_, err := s.kv.Get(ctx, ReadKey(feedID))

	return err == nil
}

func (s *Store) GetProfile(ctx context.Context) (*ProfileGlobal, error) {
	data, err := s.kv.Get(ctx, []byte(ProfileGlobalKey))
	if err != nil {
		// Return empty profile on first use.
		return &ProfileGlobal{}, nil
	}

	var profile ProfileGlobal
	if err := json.Unmarshal(data, &profile); err != nil {
		return nil, errors.Wrap(err, "unmarshal profile")
	}

	applyTagDecay(&profile)

	return &profile, nil
}

// applyTagDecay applies time-based exponential decay to tag weights.
// Half-life is 30 days; tags below 0.05 are pruned.
// Only affects tags that have been seen (LastSeen non-zero) and are older than 30 days.
func applyTagDecay(profile *ProfileGlobal) {
	const (
		halfLifeDays    = 30.0
		decayStartDays  = 30.0
		removeThreshold = 0.05
	)

	now := time.Now()
	kept := profile.TagControls[:0]

	for _, tc := range profile.TagControls {
		if tc.LastSeen.IsZero() {
			kept = append(kept, tc)
			continue
		}

		days := now.Sub(tc.LastSeen).Hours() / 24
		if days < decayStartDays {
			kept = append(kept, tc)
			continue
		}

		tc.Weight *= math.Pow(0.5, days/halfLifeDays)
		if tc.Weight < removeThreshold {
			continue // prune
		}

		kept = append(kept, tc)
	}

	profile.TagControls = kept
}

func (s *Store) SaveProfile(ctx context.Context, profile *ProfileGlobal) error {
	profile.LastUpdated = time.Now()
	data, err := json.Marshal(profile)
	if err != nil {
		return errors.Wrap(err, "marshal profile")
	}

	if err := s.kv.Set(ctx, []byte(ProfileGlobalKey), data, 0); err != nil {
		return errors.Wrap(err, "set profile")
	}

	return nil
}

// UpdateProfileFromFeedback applies a new feedback to the profile using
// read-modify-write. It upserts tag controls: boost/demote/block from the
// feedback tags. Weight increases by 0.1 per feedback, capped at 1.0.
func (s *Store) UpdateProfileFromFeedback(ctx context.Context, fb *Feedback) error {
	profile, err := s.GetProfile(ctx)
	if err != nil {
		return errors.Wrap(err, "get profile")
	}

	// Refresh weekly snapshot if >7 days old.
	if time.Since(profile.WeeklySnapshotAt) > 7*24*time.Hour {
		profile.WeeklySnapshot = make([]TagControl, len(profile.TagControls))
		copy(profile.WeeklySnapshot, profile.TagControls)
		profile.WeeklySnapshotAt = time.Now()
	}

	for _, ut := range fb.Tags {
		s.upsertTagControl(profile, ut)
	}

	profile.FeedbackCount++

	if err := s.SaveProfile(ctx, profile); err != nil {
		return errors.Wrap(err, "save profile")
	}

	return nil
}

func (s *Store) upsertTagControl(profile *ProfileGlobal, ut UserTag) {
	const weightDelta = 0.1

	for i, tc := range profile.TagControls {
		if tc.Tag != ut.Tag {
			continue
		}

		profile.TagControls[i].Action = ut.Action
		newWeight := tc.Weight + weightDelta
		if newWeight > 1.0 {
			newWeight = 1.0
		}
		profile.TagControls[i].Weight = newWeight
		profile.TagControls[i].LastSeen = time.Now()

		return
	}

	profile.TagControls = append(profile.TagControls, TagControl{
		Tag:      ut.Tag,
		Action:   ut.Action,
		Weight:   weightDelta,
		LastSeen: time.Now(),
	})
}

// FeedbackToastMessage returns a human-readable toast message explaining
// what the feedback will do, based on the tags and score.
func FeedbackToastMessage(fb *Feedback) string {
	if len(fb.Tags) == 0 {
		return "已记录反馈"
	}

	var boostTags, demoteTags []string
	for _, t := range fb.Tags {
		switch t.Action {
		case TagActionBoost:
			boostTags = append(boostTags, t.Tag)
		case TagActionDemote, TagActionBlock:
			demoteTags = append(demoteTags, t.Tag)
		}
	}

	if len(boostTags) > 0 {
		return fmt.Sprintf("已记录，将为你多推「%s」类内容", joinTags(boostTags))
	}

	if len(demoteTags) > 0 {
		return fmt.Sprintf("已记录，将减少「%s」类内容曝光", joinTags(demoteTags))
	}

	return "已记录反馈"
}

func joinTags(tags []string) string {
	result := ""
	for i, t := range tags {
		if i > 0 {
			result += " · "
		}
		result += t
	}

	return result
}
