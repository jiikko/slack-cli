package slack

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/jiikko/slack-cli/internal/auth"
	"github.com/jiikko/slack-cli/internal/config"
)

// テスト用の資格情報（本物ではない）。
const (
	sentinelCookie   = "xoxd-SENTINEL-COOKIE-DO-NOT-LEAK"
	tokenForAlpha    = "xoxc-SENTINEL-ALPHA-DO-NOT-LEAK"
	tokenForBeta     = "xoxc-SENTINEL-BETA-DO-NOT-LEAK"
	tokenInvalidated = "xoxc-SENTINEL-DEAD-DO-NOT-LEAK"
)

// tokenAwareHandler は「トークンごとに所属ワークスペースが違う」Slack を模す。
func tokenAwareHandler(t *testing.T) func(string, url.Values) (int, string) {
	t.Helper()
	return func(method string, form url.Values) (int, string) {
		if method != "auth.test" {
			return 200, `{"ok":true}`
		}
		switch form.Get("token") {
		case tokenForAlpha:
			return 200, authTestBody("alpha", "Alpha Inc", "alice")
		case tokenForBeta:
			return 200, authTestBody("beta", "Beta Inc", "bob")
		default:
			return 200, `{"ok":false,"error":"invalid_auth"}`
		}
	}
}

func alphaConfig() config.Config {
	return config.Config{Workspace: "alpha", Profile: "Profile 1"}
}

func credsWith(tokens ...string) fakeCreds {
	return fakeCreds{
		profiles: []auth.Profile{{Dir: "Profile 1"}},
		cookies:  map[string]string{"Profile 1": sentinelCookie},
		tokens:   map[string][]string{"Profile 1": tokens},
	}
}

// 設定した workspace 以外のホストへは一切リクエストを投げないこと（受け入れ基準 §12）。
//
// 🚨 判定は「組み立てた URL 文字列」ではなく「Transport に実際に到達したホスト」で行う。
// 前者だと、別経路で直接 POST する変異を素通しする。
func TestResolveOnlyContactsConfiguredWorkspace(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	rt := &recordingTransport{handle: tokenAwareHandler(t)}

	// 候補の先頭が別ワークスペース（beta）のトークンでも、送信先は alpha だけ。
	sess, err := Resolve(context.Background(), alphaConfig(),
		credsWith(tokenForBeta, tokenForAlpha), nil, withTransport(rt))
	if err != nil {
		t.Fatalf("解決できるはず: %v", err)
	}
	if got := sess.Client.Workspace(); got != "alpha" {
		t.Errorf("接続先ワークスペース: got %q, want alpha", got)
	}
	hosts := rt.uniqueHosts()
	if len(hosts) != 1 || hosts[0] != "alpha.slack.com" {
		t.Fatalf("設定外のホストへ接続した: %v", hosts)
	}
	if len(rt.hosts) < 2 {
		t.Errorf("候補 2 件を試すはずが %d 件しか送っていない（走査が壊れている）", len(rt.hosts))
	}
}

// workspace が一致しないトークンしか無ければ、採用せず停止し、見つかった一覧を案内すること。
func TestResolveRejectsMismatchedWorkspace(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	rt := &recordingTransport{handle: tokenAwareHandler(t)}

	_, err := Resolve(context.Background(), alphaConfig(), credsWith(tokenForBeta), nil, withTransport(rt))
	if err == nil {
		t.Fatal("別ワークスペースのトークンを採用してはいけない")
	}
	if !strings.Contains(err.Error(), "beta") {
		t.Errorf("見つかったワークスペース一覧が案内に無い: %v", err)
	}
	if !strings.Contains(err.Error(), "config set workspace") {
		t.Errorf("設定変更の案内が無い: %v", err)
	}
	if hosts := rt.uniqueHosts(); len(hosts) != 1 || hosts[0] != "alpha.slack.com" {
		t.Fatalf("設定外のホストへ接続した: %v", hosts)
	}
}

