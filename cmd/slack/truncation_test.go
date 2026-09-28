package main

import (
	"bytes"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jiikko/slack-cli/internal/auth"
	"github.com/jiikko/slack-cli/internal/slack"
)

// 一覧取得のエラーの振り分け:
//   - 打ち切り: 警告して取得分を表示（rc=0）
//   - 途中失敗: 取得分を表示してから返す（rc≠0。完了ではない）
//   - それ以外: 何も表示せずに返す
func TestSplitListErr(t *testing.T) {
	var w bytes.Buffer
	after, fatal := splitListErrTo(&w, &slack.TruncatedError{Method: "conversations.history", Pages: 50, Count: 10})
	if after != nil || fatal != nil {
		t.Errorf("打ち切りで取得分を捨てている / 非 0 にしている: after=%v fatal=%v", after, fatal)
	}
	if !strings.Contains(w.String(), "警告:") {
		t.Errorf("打ち切りを警告していない: %q", w.String())
	}

	partial := &slack.PartialError{Method: "conversations.history", Count: 1000, Err: errors.New("429")}
	after, fatal = splitListErrTo(&w, partial)
	if fatal != nil {
		t.Errorf("途中失敗で取得分を捨てている: %v", fatal)
	}
	if after != partial {
		t.Errorf("途中失敗を完了扱い（rc=0）にしている: %v", after)
	}

	boom := errors.New("boom")
	if after, fatal := splitListErrTo(&w, boom); fatal != boom || after != nil {
		t.Errorf("その他のエラーの扱いが違う: after=%v fatal=%v", after, fatal)
	}
}

// 途中失敗のときも、出力してから after を返すこと（出力を省かない / after を落とさない）。
// TSV・JSON・0 件のどの出力経路でも rc≠0 になること（経路ごとに return があるため全部見る）。
func TestFinishListRendersThenReturnsAfter(t *testing.T) {
	partial := &slack.PartialError{Method: "m", Count: 1, Err: errors.New("x")}
	rendered := false
	err := finishList(false, []int{1}, func() string { rendered = true; return "" }, partial)
	if !rendered {
		t.Error("部分結果を出力していない")
	}
	if err != partial {
		t.Errorf("TSV: 出力後に途中失敗を返していない（rc=0 になる）: %v", err)
	}
	if err := finishList(true, []int{1}, func() string { return "" }, partial); err != partial {
		t.Errorf("JSON: 途中失敗を返していない（rc=0 になる）: %v", err)
	}
	if err := finishList(false, []int{}, func() string { return "" }, partial); err != partial {
		t.Errorf("0 件: 途中失敗を返していない（rc=0 になる）: %v", err)
	}
}

// 🚨 一覧を取る各コマンドは、取得のエラーを splitListErr で振り分け、finishList で出力すること。
//
// `if err != nil { return err }` のままだと、打ち切り・途中失敗のときに取得できた分まで捨てる
// （history / thread にページングを入れた時点でこの経路が生まれた）。
//
// 脅威モデル: うっかり（新しいコマンドの追加・整理で素の return err に戻る）を止める。
// 意図的な迂回（別名の関数で包む等）は検出しない。
func TestListCommandsHandleTruncation(t *testing.T) {
	want := map[string]string{
		"cmdHistory":  "history.go",
		"cmdThread":   "history.go",
		"cmdChannels": "channels.go",
		"cmdUsers":    "users.go",
	}
	fset := token.NewFileSet()
	for fn, file := range want {
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		var body *ast.FuncDecl
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == fn {
				body = fd
			}
		}
		if body == nil {
			t.Fatalf("%s が %s に見つからない（走査が壊れている）", fn, file)
		}
		called := map[string]bool{}
		ast.Inspect(body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if id, ok := call.Fun.(*ast.Ident); ok {
					called[id.Name] = true
					// finishList には splitListErr が返した after を渡すこと（nil を渡すと途中失敗が rc=0 になる）。
					if id.Name == "finishList" {
						last, ok := call.Args[len(call.Args)-1].(*ast.Ident)
						if !ok || last.Name != "after" {
							t.Errorf("%s: finishList の最後の引数が after ではない（途中失敗が rc=0 になる）", fn)
						}
					}
				}
			}
			return true
		})
		for _, want := range []string{"splitListErr", "finishList"} {
			if !called[want] {
				t.Errorf("%s が %s を通っていない（打ち切り・途中失敗で取得分を捨てるか、完了に見せる）", fn, want)
			}
		}
	}
}

// 🚨 ワークスペース候補の検出（setup / config init）で、1 プロファイルだけ読めないときは
// 記録して後ろのプロファイルへ進むこと。全滅したときは、読めなかった理由と対処を添えること。
func TestDiscoverySkipsUnreadableProfile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root では権限拒否を作れない")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	chrome := filepath.Join(home, "Library", "Application Support", "Google", "Chrome")
	mk := func(profile string) string {
		dir := filepath.Join(chrome, profile)
		if err := os.MkdirAll(filepath.Join(dir, "Local Storage", "leveldb"), 0o700); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	locked := mk("Default")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })

	// 読めるプロファイルが無い → 候補なし + 理由と対処。
	found, issues, err := scanProfiles(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 || len(issues) != 1 || issues[0].Profile != "Default" {
		t.Fatalf("読めないプロファイルを記録していない: found=%v issues=%v", found, issues)
	}
	if note := auth.IssueNote(issues); !strings.Contains(note, "フルディスクアクセス") || !strings.Contains(note, "パーミッション／所有者") {
		t.Errorf("全滅時の案内が無い: %s", note)
	}

	// 後ろに正常なプロファイルがあれば、そちらが使われる。
	ok := mk("Profile 1")
	if err := os.WriteFile(filepath.Join(ok, "Local Storage", "leveldb", "000003.log"), []byte("https://acme.slack.com/"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ps := auth.ListProfiles(); len(ps) != 2 {
		t.Fatalf("前提: Default と Profile 1 が候補になるはず: %+v", ps)
	}
	found, _, err = scanProfiles(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].profile != "Profile 1" || found[0].hints[0].Domain != "acme" {
		t.Errorf("読めないプロファイルの後ろの正常なプロファイルが使われていない: %+v", found)
	}
}

// 🚨 作業領域の異常（全プロファイル共通）は、候補なしに畳まずその場で返すこと。
// 畳むと setup / config init が「ログインしてから」と誤案内する（再現済み）。
func TestDiscoveryStopsOnTempRootFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	profile := filepath.Join(home, "Library", "Application Support", "Google", "Chrome", "Default")
	if err := os.MkdirAll(filepath.Join(profile, "Local Storage", "leveldb"), 0o700); err != nil {
		t.Fatal(err)
	}
	caches := filepath.Join(home, "Library", "Caches")
	if err := os.MkdirAll(caches, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(caches, "slack-cli")); err != nil {
		t.Skipf("シンボリックリンクを作れない: %v", err)
	}
	_, _, err := scanProfiles(false)
	if !auth.IsEnvError(err) {
		t.Fatalf("作業領域の異常が握り潰された: %v", err)
	}
	if !strings.Contains(err.Error(), "作業領域") {
		t.Errorf("作業領域の案内が無い: %v", err)
	}
}
