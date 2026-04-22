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
	"strings"
	"time"

	"github.com/pkg/errors"

	"github.com/glidea/zenfeed/pkg/storage/kv"
)

// KV key patterns (single-user self-hosted, no user: prefix).
func FeedbackKey(feedID string) []byte { return namespacedKey("feedback", 0, feedID) }
func ArchiveKey(feedID string) []byte  { return namespacedKey("archive", 0, feedID) }
func ReadKey(feedID string) []byte     { return namespacedKey("read", 0, feedID) }

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

// AppliedTagSignal is the normalized signal actually written into the profile.
// It is derived from explicit user controls plus score-based article signals.
type AppliedTagSignal struct {
	Tag    string    `json:"tag"`
	Action TagAction `json:"action"`
	Delta  float64   `json:"delta"`
}

// Feedback is per-article raw user feedback stored at feedback:{feed_id}.
type Feedback struct {
	FeedID         string             `json:"feed_id"`
	Score          int                `json:"score,omitempty"`
	ScoreReason    string             `json:"score_reason,omitempty"`
	Tags           []UserTag          `json:"tags,omitempty"`
	AppliedSignals []AppliedTagSignal `json:"applied_signals,omitempty"`
	Note           string             `json:"note,omitempty"`
	Archive        bool               `json:"archive,omitempty"`
	CreatedAt      time.Time          `json:"created_at"`
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
	URL        string    `json:"url,omitempty"`
	ArchivedAt time.Time `json:"archived_at"`
}

// ReadIndexEntry is a lightweight record of a read article stored inside profile:global.
type ReadIndexEntry struct {
	FeedID string    `json:"feed_id"`
	ReadAt time.Time `json:"read_at"`
}

// ProfileGlobal is the aggregated user preference profile stored at profile:global.
type ProfileGlobal struct {
	NamespaceVersion int                 `json:"namespace_version,omitempty"`
	TagControls      []TagControl        `json:"tag_controls"`
	FeedbackCount    int                 `json:"feedback_count"`
	LastUpdated      time.Time           `json:"last_updated"`
	WeeklySnapshot   []TagControl        `json:"weekly_snapshot,omitempty"`
	WeeklySnapshotAt time.Time           `json:"weekly_snapshot_at,omitempty"`
	ReadIndex        []ReadIndexEntry    `json:"read_index,omitempty"`
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
	profile, err := s.GetProfile(ctx)
	if err != nil {
		return errors.Wrap(err, "get profile for feedback")
	}

	return s.saveFeedbackWithNamespace(ctx, profile.NamespaceVersion, fb)
}

func (s *Store) saveFeedbackWithNamespace(ctx context.Context, namespace int, fb *Feedback) error {
	if fb.CreatedAt.IsZero() {
		fb.CreatedAt = time.Now()
	}
	data, err := json.Marshal(fb)
	if err != nil {
		return errors.Wrap(err, "marshal feedback")
	}

	if err := s.kv.Set(ctx, feedbackKey(namespace, fb.FeedID), data, 0); err != nil {
		return errors.Wrap(err, "set feedback")
	}

	return nil
}

func (s *Store) GetFeedback(ctx context.Context, feedID string) (*Feedback, error) {
	profile, err := s.GetProfile(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "get profile for feedback lookup")
	}

	data, err := s.kv.Get(ctx, feedbackKey(profile.NamespaceVersion, feedID))
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
	profile, err := s.GetProfile(ctx)
	if err != nil {
		return errors.Wrap(err, "get profile for archive")
	}

	entry.ArchivedAt = time.Now()
	data, err := json.Marshal(entry)
	if err != nil {
		return errors.Wrap(err, "marshal archive entry")
	}

	if err := s.kv.Set(ctx, archiveKey(profile.NamespaceVersion, entry.FeedID), data, 0); err != nil {
		return errors.Wrap(err, "set archive entry")
	}

	indexEntry := ArchiveIndexEntry{
		FeedID:     entry.FeedID,
		Title:      entry.Labels["title"],
		Source:     entry.Labels["source"],
		URL:        entry.Labels["link"],
		ArchivedAt: entry.ArchivedAt,
	}
	profile.ArchiveIndex = upsertArchiveIndex(profile.ArchiveIndex, indexEntry)

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
	profile, err := s.GetProfile(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "get profile for archive lookup")
	}

	data, err := s.kv.Get(ctx, archiveKey(profile.NamespaceVersion, feedID))
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
	profile, err := s.GetProfile(ctx)
	if err != nil {
		return false
	}
	_, err = s.kv.Get(ctx, archiveKey(profile.NamespaceVersion, feedID))

	return err == nil
}

func (s *Store) MarkRead(ctx context.Context, feedID string) error {
	return s.MarkReadBatch(ctx, []string{feedID})
}

