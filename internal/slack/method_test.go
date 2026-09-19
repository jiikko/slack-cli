package slack

import (
	"context"
	"go/ast"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// 仕様 §7.2 の許可メソッド。ここが allowlist の正本であり、
// コード側（method.go の var 群と allowedMethods）はこれと一致していなければならない。
var specAllowed = []string{
	"auth.test",
	"search.messages",
	"search.modules",
	"conversations.list",
	"conversations.history",
	"conversations.replies",
	"conversations.info",
	"users.list",
	"users.info",
	"users.lookupByEmail",
	"team.info",
	"emoji.list",
}

// 仕様 §7.2 の禁止メソッド（副作用のあるもの）。allowlist に載っていてはいけない。
var specForbidden = []string{
	"chat.postMessage", "chat.update", "chat.delete", "chat.meMessage",
	"reactions.add", "reactions.remove",
	"files.upload", "files.completeUploadExternal",
	"conversations.create", "conversations.invite", "conversations.archive",
	"conversations.rename", "conversations.setTopic", "conversations.setPurpose",
	"pins.add", "pins.remove", "bookmarks.add",
	"users.profile.set", "users.setPresence",
}

// allowedMethods が仕様の一覧と完全に一致すること（増えても減っても落ちる）。
func TestAllowlistMatchesSpec(t *testing.T) {
	got := make([]string, 0, len(allowedMethods))
	for m := range allowedMethods {
		got = append(got, m)
	}
	sort.Strings(got)
	want := append([]string(nil), specAllowed...)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("allowlist が仕様と一致しない\n  got:  %v\n  want: %v", got, want)
	}
	for _, m := range specForbidden {
		if isAllowed(m) {
			t.Errorf("書き込み系メソッドが allowlist に入っている: %s", m)
		}
	}
}

// method.go の Method 変数と allowedMethods が食い違わないこと。
//
// 🚨 2 つを別々に持つのは意図的（片方だけ書き換える変異を検出するため）だが、
// その代わり「両者の一致」を機械で固定しないと、静かにずれる。
func TestMethodVarsAndAllowlistAgree(t *testing.T) {
	files := parsePackage(t)
	f, ok := files["method.go"]
	if !ok {
		t.Fatal("method.go が見つからない")
	}
	names := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		id, ok := lit.Type.(*ast.Ident)
		if !ok || id.Name != "Method" || len(lit.Elts) != 1 {
			return true
		}
		bl, ok := lit.Elts[0].(*ast.BasicLit)
		if !ok {
			return true
		}
		s, err := strconv.Unquote(bl.Value)
		if err != nil {
			return true
		}
		names[s] = true
		return true
	})
	if len(names) == 0 {
		t.Fatal("Method 変数を 1 つも検出できない（走査が壊れている）")
	}
	for n := range names {
		if !isAllowed(n) {
			t.Errorf("Method 変数 %q が allowedMethods に無い（実行時ガードに拒否される）", n)
		}
	}
	for m := range allowedMethods {
		if !names[m] {
			t.Errorf("allowedMethods の %q に対応する Method 変数が無い（呼べない定義）", m)
		}
	}
}

// ②: package 内でうっかり作った禁止メソッドを、送信の直前に拒否すること。
//
// 型で閉じているのは package の外だけなので、ここが最後の砦になる。
func TestDoRejectsMethodOutsideAllowlist(t *testing.T) {
	rt := &recordingTransport{}
	c, err := New("alpha", tokenForAlpha, sentinelCookie, withTransport(rt))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range specForbidden {
		_, err := c.do(context.Background(), Method{name}, url.Values{})
		if err == nil {
			t.Errorf("%s の呼び出しが通ってしまった", name)
			continue
		}
		if !strings.Contains(err.Error(), "allowlist") {
			t.Errorf("%s: 理由が allowlist だと分かるメッセージにすべき: %v", name, err)
		}
	}
	if len(rt.hosts) != 0 {
		t.Fatalf("拒否したはずのメソッドで送信が発生した: %v", rt.hosts)
	}
}
