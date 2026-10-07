package main

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jiikko/slack-cli/internal/config"
)

// isolateConfig は設定ディレクトリと HOME をテスト専用にする。
// HOME も差し替えるのは、守りが外れたときに本物の Chrome のデータを読みに行かせないため。
func isolateConfig(t *testing.T) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv(config.EnvWorkspace, "")
	t.Setenv(config.EnvProfile, "")
	config.ResetCache()
	t.Cleanup(config.ResetCache)
	dir := filepath.Join(xdg, "slack-cli")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// カレントディレクトリも差し替える（ローカル設定 .slack-cli.yml はカレントディレクトリから読む）。
	cwd := t.TempDir()
	if err := os.Chmod(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwd)
	return dir
}

func writeConfig(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readConfig(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// config set -local は指定したキーだけを .slack-cli.yml に書き、共通の config.yml を変えないこと。
func TestConfigSetLocalWritesOnlyTheKey(t *testing.T) {
	dir := isolateConfig(t)
	gpath := filepath.Join(dir, "config.yml")
	writeConfig(t, gpath, "workspace: alpha\nprofile: Default\n")

	var out, errw strings.Builder
	if err := configSet(&out, &errw, []string{"-local", "workspace", "beta"}); err != nil {
		t.Fatal(err)
	}
	local := readConfig(t, config.LocalName)
	if !strings.Contains(local, "\nworkspace: beta\n") || strings.Contains(local, "\nprofile:") {
		t.Errorf("指定したキーだけが書かれていない:\n%s", local)
	}
	if got := readConfig(t, gpath); got != "workspace: alpha\nprofile: Default\n" {
		t.Errorf("共通の config.yml が変わった:\n%s", got)
	}
	if !strings.Contains(out.String(), config.LocalName) {
		t.Errorf("書いたファイルを表示していない: %q", out.String())
	}
}

// -local を付けない config set は共通側に書き、ローカルで隠れるなら警告すること。
func TestConfigSetGlobalWarnsWhenShadowed(t *testing.T) {
	dir := isolateConfig(t)
	gpath := filepath.Join(dir, "config.yml")
	writeConfig(t, config.LocalName, "workspace: beta\n")

	var out, errw strings.Builder
	if err := configSet(&out, &errw, []string{"workspace", "alpha"}); err != nil {
		t.Fatal(err)
	}
	if got := readConfig(t, gpath); !strings.Contains(got, "\nworkspace: alpha\n") {
		t.Errorf("共通の config.yml に書かれていない:\n%s", got)
	}
	if got := readConfig(t, config.LocalName); got != "workspace: beta\n" {
		t.Errorf("ローカル設定が変わった:\n%s", got)
	}
	if !strings.Contains(errw.String(), "優先されます") {
		t.Errorf("ローカルで隠れる警告が出ていない: %q", errw.String())
	}
	if strings.Contains(out.String(), "優先されます") {
		t.Error("警告が stdout に出ている")
	}
}

// 共通の config.yml だけが壊れているとき、config set -local は書けること（ローカルしか読み書きしない）。
func TestConfigSetLocalWorksWhenGlobalBroken(t *testing.T) {
	dir := isolateConfig(t)
	writeConfig(t, filepath.Join(dir, "config.yml"), "workspace: [不正\n")
	var out, errw strings.Builder
	if err := configSet(&out, &errw, []string{"-local", "workspace", "beta"}); err != nil {
		t.Fatalf("共通が壊れているだけで -local を断っている: %v", err)
	}
	if !strings.Contains(readConfig(t, config.LocalName), "workspace: beta") {
		t.Error("ローカル設定に書かれていない")
	}
}

// フラグを引数の後ろに置いたら断ること（-local が値として読まれると、意図と違うファイルに書く）。
func TestConfigSetRejectsTrailingLocal(t *testing.T) {
	dir := isolateConfig(t)
	var out, errw strings.Builder
	err := configSet(&out, &errw, []string{"workspace", "beta", "-local"})
	var ue *config.UsageError
	if !errors.As(err, &ue) {
		t.Fatalf("後ろの -local を断っていない: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "config.yml")); err == nil {
		t.Error("共通の config.yml に書いてしまった")
	}
}

// 壊れたローカル設定があると、help 以外は実行を止めること。
// 判定は本物のフラグ解析（parseArgs）に任せる。値を取るフラグの値として -h を渡しても抜けられないこと。
func TestBrokenLocalStopsCommands(t *testing.T) {
	isolateConfig(t)
	writeConfig(t, config.LocalName, "workspace: [不正\n")
	var ue *config.UsageError

	parse := func(args ...string) (bool, error) {
		var cfg config.Config
		fs := newFlagSet("channels")
		registerCommon(fs, &cfg)
		fs.String("name", "", "")
		return parseArgs(fs, "HELP", args)
	}
	for _, args := range [][]string{nil, {"-json"}, {"-name", "-h"}, {"--", "-h"}} {
		if done, err := parse(args...); done || !errors.As(err, &ue) {
			t.Errorf("%v: 壊れたローカル設定で止まらない (help=%v, err=%v)", args, done, err)
		}
	}
	for _, args := range [][]string{{"-h"}, {"-json", "--help"}} {
		if done, err := parse(args...); !done || err != nil {
			t.Errorf("%v: help まで止めている (help=%v, err=%v)", args, done, err)
		}
	}
	for _, c := range [][]string{{}, {"get", "workspace"}, {"set", "-local", "workspace", "a"}, {"path"}} {
		if err := cmdConfig(c); !errors.As(err, &ue) {
			t.Errorf("config %v: 壊れたローカル設定で止まらない: %v", c, err)
		}
	}
	if err := cmdConfig([]string{"help"}); err != nil {
		t.Errorf("config help まで止めている: %v", err)
	}
}

// 🚨 壊れた config.yml を、config.yml を書き直すどの経路でも上書きしないこと。
// config init / setup は空の File に workspace / profile だけを入れて保存するので、
// 書き込めてしまうと他のキーが黙って消える（以前は config set だけが断っていた）。
func TestBrokenConfigIsNotOverwrittenByAnyWriter(t *testing.T) {
	for name, run := range map[string]func() error{
		"config set":  func() error { return cmdConfig([]string{"set", "default_count", "5"}) },
		"config init": func() error { return cmdConfig([]string{"init"}) },
		"setup":       func() error { return cmdSetup(nil) },
	} {
		t.Run(name, func(t *testing.T) {
			dir := isolateConfig(t)
			path := filepath.Join(dir, "config.yml")
			broken := "workspace: [不正\ndefault_count: 50\n"
			if err := os.WriteFile(path, []byte(broken), 0o600); err != nil {
				t.Fatal(err)
			}

			err := run()
			var ue *config.UsageError
			if !errors.As(err, &ue) || !strings.Contains(err.Error(), "書き込みを中止") {
				t.Fatalf("壊れた config.yml への書き込みを断っていない: %v", err)
			}
			data, rerr := os.ReadFile(path)
			if rerr != nil {
				t.Fatal(rerr)
			}
			if string(data) != broken {
				t.Errorf("config.yml が書き換えられた:\n%s", data)
			}
		})
	}
}

// config init / setup は、フラグで明示しない限りローカル設定の値を初期値に使わないこと
// （使うと、cd した先の .slack-cli.yml の値が config.yml に書き写される）。
func TestInitIgnoresLocalDefaults(t *testing.T) {
	dir := isolateConfig(t)
	writeConfig(t, filepath.Join(dir, "config.yml"), "workspace: alpha\nprofile: Default\n")
	writeConfig(t, config.LocalName, "workspace: other\nprofile: Evil\n")

	parse := func(args ...string) config.Config {
		var cfg config.Config
		fs := newFlagSet("config init")
		registerCommon(fs, &cfg)
		if err := fs.Parse(args); err != nil {
			t.Fatal(err)
		}
		ignoreLocalDefaults(fs, &cfg)
		return cfg
	}
	if cfg := parse(); cfg.Workspace != "alpha" || cfg.Profile != "Default" {
		t.Errorf("ローカル設定の値を初期値にしている: %+v", cfg)
	}
	if cfg := parse("-workspace", "beta", "-profile", "P2"); cfg.Workspace != "beta" || cfg.Profile != "P2" {
		t.Errorf("明示したフラグを捨てている: %+v", cfg)
	}
	t.Setenv(config.EnvWorkspace, "gamma")
	if cfg := parse(); cfg.Workspace != "gamma" {
		t.Errorf("環境変数を捨てている: %+v", cfg)
	}
}

// callSite は関数の中の 1 つの呼び出し。
type callSite struct {
	name    string // 呼び出した関数名（x.Parse なら Parse）
	pos     token.Pos
	guarded bool // `if ... := name(...); ... { return }` の形（結果で抜けている）
}

// callsIn はファイル内の関数 fn の中の呼び出しを、出現順に返す。
func callsIn(t *testing.T, file, fn string) []callSite {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	callName := func(c *ast.CallExpr) string {
		switch x := c.Fun.(type) {
		case *ast.Ident:
			return x.Name
		case *ast.SelectorExpr:
			return x.Sel.Name
		}
		return ""
	}
	var out []callSite
	found := false
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != fn || fd.Recv != nil {
			continue
		}
		found = true
		guarded := map[*ast.CallExpr]bool{}
		ast.Inspect(fd, func(n ast.Node) bool {
			ifs, ok := n.(*ast.IfStmt)
			if !ok {
				return true
			}
			as, ok := ifs.Init.(*ast.AssignStmt)
			if !ok || len(as.Rhs) != 1 {
				return true
			}
			c, ok := as.Rhs[0].(*ast.CallExpr)
			if !ok {
				return true
			}
			for _, st := range ifs.Body.List {
				if _, ok := st.(*ast.ReturnStmt); ok {
					guarded[c] = true
				}
			}
			return true
		})
		ast.Inspect(fd, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok {
				out = append(out, callSite{name: callName(c), pos: c.Pos(), guarded: guarded[c]})
			}
			return true
		})
	}
	if !found {
		t.Fatalf("%s に関数 %s が無い", file, fn)
	}
	return out
}

// 🚨 守りが「呼ばれている」「結果で抜けている」「使われる前（または解析の後）に呼ばれている」ことを固定する。
// ヘルパーの単体テストは配線を守らない（呼び出しを消しても、結果を捨てても、後ろへ動かしても緑のまま）。
func TestLocalConfigGuardsAreWired(t *testing.T) {
	type rule struct {
		file, fn, guard string
		mustGuard       bool   // 結果で return している形に限る
		before, after   string // guard がこの呼び出しより前 / 後ろにあること
	}
	rules := []rule{
		// 壊れたローカル設定の検査は、本物のフラグ解析の後ろ（help は ErrHelp で先に抜ける）。
		{file: "main.go", fn: "parseArgs", guard: "checkLocalConfig", mustGuard: true, after: "Parse"},
		// フラグを持たない config / config get も検査してから表示する。
		{file: "config_cmd.go", fn: "cmdConfig", guard: "checkLocalConfig", mustGuard: true, before: "configShow"},
		// config get の検査はここでは固定しない（cmdConfig の最初の検査は show の case のものなので、位置の比較では
		// get の case から検査を消しても緑のまま）。get は TestBrokenLocalStopsCommands が挙動で固定している。
		// 同じく、guarded は if の条件の向きまでは見ない（反転は挙動のテストが捕まえる）。
		// config.yml に保存するコマンドは、検出・接続より前にローカル設定の初期値を外す。
		{file: "config_cmd.go", fn: "configInit", guard: "ignoreLocalDefaults", before: "scanProfiles"},
		{file: "config_cmd.go", fn: "configInit", guard: "ignoreLocalDefaults", before: "openSession"},
		{file: "setup.go", fn: "cmdSetup", guard: "ignoreLocalDefaults", before: "scanProfiles"},
		{file: "setup.go", fn: "cmdSetup", guard: "ignoreLocalDefaults", before: "openSession"},
	}
	// Slack に問い合わせるコマンドは、parseArgs（= ローカル設定の検査）を通ってから接続する。
	for file, fns := range map[string][]string{
		"search.go": {"cmdSearch"}, "channels.go": {"cmdChannels"}, "history.go": {"cmdHistory", "cmdThread"},
		"users.go": {"cmdUsers"}, "whoami.go": {"cmdWhoami"}, "setup.go": {"cmdSetup"}, "config_cmd.go": {"configInit"},
	} {
		for _, fn := range fns {
			rules = append(rules, rule{file: file, fn: fn, guard: "parseArgs", mustGuard: true, before: "openSession"})
		}
	}
	for _, r := range rules {
		calls := callsIn(t, r.file, r.fn)
		first := func(name string, guardedOnly bool) token.Pos {
			for _, c := range calls {
				if c.name == name && (!guardedOnly || c.guarded) {
					return c.pos
				}
			}
			return token.NoPos
		}
		g := first(r.guard, r.mustGuard)
		if !g.IsValid() {
			t.Errorf("%s が %s を呼んでいない（または結果で抜けていない）", r.fn, r.guard)
			continue
		}
		if r.before != "" {
			b := first(r.before, false)
			if !b.IsValid() {
				t.Errorf("%s に %s の呼び出しが無い（テストを更新すること）", r.fn, r.before)
			} else if g > b {
				t.Errorf("%s で %s が %s より後ろにある", r.fn, r.guard, r.before)
			}
		}
		if r.after != "" {
			a := first(r.after, false)
			if !a.IsValid() {
				t.Errorf("%s に %s の呼び出しが無い（テストを更新すること）", r.fn, r.after)
			} else if g < a {
				t.Errorf("%s で %s が %s より前にある", r.fn, r.guard, r.after)
			}
		}
	}
}
