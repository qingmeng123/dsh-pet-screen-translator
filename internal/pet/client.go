// Package pet 是 dsh-pet 的 HTTP 客户端：菜单、点击轮询、占用锁、气泡。
//
// 只依赖标准库；所有请求都带超时，失败返回带状态码的错误，由上层决定重试还是放弃。
package pet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Client 访问同一台机器上的 dsh-pet。
type Client struct {
	base string
	http *http.Client
}

// New 构造客户端。port 是 DSH Web 服务端口，basePath 一般是 /dsh-pet-7340。
func New(port int, basePath string) *Client {
	return &Client{
		base: fmt.Sprintf("http://127.0.0.1:%d%s", port, basePath),
		http: &http.Client{Timeout: 20 * time.Second},
	}
}

// MenuEvent 是一次点击的结果：Label 为被点的菜单文字，PetID 为哪只宠物。
type MenuEvent struct {
	Label string
	PetID string
}

// RegisterMenu 注册（覆盖）某个 owner 的菜单项。
func (c *Client) RegisterMenu(ctx context.Context, owner string, labels []string, target string) error {
	body := map[string]any{"owner": owner, "labels": labels}
	if target != "" {
		body["target"] = target
	}
	var out struct {
		OK bool `json:"ok"`
	}
	return c.postJSON(ctx, "/menu/item", body, &out)
}

// PollEvents 长轮询取一次点击；没有点击时返回 nil, nil。
func (c *Client) PollEvents(ctx context.Context, owner string, wait time.Duration) (*MenuEvent, error) {
	url := fmt.Sprintf("%s/menu/events?owner=%s&wait=%d", c.base, owner, wait.Milliseconds())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	var payload struct {
		Events map[string]any `json:"events"`
	}
	if err := json.NewDecoder(res.Body).Decode(&payload); err != nil {
		return nil, err
	}
	for label, v := range payload.Events {
		petID, _ := v.(string)
		return &MenuEvent{Label: label, PetID: petID}, nil
	}
	return nil, nil
}

// Lock 取占用锁，返回 lockId。
func (c *Client) Lock(ctx context.Context, petID string, ttl time.Duration) (string, error) {
	var out struct {
		OK     bool   `json:"ok"`
		LockID string `json:"lockId"`
		Reason string `json:"reason"`
	}
	url := fmt.Sprintf("/lock?pet=%s&surface=any", petID)
	if err := c.postJSON(ctx, url, map[string]any{"ttl": ttl.Milliseconds()}, &out); err != nil {
		return "", err
	}
	if !out.OK || out.LockID == "" {
		return "", fmt.Errorf("lock refused: %s", out.Reason)
	}
	return out.LockID, nil
}

// Release 释放占用锁（幂等：锁已没了也当成功）。
func (c *Client) Release(ctx context.Context, petID, lockID string) error {
	var out struct {
		OK bool `json:"ok"`
	}
	return c.postJSON(ctx, "/lock/release?pet="+petID, map[string]any{"lockId": lockID}, &out)
}

// Broadcast 往气泡里写一句话（需锁）。
//
// speak=true 时让桌宠念出来；dsh-pet 要求 surface 指定到具体一端（desktop / web）
// 才会写 voiceTo，传 any 时只弹气泡。surface 为空按 any 处理。
func (c *Client) Broadcast(ctx context.Context, petID, lockID, text string, duration time.Duration, speak bool, surface string) error {
	var out struct {
		OK     bool   `json:"ok"`
		Reason string `json:"reason"`
	}
	if surface == "" {
		surface = "any"
	}
	path := fmt.Sprintf("/broadcast?pet=%s&surface=%s&lock=%s", petID, url.QueryEscape(surface), lockID)
	body := map[string]any{"text": text, "duration": duration.Milliseconds(), "anim": false}
	if speak {
		body["speak"] = true
	}
	if err := c.postJSON(ctx, path, body, &out); err != nil {
		return err
	}
	if !out.OK {
		return fmt.Errorf("broadcast refused: %s", out.Reason)
	}
	return nil
}

func (c *Client) postJSON(ctx context.Context, path string, body any, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return err
	}
	if res.StatusCode >= 400 {
		return fmt.Errorf("http %d: %s", res.StatusCode, string(data))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}
