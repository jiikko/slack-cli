package output

import (
	"strings"
	"testing"
	"time"
)

type row struct{ A, B string }

func testRegistry() *Registry[row] {
	return NewRegistry[row]("a", []string{"a", "b"}, map[string]Column[row]{
		"a": {Header: "A", Value: func(r row) string { return r.A }},
		"b": {Header: "B", Value: func(r row) string { return r.B }},
	})
}

// TSV のセルを無害化すること（タブ・改行で列がずれない、ESC を端末へ流さない）。
//
// 🚨 Slack のメッセージ本文は人が自由に書ける値。ここを通さないと
// awk / cut が壊れ、ESC シーケンスが端末に届く。
func TestRenderSanitizesCells(t *testing.T) {
	rows := []row{
		{A: "タブ\tと\n改行", B: "ok"},
		{A: "\x1b]0;PWNED\x07普通の値", B: "\x7f削除文字"},
	}
	out := testRegistry().Render(rows, []string{"a", "b"}, true)

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 { // ヘッダ + 2 行
		t.Fatalf("行数が違う（改行が潰れていない）: %d\n%q", len(lines), out)
	}
	for i, line := range lines {
		if n := strings.Count(line, "\t"); n != 1 {
			t.Errorf("%d 行目のタブが %d 個（1 個であるべき）: %q", i+1, n, line)
		}
	}
	if strings.ContainsRune(out, 0x1b) || strings.ContainsRune(out, 0x07) || strings.ContainsRune(out, 0x7f) {
		t.Errorf("制御文字が残っている: %q", out)
	}
	// 値そのものは消さない（無害化は「潰す」であって「捨てる」ではない）。
	if !strings.Contains(out, "普通の値") || !strings.Contains(out, "削除文字") {
		t.Errorf("値まで落としている: %q", out)
	}
}

// ヘッダの有無を制御できること。
func TestRenderHeaderToggle(t *testing.T) {
	rows := []row{{A: "1", B: "2"}}
	with := testRegistry().Render(rows, []string{"a"}, true)
	without := testRegistry().Render(rows, []string{"a"}, false)
	if !strings.HasPrefix(with, "A\n") {
		t.Errorf("ヘッダが無い: %q", with)
	}
	if strings.Contains(without, "A\n") {
		t.Errorf("-no-header でヘッダが出ている: %q", without)
	}
}

// カラム指定の検証（不明なカラムは案内つきで弾く）。
func TestParseColumns(t *testing.T) {
	r := testRegistry()
	got, err := r.Parse("b,a")
	if err != nil || strings.Join(got, ",") != "b,a" {
		t.Errorf("指定順を保つべき: %v %v", got, err)
	}
	if got, _ := r.Parse(""); strings.Join(got, ",") != "a" {
		t.Errorf("空指定は既定を使うべき: %v", got)
	}
	_, err = r.Parse("a,zzz")
	if err == nil {
		t.Fatal("不明なカラムは弾くべき")
	}
	if !strings.Contains(err.Error(), "指定可能") {
		t.Errorf("案内が無い: %v", err)
	}
	if _, err := r.Parse(","); err == nil {
		t.Error("有効なカラムが 0 件ならエラーにすべき")
	}
}

// ts の整形（解釈できない値は捨てずにそのまま出す）。
func TestFormatTs(t *testing.T) {
	want := time.Unix(1725000000, 0).Local().Format("2006-01-02 15:04:05")
	if got := FormatTs("1725000000.123456"); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := FormatTs("1725000000"); got != want {
		t.Errorf("小数部なし: got %q", got)
	}
	for _, bad := range []string{"", "abc", "0", "-1"} {
		if got := FormatTs(bad); got != bad {
			t.Errorf("解釈できない値は温存すべき: %q -> %q", bad, got)
		}
	}
}

// JSON 出力は HTML エスケープしないこと（permalink の & が壊れる）。
func TestPrintJSONKeepsRawCharacters(t *testing.T) {
	var sb strings.Builder
	if err := PrintJSON(&sb, map[string]string{"url": "https://x.slack.com/?a=1&b=2<>"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sb.String(), "&b=2<>") {
		t.Errorf("エスケープされている: %s", sb.String())
	}
}
