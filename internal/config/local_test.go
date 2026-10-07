package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isolateLocal は共通の設定ディレクトリとカレントディレクトリをテスト専用にし、
// (共通の config.yml のパス, カレントディレクトリ) を返す。
func isolateLocal(t *testing.T, global string) (string, string) {
	t.Helper()
	dir := isolate(t)
	t.Setenv(EnvWorkspace, "")
	t.Setenv(EnvProfile, "")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	gpath := filepath.Join(dir, "config.yml")
	if global != "" {
		writeFile(t, gpath, global, 0o600)
	}
	cwd := t.TempDir()
	if err := os.Chmod(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwd)
	return gpath, cwd
}

func writeFile(t *testing.T, path, body string, perm os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), perm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, perm); err != nil { // umask に削られない値にする
		t.Fatal(err)
	}
}

// 書いてあるキーだけがローカルで上書きされ、書いていないキーは共通側の値になること。
func TestLocalOverridesPerKey(t *testing.T) {
	_, cwd := isolateLocal(t, "workspace: alpha\nprofile: Default\ndefault_count: 30\n")
	writeFile(t, filepath.Join(cwd, LocalName), "workspace: beta\n", 0o600)

	got := Effective()
	if got.Workspace != "beta" {
		t.Errorf("workspace: ローカルが優先されていない: %q", got.Workspace)
	}
	if got.Profile != "Default" || got.DefaultCount != 30 {
		t.Errorf("ローカルに無いキーが共通側の値になっていない: %+v", got)
	}
	if ws, _, _ := Defaults(); ws != "beta" {
		t.Errorf("Defaults がローカルを見ていない: %q", ws)
	}
	if o := Origin("workspace"); o != filepath.Join(cwd, LocalName) {
		t.Errorf("workspace の出所: %q", o)
	}
	if o := Origin("profile"); !strings.HasSuffix(o, filepath.Join("slack-cli", "config.yml")) {
		t.Errorf("profile の出所: %q", o)
	}
}

// 環境変数はローカル設定より優先されること（優先順位: flag > env > local > global）。
func TestEnvBeatsLocal(t *testing.T) {
	_, cwd := isolateLocal(t, "")
	writeFile(t, filepath.Join(cwd, LocalName), "workspace: beta\n", 0o600)
	t.Setenv(EnvWorkspace, "gamma")
	if ws, _, _ := Defaults(); ws != "gamma" {
		t.Errorf("環境変数がローカルに負けている: %q", ws)
	}
	if n := LocalNotice("gamma", "auto"); n != "" {
		t.Errorf("環境変数で決まったのにローカル使用の通知が出る: %q", n)
	}
}

// 別名 team はファイルごとに解いてから合わせること（ローカルの team: は共通の workspace: に勝つ）。
func TestLocalTeamAliasBeatsGlobalWorkspace(t *testing.T) {
	_, cwd := isolateLocal(t, "workspace: alpha\n")
	writeFile(t, filepath.Join(cwd, LocalName), "team: beta\n", 0o600)
	if got := Effective().Workspace; got != "beta" {
		t.Errorf("ローカルの team が共通の workspace に負けている: %q", got)
	}
}

// ローカルの default_count が 0 以下なら未設定として共通側へ落ちること。
func TestLocalNonPositiveCountFallsBack(t *testing.T) {
	_, cwd := isolateLocal(t, "default_count: 30\n")
	writeFile(t, filepath.Join(cwd, LocalName), "default_count: -1\n", 0o600)
	if _, _, n := Defaults(); n != 30 {
		t.Errorf("負の default_count が共通の値を隠している: %d", n)
	}
}

