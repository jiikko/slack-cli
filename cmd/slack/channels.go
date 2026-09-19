package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jiikko/slack-cli/internal/config"
	"github.com/jiikko/slack-cli/internal/slack"
)

const channelsHelp = `slack channels - チャンネル一覧を表示する（conversations.list）

使い方:
  slack channels [オプション]

オプション:
  -types <list>        取得する種別（既定 public_channel,private_channel）
  -name <substr>       名前の部分一致でフィルタ
  -c, -columns <list>  表示カラム。既定: id,name,is_private,num_members,topic
  -no-header           ヘッダ行を出さない
  -n <数>              最大件数（既定 0=全件）
  -json                JSON で出力
  （共通オプション -workspace / -profile / -token は slack --help を参照）

指定可能なカラム:
  id / name / is_private / is_archived / num_members / topic / purpose

例:
  slack channels
  slack channels -name dev
  slack channels -types public_channel -c id,name
  slack channels -json | jq -r '.[] | select(.is_private) | .name'
`

func cmdChannels(args []string) error {
	var cfg config.Config
	var types, nameFilter, columnsSpec string
	var noHeader bool
	var limit int

	fs := newFlagSet("channels", channelsHelp)
	registerCommon(fs, &cfg)
	fs.StringVar(&types, "types", "public_channel,private_channel", "取得する種別（カンマ区切り）")
	fs.StringVar(&nameFilter, "name", "", "名前の部分一致フィルタ")
	fs.StringVar(&columnsSpec, "columns", channelColumns.Defaults(), "表示カラム。指定可能: "+channelColumns.Available())
	fs.StringVar(&columnsSpec, "c", channelColumns.Defaults(), "-columns の別名")
	fs.BoolVar(&noHeader, "no-header", false, "ヘッダ行を出力しない")
	fs.IntVar(&limit, "n", 0, "最大件数（0=全件）")
	if done, err := parseArgs(fs, channelsHelp, args); err != nil || done {
		return err
	}
	if err := checkNoTrailingFlags(fs, fs.Args()); err != nil {
		return err
	}

	cols, err := parseCols(channelColumns, columnsSpec)
	if err != nil {
		return err
	}

	sess, err := openSession(cfg)
	if err != nil {
		return err
	}
	// 名前フィルタがあるときは全件取ってから絞る（API 側に部分一致の絞り込みが無い）。
	fetch := limit
	if nameFilter != "" {
		fetch = 0
	}
	channels, err := sess.Client.Channels(context.Background(), types, fetch)
	if err != nil {
		// 🚨 打ち切りは「結果はあるが不完全」。黙って完全な一覧に見せない。
		if !slack.IsTruncated(err) {
			return err
		}
		fmt.Fprintf(os.Stderr, "警告: %v\n", err)
	}
	if nameFilter != "" {
		var filtered []slack.Channel
		for _, c := range channels {
			if containsFold(c.Name, nameFilter) {
				filtered = append(filtered, c)
			}
		}
		channels = filtered
		if limit > 0 && len(channels) > limit {
			channels = channels[:limit]
		}
	}

	if cfg.JSON {
		return printJSON(channels)
	}
	if len(channels) == 0 {
		fmt.Fprintln(os.Stderr, "0 件")
		return nil
	}
	fmt.Print(channelColumns.Render(channels, cols, !noHeader))
	return nil
}
