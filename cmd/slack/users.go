package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jiikko/slack-cli/internal/config"
	"github.com/jiikko/slack-cli/internal/slack"
)

const usersHelp = `slack users - ユーザー一覧を表示する（users.list）

使い方:
  slack users [オプション]

オプション:
  -name <substr>       name / real_name の部分一致でフィルタ
  -c, -columns <list>  表示カラム。既定: id,name,real_name,is_bot
  -no-header           ヘッダ行を出さない
  -n <数>              最大件数（0=全件）
  -bots                ボットも含める（既定は人間のみ）
  -deleted             無効化済みユーザーも含める
  -json                JSON で出力
  （共通オプション -workspace / -profile / -token は slack --help を参照）

指定可能なカラム:
  id / name / real_name / email / is_bot / deleted
  ※ email はワークスペースの設定・権限によっては空になる。

例:
  slack users -name tanaka
  slack users -c id,name,email -no-header | grep example.com
`

func cmdUsers(args []string) error {
	var cfg config.Config
	var nameFilter, columnsSpec string
	var noHeader, withBots, withDeleted bool
	var limit int

	fs := newFlagSet("users")
	registerCommon(fs, &cfg)
	fs.StringVar(&nameFilter, "name", "", "name / real_name の部分一致フィルタ")
	fs.StringVar(&columnsSpec, "columns", userColumns.Defaults(), "表示カラム。指定可能: "+userColumns.Available())
	fs.StringVar(&columnsSpec, "c", userColumns.Defaults(), "-columns の別名")
	fs.BoolVar(&noHeader, "no-header", false, "ヘッダ行を出力しない")
	fs.BoolVar(&withBots, "bots", false, "ボットも含める")
	fs.BoolVar(&withDeleted, "deleted", false, "無効化済みユーザーも含める")
	fs.IntVar(&limit, "n", 0, "最大件数（0=全件）")
	if done, err := parseArgs(fs, usersHelp, args); err != nil || done {
		return err
	}
	if err := checkNoTrailingFlags(fs, fs.Args()); err != nil {
		return err
	}

	cols, err := parseCols(userColumns, columnsSpec)
	if err != nil {
		return err
	}

	sess, err := openSession(cfg)
	if err != nil {
		return err
	}
	users, err := sess.Client.Users(context.Background(), 0)
	if err != nil {
		if !slack.IsTruncated(err) {
			return err
		}
		fmt.Fprintf(os.Stderr, "警告: %v\n", err)
	}

	var filtered []slack.User
	for _, u := range users {
		if !withBots && u.IsBot {
			continue
		}
		if !withDeleted && u.Deleted {
			continue
		}
		if nameFilter != "" && !containsFold(u.Name, nameFilter) && !containsFold(u.DisplayName(), nameFilter) {
			continue
		}
		filtered = append(filtered, u)
		if limit > 0 && len(filtered) >= limit {
			break
		}
	}

	if cfg.JSON {
		return printJSON(filtered)
	}
	if len(filtered) == 0 {
		fmt.Fprintln(os.Stderr, "0 件")
		return nil
	}
	fmt.Print(userColumns.Render(filtered, cols, !noHeader))
	return nil
}
