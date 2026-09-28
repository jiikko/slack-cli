package slack

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/jiikko/slack-cli/internal/auth"
	"github.com/jiikko/slack-cli/internal/config"
)

// streamTransport は本文をストリームで返す Transport（巨大な応答をテスト側で確保せずに作る）。
type streamTransport struct {
	status int
	header http.Header
	body   func() io.Reader
}

func (st *streamTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		_, _ = io.Copy(io.Discard, req.Body)
	}
	h := st.header
	if h == nil {
		h = http.Header{}
	}
	return &http.Response{
		StatusCode: st.status,
		Header:     h,
		Body:       io.NopCloser(st.body()),
		Request:    req,
	}, nil
}

func clientWith(t *testing.T, rt http.RoundTripper) *Client {
	t.Helper()
	c, err := New("alpha", tokenForAlpha, sentinelCookie, WithHTTPClient(&http.Client{Transport: rt}))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// jsonOfSize は ok:true の JSON を、ちょうど size バイトになるよう組み立てて返す。
func jsonOfSize(size int64) io.Reader {
	prefix, suffix := `{"ok":true,"pad":"`, `"}`
	pad := size - int64(len(prefix)+len(suffix))
	return io.MultiReader(strings.NewReader(prefix), io.LimitReader(repeatReader('a'), pad), strings.NewReader(suffix))
}

type repeatReader byte

func (r repeatReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(r)
	}
	return len(p), nil
}

// 🚨 上限を超えた応答を黙って切り詰めないこと。
//
// 切り詰めた本文は JSON として壊れているので、「JSON ではない応答 = 再ログインして」
// という誤った案内に化ける。上限ちょうどは受け付ける（境界の取り違えを防ぐ）。
func TestOversizedResponseIsReported(t *testing.T) {
	over := clientWith(t, &streamTransport{status: 200, body: func() io.Reader { return jsonOfSize(maxResponseBytes + 1) }})
	_, err := over.AuthTest(context.Background())
	var tooLarge *ResponseTooLargeError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("上限超過として分類されていない: %T %v", err, err)
	}
	if strings.Contains(err.Error(), "再ログイン") {
		t.Errorf("上限超過を再ログインの案内に化けさせている: %v", err)
	}

	exact := clientWith(t, &streamTransport{status: 200, body: func() io.Reader { return jsonOfSize(maxResponseBytes) }})
	if _, err := exact.AuthTest(context.Background()); err != nil {
		t.Errorf("上限ちょうどの応答を拒否した: %v", err)
	}
}

// Retry-After は秒数か HTTP-date（RFC 9110 §10.2.3）。整数のときだけ「秒」を付けること。
func TestRetryAfterFormats(t *testing.T) {
	cases := []struct {
		header  string
		want    string
		notWant string
	}{
		{"30", "30 秒待って", ""},
		{"Wed, 21 Oct 2015 07:28:00 GMT", `"Wed, 21 Oct 2015 07:28:00 GMT"`, "GMT 秒"},
		{"", "しばらく待って", " 秒"},
	}
	for _, c := range cases {
		h := http.Header{}
		if c.header != "" {
			h.Set("Retry-After", c.header)
		}
		cl := clientWith(t, &streamTransport{status: 429, header: h, body: func() io.Reader { return strings.NewReader("") }})
		_, err := cl.AuthTest(context.Background())
		if err == nil {
			t.Fatalf("%q: エラーになるべき", c.header)
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: %q を含むべき: %v", c.header, c.want, err)
		}
		if c.notWant != "" && strings.Contains(err.Error(), c.notWant) {
			t.Errorf("%q: %q を含んではいけない: %v", c.header, c.notWant, err)
		}
	}
}

// JSON ではない応答（ログインページ）は型付きのエラーにすること。
func TestNonJSONResponseIsTyped(t *testing.T) {
	rt := &recordingTransport{handle: func(string, url.Values) (int, string) {
		return 200, "<!doctype html><html>login</html>"
	}}
	_, err := newTestClient(t, rt).AuthTest(context.Background())
	var nj *NotJSONError
	if !errors.As(err, &nj) {
		t.Fatalf("NotJSONError であるべき: %T %v", err, err)
	}
}

