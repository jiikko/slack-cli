package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/jiikko/slack-cli/internal/config"
)

const searchHelp = `slack search - メッセージを検索する（search.messages）

使い方:
  slack search [オプション] <クエリ...>

  クエリは複数語をそのまま並べてよい（内部でスペース連結）。
  Slack の検索構文をそのまま透過する（in: / from: / before: / after: / has: 等）。
  並び順は新しい順（関連度順ではない）。

オプション:
  -c, -columns <list>  表示カラム（カンマ区切り）。既定: datetime,channel,user,text
  -no-header           ヘッダ行を出さない（awk -F'\t' 等でパースしやすい）
  -n <数>              取得件数（既定は config.yml の default_count、未設定なら 20）
  -page <数>           ページ番号（既定 1）
  -in <channel>        チャンネル絞り込み（クエリに in:#channel を付ける）
  -from <user>         投稿者絞り込み（クエリに from:@user を付ける）
  -json                JSON で出力
  （共通オプション -workspace / -profile / -token は slack --help を参照）

指定可能なカラム:
  ts          Slack の ts（1725000000.123456）
  datetime    ts をローカル時刻の日時にしたもの
  channel     #チャンネル名
  user        表示名（無ければユーザー ID）
  text        本文（改行・タブは空白に潰す）
  permalink   メッセージへのリンク

例:
  slack search 'デプロイ 失敗'
  slack search -in general -n 50 'リリース'
  slack search -from alice 'after:2026-09-01 障害'
  slack search -no-header -c permalink 'キーワード' | head -20
  slack search -json 'キーワード' | jq -r '.[].permalink'
`

func cmdSearch(args []string) error {
	var cfg config.Config
	var count, page int
	var columnsSpec, inChannel, fromUser string
	var noHeader bool

	fs := newFlagSet("search")
	registerCommon(fs, &cfg)
	fs.IntVar(&count, "n", cfg.Count, "取得件数")
	fs.IntVar(&page, "page", 1, "ページ番号")
	fs.StringVar(&columnsSpec, "columns", messageColumns.Defaults(), "表示カラム（カンマ区切り）。指定可能: "+messageColumns.Available())
	fs.StringVar(&columnsSpec, "c", messageColumns.Defaults(), "-columns の別名")
	fs.StringVar(&inChannel, "in", "", "チャンネル絞り込み（in:#channel を付与）")
	fs.StringVar(&fromUser, "from", "", "投稿者絞り込み（from:@user を付与）")
	fs.BoolVar(&noHeader, "no-header", false, "ヘッダ行を出力しない")
	if done, err := parseArgs(fs, searchHelp, args); err != nil || done {
		return err
	}
	if err := checkNoTrailingFlags(fs, fs.Args()); err != nil {
		return err
	}

	query := strings.TrimSpace(strings.Join(fs.Args(), " "))
	query = strings.TrimSpace(buildQueryPrefix(inChannel, fromUser) + " " + query)
	if query == "" {
		return &config.UsageError{Msg: "エラー: 検索クエリを指定してください。\n使い方: slack search [オプション] <クエリ...>\n例:     slack search 'デプロイ 失敗'\n詳細:   slack search --help"}
	}

	cols, err := parseCols(messageColumns, columnsSpec)
	if err != nil {
		return err
	}

	sess, err := openSession(cfg)
	if err != nil {
		return err
	}
	res, err := sess.Client.Search(context.Background(), query, count, page)
	if err != nil {
		return err
	}

	if cfg.JSON {
		return printJSON(res.Matches)
	}
	if len(res.Matches) == 0 {
		fmt.Fprintln(os.Stderr, "0 件")
		return nil
	}
	fmt.Print(messageColumns.Render(res.Matches, cols, !noHeader))
	if res.Pages > 1 {
		fmt.Fprintf(os.Stderr, "(全 %d 件 / %d ページ中 %d ページ目。-page で切り替え)\n", res.Total, res.Pages, res.Page)
	}
	return nil
}

// buildQueryPrefix は -in / -from をクエリの接頭辞へ変換する。
func buildQueryPrefix(inChannel, fromUser string) string {
	var parts []string
	if c := strings.TrimSpace(inChannel); c != "" {
		parts = append(parts, "in:#"+strings.TrimPrefix(c, "#"))
	}
	if u := strings.TrimSpace(fromUser); u != "" {
		parts = append(parts, "from:@"+strings.TrimPrefix(u, "@"))
	}
	return strings.Join(parts, " ")
}
