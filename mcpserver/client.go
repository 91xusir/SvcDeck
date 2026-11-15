package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const defaultAPIURL = "http://127.0.0.1:8088"

// Config 是 MCP 模式连接面板所需的配置。
// MCP 模式不读写 SQLite，只通过面板 HTTP API 操作，因此不需要 -data/-db。
type Config struct {
	BaseURL string
	Token   string
	Timeout time.Duration
	// TokenSource 可选：面板运行中改 token 时，HTTP MCP 内部调用应立即使用新值。
	// 为空时使用 Token。
	TokenSource func() string
}

// LoadConfig 从环境变量读取配置，供 SvcDeck.exe -mcp 使用。
// 命令行覆盖（-url / -token）由根包 main.go 在调用 Run 前处理。
func LoadConfig() (Config, error) {
	base := strings.TrimSpace(os.Getenv("SVCDECK_URL"))
	if base == "" {
		base = defaultAPIURL
	}

	timeout := 15 * time.Second
	if raw := strings.TrimSpace(os.Getenv("SVCDECK_TIMEOUT_SEC")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 120 {
			return Config{}, fmt.Errorf("SVCDECK_TIMEOUT_SEC 必须是 1..120 的整数")
		}
		timeout = time.Duration(n) * time.Second
	}

	return Config{
		BaseURL: base,
		Token:   strings.TrimSpace(os.Getenv("SVCDECK_TOKEN")),
		Timeout: timeout,
	}, nil
}

func (c Config) normalized() (Config, error) {
	if c.BaseURL == "" {
		c.BaseURL = defaultAPIURL
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil {
		return Config{}, fmt.Errorf("SVCDECK_URL 无效: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return Config{}, fmt.Errorf("SVCDECK_URL 必须是 http/https 地址: %s", c.BaseURL)
	}
	if u.Host == "" {
		return Config{}, fmt.Errorf("SVCDECK_URL 缺少主机名: %s", c.BaseURL)
	}
	if c.Timeout <= 0 {
		c.Timeout = 15 * time.Second
	}
	if c.Timeout > 120*time.Second {
		return Config{}, fmt.Errorf("SVCDECK_TIMEOUT_SEC 不能超过 120")
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
			msg += "（检查 SVCDECK_TOKEN 环境变量）"
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
