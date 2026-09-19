// Package output は TSV / JSON の整形を担う。
//
// 既定は TSV（esa-cli 準拠）。`-json` で JSON、`-no-header` でヘッダ抑制。
package output

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Sanitize は TSV の 1 セルを無害化する。
//
// 🚨 Slack のメッセージ本文・チャンネル名・ユーザー名は人が自由に書ける値で、
// タブや改行が入ると TSV の列がずれて awk / cut が壊れる。ESC が入れば端末そのものを
// 操作される（タイトルバー書き換え・カーソル移動・色の固定など）。
// 通すのは**値だけ**。カラム名はソース定数なので通さない。
func Sanitize(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			b.WriteRune(' ')
		case r < 0x20 || r == 0x7f:
			// ESC を含む C0 制御文字と DEL は落とす
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Column は 1 カラムの定義（見出しと値の取り出し）。
type Column[T any] struct {
	Header string
	Value  func(T) string
}

// Registry はコマンドごとのカラム定義。
type Registry[T any] struct {
	cols     map[string]Column[T]
	order    []string // 案内表示に使う正規の並び（エイリアスを含まない）
	defaults string
}

// NewRegistry はカラム定義を作る。order は `-c` のヘルプに出す正規の並び。
func NewRegistry[T any](defaults string, order []string, cols map[string]Column[T]) *Registry[T] {
	return &Registry[T]{cols: cols, order: order, defaults: defaults}
}

// Defaults は既定のカラム指定（カンマ区切り）を返す。
func (r *Registry[T]) Defaults() string { return r.defaults }

// Available は指定可能なカラム名を案内用に返す。
func (r *Registry[T]) Available() string {
	if len(r.order) > 0 {
		return strings.Join(r.order, ", ")
	}
	names := make([]string, 0, len(r.cols))
	for n := range r.cols {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// Parse は "ts,channel,text" のような指定を検証して展開する。
func (r *Registry[T]) Parse(spec string) ([]string, error) {
	if strings.TrimSpace(spec) == "" {
		spec = r.defaults
	}
	var cols []string
	for _, raw := range strings.Split(spec, ",") {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		if _, ok := r.cols[name]; !ok {
			return nil, fmt.Errorf("不明なカラム %q。指定可能: %s", name, r.Available())
		}
		cols = append(cols, name)
	}
	if len(cols) == 0 {
		return nil, fmt.Errorf("有効なカラムがありません")
	}
	return cols, nil
}

// Render はタブ区切りでヘッダ + 各行を返す（全角/半角の桁揃えはせず、タブに委ねる）。
func (r *Registry[T]) Render(items []T, cols []string, header bool) string {
	var sb strings.Builder
	if header {
		hs := make([]string, len(cols))
		for i, c := range cols {
			hs[i] = r.cols[c].Header
		}
		sb.WriteString(strings.Join(hs, "\t"))
		sb.WriteByte('\n')
	}
	for i := range items {
		vs := make([]string, len(cols))
		for j, c := range cols {
			vs[j] = Sanitize(r.cols[c].Value(items[i]))
		}
		sb.WriteString(strings.Join(vs, "\t"))
		sb.WriteByte('\n')
	}
	return sb.String()
}

// PrintJSON は v を整形した JSON として書き出す。
func PrintJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// FormatTs は Slack の ts（"1725000000.123456"）を人が読める日時にする。
// 解釈できない値はそのまま返す（勝手に空にしない）。
func FormatTs(ts string) string {
	sec := ts
	if i := strings.IndexByte(ts, '.'); i >= 0 {
		sec = ts[:i]
	}
	n, err := strconv.ParseInt(sec, 10, 64)
	if err != nil || n <= 0 {
		return ts
	}
	return time.Unix(n, 0).Local().Format("2006-01-02 15:04:05")
}

// Bool は TSV 向けの true/false 表記。
func Bool(b bool) string { return strconv.FormatBool(b) }

// Int は TSV 向けの数値表記。
func Int(n int) string { return strconv.Itoa(n) }
