package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const defaultAPIURL = "http://127.0.0.1:8088"

// Config 是内置 MCP HTTP 端点连接面板所需的配置。
// MCP 只通过面板 REST API 操作，不直接读写 SQLite。
type Config struct {
	BaseURL string
	Token   string
	Timeout time.Duration
	// TokenSource 可选：面板运行中改 token 时，MCP 内部调用应立即使用新值。
	// 为空时使用 Token。
	TokenSource func() string
}

func (c Config) normalized() (Config, error) {
	if c.BaseURL == "" {
		c.BaseURL = defaultAPIURL
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil {
		return Config{}, fmt.Errorf("面板地址无效: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return Config{}, fmt.Errorf("面板地址必须是 http/https: %s", c.BaseURL)
	}
	if u.Host == "" {
		return Config{}, fmt.Errorf("面板地址缺少主机名: %s", c.BaseURL)
	}
	if c.Timeout <= 0 {
		c.Timeout = 15 * time.Second
	}
	if c.Timeout > 10*time.Minute {
		return Config{}, fmt.Errorf("MCP 请求超时不能超过 10m")
	}
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
	c.Token = strings.TrimSpace(c.Token)
	return c, nil
}

type apiClient struct {
	baseURL     string
	token       string
	tokenSource func() string
	hc          *http.Client
}

func newAPIClient(cfg Config) (*apiClient, error) {
	cfg, err := cfg.normalized()
	if err != nil {
		return nil, err
	}
	return &apiClient{
		baseURL:     cfg.BaseURL,
		token:       cfg.Token,
		tokenSource: cfg.TokenSource,
		hc: &http.Client{
			Timeout: cfg.Timeout,
		},
	}, nil
}

func (c *apiClient) do(ctx context.Context, method, path string, query url.Values, body any) (json.RawMessage, error) {
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("序列化请求体失败: %w", err)
		}
		reader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, u, reader)
	if err != nil {
		return nil, fmt.Errorf("构造请求失败: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	token := c.token
	if c.tokenSource != nil {
		token = c.tokenSource()
	}
	if token != "" {
		req.Header.Set("X-Panel-Token", token)
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 %s %s 失败: %w", method, path, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(data))
		var er struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &er) == nil && er.Error != "" {
			msg = er.Error
		}
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		if resp.StatusCode == http.StatusUnauthorized {
			msg += "（检查面板访问令牌）"
		}
		return nil, fmt.Errorf("SvcDeck API %s %s 返回 HTTP %d: %s", method, path, resp.StatusCode, msg)
	}
	return json.RawMessage(data), nil
}

func decodeRaw(raw json.RawMessage, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	return dec.Decode(dst)
}

func servicePath(id string) string {
	return "/api/services/" + url.PathEscape(id)
}
