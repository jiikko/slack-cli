package main

import (
	"errors"
	"flag"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/jiikko/slack-cli/internal/config"
)

// 終了コードの出し分け（仕様 §8: 0 成功 / 1 実行時エラー / 2 使い方エラー）。
func TestExitCodeFor(t *testing.T) {
	if got := exitCodeFor(nil); got != 0 {
		t.Errorf("成功: got %d", got)
	}
	if got := exitCodeFor(&config.UsageError{Msg: "bad"}); got != 2 {
		t.Errorf("使い方の誤り: got %d, want 2", got)
	}
	if got := exitCodeFor(errors.New("boom")); got != 1 {
		t.Errorf("実行時エラー: got %d, want 1", got)
	}
	// ラップされていても分類できること。
	wrapped := errors.Join(errors.New("context"), &config.UsageError{Msg: "bad"})
	if got := exitCodeFor(wrapped); got != 2 {
		t.Errorf("ラップされた使い方エラー: got %d, want 2", got)
	}
}

// 引数の後ろに置いたフラグを検出すること（黙って 0 件になるのを防ぐ）。
func TestCheckNoTrailingFlags(t *testing.T) {
	newFS := func() *flag.FlagSet {
		fs := flag.NewFlagSet("search", flag.ContinueOnError)
		fs.String("c", "", "columns")
		fs.Int("n", 0, "count")
		fs.Bool("json", false, "json")
		return fs
	}
	cases := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{name: "クエリだけ", args: []string{"in:#general", "キーワード"}},
		{name: "Slack の除外検索は誤検出しない", args: []string{"-退職"}},
		{name: "後ろに -c", args: []string{"キーワード", "-c", "text"}, wantErr: true},
		{name: "後ろに -json", args: []string{"キーワード", "-json"}, wantErr: true},
		{name: "後ろに --n=5", args: []string{"キーワード", "--n=5"}, wantErr: true},
	}
	for _, c := range cases {
		err := checkNoTrailingFlags(newFS(), c.args)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err=%v, wantErr=%v", c.name, err, c.wantErr)
		}
		if c.wantErr && err != nil {
			var ue *config.UsageError
			if !errors.As(err, &ue) {
				t.Errorf("%s: 使い方エラー（rc=2）にすべき: %T", c.name, err)
			}
		}
	}
}