// 🚨 -token / SLACK_CLI_TOKEN で明示指定したトークンも、ワークスペース一致の検証を通ること。
// ここを免除すると、安全装置の中心がフラグ 1 つで無効化される。
func TestExplicitTokenIsStillVerified(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	rt := &recordingTransport{handle: tokenAwareHandler(t)}

	cfg := alphaConfig()
	cfg.Token = tokenForBeta // 別ワークスペースのトークンを明示指定
	// 抽出側には正しいトークンがあるが、明示指定が優先される = 検証をすり抜けてはいけない。
	_, err := Resolve(context.Background(), cfg, credsWith(tokenForAlpha), nil, withTransport(rt))
	if err == nil {
		t.Fatal("明示指定のトークンが検証を素通りした（ワークスペース限定が無効化される）")
	}
	if !strings.Contains(err.Error(), "beta") {
		t.Errorf("案内に検出したワークスペースが無い: %v", err)
	}
	if hosts := rt.uniqueHosts(); len(hosts) != 1 || hosts[0] != "alpha.slack.com" {
		t.Fatalf("設定外のホストへ接続した: %v", hosts)
	}
}

// 無効なトークンは次の候補へ進み、有効なものが見つかれば採用すること。
func TestResolveSkipsInvalidTokenAndContinues(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	rt := &recordingTransport{handle: tokenAwareHandler(t)}

	sess, err := Resolve(context.Background(), alphaConfig(),
		credsWith(tokenInvalidated, tokenForAlpha), nil, withTransport(rt))
	if err != nil {
		t.Fatalf("2 番目の候補で成功するはず: %v", err)
	}
	if sess.Auth.Team != "Alpha Inc" {
		t.Errorf("採用したトークンが違う: %+v", sess.Auth)
	}
}

// ネットワーク障害は候補を変えても直らないので、その場で返すこと
// （候補の数だけ再試行して「全部ダメでした」に化けさせない）。
func TestResolveReturnsNetworkErrorImmediately(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	boom := errors.New("dial tcp: ネットワークに到達できません")
	rt := &recordingTransport{err: boom}

	_, err := Resolve(context.Background(), alphaConfig(),
		credsWith(tokenForAlpha, tokenForBeta), nil, withTransport(rt))
	if err == nil {
		t.Fatal("通信エラーは伝播すべき")
	}
	if !strings.Contains(err.Error(), "ネットワークに到達できません") {
		t.Errorf("元のエラーが失われている: %v", err)
	}
	if len(rt.hosts) != 1 {
		t.Errorf("通信エラー後も試行を続けている: %d 回送信した", len(rt.hosts))
	}
}

// 🚨 cookie / トークンの生値が stderr・エラーメッセージに出ないこと（受け入れ基準 §12）。
func TestNoSecretsInStderrOrErrors(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	secrets := []string{sentinelCookie, tokenForAlpha, tokenForBeta, tokenInvalidated}

	check := func(label, text string) {
		for _, s := range secrets {
			if strings.Contains(text, s) {
				t.Errorf("%s に資格情報の生値が出ている: %q", label, s)
			}
		}
	}

	// 成功経路（stderr に接続先を 1 行出す）
	var stderr bytes.Buffer
	rt := &recordingTransport{handle: tokenAwareHandler(t)}
	if _, err := Resolve(context.Background(), alphaConfig(),
		credsWith(tokenForBeta, tokenForAlpha), &stderr, withTransport(rt)); err != nil {
		t.Fatal(err)
	}
	check("stderr(成功)", stderr.String())

	// 不一致エラー経路
	stderr.Reset()
	rt = &recordingTransport{handle: tokenAwareHandler(t)}
	_, err := Resolve(context.Background(), alphaConfig(), credsWith(tokenForBeta), &stderr, withTransport(rt))
	if err == nil {
		t.Fatal("不一致はエラーになるはず")
	}
	check("stderr(不一致)", stderr.String())
	check("エラーメッセージ(不一致)", err.Error())

	// 通信エラー経路（url.Error はリクエスト URL を含む。token を URL に置くと必ずここで漏れる）
	rt = &recordingTransport{err: errors.New("boom")}
	_, err = Resolve(context.Background(), alphaConfig(), credsWith(tokenForAlpha), &stderr, withTransport(rt))
	if err == nil {
		t.Fatal("通信エラーはエラーになるはず")
	}
	check("エラーメッセージ(通信エラー)", err.Error())

	// 認証エラー経路
	rt = &recordingTransport{handle: tokenAwareHandler(t)}
	_, err = Resolve(context.Background(), alphaConfig(), credsWith(tokenInvalidated), &stderr, withTransport(rt))
	if err == nil {
		t.Fatal("無効トークンはエラーになるはず")
	}
	check("エラーメッセージ(認証失敗)", err.Error())
}

