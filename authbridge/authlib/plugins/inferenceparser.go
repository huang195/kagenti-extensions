package plugins

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
)

// InferenceParser parses outbound OpenAI-compatible LLM inference requests
// and populates pctx.Extensions.Inference for downstream policy plugins.
type InferenceParser struct{}

func NewInferenceParser() *InferenceParser { return &InferenceParser{} }

func (p *InferenceParser) Name() string { return "inference-parser" }

func (p *InferenceParser) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{
		Writes:     []string{"inference"},
		BodyAccess: true,
	}
}

func (p *InferenceParser) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	if pctx.Path != "/v1/chat/completions" && pctx.Path != "/v1/completions" {
		return pipeline.Action{Type: pipeline.Continue}
	}

	if len(pctx.Body) == 0 {
		slog.Debug("inference-parser: no body, skipping")
		return pipeline.Action{Type: pipeline.Continue}
	}

	var req inferenceRequest
	if err := json.Unmarshal(pctx.Body, &req); err != nil {
		slog.Debug("inference-parser: invalid JSON", "error", err)
		return pipeline.Action{Type: pipeline.Continue}
	}

	ext := &pipeline.InferenceExtension{
		Model:       req.Model,
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
		Stream:      req.Stream,
	}

	for _, msg := range req.Messages {
		ext.Messages = append(ext.Messages, pipeline.InferenceMessage{
			Role:    msg.Role,
			Content: msg.Content,
		})
	}

	for _, tool := range req.Tools {
		if tool.Function.Name != "" {
			ext.Tools = append(ext.Tools, tool.Function.Name)
		}
	}

	pctx.Extensions.Inference = ext

	slog.Info("inference-parser", "model", ext.Model)
	slog.Debug("inference-parser: extracted", "model", ext.Model, "messages", len(ext.Messages), "stream", ext.Stream, "tools", len(ext.Tools))

	return pipeline.Action{Type: pipeline.Continue}
}

func (p *InferenceParser) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

type inferenceRequest struct {
	Model       string             `json:"model"`
	Messages    []inferenceMessage `json:"messages"`
	Temperature *float64           `json:"temperature"`
	MaxTokens   *int               `json:"max_tokens"`
	Stream      bool               `json:"stream"`
	Tools       []inferenceTool    `json:"tools"`
}

type inferenceMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type inferenceTool struct {
	Type     string            `json:"type"`
	Function inferenceFunction `json:"function"`
}

type inferenceFunction struct {
	Name string `json:"name"`
}