// 親ディレクトリの .slack-cli.yml はさかのぼって読まないこと。
func TestParentLocalIsNotRead(t *testing.T) {
	_, cwd := isolateLocal(t, "workspace: alpha\n")
	writeFile(t, filepath.Join(cwd, LocalName), "workspace: beta\n", 0o600)
	child := filepath.Join(cwd, "child")
	if err := os.Mkdir(child, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(child)
	resetCache()
	if got := Effective().Workspace; got != "alpha" {
		t.Errorf("親ディレクトリのローカル設定を読んでいる: %q", got)
	}
}

// 他人が差し替えられるローカル設定は無視すること。
func TestUnsafeLocalIsIgnored(t *testing.T) {
	cases := map[string]func(t *testing.T, cwd string){
		"シンボリックリンク": func(t *testing.T, cwd string) {
			target := filepath.Join(t.TempDir(), "real.yml")
			writeFile(t, target, "workspace: beta\n", 0o600)
			if err := os.Symlink(target, filepath.Join(cwd, LocalName)); err != nil {
				t.Fatal(err)
			}
		},
		"グループが書き込めるファイル": func(t *testing.T, cwd string) {
			writeFile(t, filepath.Join(cwd, LocalName), "workspace: beta\n", 0o620)
		},
		"他人が書き込めるファイル": func(t *testing.T, cwd string) {
			writeFile(t, filepath.Join(cwd, LocalName), "workspace: beta\n", 0o602)
		},
		"ディレクトリ": func(t *testing.T, cwd string) {
			if err := os.Mkdir(filepath.Join(cwd, LocalName), 0o700); err != nil {
				t.Fatal(err)
			}
		},
		"他人が書き込めるディレクトリ": func(t *testing.T, cwd string) {
			writeFile(t, filepath.Join(cwd, LocalName), "workspace: beta\n", 0o600)
			if err := os.Chmod(cwd, 0o707); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			_, cwd := isolateLocal(t, "workspace: alpha\n")
			setup(t, cwd)
			if got := Effective().Workspace; got != "alpha" {
				t.Errorf("無視すべきローカル設定を読んだ: %q", got)
			}
			if LocalIgnored() == "" {
				t.Error("無視した理由が記録されていない（警告が出ない）")
			}
			if LocalProblem() != nil {
				t.Errorf("無視すべきものを読み込みエラー扱いにしている（実行が止まる）: %v", LocalProblem())
			}
			if _, err := SaveLocal(File{Workspace: "gamma"}); err == nil {
				t.Error("無視したローカル設定に書き込めてしまう")
			}
		})
	}
}

// 壊れたローカル設定は「読めなかった」と分かる形にすること（黙って共通側で動かない）。
func TestBrokenLocalIsReported(t *testing.T) {
	_, cwd := isolateLocal(t, "workspace: alpha\n")
	writeFile(t, filepath.Join(cwd, LocalName), "workspace: [不正\n", 0o600)
	if LocalProblem() == nil {
		t.Error("壊れたローカル設定を検出できていない")
	}
	if _, err := SaveLocal(File{Workspace: "gamma"}); err == nil {
		t.Error("壊れたローカル設定に上書きできてしまう")
	}
}

