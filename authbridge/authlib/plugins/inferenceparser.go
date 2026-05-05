package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"

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

// OnResponse populates the response-side fields (Completion, FinishReason,
// token counts) on pctx.Extensions.Inference. Handles both non-streaming
// JSON responses and SSE streams from OpenAI-compatible servers.
func (p *InferenceParser) OnResponse(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	if len(pctx.ResponseBody) == 0 || pctx.Extensions.Inference == nil {
		return pipeline.Action{Type: pipeline.Continue}
	}

	if pctx.Extensions.Inference.Stream {
		parseInferenceSSE(pctx.ResponseBody, pctx.Extensions.Inference)
	} else {
		parseInferenceJSON(pctx.ResponseBody, pctx.Extensions.Inference)
	}

	ext := pctx.Extensions.Inference
	slog.Info("inference-parser: response",
		"model", ext.Model,
		"finishReason", ext.FinishReason,
		"promptTokens", ext.PromptTokens,
		"completionTokens", ext.CompletionTokens,
	)
	return pipeline.Action{Type: pipeline.Continue}
}

// parseInferenceJSON parses a non-streaming OpenAI chat/completions response.
func parseInferenceJSON(body []byte, ext *pipeline.InferenceExtension) {
	var resp inferenceResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		slog.Debug("inference-parser: invalid response JSON", "error", err)
		return
	}
	if len(resp.Choices) > 0 {
		ext.Completion = resp.Choices[0].Message.Content
		ext.FinishReason = resp.Choices[0].FinishReason
	}
	ext.PromptTokens = resp.Usage.PromptTokens
	ext.CompletionTokens = resp.Usage.CompletionTokens
	ext.TotalTokens = resp.Usage.TotalTokens
}

// parseInferenceSSE concatenates content deltas across SSE events and captures
// the last finish_reason and usage block (sent when stream_options.include_usage
// is set). The stream terminates with a "data: [DONE]" marker which is skipped.
func parseInferenceSSE(body []byte, ext *pipeline.InferenceExtension) {
	var completion strings.Builder
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		var chunk inferenceStreamChunk
		if json.Unmarshal(data, &chunk) != nil {
			continue
		}
		for _, c := range chunk.Choices {
			if c.Delta.Content != "" {
				completion.WriteString(c.Delta.Content)
			}
			if c.FinishReason != "" {
				ext.FinishReason = c.FinishReason
			}
		}
		if chunk.Usage.TotalTokens > 0 {
			ext.PromptTokens = chunk.Usage.PromptTokens
			ext.CompletionTokens = chunk.Usage.CompletionTokens
			ext.TotalTokens = chunk.Usage.TotalTokens
		}
	}
	ext.Completion = completion.String()
}

type inferenceResponse struct {
	Choices []inferenceChoice `json:"choices"`
	Usage   inferenceUsage    `json:"usage"`
}

type inferenceChoice struct {
	Message      inferenceMessage `json:"message"`
	FinishReason string           `json:"finish_reason"`
}

type inferenceStreamChunk struct {
	Choices []inferenceStreamChoice `json:"choices"`
	Usage   inferenceUsage          `json:"usage"`
}

type inferenceStreamChoice struct {
	Delta        inferenceDelta `json:"delta"`
	FinishReason string         `json:"finish_reason"`
}

type inferenceDelta struct {
	Content string `json:"content"`
}

type inferenceUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
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
