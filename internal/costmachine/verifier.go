package costmachine

import (
	"bytes"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"os"
	"strings"
	"time"

	"github.com/j-s-te/reimbursement/internal/config"
)

var ErrToken = errors.New("invalid cost machine token")

type Verifier struct {
	options config.CostMachine
	key     ed25519.PublicKey
}
type claims struct {
	Issuer          string   `json:"iss"`
	Audience        string   `json:"aud"`
	Subject         string   `json:"sub"`
	OAuthClientID   string   `json:"oauth_client_id"`
	ApplicationID   string   `json:"application_id"`
	EnvironmentID   string   `json:"environment_id"`
	TenantID        string   `json:"tenant_id"`
	ApplicationCode string   `json:"application_code"`
	EnvironmentCode string   `json:"environment_code"`
	TokenUse        string   `json:"token_use"`
	Scopes          []string `json:"scope"`
	IssuedAt        int64    `json:"iat"`
	NotBefore       int64    `json:"nbf"`
	ExpiresAt       int64    `json:"exp"`
}

// New 加载此用途唯一信任配置与平台验签公钥。
// 参数：options为启动期已校验的独立机器配置。
// 返回值：严格验签器；关闭用途返回nil，公钥不可用返回错误。
func New(options config.CostMachine) (*Verifier, error) {
	if !options.Enabled {
		return nil, nil
	}
	raw, err := os.ReadFile(options.PublicKeyPath)
	if err != nil {
		return nil, ErrToken
	}
	block, rest := pem.Decode(raw)
	if block == nil || len(bytes.TrimSpace(rest)) != 0 {
		return nil, ErrToken
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, ErrToken
	}
	key, ok := parsed.(ed25519.PublicKey)
	if !ok {
		return nil, ErrToken
	}
	return &Verifier{options: options, key: key}, nil
}

// uniqueJSON 拒绝重复顶层字段及尾随内容，避免不同消费者对签名声明作不同解释。
// 参数：raw为JSON对象，dest为目标声明。
// 返回值：解码错误或无歧义声明。
func uniqueJSON(raw []byte, dest any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	start, err := dec.Token()
	if err != nil || start != json.Delim('{') {
		return ErrToken
	}
	seen := map[string]bool{}
	for dec.More() {
		key, e := dec.Token()
		if e != nil {
			return ErrToken
		}
		name, ok := key.(string)
		if !ok || seen[name] {
			return ErrToken
		}
		seen[name] = true
		var value json.RawMessage
		if dec.Decode(&value) != nil {
			return ErrToken
		}
	}
	if _, err = dec.Token(); err != nil {
		return ErrToken
	}
	var tail any
	if dec.Decode(&tail) != io.EOF {
		return ErrToken
	}
	return json.Unmarshal(raw, dest)
}

// Verify 严格校验签名、注册身份、唯一scope及有效时间后仅返回可信租户。
// 参数：raw为平台签发机器JWT，now为服务器当前UTC时间。
// 返回值：签名租户；任意声明漂移均失败关闭。
func (v *Verifier) Verify(raw string, now time.Time) (string, error) {
	if v == nil || len(raw) > 16384 {
		return "", ErrToken
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return "", ErrToken
	}
	header, e := base64.RawURLEncoding.Strict().DecodeString(parts[0])
	if e != nil {
		return "", ErrToken
	}
	var h struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
	}
	if uniqueJSON(header, &h) != nil || h.Alg != "EdDSA" || h.Typ != "JWT" {
		return "", ErrToken
	}
	signature, e := base64.RawURLEncoding.Strict().DecodeString(parts[2])
	if e != nil || !ed25519.Verify(v.key, []byte(parts[0]+"."+parts[1]), signature) {
		return "", ErrToken
	}
	payload, e := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if e != nil {
		return "", ErrToken
	}
	var c claims
	if uniqueJSON(payload, &c) != nil {
		return "", ErrToken
	}
	o := v.options
	if c.Issuer != o.Issuer || c.Audience != o.Audience || c.Subject != o.ClientID || c.OAuthClientID != o.OAuthClientID || c.ApplicationID != o.ApplicationID || c.EnvironmentID != o.EnvironmentID || c.TenantID != o.TenantID || c.ApplicationCode != "settlement" || c.ApplicationCode != o.CallerApplicationCode || c.EnvironmentCode != o.CallerEnvironmentCode || c.TokenUse != "application" || len(c.Scopes) != 1 || c.Scopes[0] != "reimbursement.cost_candidate.read" {
		return "", ErrToken
	}
	if c.IssuedAt <= 0 || c.IssuedAt > now.Unix() || c.NotBefore < c.IssuedAt || c.NotBefore > now.Unix() || c.ExpiresAt <= now.Unix() || c.ExpiresAt <= c.NotBefore || c.ExpiresAt-c.IssuedAt > 900 {
		return "", ErrToken
	}
	return c.TenantID, nil
}
