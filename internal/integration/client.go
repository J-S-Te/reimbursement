package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

var ErrUnavailable = errors.New("integration unavailable")

type Client struct {
	PlatformURL, BaseURL, ClientID, Secret, Scope string
	HTTP                                          *http.Client
	mu                                            sync.Mutex
	token                                         string
	expires                                       time.Time
}

// NewClient 创建独立用途机器客户端，禁止自动跟随跨源重定向。
// 参数：platform 为控制面；base 为目标服务；id、secret 为服务凭据；scope 为精确权限集合。
// 返回值：有超时边界的机器客户端。
func NewClient(platform, base, id, secret, scope string) *Client {
	return &Client{PlatformURL: strings.TrimRight(platform, "/"), BaseURL: strings.TrimRight(base, "/"), ClientID: id, Secret: secret, Scope: scope,
		HTTP: &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

// scopeSet 将空格分隔权限归一为排序集合，同时拒绝重复权限。
// 参数：value 为OAuth scope文本。
// 返回值：规范集合文本；重复或空集合返回空串。
func scopeSet(value string) string {
	values := strings.Fields(value)
	sort.Strings(values)
	for i, v := range values {
		if i > 0 && v == values[i-1] {
			return ""
		}
	}
	return strings.Join(values, " ")
}

// Token 获取并短时缓存单一用途的精确scope令牌。
// 参数：ctx 为超时及取消边界。
// 返回值：不外泄的Bearer令牌；配置、认证或scope漂移时返回错误。
func (c *Client) Token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ClientID == "" || c.Secret == "" || scopeSet(c.Scope) == "" {
		return "", ErrUnavailable
	}
	if c.token != "" && time.Until(c.expires) > 30*time.Second {
		return c.token, nil
	}
	form := url.Values{"grant_type": {"client_credentials"}, "scope": {c.Scope}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.PlatformURL+"/oauth2/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", ErrUnavailable
	}
	req.SetBasicAuth(c.ClientID, c.Secret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", ErrUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", ErrUnavailable
	}
	var body struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		Scope       string `json:"scope"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body) != nil || body.AccessToken == "" || !strings.EqualFold(body.TokenType, "Bearer") || body.ExpiresIn < 1 || body.ExpiresIn > 86400 || scopeSet(body.Scope) != scopeSet(c.Scope) {
		return "", ErrUnavailable
	}
	c.token = body.AccessToken
	c.expires = time.Now().Add(time.Duration(body.ExpiresIn) * time.Second)
	return c.token, nil
}

// Call 调用受控服务路径并读取有界JSON信封。
// 参数：ctx 为调用边界；method/path为固定协议路径；input为服务端组装报文。
// 返回值：data字段；拒绝重定向、非JSON、超限及非成功响应。
func (c *Client) Call(ctx context.Context, method, path string, input any) (json.RawMessage, error) {
	token, err := c.Token(ctx)
	if err != nil {
		return nil, err
	}
	var content []byte
	if input != nil {
		content, err = json.Marshal(input)
		if err != nil {
			return nil, err
		}
	}
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		return nil, ErrUnavailable
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, bytes.NewReader(content))
	if err != nil {
		return nil, ErrUnavailable
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("%w: HTTP %d", ErrUnavailable, resp.StatusCode)
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "json") {
		return nil, ErrUnavailable
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if err != nil || len(raw) > 4<<20 {
		return nil, ErrUnavailable
	}
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &envelope) != nil || len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return nil, ErrUnavailable
	}
	return envelope.Data, nil
}
