// Package slack は Slack の Web API を **読み取り専用** かつ **単一ワークスペース限定** で叩く。
//
// 安全装置は 2 つあり、どちらも「うっかり」では外れないようにしてある:
//   - メソッドの allowlist（method.go）: 型で閉じ、送信直前にも突き合わせる
//   - ワークスペース限定（本ファイル）: 接続先ホストは New で固定し、
//     リクエスト組み立ては do の 1 箇所だけに集約する
package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jiikko/slack-cli/internal/config"
)

const userAgent = "slack-cli (Chrome cookie session; read-only)"

// maxResponseBytes はレスポンスの読み取り上限（暴走した応答でメモリを食い潰さない）。
const maxResponseBytes = 32 * 1024 * 1024

// Client は https://<workspace>.slack.com/api/ だけを叩くクライアント。
type Client struct {
	http      *http.Client
	workspace string // acme
	host      string // acme.slack.com
	token     string // xoxc-…
	cookie    string // d cookie の値（xoxd-…）
}

// Option は Client の組み立てオプション（テストでの差し替え用）。
type Option func(*Client)

// WithHTTPClient は HTTP クライアントを差し替える。
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.http = h }
}

// newHTTPClient は資格情報を持ち越さないクライアントを作る。
//
// 🚨 Slack の API はリダイレクトを返さない。Go の既定は 3xx を追い、Cookie /
// Authorization は**別ドメインへは剥がれる**が **https→http のダウングレードでは
// 剥がれない**（stdlib はホスト名しか比較せず scheme を見ない）。
// ここでは追う理由が無いので、リダイレクトは一切追わずにエラーにする
// （= ワークスペース限定を 3xx で迂回されない）。
func newHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return fmt.Errorf("API がリダイレクト（%s）を返しました。資格情報を送らずに中止します", req.URL.Redacted())
		},
	}
}

// New は対象ワークスペース専用のクライアントを作る。
//
// workspace はここで検証し、以後 host は固定される。呼び出し側が URL を組み立てる
// 経路は無い（do 以外にリクエストを作る場所を作らないこと）。
func New(workspace, token, cookie string, opts ...Option) (*Client, error) {
	ws := config.NormalizeWorkspace(workspace)
	if err := config.ValidateWorkspace(ws); err != nil {
		return nil, err
	}
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("API トークン（xoxc-…）が空です")
	}
	if err := validateCookieValue(cookie); err != nil {
		return nil, err
	}
	c := &Client{
		http:      newHTTPClient(),
		workspace: ws,
		host:      ws + ".slack.com",
		token:     token,
		cookie:    cookie,
	}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

// Workspace は接続先ワークスペース（サブドメイン）を返す。
func (c *Client) Workspace() string { return c.workspace }

// Host は接続先ホストを返す。
func (c *Client) Host() string { return c.host }

// validateCookieValue は cookie 値がヘッダに載せて安全な文字だけかを確かめる。
//
// 🚨 値は Chrome の DB を復号して得たもの。制御文字（CR/LF）が混ざると
// ヘッダインジェクションの経路になるため、送信前に弾く。
func validateCookieValue(v string) error {
	if strings.TrimSpace(v) == "" {
		return fmt.Errorf("セッション cookie（d）が空です")
	}
	for i := 0; i < len(v); i++ {
		b := v[i]
		if b < 0x21 || b > 0x7e || b == '"' || b == ';' || b == ',' || b == '\\' {
			return fmt.Errorf("セッション cookie に使えない文字が含まれています（%d バイト目）", i+1)
		}
	}
	return nil
}

// APIError は ok:false のレスポンス。
type APIError struct {
	Method string
	Code   string
}

// 再ログインの案内を出すべきエラーコード（仕様 §8）。
var authErrorCodes = map[string]bool{
	"not_authed":       true,
	"invalid_auth":     true,
	"token_revoked":    true,
	"token_expired":    true,
	"account_inactive": true,
	"no_permission":    false, // 権限不足は再ログインでは直らない
}

// IsAuth は「Chrome で再ログインすれば直る」種類のエラーかを返す。
func (e *APIError) IsAuth() bool { return authErrorCodes[e.Code] }