// 🚨 ある候補でログインページが返っても、後ろの候補（別プロファイル）を試すこと。
//
// ログインページ = その cookie のセッションが切れているだけ。ネットワーク障害と同じ扱いで
// 即座に返すと、別プロファイルに有効なセッションがあっても使われない。
func TestResolveTriesNextProfileAfterLoginPage(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	rt := &recordingTransport{handle: func(method string, form url.Values) (int, string) {
		if form.Get("token") == tokenInvalidated {
			return 200, "<!doctype html><html>login</html>" // セッション切れ
		}
		return 200, authTestBody("alpha", "Alpha Inc", "alice")
	}}
	creds := fakeCreds{
		profiles: []auth.Profile{{Dir: "Profile 1"}, {Dir: "Profile 2"}},
		cookies:  map[string]string{"Profile 1": sentinelCookie, "Profile 2": sentinelCookie},
		tokens:   map[string][]string{"Profile 1": {tokenInvalidated}, "Profile 2": {tokenForAlpha}},
	}
	sess, err := Resolve(context.Background(), config.Config{Workspace: "alpha", Profile: auth.ProfileAuto},
		creds, nil, withTransport(rt))
	if err != nil {
		t.Fatalf("後ろのプロファイルで解決できるはず: %v", err)
	}
	if sess.Profile != "Profile 2" {
		t.Errorf("採用したプロファイル: %q", sess.Profile)
	}
	if len(rt.hosts) != 2 {
		t.Errorf("送信回数: got %d, want 2", len(rt.hosts))
	}
}

// 🚨 Keychain 拒否・フルディスクアクセス不足（プロファイルに依存しない問題）は、
// 見つけた時点でその案内のまま返すこと。
//
// 次のプロファイルへ進むと、後ろのプロファイルの別のエラー（cookie が無い等）で
// 案内が上書きされ、本当の原因にたどり着けない。
func TestResolveStopsOnEnvironmentError(t *testing.T) {
	envErr := &auth.EnvError{Msg: "フルディスクアクセスを付与してください（模擬）"}
	for name, creds := range map[string]fakeCreds{
		"cookie 側": {
			profiles:   []auth.Profile{{Dir: "Profile 1"}, {Dir: "Profile 2"}},
			cookieErrs: map[string]error{"Profile 1": envErr},
			// Profile 2 は cookie が無い（別の、プロファイル固有のエラーになる）
		},
		"token 側": {
			profiles:  []auth.Profile{{Dir: "Profile 1"}, {Dir: "Profile 2"}},
			cookies:   map[string]string{"Profile 1": sentinelCookie},
			tokenErrs: map[string]error{"Profile 1": envErr},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			rt := &recordingTransport{handle: tokenAwareHandler(t)}
			_, err := Resolve(context.Background(), config.Config{Workspace: "alpha", Profile: auth.ProfileAuto},
				creds, nil, withTransport(rt))
			if !auth.IsEnvError(err) {
				t.Fatalf("環境の問題が別の案内に化けた: %v", err)
			}
			if !strings.Contains(err.Error(), "フルディスクアクセス") {
				t.Errorf("案内が失われている: %v", err)
			}
			if len(rt.hosts) != 0 {
				t.Errorf("資格情報が取れていないのに送信した: %d 回", len(rt.hosts))
			}
		})
	}
}

