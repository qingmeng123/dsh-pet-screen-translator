// Package translate 是 DeepSeek 客户端，提供两条通道：
//
//   - 图像：把 JPEG 以 data URL 发过去（视觉通道，实测 1280×960 约 780 prompt tokens）
//   - 文本：把 OCR 结果发过去（文本通道，约 140 tokens）
//
// 两条通道都支持流式：边生成边把累计译文回调出去，减少"等一整段"的体感延迟。
// key 不落日志；每次调用都把 usage 带回来，便于上层记成本。
package translate

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Usage 是 OpenAI 兼容接口返回的 token 统计。
type Usage struct {
	Prompt     int `json:"prompt_tokens"`
	Completion int `json:"completion_tokens"`
}

// Result 是一次翻译结果。
type Result struct {
	Text  string
	Usage Usage
}

// Client 调 DeepSeek 的 chat/completions。
type Client struct {
	BaseURL string
	APIKey  string
	Model   string
	// MaxTokens 0 = 不限制。注意：太小会把预算花在推理上，正文为空。
	MaxTokens int
	// ReasoningEffort "none" 可完全跳过思考（实测延迟从 2.8s 降到 ~0.7s）。
	ReasoningEffort string
	HTTPClient      *http.Client
}

// New 构造客户端；model 取自 DSH 当前对话模型（两者同名同源）。
func New(baseURL, apiKey, model string, maxTokens int, timeout time.Duration) *Client {
	return &Client{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		APIKey:     apiKey,
		Model:      model,
		MaxTokens:  maxTokens,
		HTTPClient: &http.Client{Timeout: timeout},
	}
}

