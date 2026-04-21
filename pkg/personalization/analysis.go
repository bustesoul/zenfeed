package personalization

import (
	"encoding/json"
	"strings"

	"github.com/glidea/zenfeed/pkg/model"
)

const (
	LabelArticleAnalysis = "article_analysis"
	labelTags            = "tags"
)

type ArticleAnalysis struct {
	BaseQualityScore int      `json:"base_quality_score"`
	PrimaryTopic     string   `json:"primary_topic"`
	Format           string   `json:"format"`
	TopicsJSON       []string `json:"topics_json"`
}

func ParseArticleAnalysis(labels model.Labels) (ArticleAnalysis, bool) {
	raw := labels.Get(LabelArticleAnalysis)
	if strings.TrimSpace(raw) == "" {
		return ArticleAnalysis{}, false
	}

	var analysis ArticleAnalysis
	if err := json.Unmarshal([]byte(raw), &analysis); err != nil {
		return ArticleAnalysis{}, false
	}

	analysis.BaseQualityScore = clampInt(analysis.BaseQualityScore, 0, 10)

	return analysis, true
}

func FeedSignalTags(feed *model.Feed) []string {
	if feed == nil {
		return nil
	}

	seen := make(map[string]struct{}, 8)
	var tags []string
	add := func(tag string) {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			return
		}
		key := normalizeTag(tag)
		if key == "" {
			return
		}
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		tags = append(tags, tag)
	}

	if analysis, ok := ParseArticleAnalysis(feed.Labels); ok {
		add(analysis.PrimaryTopic)
		add(analysis.Format)
		for _, topic := range analysis.TopicsJSON {
			add(topic)
		}
	}

	for _, rawTag := range splitLooseTags(feed.Labels.Get(labelTags)) {
		add(rawTag)
	}

	add(feed.Labels.Get(model.LabelSource))

	return tags
}

func DefaultFeedbackTags(feed *model.Feed) []string {
	if feed == nil {
		return nil
	}

	if analysis, ok := ParseArticleAnalysis(feed.Labels); ok {
		var tags []string
		if strings.TrimSpace(analysis.PrimaryTopic) != "" {
			tags = append(tags, strings.TrimSpace(analysis.PrimaryTopic))
		}
		if strings.TrimSpace(analysis.Format) != "" {
			tags = append(tags, strings.TrimSpace(analysis.Format))
		}
		if len(tags) > 0 {
			return tags
		}
	}

	all := FeedSignalTags(feed)
	if len(all) > 2 {
		return all[:2]
	}

	return all
}

func BaseQualityScore(feed *model.Feed) int {
	if feed == nil {
		return 6
	}
	if analysis, ok := ParseArticleAnalysis(feed.Labels); ok && analysis.BaseQualityScore > 0 {
		return analysis.BaseQualityScore
	}

	return 6
}

func MatchesTag(control string, candidates []string) bool {
	needle := normalizeTag(control)
	if needle == "" {
		return false
	}

	for _, candidate := range candidates {
		current := normalizeTag(candidate)
		if current == "" {
			continue
		}
		if current == needle {
			return true
		}
		if strings.HasPrefix(current, needle+"/") || strings.HasPrefix(needle, current+"/") {
			return true
		}
	}

	return false
}

func splitLooseTags(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	return strings.FieldsFunc(raw, func(r rune) bool {
		switch r {
		case ',', '，', ';', '；', '|', '\n', '\t':
			return true
		default:
			return false
		}
	})
}

func normalizeTag(tag string) string {
	tag = strings.TrimSpace(strings.ToLower(tag))
	tag = strings.ReplaceAll(tag, " ", "")
	tag = strings.ReplaceAll(tag, "／", "/")
	tag = strings.ReplaceAll(tag, " / ", "/")

	return tag
}

func clampInt(v, minV, maxV int) int {
	if v < minV {
		return minV
	}
	if v > maxV {
		return maxV
	}

	return v
}