func (s *Store) MarkReadBatch(ctx context.Context, feedIDs []string) error {
	if len(feedIDs) == 0 {
		return nil
	}

	profile, err := s.GetProfile(ctx)
	if err != nil {
		return errors.Wrap(err, "get profile for read state")
	}

	for _, feedID := range feedIDs {
		entry := &ReadEntry{FeedID: feedID, ReadAt: time.Now()}
		if err := s.saveReadEntryWithNamespace(ctx, profile.NamespaceVersion, entry); err != nil {
			return err
		}
		profile.ReadIndex = upsertReadIndex(profile.ReadIndex, ReadIndexEntry{
			FeedID: feedID,
			ReadAt: entry.ReadAt,
		})
	}

	const maxReadIndex = 2000
	if len(profile.ReadIndex) > maxReadIndex {
		profile.ReadIndex = profile.ReadIndex[len(profile.ReadIndex)-maxReadIndex:]
	}

	if err := s.SaveProfile(ctx, profile); err != nil {
		return errors.Wrap(err, "save profile after mark read")
	}

	return nil
}

func (s *Store) saveReadEntryWithNamespace(ctx context.Context, namespace int, entry *ReadEntry) error {
	data, err := json.Marshal(entry)
	if err != nil {
		return errors.Wrap(err, "marshal read entry")
	}

	if err := s.kv.Set(ctx, readKey(namespace, entry.FeedID), data, 0); err != nil {
		return errors.Wrap(err, "set read entry")
	}

	return nil
}

func (s *Store) IsRead(ctx context.Context, feedID string) bool {
	profile, err := s.GetProfile(ctx)
	if err != nil {
		return false
	}

	return s.IsReadInNamespace(ctx, profile.NamespaceVersion, feedID)
}

func (s *Store) IsReadInNamespace(ctx context.Context, namespace int, feedID string) bool {
	_, err := s.kv.Get(ctx, readKey(namespace, feedID))

	return err == nil
}

