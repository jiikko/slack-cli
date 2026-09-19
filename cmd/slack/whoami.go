package main

import (
	"fmt"

	"github.com/jiikko/slack-cli/internal/config"
)

const whoamiHelp = `slack whoami - 接続中のユーザーとワークスペースを表示する（auth.test）

設定と疎通の確認に使う。設定した workspace と一致しない場合はエラーになる。

使い方:
  slack whoami [-json]
  （共通オプション -workspace / -profile / -token は slack --help を参照）
`

func cmdWhoami(args []string) error {
	var cfg config.Config
	fs := newFlagSet("whoami", whoamiHelp)
	registerCommon(fs, &cfg)
	if done, err := parseArgs(fs, whoamiHelp, args); err != nil || done {
		return err
	}

	sess, err := openSession(cfg)
	if err != nil {
		return err
	}
	at := sess.Auth

	if cfg.JSON {
		return printJSON(map[string]string{
			"workspace": sess.Client.Workspace(),
			"url":       at.URL,
			"team":      at.Team,
			"team_id":   at.TeamID,
			"user":      at.User,
			"user_id":   at.UserID,
			"profile":   sess.Profile,
		})
	}
	fmt.Printf("%-12s %s\n", "workspace:", sess.Client.Workspace())
	fmt.Printf("%-12s %s\n", "team:", at.Team)
	fmt.Printf("%-12s %s\n", "team_id:", at.TeamID)
	fmt.Printf("%-12s %s\n", "user:", at.User)
	fmt.Printf("%-12s %s\n", "user_id:", at.UserID)
	fmt.Printf("%-12s %s\n", "url:", at.URL)
	fmt.Printf("%-12s %s\n", "profile:", sess.Profile)
	return nil
}