// 🚨 1 プロファイルだけ読めない（アクセス拒否・復号失敗・読めないファイル）ときは、記録して
// 後ろのプロファイルへ進むこと。探索全体を止めると、正常なプロファイルが使えない
// （EnvError にしていた版で実際に退行した）。
func TestResolveContinuesPastUnreadableProfile(t *testing.T) {
	denied := &auth.ReadError{Kind: auth.ReadDenied, Msg: "Cookie DB を読み取れませんでした（アクセス拒否）: 模擬"}
	for name, creds := range map[string]fakeCreds{
		"cookie 側": {
			profiles:   []auth.Profile{{Dir: "Profile 1"}, {Dir: "Profile 2"}},
			cookieErrs: map[string]error{"Profile 1": denied},
			cookies:    map[string]string{"Profile 2": sentinelCookie},
			tokens:     map[string][]string{"Profile 2": {tokenForAlpha}},
		},
		"token 側": {
			profiles:  []auth.Profile{{Dir: "Profile 1"}, {Dir: "Profile 2"}},
			cookies:   map[string]string{"Profile 1": sentinelCookie, "Profile 2": sentinelCookie},
			tokenErrs: map[string]error{"Profile 1": denied},
			tokens:    map[string][]string{"Profile 2": {tokenForAlpha}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			rt := &recordingTransport{handle: tokenAwareHandler(t)}
			sess, err := Resolve(context.Background(), config.Config{Workspace: "alpha", Profile: auth.ProfileAuto},
				creds, nil, withTransport(rt))
			if err != nil {
				t.Fatalf("後ろの正常なプロファイルで解決できるはず: %v", err)
			}
			if sess.Profile != "Profile 2" {
				t.Errorf("採用したプロファイル: %q", sess.Profile)
			}
		})
	}
}

// どの候補も成功しなかったときは、読めなかったプロファイルと対処（フルディスクアクセス、
// またはディレクトリのパーミッション／所有者）を、どの失敗の分岐でも案内に添えること。
func TestResolveReportsReadIssuesWhenAllFail(t *testing.T) {
	denied := &auth.ReadError{Kind: auth.ReadDenied, Msg: "Cookie DB を読み取れませんでした（アクセス拒否）: 模擬"}
	undecryptable := &auth.ReadError{Kind: auth.DecryptFailed, Msg: "すべて復号に失敗しました（模擬）"}
	cases := map[string]struct {
		creds fakeCreds
		want  []string
	}{
		"最後の失敗が別の理由（cookie が無い）": {
			creds: fakeCreds{
				profiles:   []auth.Profile{{Dir: "Profile 1"}, {Dir: "Profile 2"}},
				cookieErrs: map[string]error{"Profile 1": denied},
			},
			want: []string{"Profile 1", "フルディスクアクセス", "パーミッション／所有者"},
		},
		"最後の失敗も読み取りの問題（1 プロファイルだけ）": {
			creds: fakeCreds{
				profiles:   []auth.Profile{{Dir: "Profile 1"}},
				cookieErrs: map[string]error{"Profile 1": undecryptable},
			},
			want: []string{"Profile 1", "鍵が合っていない"},
		},
		"別ワークスペースのトークンが見つかった": {
			creds: fakeCreds{
				profiles:   []auth.Profile{{Dir: "Profile 1"}, {Dir: "Profile 2"}},
				cookieErrs: map[string]error{"Profile 1": denied},
				cookies:    map[string]string{"Profile 2": sentinelCookie},
				tokens:     map[string][]string{"Profile 2": {tokenForBeta}},
			},
			want: []string{"beta", "config set workspace", "Profile 1", "フルディスクアクセス"},
		},
		"後ろで認証が通らなかった": {
			creds: fakeCreds{
				profiles:   []auth.Profile{{Dir: "Profile 1"}, {Dir: "Profile 2"}},
				cookieErrs: map[string]error{"Profile 1": denied},
				cookies:    map[string]string{"Profile 2": sentinelCookie},
				tokens:     map[string][]string{"Profile 2": {tokenInvalidated}},
			},
			want: []string{"いずれも認証が通りませんでした", "Profile 1", "フルディスクアクセス"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			rt := &recordingTransport{handle: tokenAwareHandler(t)}
			_, err := Resolve(context.Background(), config.Config{Workspace: "alpha", Profile: auth.ProfileAuto},
				tc.creds, nil, withTransport(rt))
			if err == nil {
				t.Fatal("失敗するはず")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("案内に %q が無い: %v", w, err)
				}
			}
		})
	}
}
