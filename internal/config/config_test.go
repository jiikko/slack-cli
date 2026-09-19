package config

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// isolate は設定ディレクトリをテスト専用にし、読み込みキャッシュを捨てる。
func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	resetCache()
	t.Cleanup(resetCache)
	return filepath.Join(dir, "slack-cli")
}

func resetCache() {
	loadOnce = sync.Once{}
	loadCached = File{}
	loadErr = nil
}

// 優先順位が「環境変数 > config.yml > 組み込み既定」であること。
// （フラグはこの既定値を上書きする形なので、flag > env > file > default になる）
func TestResolvePrecedence(t *testing.T) {
	isolate(t)
	const env = "SLACK_CLI_TEST_KEY"

	t.Setenv(env, "")
	if v, src := ResolveDefaultSource(env, "", "builtin"); v != "builtin" || src != "default" {
		t.Errorf("既定: got %q (%s)", v, src)
	}
	if v, src := ResolveDefaultSource(env, "fromfile", "builtin"); v != "fromfile" || src != "file" {
		t.Errorf("file: got %q (%s)", v, src)
	}
	t.Setenv(env, "fromenv")
	if v, src := ResolveDefaultSource(env, "fromfile", "builtin"); v != "fromenv" || src != "env:"+env {
		t.Errorf("env: got %q (%s)", v, src)
	}
}

// 数値項目も同じ優先順位で、壊れた環境変数は無視して次へ落ちること。
func TestResolveDefaultInt(t *testing.T) {
	const env = "SLACK_CLI_TEST_COUNT"
	t.Setenv(env, "")
	if v, src := ResolveDefaultInt(env, 0, 20); v != 20 || src != "default" {
		t.Errorf("既定: %d (%s)", v, src)
	}
	if v, src := ResolveDefaultInt(env, 50, 20); v != 50 || src != "file" {
		t.Errorf("file: %d (%s)", v, src)
	}
	t.Setenv(env, "77")
	if v, src := ResolveDefaultInt(env, 50, 20); v != 77 || src != "env:"+env {
		t.Errorf("env: %d (%s)", v, src)
	}
	for _, bad := range []string{"abc", "0", "-5"} {
		t.Setenv(env, bad)
		if v, _ := ResolveDefaultInt(env, 50, 20); v != 50 {
			t.Errorf("壊れた環境変数 %q を採用している: %d", bad, v)
		}
	}
}

