package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jiikko/slack-cli/internal/config"
)

const historyHelp = `slack history - チャンネルのメッセージを取得する（conversations.history）

使い方:
  slack history [オプション] <#チャンネル名|チャンネルID>

  #name を渡した場合は conversations.list で ID を解決してから取得する。

オプション:
  -n <数>              取得件数（既定 50）
  -oldest <ts>         この ts より新しいメッセージ
  -latest <ts>         この ts より古いメッセージ
  -c, -columns <list>  表示カラム。既定: datetime,channel,user,text
  -no-header           ヘッダ行を出さない
  -json                JSON で出力
  （共通オプション -workspace / -profile / -token は slack --help を参照）

例:
  slack history '#general'
  slack history -n 200 C0123456789
  slack history -oldest 1725000000.000000 '#dev'
`

const threadHelp = `slack thread - スレッドの返信を取得する（conversations.replies）

使い方:
  slack thread [オプション] <#チャンネル名|チャンネルID> <thread_ts>

  thread_ts は親メッセージの ts（slack search -c ts,channel,text で得られる）。

オプション:
  -n <数>              取得件数（既定 200）
  -c, -columns <list>  表示カラム。既定: datetime,channel,user,text
  -no-header           ヘッダ行を出さない
  -json                JSON で出力

例:
  slack thread '#general' 1725000000.123456
  slack thread -json C0123456789 1725000000.123456 | jq -r '.[].text'
`

func cmdHistory(args []string) error {
	var cfg config.Config
	var count int
	var oldest, latest, columnsSpec string
	var noHeader bool

	fs := newFlagSet("history", historyHelp)
	registerCommon(fs, &cfg)
	fs.IntVar(&count, "n", 50, "取得件数")
	fs.StringVar(&oldest, "oldest", "", "この ts より新しいメッセージ")
	fs.StringVar(&latest, "latest", "", "この ts より古いメッセージ")
	fs.StringVar(&columnsSpec, "columns", messageColumns.Defaults(), "表示カラム。指定可能: "+messageColumns.Available())
	fs.StringVar(&columnsSpec, "c", messageColumns.Defaults(), "-columns の別名")
	fs.BoolVar(&noHeader, "no-header", false, "ヘッダ行を出力しない")
	if done, err := parseArgs(fs, historyHelp, args); err != nil || done {
		return err
	}
	if err := checkNoTrailingFlags(fs, fs.Args()); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return &config.UsageError{Msg: "エラー: チャンネルを指定してください。\n使い方: slack history [オプション] <#チャンネル名|チャンネルID>\n詳細:   slack history --help"}
	}

	cols, err := parseCols(messageColumns, columnsSpec)
	if err != nil {
		return err
	}

	sess, err := openSession(cfg)
	if err != nil {
		return err
	}
	ctx := context.Background()
	channelID, err := sess.Client.ResolveChannel(ctx, fs.Arg(0))
	if err != nil {
		return err
	}
	msgs, err := sess.Client.History(ctx, channelID, count, oldest, latest)
	if err != nil {
		return err
	}

	if cfg.JSON {
		return printJSON(msgs)
	}
	if len(msgs) == 0 {
		fmt.Fprintln(os.Stderr, "0 件")
		return nil
	}
	fmt.Print(messageColumns.Render(msgs, cols, !noHeader))
	return nil
}

func cmdThread(args []string) error {
	var cfg config.Config
	var count int
	var columnsSpec string
	var noHeader bool

	fs := newFlagSet("thread", threadHelp)
	registerCommon(fs, &cfg)
	fs.IntVar(&count, "n", 200, "取得件数")
	fs.StringVar(&columnsSpec, "columns", messageColumns.Defaults(), "表示カラム。指定可能: "+messageColumns.Available())
	fs.StringVar(&columnsSpec, "c", messageColumns.Defaults(), "-columns の別名")
	fs.BoolVar(&noHeader, "no-header", false, "ヘッダ行を出力しない")
	if done, err := parseArgs(fs, threadHelp, args); err != nil || done {
		return err
	}
	if err := checkNoTrailingFlags(fs, fs.Args()); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return &config.UsageError{Msg: "エラー: チャンネルと thread_ts を指定してください。\n使い方: slack thread <#チャンネル名|チャンネルID> <thread_ts>\n詳細:   slack thread --help"}
	}

	cols, err := parseCols(messageColumns, columnsSpec)
	if err != nil {
		return err
	}

	sess, err := openSession(cfg)
	if err != nil {
		return err
	}
	ctx := context.Background()
	channelID, err := sess.Client.ResolveChannel(ctx, fs.Arg(0))
	if err != nil {
		return err
	}
	msgs, err := sess.Client.Replies(ctx, channelID, fs.Arg(1), count)
	if err != nil {
		return err
	}

	if cfg.JSON {
		return printJSON(msgs)
	}
	if len(msgs) == 0 {
		fmt.Fprintln(os.Stderr, "0 件")
		return nil
	}
	fmt.Print(messageColumns.Render(msgs, cols, !noHeader))
	return nil
}
