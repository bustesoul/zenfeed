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

package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/pkg/errors"
	oai "github.com/sashabaranov/go-openai"

	"github.com/glidea/zenfeed/pkg/component"
	"github.com/glidea/zenfeed/pkg/model"
	"github.com/glidea/zenfeed/pkg/telemetry"
	telemetrymodel "github.com/glidea/zenfeed/pkg/telemetry/model"
	runtimeutil "github.com/glidea/zenfeed/pkg/util/runtime"
)

type openaiResponses struct {
	*component.Base[Config, struct{}]
	text
}

func newOpenAIResponses(c *Config) LLM {
	oaiConfig := oai.DefaultConfig(c.APIKey)
	oaiConfig.BaseURL = c.Endpoint
	oaiClient := oai.NewClientWithConfig(oaiConfig)
	embeddingSpliter := newEmbeddingSpliter(1536, 64)

	base := component.New(&component.BaseConfig[Config, struct{}]{
		Name:     "LLM/openai-responses",
		Instance: c.Name,
		Config:   c,
	})

	return &openaiResponses{
		Base: base,
		text: &openaiResponsesText{
			Base:             base,
			apiKey:           c.APIKey,
			baseURL:          c.Endpoint,
			oaiClient:        oaiClient,
			embeddingSpliter: embeddingSpliter,
		},
	}
}

func (o *openaiResponses) WAV(_ context.Context, _ string, _ []Speaker) (io.ReadCloser, error) {
	return nil, errors.New("not supported")
}

type openaiResponsesText struct {
	*component.Base[Config, struct{}]

	apiKey           string
	baseURL          string
	oaiClient        *oai.Client
	embeddingSpliter embeddingSpliter
}

type responsesRequest struct {
	Model string             `json:"model"`
	Input []responsesMessage `json:"input"`
}

type responsesMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type responsesResponse struct {
	Output []responsesOutput `json:"output"`
	Usage  responsesUsage    `json:"usage"`
}

type responsesOutput struct {
	Type    string             `json:"type"`
	Content []responsesContent `json:"content"`
}

type responsesContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type responsesUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

func (o *openaiResponsesText) String(ctx context.Context, messages []string) (value string, err error) {
	ctx = telemetry.StartWith(ctx, append(o.TelemetryLabels(), telemetrymodel.KeyOperation, "String")...)
	defer func() { telemetry.End(ctx, err) }()

	config := o.Config()
	if config.Model == "" {
		return "", errors.New("model is not set")
	}

	input := make([]responsesMessage, 0, len(messages))
	for _, m := range messages {
		input = append(input, responsesMessage{Role: "user", Content: m})
	}

	bodyBytes, err := json.Marshal(responsesRequest{Model: config.Model, Input: input})
	if err != nil {
		return "", errors.Wrap(err, "marshal request")
	}

	endpoint := strings.TrimRight(o.baseURL, "/") + "/responses"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", errors.Wrap(err, "create request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+o.apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", errors.Wrap(err, "do request")
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)

		return "", errors.Errorf("responses API error %d: %s", resp.StatusCode, body)
	}

	var result responsesResponse
	if err = json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", errors.Wrap(err, "decode response")
	}

	for _, out := range result.Output {
		if out.Type != "message" {
			continue
		}
		for _, content := range out.Content {
			if content.Type == "output_text" && content.Text != "" {
				lvs := []string{o.Name(), o.Instance(), "String"}
				promptTokens.WithLabelValues(lvs...).Add(float64(result.Usage.InputTokens))
				completionTokens.WithLabelValues(lvs...).Add(float64(result.Usage.OutputTokens))
				totalTokens.WithLabelValues(lvs...).Add(float64(result.Usage.TotalTokens))

				return content.Text, nil
			}
		}
	}

	return "", errors.New("no text content in responses output")
}

func (o *openaiResponsesText) EmbeddingLabels(ctx context.Context, labels model.Labels) (value [][]float32, err error) {
	ctx = telemetry.StartWith(ctx, append(o.TelemetryLabels(), telemetrymodel.KeyOperation, "EmbeddingLabels")...)
	defer func() { telemetry.End(ctx, err) }()

	config := o.Config()
	if config.EmbeddingModel == "" {
		return nil, errors.New("embedding model is not set")
	}
	splits, err := o.embeddingSpliter.Split(labels)
	if err != nil {
		return nil, errors.Wrap(err, "split embedding")
	}

	vecs := make([][]float32, 0, len(splits))
	for _, split := range splits {
		text := runtimeutil.Must1(json.Marshal(split))
		vec, err := o.Embedding(ctx, string(text))
		if err != nil {
			return nil, errors.Wrap(err, "embedding")
		}
		vecs = append(vecs, vec)
	}

	return vecs, nil
}

func (o *openaiResponsesText) Embedding(ctx context.Context, s string) (value []float32, err error) {
	ctx = telemetry.StartWith(ctx, append(o.TelemetryLabels(), telemetrymodel.KeyOperation, "Embedding")...)
	defer func() { telemetry.End(ctx, err) }()

	config := o.Config()
	if config.EmbeddingModel == "" {
		return nil, errors.New("embedding model is not set")
	}
	vec, err := o.oaiClient.CreateEmbeddings(ctx, oai.EmbeddingRequest{
		Input:          []string{s},
		Model:          oai.EmbeddingModel(config.EmbeddingModel),
		EncodingFormat: oai.EmbeddingEncodingFormatFloat,
	})
	if err != nil {
		return nil, errors.Wrap(err, "create embeddings")
	}
	if len(vec.Data) == 0 {
		return nil, errors.New("no embedding data returned")
	}

	lvs := []string{o.Name(), o.Instance(), "Embedding"}
	promptTokens.WithLabelValues(lvs...).Add(float64(vec.Usage.PromptTokens))
	completionTokens.WithLabelValues(lvs...).Add(float64(vec.Usage.CompletionTokens))
	totalTokens.WithLabelValues(lvs...).Add(float64(vec.Usage.TotalTokens))

	return vec.Data[0].Embedding, nil
}