// -in / -from がクエリの接頭辞になること。
func TestBuildQueryPrefix(t *testing.T) {
	cases := map[string]string{}
	cases[buildQueryPrefix("general", "")] = "in:#general"
	cases[buildQueryPrefix("#general", "")] = "in:#general"
	cases[buildQueryPrefix("", "alice")] = "from:@alice"
	cases[buildQueryPrefix("", "@alice")] = "from:@alice"
	cases[buildQueryPrefix("general", "alice")] = "in:#general from:@alice"
	cases[buildQueryPrefix("", "")] = ""
	for got, want := range cases {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

// 🚨 --help は stdout へ 1 回だけ出ること。
//
// flag パッケージは ContinueOnError のとき、失敗時に自分でエラー文を Output へ書き、
// さらに Usage を呼ぶ。既定のままだと help が 2 回（Usage 経由と parseArgs 経由）出て、
// フラグの誤りでは「flag の生エラー + help + こちらのエラー」が重なる。
func TestHelpIsPrintedOnce(t *testing.T) {
	const help = "SLACK-CLI-HELP-SENTINEL\n"

	// --help: stdout に 1 回だけ。stderr には出さない。
	stdout, stderr, done, err := captureParseArgs(t, help, []string{"--help"})
	if err != nil || !done {
		t.Fatalf("--help は正常終了すべき: done=%v err=%v", done, err)
	}
	if n := strings.Count(stdout, "SLACK-CLI-HELP-SENTINEL"); n != 1 {
		t.Errorf("help が stdout に %d 回出ている（1 回であるべき）", n)
	}
	// 🚨 flag パッケージ自身には**何も**出力させない（出力は parseArgs に一本化する）。
	// 「sentinel が出ていないこと」だけを見ると、flag が別の文言を出す変異を素通しする。
	if stderr != "" {
		t.Errorf("--help で stderr へ出力している（flag パッケージの Usage が生きている）: %q", stderr)
	}

	// フラグの誤り: 出力は 1 本のエラーだけ。help 全文を撒かない。
	stdout, stderr, done, err = captureParseArgs(t, help, []string{"-nosuchflag"})
	if err == nil || done {
		t.Fatal("フラグの誤りはエラーにすべき")
	}
	if exitCodeFor(err) != 2 {
		t.Errorf("終了コード: got %d, want 2", exitCodeFor(err))
	}
	if stdout != "" || stderr != "" {
		t.Errorf("フラグ誤りの出力が二重になっている（メッセージは戻り値の error だけで運ぶ）: stdout=%q stderr=%q", stdout, stderr)
	}
	if !strings.Contains(err.Error(), "--help") {
		t.Errorf("help への誘導が無い: %v", err)
	}
}

// captureParseArgs は parseArgs の stdout / stderr を捕まえる。
func captureParseArgs(t *testing.T, help string, args []string) (stdout, stderr string, done bool, err error) {
	t.Helper()
	outR, outW, _ := os.Pipe()
	errR, errW, _ := os.Pipe()
	origOut, origErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	defer func() { os.Stdout, os.Stderr = origOut, origErr }()

	fs := newFlagSet("search")
	fs.String("c", "", "columns")
	done, err = parseArgs(fs, help, args)

	outW.Close()
	errW.Close()
	o, _ := io.ReadAll(outR)
	e, _ := io.ReadAll(errR)
	return string(o), string(e), done, err
}

// カラム指定の誤りは「使い方エラー」(rc=2) にすること。
//
// 🚨 rc=1（実行時エラー）だと、スクリプトから「Slack 側の問題」と区別できない。
func TestParseColsReturnsUsageError(t *testing.T) {
	for _, spec := range []string{"nosuchcol", "id,nosuchcol", ","} {
		_, err := parseCols(channelColumns, spec)
		if err == nil {
			t.Errorf("%q: エラーにすべき", spec)
			continue
		}
		if got := exitCodeFor(err); got != 2 {
			t.Errorf("%q: 終了コード %d（使い方エラー=2 であるべき）: %v", spec, got, err)
		}
	}
	// 正しい指定は通ること（canary: 常にエラーを返す実装になっていないこと）。
	if _, err := parseCols(channelColumns, "id,name"); err != nil {
		t.Errorf("正しい指定が通らない: %v", err)
	}
	// 各コマンドのレジストリすべてで同じ扱いになること。
	if _, err := parseCols(messageColumns, "nosuchcol"); exitCodeFor(err) != 2 {
		t.Error("search / history のカラム誤りが rc=2 になっていない")
	}
	if _, err := parseCols(userColumns, "nosuchcol"); exitCodeFor(err) != 2 {
		t.Error("users のカラム誤りが rc=2 になっていない")
	}
}

// 🚨 一時コピーの後始末が main の経路から実際に呼ばれていることを固定する。
//
// 機構（cleanup.go）の単体テストは「呼ばれていること」を 1 mm も守らない。
// 呼び出しを消す変異を当てても、機構のテストは全部 green のままになる。
func TestCleanupIsWiredInMain(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var mainFn *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "main" && fd.Recv == nil {
			mainFn = fd
		}
	}
	if mainFn == nil {
		t.Fatal("main 関数が見つからない")
	}

	// 🚨 「main のどこかで呼ばれている」だけでは足りない。サブコマンドの switch は
	// case の中で return するので、掃除の呼び出しが switch より後ろにあると
	// slack help では到達しない（＝このテストが名指しで守ると言っている退行が、
	// 呼び出しを末尾へ動かすだけで緑のまま再現する）。位置まで固定する。
	var switchPos token.Pos
	ast.Inspect(mainFn, func(n ast.Node) bool {
		if sw, ok := n.(*ast.SwitchStmt); ok && !switchPos.IsValid() {
			switchPos = sw.Pos()
		}
		return true
	})
	if !switchPos.IsValid() {
		t.Fatal("main のサブコマンド分岐（switch）が見つからない")
	}

	calls := map[string]int{}
	callPos := map[string]token.Pos{}
	ast.Inspect(mainFn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "auth" {
			return true
		}
		calls[sel.Sel.Name]++
		if _, seen := callPos[sel.Sel.Name]; !seen {
			callPos[sel.Sel.Name] = call.Pos()
		}
		return true
	})

	if calls["InstallCleanupOnSignal"] == 0 {
		t.Error("シグナル経路の後始末（②）が main で仕掛けられていない")
	}
	// 🚨 ③（前回の残骸の掃除）は main から呼ぶこと。資格情報を読む経路に置くと
	// slack help / slack config では走らず、SIGKILL で残ったコピーが残り続ける。
	if calls["SweepStaleTempDirs"] == 0 {
		t.Error("起動時の掃除（③）が main で呼ばれていない")
	} else if callPos["SweepStaleTempDirs"] > switchPos {
		t.Error("起動時の掃除（③）がサブコマンド分岐より後ろにある。" +
			"case の中で return するため slack help 等では到達しない")
	}
	if pos, ok := callPos["InstallCleanupOnSignal"]; ok && pos > switchPos {
		t.Error("シグナル経路の後始末（②）がサブコマンド分岐より後ろにある")
	}
	// ①（defer）と、os.Exit の前（defer が走らない経路）の 2 箇所で呼ぶ。
	if calls["RunAllCleanups"] < 2 {
		t.Errorf("後始末の呼び出しが %d 箇所。os.Exit で終わる経路では defer が走らないため、"+
			"defer とエラー終了の両方で呼ぶこと", calls["RunAllCleanups"])
	}
}

// 🚨 資格情報を表示しうる箇所で、マスクせずに値を出していないこと。
//
// config show はトークンの設定状況を出すので、ここが唯一の露出候補になる。
func TestConfigShowMasksToken(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "config_cmd.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	src := 0
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Getenv" {
			return true
		}
		// os.Getenv(config.EnvToken) の結果は Mask を通してから表示すること。
		for _, a := range call.Args {
			if s, ok := a.(*ast.SelectorExpr); ok && s.Sel.Name == "EnvToken" {
				src++
			}
		}
		return true
	})
	if src == 0 {
		t.Skip("config show がトークンに触れていない（露出経路が無い）")
	}
	masked := strings.Contains(readFile(t, "config_cmd.go"), "auth.Mask(")
	if !masked {
		t.Error("トークンを Mask せずに表示している")
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	ast.Inspect(f, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if id, ok := sel.X.(*ast.Ident); ok {
				sb.WriteString(id.Name + "." + sel.Sel.Name + "(")
			}
		}
		return true
	})
	return sb.String()
}