type chatRequest struct {
	Model       string        `json:"model"`
	Temperature float64       `json:"temperature"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Reasoning   string        `json:"reasoning_effort,omitempty"`
	Stream      bool          `json:"stream,omitempty"`
	Messages    []chatMessage `json:"messages"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type textPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type imageURL struct {
	URL string `json:"url"`
}

type imagePart struct {
	Type     string   `json:"type"`
	ImageURL imageURL `json:"image_url"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage Usage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *Usage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// TranslateImage 用视觉通道翻译一张 JPEG。
func (c *Client) TranslateImage(ctx context.Context, jpeg []byte, system, prompt string) (Result, error) {
	return c.do(ctx, c.imageRequest(jpeg, system, prompt))
}

// TranslateText 用文本通道翻译 OCR 结果。
func (c *Client) TranslateText(ctx context.Context, text, system string) (Result, error) {
	return c.do(ctx, c.textRequest(text, system))
}

// StreamImage 流式翻译一张 JPEG；onUpdate 会收到**累计**译文（可能是空串开头）。
func (c *Client) StreamImage(ctx context.Context, jpeg []byte, system, prompt string, onUpdate func(string)) (Result, error) {
	return c.stream(ctx, c.imageRequest(jpeg, system, prompt), onUpdate)
}

// StreamText 流式翻译一段文本。
func (c *Client) StreamText(ctx context.Context, text, system string, onUpdate func(string)) (Result, error) {
	return c.stream(ctx, c.textRequest(text, system), onUpdate)
}

func (c *Client) imageRequest(jpeg []byte, system, prompt string) chatRequest {
	dataURL := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(jpeg)
	return chatRequest{
		Model:       c.Model,
		Temperature: 0.2,
		MaxTokens:   c.MaxTokens,
		Reasoning:   c.ReasoningEffort,
		Messages: []chatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: []any{
				textPart{Type: "text", Text: prompt},
				imagePart{Type: "image_url", ImageURL: imageURL{URL: dataURL}},
			}},
		},
	}
}

func (c *Client) textRequest(text, system string) chatRequest {
	return chatRequest{
		Model:       c.Model,
		Temperature: 0.2,
		MaxTokens:   c.MaxTokens,
		Reasoning:   c.ReasoningEffort,
		Messages: []chatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: text},
		},
	}
}

func (c *Client) do(ctx context.Context, req chatRequest) (Result, error) {
	res, raw, err := c.post(ctx, req)
	if err != nil {
		return Result{}, err
	}
	if res.StatusCode == 422 && strings.Contains(string(raw), "reasoning_effort") && req.Reasoning != "" {
		req.Reasoning = ""
		res, raw, err = c.post(ctx, req)
		if err != nil {
			return Result{}, err
		}
	}
	return parseChat(res, raw)
}

// stream 发流式请求，逐块解析 SSE，把累计文本回调出去。
func (c *Client) stream(ctx context.Context, req chatRequest, onUpdate func(string)) (Result, error) {
	req.Stream = true
	res, err := c.postStream(ctx, req)
	if err != nil {
		return Result{}, err
	}
	defer res.Body.Close()

	if res.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 8<<10))
		// 服务端不认 reasoning_effort：去掉重试一次
		if res.StatusCode == 422 && strings.Contains(string(raw), "reasoning_effort") && req.Reasoning != "" {
			req.Reasoning = ""
			res, err = c.postStream(ctx, req)
			if err != nil {
				return Result{}, err
			}
			defer res.Body.Close()
			if res.StatusCode >= 400 {
				raw, _ = io.ReadAll(io.LimitReader(res.Body, 8<<10))
				return Result{}, fmt.Errorf("api %d: %s", res.StatusCode, truncate(string(raw), 160))
			}
		} else {
			return Result{}, fmt.Errorf("api %d: %s", res.StatusCode, truncate(string(raw), 160))
		}
	}

	var (
		text    strings.Builder
		usage   Usage
		finish  string
		scanner = bufio.NewScanner(res.Body)
	)
	// 一帧可能比较长（含图片时 prompt 信息不回传，但 delta 可能较长），放宽缓冲
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}
		var ch streamChunk
		if err := json.Unmarshal([]byte(payload), &ch); err != nil {
			continue // 忽略无法解析的心跳/注释块
		}
		if ch.Error != nil {
			return Result{}, fmt.Errorf("stream error: %s", truncate(ch.Error.Message, 160))
		}
		if ch.Usage != nil {
			usage = *ch.Usage
		}
		if len(ch.Choices) == 0 {
			continue
		}
		if ch.Choices[0].FinishReason != "" {
			finish = ch.Choices[0].FinishReason
		}
		if d := ch.Choices[0].Delta.Content; d != "" {
			text.WriteString(d)
			if onUpdate != nil {
				onUpdate(text.String())
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return Result{}, err
	}

	out := strings.TrimSpace(text.String())
	if out == "" {
		return Result{}, fmt.Errorf("empty stream (finish=%s)", finish)
	}
	return Result{Text: out, Usage: usage}, nil
}

func (c *Client) post(ctx context.Context, req chatRequest) (*http.Response, []byte, error) {
	res, err := c.postStream(ctx, req)
	if err != nil {
		return nil, nil, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return nil, nil, err
	}
	return res, raw, nil
}

func (c *Client) postStream(ctx context.Context, req chatRequest) (*http.Response, error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	if req.Stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	}
	return c.HTTPClient.Do(httpReq)
}

func parseChat(res *http.Response, raw []byte) (Result, error) {
	var out chatResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return Result{}, fmt.Errorf("bad response (%d): %s", res.StatusCode, truncate(string(raw), 160))
	}
	if res.StatusCode >= 400 || out.Error != nil {
		msg := "unknown"
		if out.Error != nil {
			msg = out.Error.Message
		}
		return Result{}, fmt.Errorf("api %d: %s", res.StatusCode, truncate(msg, 160))
	}
	if len(out.Choices) == 0 {
		return Result{}, fmt.Errorf("empty choices")
	}
	text := strings.TrimSpace(out.Choices[0].Message.Content)
	if text == "" {
		return Result{}, fmt.Errorf("empty text (finish=%s reasoning=%d chars, usage=%d/%d)",
			out.Choices[0].FinishReason, len([]rune(out.Choices[0].Message.ReasoningContent)),
			out.Usage.Prompt, out.Usage.Completion)
	}
	return Result{Text: text, Usage: out.Usage}, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