// 🚨 設定できると案内しているキーが、実際に保存・再読込できること。
// フィールドを足し忘れると「保存しました」と表示しながら何も書かれない。
func TestEveryKeyRoundTrips(t *testing.T) {
	dir := isolate(t)
	values := map[string]string{
		"workspace":     "alpha",
		"profile":       "Profile 3",
		"default_count": "42",
	}
	if len(values) != len(Keys) {
		t.Fatalf("Keys が増減している（テストを更新すること）: %v", Keys)
	}

	fc := Load()
	for _, k := range Keys {
		if err := Set(&fc, k, values[k]); err != nil {
			t.Fatalf("%s: %v", k, err)
		}
	}
	if err := Save(fc); err != nil {
		t.Fatal(err)
	}

	resetCache()
	reloaded := Load()
	for _, k := range Keys {
		got, err := Get(reloaded, k)
		if err != nil {
			t.Errorf("%s: %v", k, err)
			continue
		}
		if got != values[k] {
			t.Errorf("%s: 保存されていない（got %q, want %q）", k, got, values[k])
		}
	}

	// 資格情報を置く場所ではないので、パーミッションは自分だけ。
	info, err := os.Stat(filepath.Join(dir, "config.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("config.yml のパーミッション: got %o, want 600", perm)
	}
}

// team は workspace の別名として読め、保存時は workspace へ正規化されること。
func TestTeamAliasIsAcceptedAndNormalized(t *testing.T) {
	dir := isolate(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte("team: alpha\nprofile: Default\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	fc := Load()
	if got, _ := Get(fc, "workspace"); got != "alpha" {
		t.Errorf("team を workspace として読めていない: %q", got)
	}
	if err := Set(&fc, "profile", "Profile 2"); err != nil {
		t.Fatal(err)
	}
	if err := Save(fc); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "workspace: alpha") {
		t.Errorf("workspace へ正規化されていない:\n%s", data)
	}
	if strings.Contains(string(data), "team:") {
		t.Errorf("別名が残っている（出典が二重になる）:\n%s", data)
	}
}

// 壊れた config.yml は「読めなかった」と分かる形にすること（ゼロ値で上書きしない）。
func TestBrokenConfigIsReportedNotSilentlyReset(t *testing.T) {
	dir := isolate(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yml"), []byte("workspace: [不正\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if Problem() == nil {
		t.Error("解析エラーを検出できていない（config set が既存設定を消す経路になる）")
	}
}

// ワークスペース名の検証（接続先ホストの組み立てに直結する）。
func TestValidateWorkspace(t *testing.T) {
	ok := []string{"alpha", "a", "my-team-1", "acme-inc"}
	ng := []string{"", "Alpha", "alpha.slack.com", "alpha/beta", "alpha_1", "-alpha", "alpha-", "ほげ", strings.Repeat("a", 64)}
	for _, s := range ok {
		if err := ValidateWorkspace(s); err != nil {
			t.Errorf("%q は有効にすべき: %v", s, err)
		}
	}
	for _, s := range ng {
		if err := ValidateWorkspace(s); err == nil {
			t.Errorf("%q は拒否すべき", s)
		}
	}
}

// URL 形式は正規化し、素の文字列は読み替えないこと。
//
// 🚨 何でも最初の / で切ると、明らかにおかしい入力（alpha/../beta）を黙って
// 別の値に読み替えてしまう。URL に見えるものだけ切り詰める。
func TestNormalizeWorkspace(t *testing.T) {
	cases := map[string]string{
		"https://alpha.slack.com/":                  "alpha",
		"https://alpha.slack.com/messages/C1":       "alpha",
		"http://alpha.slack.com":                    "alpha",
		"alpha.slack.com":                           "alpha",
		"ALPHA.slack.com":                           "alpha",
		"alpha":                                     "alpha",
		"  alpha  ":                                 "alpha",
		"alpha/../beta":                             "alpha/../beta", // 読み替えず、検証で弾く
		"https://evil.example.com/alpha.slack.com/": "evil.example.com",
	}
	for in, want := range cases {
		if got := NormalizeWorkspace(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
	// 読み替えなかったものは検証で落ちること（正規化と検証の合わせ技で守る）。
	if err := ValidateWorkspace(NormalizeWorkspace("alpha/../beta")); err == nil {
		t.Error("alpha/../beta が通ってしまう")
	}
	if err := ValidateWorkspace(NormalizeWorkspace("https://evil.example.com/alpha.slack.com/")); err == nil {
		t.Error("別ホストの URL が通ってしまう")
	}
}

// プロファイルの検出結果キャッシュは、プロファイル名だけを保存すること。
func TestProfileCacheStoresOnlyProfileName(t *testing.T) {
	dir := isolate(t)
	WriteProfileCache("alpha", "Profile 3")
	if got := ReadProfileCache("alpha"); got != "Profile 3" {
		t.Errorf("got %q", got)
	}
	if got := ReadProfileCache("beta"); got != "" {
		t.Errorf("ワークスペースごとに分かれていない: %q", got)
	}
	data, err := os.ReadFile(filepath.Join(dir, "cache", "profile-alpha"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "xoxc") || strings.Contains(string(data), "xoxd") {
		t.Errorf("資格情報がキャッシュに書かれている: %q", data)
	}
}

// 不明なキーは弾くこと。
func TestUnknownKeyRejected(t *testing.T) {
	isolate(t)
	fc := Load()
	if err := Set(&fc, "token", "xoxc-secret"); err == nil {
		t.Error("token は config.yml に保存できてはいけない（資格情報を保存しない方針）")
	}
	if IsKey("token") {
		t.Error("token を設定可能キーにしてはいけない")
	}
	if !IsKey("team") {
		t.Error("team は workspace の別名として受けるべき")
	}
}