func (e *APIError) Error() string {
	if e.IsAuth() {
		return fmt.Sprintf(
			"Slack の認証に失敗しました（%s: %s）。\n"+
				"  Chrome で対象ワークスペースに再ログインしてから、もう一度実行してください。\n"+
				"  ログイン後もこのエラーが続く場合は Chrome を完全に終了（cmd+Q）してから再実行してください。",
			e.Method, e.Code)
	}
	return fmt.Sprintf("Slack API エラー（%s: %s）", e.Method, e.Code)
}

// do は Slack API を 1 回呼ぶ。**HTTP リクエストを発行する唯一の場所**。
//
// 🚨 ここを増やさないこと。ホスト固定・allowlist・資格情報の載せ方は、この 1 箇所に
// 集約されていることで初めて「テストで担保できる」形になる。
// wiring_test.go が「http クライアントの Do 呼び出しはこの関数の中だけ」を AST で固定している。
func (c *Client) do(ctx context.Context, m Method, params url.Values) (json.RawMessage, error) {
	// ②: allowlist の実行時ガード（型で閉じていても package 内の新しいコードは通りうる）。
	if !isAllowed(m.name) {
		return nil, fmt.Errorf(
			"読み取り専用 allowlist に無いメソッドは呼べません: %q。\n"+
				"  このツールは読み取り専用です（internal/slack/method.go の allowlist を参照）", m.name)
	}

	endpoint := &url.URL{Scheme: "https", Host: c.host, Path: "/api/" + m.name}

	// 🚨 token は**必ずボディへ**。クエリ文字列に入れると、url.Error や
	// リダイレクト・プロキシのログなど「URL を含む文字列」すべてに載って漏れる。
	body := url.Values{}
	for k, vs := range params {
		body[k] = vs
	}
	body.Set("token", c.token)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), strings.NewReader(body.Encode()))
	if err != nil {
		return nil, err
	}

	// 最後の砦: 送信直前に接続先を確認する（組み立てを間違えても他ワークスペースへは出さない）。
	if req.URL.Scheme != "https" || req.URL.Host != c.host {
		return nil, fmt.Errorf("接続先が対象ワークスペース（%s）と異なります: %s", c.host, req.URL.Redacted())
	}

	req.Header.Set("Cookie", "d="+c.cookie)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("リクエスト失敗（%s）: %w", endpoint.Redacted(), err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, err
	}

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, &APIError{Method: m.name, Code: "invalid_auth"}
	case http.StatusTooManyRequests:
		retry := resp.Header.Get("Retry-After")
		if retry == "" {
			retry = "しばらく"
		} else {
			retry += " 秒"
		}
		return nil, fmt.Errorf("レート制限（429）: %s。%s待って再実行してください", m.name, retry)
	default:
		return nil, fmt.Errorf("予期しないステータス %d（%s）", resp.StatusCode, m.name)
	}

	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		// HTML が返る = ログインページ等（セッション切れ）の可能性が高い。
		return nil, fmt.Errorf("JSON ではない応答を受け取りました（%s）。Chrome で再ログインしてから試してください", m.name)
	}
	if !env.OK {
		code := env.Error
		if code == "" {
			code = "unknown_error"
		}
		return nil, &APIError{Method: m.name, Code: code}
	}
	return json.RawMessage(raw), nil
}

// call は do を呼び、結果を v へデコードする。
func (c *Client) call(ctx context.Context, m Method, params url.Values, v any) (json.RawMessage, error) {
	raw, err := c.do(ctx, m, params)
	if err != nil {
		return nil, err
	}
	if v != nil {
		if err := json.Unmarshal(raw, v); err != nil {
			return nil, fmt.Errorf("%s のレスポンス解析に失敗: %w", m.name, err)
		}
	}
	return raw, nil
}

// nextCursor は cursor ページングの次カーソルを取り出す。
func nextCursor(raw json.RawMessage) string {
	var env envelope
	if json.Unmarshal(raw, &env) != nil {
		return ""
	}
	return strings.TrimSpace(env.ResponseMetadata.NextCursor)
}