func (s *Store) GetProfile(ctx context.Context) (*ProfileGlobal, error) {
	data, err := s.kv.Get(ctx, []byte(ProfileGlobalKey))
	switch {
	case err == nil:
	case errors.Is(err, kv.ErrNotFound):
		// Return empty profile on first use.
		return &ProfileGlobal{}, nil
	default:
		return nil, errors.Wrap(err, "get profile")
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
	kept := make([]TagControl, 0, len(profile.TagControls))

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
	if profile.NamespaceVersion < 0 {
		profile.NamespaceVersion = 0
	}
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

func (s *Store) Reset(ctx context.Context) error {
	profile, err := s.GetProfile(ctx)
	if err != nil {
		return errors.Wrap(err, "get profile before reset")
	}

	oldNamespace := profile.NamespaceVersion
	if err := s.SaveProfile(ctx, &ProfileGlobal{
		NamespaceVersion: profile.NamespaceVersion + 1,
	}); err != nil {
		return err
	}

	if err := s.deleteNamespaceData(ctx, oldNamespace); err != nil {
		return errors.Wrap(err, "delete namespace data")
	}

	return nil
}

// ReplaceFeedbackProfile applies a feedback update using read-modify-write.
// If prev is non-nil, its applied signals are removed before next is added.
// FeedbackCount tracks unique feedback articles rather than submission times.
func (s *Store) ReplaceFeedbackProfile(ctx context.Context, prev, next *Feedback) error { //nolint:cyclop
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

	if prev == nil && next != nil {
		profile.FeedbackCount++
	}
	if prev != nil && next == nil && profile.FeedbackCount > 0 {
		profile.FeedbackCount--
	}

	for _, signal := range collectEffectiveSignals(prev) {
		applySignal(profile, signal, -1)
	}
	for _, signal := range collectEffectiveSignals(next) {
		applySignal(profile, signal, 1)
	}

	if err := s.SaveProfile(ctx, profile); err != nil {
		return errors.Wrap(err, "save profile")
	}

	return nil
}

func collectEffectiveSignals(fb *Feedback) []AppliedTagSignal {
	if fb == nil {
		return nil
	}
	if len(fb.AppliedSignals) > 0 {
		return fb.AppliedSignals
	}

	signals := make([]AppliedTagSignal, 0, len(fb.Tags))
	for _, tag := range fb.Tags {
		signals = append(signals, AppliedTagSignal{
			Tag:    tag.Tag,
			Action: tag.Action,
			Delta:  0.6,
		})
	}

	return signals
}

func applySignal(profile *ProfileGlobal, signal AppliedTagSignal, direction float64) {
	if profile == nil || strings.TrimSpace(signal.Tag) == "" || signal.Delta <= 0 {
		return
	}

	normalizedDelta := signal.Delta
	if direction < 0 {
		normalizedDelta = -normalizedDelta
	}

	switch signal.Action {
	case TagActionBoost:
		mergeSignedTagControl(profile, signal.Tag, normalizedDelta)
	case TagActionDemote:
		mergeSignedTagControl(profile, signal.Tag, -normalizedDelta)
	case TagActionBlock:
		mergeBlockTagControl(profile, signal.Tag, direction > 0)
	}
}

func mergeSignedTagControl(profile *ProfileGlobal, tag string, delta float64) {
	for i, tc := range profile.TagControls {
		if tc.Tag != tag {
			continue
		}
		if tc.Action == TagActionBlock {
			if delta > 0 {
				profile.TagControls[i].LastSeen = time.Now()
			}

			return
		}

		next := signedWeight(tc) + delta
		if math.Abs(next) < 0.05 {
			profile.TagControls = append(profile.TagControls[:i], profile.TagControls[i+1:]...)

			return
		}

		profile.TagControls[i].Action = actionFromSignedWeight(next)
		profile.TagControls[i].Weight = clampWeight(math.Abs(next))
		profile.TagControls[i].LastSeen = time.Now()

		return
	}

	if math.Abs(delta) < 0.05 {
		return
	}

	profile.TagControls = append(profile.TagControls, TagControl{
		Tag:      tag,
		Action:   actionFromSignedWeight(delta),
		Weight:   clampWeight(math.Abs(delta)),
		LastSeen: time.Now(),
	})
}

func mergeBlockTagControl(profile *ProfileGlobal, tag string, enable bool) {
	for i, tc := range profile.TagControls {
		if tc.Tag != tag {
			continue
		}
		if !enable {
			profile.TagControls = append(profile.TagControls[:i], profile.TagControls[i+1:]...)

			return
		}

		profile.TagControls[i].Action = TagActionBlock
		profile.TagControls[i].Weight = 1
		profile.TagControls[i].LastSeen = time.Now()

		return
	}

	if !enable {
		return
	}

	profile.TagControls = append(profile.TagControls, TagControl{
		Tag:      tag,
		Action:   TagActionBlock,
		Weight:   1,
		LastSeen: time.Now(),
	})
}

func signedWeight(tc TagControl) float64 {
	switch tc.Action {
	case TagActionBoost:
		return tc.Weight
	case TagActionDemote:
		return -tc.Weight
	default:
		return 0
	}
}

func actionFromSignedWeight(weight float64) TagAction {
	if weight < 0 {
		return TagActionDemote
	}

	return TagActionBoost
}

func clampWeight(weight float64) float64 {
	if weight < 0 {
		return 0
	}
	if weight > 1.5 {
		return 1.5
	}

	return weight
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

func feedbackKey(namespace int, feedID string) []byte {
	return namespacedKey("feedback", namespace, feedID)
}

func archiveKey(namespace int, feedID string) []byte {
	return namespacedKey("archive", namespace, feedID)
}

func readKey(namespace int, feedID string) []byte {
	return namespacedKey("read", namespace, feedID)
}

func namespacedKey(prefix string, namespace int, feedID string) []byte {
	if namespace <= 0 {
		return []byte(prefix + ":" + feedID)
	}

	return []byte(fmt.Sprintf("%s:v%d:%s", prefix, namespace, feedID))
}

func namespacedPrefix(prefix string, namespace int) []byte {
	if namespace <= 0 {
		return []byte(prefix + ":")
	}

	return []byte(fmt.Sprintf("%s:v%d:", prefix, namespace))
}

func (s *Store) deleteNamespaceData(ctx context.Context, namespace int) error {
	prefixes := [][]byte{
		namespacedPrefix("feedback", namespace),
		namespacedPrefix("archive", namespace),
		namespacedPrefix("read", namespace),
	}

	for _, prefix := range prefixes {
		keys, err := s.kv.Keys(ctx, prefix)
		if err != nil {
			return errors.Wrapf(err, "list keys for prefix %s", prefix)
		}

		for _, key := range keys {
			if err := s.kv.Delete(ctx, key); err != nil {
				return errors.Wrapf(err, "delete key %s", key)
			}
		}
	}

	return nil
}

func upsertArchiveIndex(entries []ArchiveIndexEntry, next ArchiveIndexEntry) []ArchiveIndexEntry {
	deduped := make([]ArchiveIndexEntry, 0, len(entries)+1)
	replaced := false
	for _, current := range entries {
		if current.FeedID != next.FeedID {
			deduped = append(deduped, current)

			continue
		}

		deduped = append(deduped, next)
		replaced = true
	}
	if !replaced {
		deduped = append(deduped, next)
	}

	return deduped
}

func upsertReadIndex(entries []ReadIndexEntry, next ReadIndexEntry) []ReadIndexEntry {
	deduped := make([]ReadIndexEntry, 0, len(entries)+1)
	replaced := false
	for _, current := range entries {
		if current.FeedID != next.FeedID {
			deduped = append(deduped, current)

			continue
		}

		deduped = append(deduped, next)
		replaced = true
	}
	if !replaced {
		deduped = append(deduped, next)
	}

	return deduped
}