// SaveLocal は渡したキーだけを書き、共通の config.yml とシンボリックリンクの先を触らないこと。
func TestSaveLocalWritesOnlyLocal(t *testing.T) {
	gpath, cwd := isolateLocal(t, "workspace: alpha\nprofile: Default\n")
	before, err := os.ReadFile(gpath)
	if err != nil {
		t.Fatal(err)
	}

	fc := LocalFile()
	if err := Set(&fc, "workspace", "beta"); err != nil {
		t.Fatal(err)
	}
	path, err := SaveLocal(fc)
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(cwd, LocalName) {
		t.Errorf("書いたパス: %q", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "workspace: beta") {
		t.Errorf("workspace が書かれていない:\n%s", data)
	}
	if strings.Contains(string(data), "\nprofile:") {
		t.Errorf("共通側の値がローカルに写っている:\n%s", data)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("パーミッション: %o", info.Mode().Perm())
	}
	after, _ := os.ReadFile(gpath)
	if string(after) != string(before) {
		t.Errorf("共通の config.yml が変わった:\n%s", after)
	}
	if left, _ := filepath.Glob(filepath.Join(cwd, LocalName+".tmp-*")); len(left) != 0 {
		t.Errorf("一時ファイルが残っている: %v", left)
	}
}

// ローカル設定で決まった値だけを通知すること。
func TestLocalNotice(t *testing.T) {
	_, cwd := isolateLocal(t, "")
	writeFile(t, filepath.Join(cwd, LocalName), "workspace: beta\nprofile: Profile 2\n", 0o600)
	n := LocalNotice("beta", "Profile 2")
	if !strings.Contains(n, "workspace=beta") || !strings.Contains(n, "profile=Profile 2") || !strings.Contains(n, LocalName) {
		t.Errorf("通知: %q", n)
	}
	if n := LocalNotice("other", "auto"); n != "" {
		t.Errorf("フラグで上書きされたのに通知が出る: %q", n)
	}
}

// ローカル設定が無ければ、今までどおり共通の config.yml だけで決まること。
func TestNoLocalKeepsGlobalBehavior(t *testing.T) {
	isolateLocal(t, "workspace: alpha\n")
	if got := Effective().Workspace; got != "alpha" {
		t.Errorf("got %q", got)
	}
	if LocalPresent() || LocalIgnored() != "" || LocalProblem() != nil {
		t.Error("ローカル設定が無いのに、あることになっている")
	}
	if n := LocalNotice("alpha", "auto"); n != "" {
		t.Errorf("通知: %q", n)
	}
}

// 祖先のディレクトリに実行権限が無くても、ローカル設定の有無で止まらないこと（絶対パスで開くと EACCES になる）。
func TestLocalReadDoesNotNeedAncestorSearchPermission(t *testing.T) {
	for name, body := range map[string]string{"無い": "", "ある": "workspace: beta\n"} {
		t.Run(name, func(t *testing.T) {
			_, cwd := isolateLocal(t, "workspace: alpha\n")
			inner := filepath.Join(cwd, "a", "inner")
			if err := os.MkdirAll(inner, 0o700); err != nil {
				t.Fatal(err)
			}
			if body != "" {
				writeFile(t, filepath.Join(inner, LocalName), body, 0o600)
			}
			t.Chdir(inner)
			parent := filepath.Join(cwd, "a")
			if err := os.Chmod(parent, 0o000); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chmod(parent, 0o700) })
			resetCache()

			if err := LocalProblem(); err != nil {
				t.Fatalf("祖先の権限だけで読み込みエラーになる（全コマンドが止まる）: %v", err)
			}
			want := "alpha"
			if body != "" {
				want = "beta"
			}
			if got := Effective().Workspace; got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}
}

// 他人が書き込めるディレクトリにある読めないファイルは、止めずに無視すること
// （/tmp に他人が 0600 で置いた 1 ファイルで、そこで実行した全員が止まらないように）。
func TestUnreadableLocalInUnsafeDirIsIgnoredNotFatal(t *testing.T) {
	_, cwd := isolateLocal(t, "workspace: alpha\n")
	writeFile(t, filepath.Join(cwd, LocalName), "workspace: beta\n", 0o000)
	if err := os.Chmod(cwd, 0o707); err != nil {
		t.Fatal(err)
	}
	if err := LocalProblem(); err != nil {
		t.Fatalf("無視すべきものを読み込みエラーにしている: %v", err)
	}
	if LocalIgnored() == "" {
		t.Error("無視した理由が記録されていない")
	}
	if got := Effective().Workspace; got != "alpha" {
		t.Errorf("got %q", got)
	}
}

// 0 以下の default_count は、値の解決でも出所の表示でも「未設定」として同じに扱うこと。
func TestNonPositiveLocalCountIsUnsetEverywhere(t *testing.T) {
	gpath, cwd := isolateLocal(t, "default_count: 50\n")
	writeFile(t, filepath.Join(cwd, LocalName), "default_count: -5\n", 0o600)
	if LocalHas("default_count") {
		t.Error("負の default_count をローカルにあると判定している（上書きの警告が誤って出る）")
	}
	if o := Origin("default_count"); o != gpath {
		t.Errorf("出所: got %q, want %q（値は共通側から来ている）", o, gpath)
	}
	if got := Effective().DefaultCount; got != 50 {
		t.Errorf("値: %d", got)
	}
}

// 中を調べられないカレントディレクトリ（r だけで x が無い）では、ローカル設定を「無い」に丸めず、
// 無視したことを記録すること（黙って共通側の値で動かない）。
func TestUnsearchableCwdIsReportedNotSilentlyAbsent(t *testing.T) {
	_, cwd := isolateLocal(t, "workspace: alpha\n")
	writeFile(t, filepath.Join(cwd, LocalName), "workspace: beta\n", 0o600)
	if err := os.Chmod(cwd, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(cwd, 0o700) })
	if LocalIgnored() == "" {
		t.Error("調べられなかったのに、ローカル設定が無いことになっている（警告が出ない）")
	}
	if err := LocalProblem(); err != nil {
		t.Errorf("読み込みエラーにしている（全コマンドが止まる）: %v", err)
	}
}