// 🚨 プロファイルを固定しているときは「そのプロファイルしか探していない」ことを案内すること。
//
// これが無いと、見つかったワークスペースの一覧が「Chrome 全体を探した結果」に読め、
// 別プロファイルにログインしている対象を「無い」と誤診する（実際に誤診した）。
func TestFailureTellsProfileScopeWhenFixed(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	// プロファイル固定 + 別ワークスペースのトークンしか無い
	rt := &recordingTransport{handle: tokenAwareHandler(t)}
	cfg := alphaConfig() // Profile: "Profile 1"
	_, err := Resolve(context.Background(), cfg, credsWith(tokenForBeta), nil, withTransport(rt))
	if err == nil {
		t.Fatal("エラーになるはず")
	}
	for _, want := range []string{"Profile 1", "-profile auto", "slack setup"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("固定時の案内に %q が無い:\n%v", want, err)
		}
	}

	// auto のときは「固定されているため」の案内を出さない（誤った誘導をしない）
	rt2 := &recordingTransport{handle: tokenAwareHandler(t)}
	autoCfg := cfg
	autoCfg.Profile = auth.ProfileAuto
	_, err = Resolve(context.Background(), autoCfg, credsWith(tokenForBeta), nil, withTransport(rt2))
	if err == nil {
		t.Fatal("エラーになるはず")
	}
	if strings.Contains(err.Error(), "-profile auto") {
		t.Errorf("auto なのに -profile auto を勧めている:\n%v", err)
	}
}

// workspace 未設定・不正値は使い方エラー（rc=2）にすること。
func TestResolveRequiresValidWorkspace(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cases := []struct{ name, ws string }{
		{"未設定", ""},
		{"別ホストを埋め込む", "evil.example.com"},
		{"大文字や記号", "Alpha_1"},
	}
	for _, c := range cases {
		rt := &recordingTransport{handle: tokenAwareHandler(t)}
		_, err := Resolve(context.Background(), config.Config{Workspace: c.ws, Profile: "Profile 1"},
			credsWith(tokenForAlpha), nil, withTransport(rt))
		if err == nil {
			t.Errorf("%s: エラーになるべき", c.name)
			continue
		}
		var ue *config.UsageError
		if !errors.As(err, &ue) {
			t.Errorf("%s: 使い方エラー（rc=2）にすべき: %T %v", c.name, err, err)
		}
		if len(rt.hosts) != 0 {
			t.Errorf("%s: 検証前にリクエストを投げている: %v", c.name, rt.hosts)
		}
	}
}

// auth.test の url が想定外の形なら「一致」に丸めないこと（判定不能は不一致として扱う）。
func TestTeamDomainRejectsUnexpectedURL(t *testing.T) {
	cases := map[string]string{
		"空":           "",
		"別ドメイン":       "https://alpha.example.com/",
		"slack.com 直": "https://slack.com/",
		"スキーム無し":      "alpha.slack.com",
		"サブドメインに見せかけたパス": "https://evil.example.com/alpha.slack.com/",
	}
	for name, u := range cases {
		at := &AuthTest{URL: u}
		if got := at.TeamDomain(); got == "alpha" {
			t.Errorf("%s: %q を alpha と判定してはいけない", name, u)
		}
	}
	if got := (&AuthTest{URL: "https://alpha.slack.com/"}).TeamDomain(); got != "alpha" {
		t.Errorf("正常な url の判定に失敗: got %q", got)
	}
}
