package slack

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// parsePackage はこの package の .go ファイル（テストを除く）を AST で読む。
func parsePackage(t *testing.T) map[string]*ast.File {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	out := map[string]*ast.File{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("%s の解析に失敗: %v", name, err)
		}
		out[name] = f
	}
	if len(out) == 0 {
		t.Fatal("解析対象のファイルが 0 件（テストが何も検査していない）")
	}
	return out
}

// exprStr は "c.http" のような単純な式を文字列にする（判定用の最小実装）。
func exprStr(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return exprStr(v.X) + "." + v.Sel.Name
	}
	return ""
}

// enclosingFunc は pos を含む関数名を返す。
func enclosingFunc(f *ast.File, pos token.Pos) string {
	name := ""
	ast.Inspect(f, func(n ast.Node) bool {
		fd, ok := n.(*ast.FuncDecl)
		if !ok {
			return true
		}
		if fd.Pos() <= pos && pos <= fd.End() {
			name = fd.Name.Name
		}
		return true
	})
	return name
}

// HTTP リクエストを発行する経路が do() の 1 箇所だけであることを固定する。
//
// 🚨 これが無いと「ホスト固定・allowlist・資格情報の載せ方」を do で守っても、
// 別の関数が直接 http.Post する変更を検出できない。単体テストは
// 「その関数が呼ばれていないこと」を 1 mm も守らない。
//
// # 脅威モデル（このゲートが止めるもの / 止めないもの）
//
// 止めるもの: 「うっかり別経路を足す」。api.go に http.Post を書く、resolve.go で
// 直接リクエストを組む、といった素直な書き方を機械で止める。
//
// 🚨 止めないもの（構文ベースのゲートなので原理的に迂回できる）:
//   - http.Client を別名の変数・フィールドに取ってから Do する
//     （例: cl := c.http; cl.Do(req) / type wrapper struct{ h *http.Client }）
//   - net/http 以外のスタック（net.Dial / 生の TLS / 外部ライブラリ）を使う
//   - リフレクションや関数値経由の呼び出し
//
// これらを塞ごうとすると迂回が無限に出る（規則の軸が構文にあるため）。意図的な迂回は
// レビューの責務とし、このゲートは「うっかり」だけを担当する。実効的な防御は
// resolve_test.go の「到達したホストの集合」検査（意味ベース）であって、こちらは補助。
func TestOnlyDoIssuesHTTPRequests(t *testing.T) {
	files := parsePackage(t)

	// 監視するのは「リクエストを作る / 送る」呼び出しだけ。
	//
	// 🚨 メソッド名だけで判定すると resp.Header.Get のような無関係な呼び出しを拾う
	// （最初に書いたときに実際に踏んだ）。レシーバが http パッケージか、
	// Client が持つ HTTP クライアント（c.http）であることまで見る。
	watched := map[string]bool{
		"Do": true, "Post": true, "PostForm": true, "Get": true, "Head": true,
		"NewRequest": true, "NewRequestWithContext": true,
	}
	allowedIn := map[string]string{ // 呼び出し名 -> 許される関数名
		"Do":                    "do",
		"NewRequestWithContext": "do",
	}

	found := map[string]int{}
	for name, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !watched[sel.Sel.Name] {
				return true
			}
			switch exprStr(sel.X) {
			case "http", "c.http":
			default:
				return true // net/http とも Client の HTTP クライアントとも無関係
			}
			fn := enclosingFunc(f, call.Pos())
			found[sel.Sel.Name]++
			if want, ok := allowedIn[sel.Sel.Name]; !ok || fn != want || name != "client.go" {
				t.Errorf("HTTP リクエストの発行が do() の外にある: %s の %s() で %s を呼んでいる", name, fn, sel.Sel.Name)
			}
			return true
		})
	}

	// canary: 本走査が実際に何かを見ていること（抽出が空 = 違反 0 件 = 緑 を塞ぐ）。
	if found["Do"] != 1 {
		t.Errorf("http クライアントの Do 呼び出しが %d 件（1 件であるべき）。走査が壊れているか、経路が増えている", found["Do"])
	}
	if found["NewRequestWithContext"] != 1 {
		t.Errorf("リクエスト組み立てが %d 件（1 件であるべき）", found["NewRequestWithContext"])
	}
}

// Method の値は method.go の allowlist 以外で作られないことを固定する。
//
// 型で閉じているのは「package の外」だけなので、package 内で Method{"chat.postMessage"} と
// 書けてしまう。ここで literal の出現場所を固定し、実行時ガード（isAllowed）と二重に守る。
func TestMethodLiteralsOnlyInMethodFile(t *testing.T) {
	files := parsePackage(t)
	count := 0
	for name, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			id, ok := lit.Type.(*ast.Ident)
			if !ok || id.Name != "Method" {
				return true
			}
			count++
			if name != "method.go" {
				t.Errorf("Method の値が %s で作られている（allowlist は method.go だけで定義する）", name)
			}
			return true
		})
	}
	if count == 0 {
		t.Error("Method リテラルが 1 つも見つからない（走査が壊れている）")
	}
}
