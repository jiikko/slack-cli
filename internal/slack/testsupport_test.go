package slack

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/jiikko/slack-cli/internal/auth"
)

// recordingTransport は「実際にどのホストへ、何を送ったか」を記録する差し替え用 Transport。
//
// 🚨 ワークスペース限定の検証は、組み立てた URL 文字列を比較するのでは足りない
// （別経路で直に POST する変異を素通しする）。到達したホストの集合を記録して、
// 対象ワークスペース以外が 1 件も無いことを見る。
type recordingTransport struct {
	mu      sync.Mutex
	hosts   []string
	urls    []string // 完全な URL。token が URL に載る変異を検出するために必要
	methods []string
	forms   []url.Values
	headers []http.Header

	// handle は (メソッド名, フォーム値) を受けて JSON 本文を返す。
	handle func(method string, form url.Values) (status int, body string)
	// err を返すと通信エラーを模す。
	err error
}

func (rt *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(req.Body)
	form, _ := url.ParseQuery(string(body))
	method := strings.TrimPrefix(req.URL.Path, "/api/")

	rt.mu.Lock()
	rt.hosts = append(rt.hosts, req.URL.Host)
	rt.urls = append(rt.urls, req.URL.String())
	rt.methods = append(rt.methods, method)
	rt.forms = append(rt.forms, form)
	rt.headers = append(rt.headers, req.Header.Clone())
	rt.mu.Unlock()

	if rt.err != nil {
		return nil, rt.err
	}
	status, s := 200, `{"ok":true}`
	if rt.handle != nil {
		status, s = rt.handle(method, form)
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(s)),
		Request:    req,
	}, nil
}

// uniqueHosts は到達したホストの集合を返す。
func (rt *recordingTransport) uniqueHosts() []string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for _, h := range rt.hosts {
		if !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	return out
}

// withTransport は Client に差し替え Transport を挿す Option を返す。
func withTransport(rt *recordingTransport) Option {
	return WithHTTPClient(&http.Client{Transport: rt})
}

// authTestBody は auth.test の応答を作る。
func authTestBody(domain, team, user string) string {
	return fmt.Sprintf(`{"ok":true,"url":"https://%s.slack.com/","team":%q,"team_id":"T1","user":%q,"user_id":"U1"}`,
		domain, team, user)
}

// fakeCreds は Chrome にも Keychain にも触れずに解決経路を回すための資格情報源。
type fakeCreds struct {
	profiles []auth.Profile
	cookies  map[string]string   // profile -> d cookie
	tokens   map[string][]string // profile -> xoxc トークン候補（順序つき）
	tokenErr error
}

func (f fakeCreds) Profiles() []auth.Profile { return f.profiles }

func (f fakeCreds) Cookie(profile, host string) (string, error) {
	v, ok := f.cookies[profile]
	if !ok {
		return "", fmt.Errorf("%s 宛ての cookie がプロファイル %q にありません", host, profile)
	}
	return v, nil
}

func (f fakeCreds) Tokens(profile, workspace string) ([]string, error) {
	if f.tokenErr != nil {
		return nil, f.tokenErr
	}
	return f.tokens[profile], nil
}
