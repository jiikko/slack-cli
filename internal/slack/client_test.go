package slack

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func newTestClient(t *testing.T, rt *recordingTransport) *Client {
	t.Helper()
	c, err := New("alpha", tokenForAlpha, sentinelCookie, withTransport(rt))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// トークンは必ずボディに載せ、URL には入れないこと。
//
// 🚨 URL に入れると url.Error・プロキシのログ・リダイレクト先など
// 「URL を含む文字列」すべてに載って構造的に漏れる。
func TestTokenGoesInBodyNotURL(t *testing.T) {
	rt := &recordingTransport{}
	c := newTestClient(t, rt)
	if _, err := c.AuthTest(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(rt.forms) != 1 {
		t.Fatalf("送信が %d 件", len(rt.forms))
	}
	if got := rt.forms[0].Get("token"); got != tokenForAlpha {
		t.Errorf("token がボディに無い: %q", got)
	}
	// 🚨 Host だけを見ると、クエリ文字列に token を載せる変異を素通しする（実測で踏んだ）。
	// 完全な URL で見ること。
	if strings.Contains(rt.urls[0], tokenForAlpha) {
		t.Errorf("token が URL に載っている: %s", maskAll(rt.urls[0]))
	}
	if strings.Contains(rt.urls[0], "token") {
		t.Errorf("token をクエリ文字列に置いている: %s", maskAll(rt.urls[0]))
	}
}

// Cookie は d だけを送ること（露出面を最小にする）。
func TestOnlyDCookieIsSent(t *testing.T) {
	rt := &recordingTransport{}
	c := newTestClient(t, rt)
	if _, err := c.AuthTest(context.Background()); err != nil {
		t.Fatal(err)
	}
	cookie := rt.headers[0].Get("Cookie")
	if cookie != "d="+sentinelCookie {
		t.Errorf("Cookie ヘッダが想定外: %q", maskAll(cookie))
	}
	if strings.Count(cookie, "=") != 1 {
		t.Errorf("d 以外の Cookie が載っている: %q", maskAll(cookie))
	}
}

// maskAll はテストの失敗メッセージにセンチネル値を出さないための保険。
func maskAll(s string) string {
	for _, secret := range []string{sentinelCookie, tokenForAlpha, tokenForBeta} {
		s = strings.ReplaceAll(s, secret, "<redacted>")
	}
	return s
}

// リクエストは常に POST で、対象ワークスペースの /api/<method> へ行くこと。
func TestRequestShape(t *testing.T) {
	rt := &recordingTransport{}
	c := newTestClient(t, rt)
	if _, err := c.Search(context.Background(), "hello", 5, 2); err != nil {
		t.Fatal(err)
	}
	if rt.hosts[0] != "alpha.slack.com" {
		t.Errorf("接続先: %q", rt.hosts[0])
	}
	if rt.methods[0] != "search.messages" {
		t.Errorf("メソッド: %q", rt.methods[0])
	}
	form := rt.forms[0]
	if form.Get("query") != "hello" || form.Get("count") != "5" || form.Get("page") != "2" {
		t.Errorf("パラメータが渡っていない: %v", form)
	}
}

// リダイレクトは一切追わないこと（3xx でワークスペース限定を迂回されない）。
func TestRedirectsAreRefused(t *testing.T) {
	policy := newHTTPClient().CheckRedirect
	if policy == nil {
		t.Fatal("CheckRedirect が設定されていない（既定のまま追ってしまう）")
	}
	req, _ := http.NewRequest(http.MethodPost, "https://beta.slack.com/api/auth.test", nil)
	if err := policy(req, nil); err == nil {
		t.Error("リダイレクトを追ってはいけない")
	}

	// 実サーバでも追わないこと（方針関数だけのテストは配線を守らない）。
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/moved", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	resp, err := newHTTPClient().Get(srv.URL + "/")
	if err == nil {
		resp.Body.Close()
		t.Error("リダイレクトが追われた")
	}
	if hits != 1 {
		t.Errorf("リダイレクト先まで到達した: hits=%d", hits)
	}
}

// ok:false のエラーコードを分類し、再ログイン案内を出すこと。
func TestAPIErrorClassification(t *testing.T) {
	cases := []struct {
		code     string
		wantAuth bool
	}{
		{"invalid_auth", true},
		{"not_authed", true},
		{"token_revoked", true},
		{"account_inactive", true},
		{"channel_not_found", false},
		{"ratelimited", false},
	}
	for _, c := range cases {
		rt := &recordingTransport{handle: func(string, url.Values) (int, string) {
			return 200, `{"ok":false,"error":"` + c.code + `"}`
		}}
		cl := newTestClient(t, rt)
		_, err := cl.AuthTest(context.Background())
		if err == nil {
			t.Fatalf("%s: エラーになるべき", c.code)
		}
		apiErr, ok := err.(*APIError)
		if !ok {
			t.Fatalf("%s: APIError であるべき: %T", c.code, err)
		}
		if apiErr.IsAuth() != c.wantAuth {
			t.Errorf("%s: IsAuth=%v, want %v", c.code, apiErr.IsAuth(), c.wantAuth)
		}
		if c.wantAuth && !strings.Contains(err.Error(), "再ログイン") {
			t.Errorf("%s: 再ログインの案内が無い: %v", c.code, err)
		}
	}
}

// HTTP ステータス由来の失敗を、意味の分かるエラーにすること。
func TestHTTPStatusHandling(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"401 は認証エラー", 401, "", "再ログイン"},
		{"429 はレート制限", 429, "", "レート制限"},
		{"500 はステータスを示す", 500, "", "予期しないステータス 500"},
		{"HTML が返ったら再ログイン案内", 200, "<!doctype html><html>", "JSON ではない応答"},
	}
	for _, c := range cases {
		rt := &recordingTransport{handle: func(string, url.Values) (int, string) { return c.status, c.body }}
		cl := newTestClient(t, rt)
		_, err := cl.AuthTest(context.Background())
		if err == nil {
			t.Errorf("%s: エラーになるべき", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: メッセージが想定と違う: %v", c.name, err)
		}
	}
}

// New は不正なワークスペース・資格情報を受け付けないこと。
func TestNewValidatesInputs(t *testing.T) {
	cases := []struct {
		name, ws, token, cookie string
	}{
		{"ワークスペースが空", "", tokenForAlpha, sentinelCookie},
		{"ワークスペースに別ホスト", "evil.example.com", tokenForAlpha, sentinelCookie},
		{"ワークスペースに /", "alpha/../beta", tokenForAlpha, sentinelCookie},
		{"トークンが空", "alpha", "", sentinelCookie},
		{"cookie が空", "alpha", tokenForAlpha, ""},
		{"cookie に改行（ヘッダインジェクション）", "alpha", tokenForAlpha, "abc\r\nX-Evil: 1"},
		{"cookie に区切り文字", "alpha", tokenForAlpha, "abc;def"},
	}
	for _, c := range cases {
		if _, err := New(c.ws, c.token, c.cookie); err == nil {
			t.Errorf("%s: 受け付けてはいけない", c.name)
		}
	}
	// URL 形式のワークスペースは正規化して受ける。
	cl, err := New("https://alpha.slack.com/", tokenForAlpha, sentinelCookie)
	if err != nil {
		t.Fatalf("正規化できるはず: %v", err)
	}
	if cl.Host() != "alpha.slack.com" {
		t.Errorf("host: %q", cl.Host())
	}
}

// チャンネル ID の判定（#name との取り違えで conversations.list を無駄に叩かない）。
func TestLooksLikeChannelID(t *testing.T) {
	yes := []string{"C0123456789", "GABCDEFGHIJ", "D01234567890"}
	no := []string{"#general", "general", "C012", "c0123456789", "C012345678-"}
	for _, s := range yes {
		if !looksLikeChannelID(s) {
			t.Errorf("%q は ID と判定すべき", s)
		}
	}
	for _, s := range no {
		if looksLikeChannelID(s) {
			t.Errorf("%q は ID と判定してはいけない", s)
		}
	}
}
