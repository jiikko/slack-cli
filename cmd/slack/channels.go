package main

import (
	"context"

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
  -refresh             キャッシュを使わずに取り直す
  -json                JSON で出力

全件の取得結果は 1 時間キャッシュする（-types・ユーザーごと。使ったときは stderr に 1 行出る）。

指定可能なカラム:
  id / name / is_private / is_archived / num_members / topic / purpose

例:
  slack channels
  slack channels -name dev
  slack channels -types public_channel -c id,name
  slack channels -json | jq -r '.[] | select(.is_private) | .name'
` + commonOptionsHelp + commonTailHelp

func cmdChannels(args []string) error {
	var cfg config.Config
	var types, nameFilter, columnsSpec string
	var noHeader, refresh bool
	var limit int

	fs := newFlagSet("channels")
	registerCommon(fs, &cfg)
	fs.StringVar(&types, "types", "public_channel,private_channel", "取得する種別（カンマ区切り）")
	fs.StringVar(&nameFilter, "name", "", "名前の部分一致フィルタ")
	fs.StringVar(&columnsSpec, "columns", channelColumns.Defaults(), "表示カラム。指定可能: "+channelColumns.Available())
	fs.StringVar(&columnsSpec, "c", channelColumns.Defaults(), "-columns の別名")
	fs.BoolVar(&noHeader, "no-header", false, "ヘッダ行を出力しない")
	fs.IntVar(&limit, "n", 0, "最大件数（0=全件）")
	fs.BoolVar(&refresh, "refresh", false, "キャッシュを使わずに取り直す")
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
	useChannelCache(sess, refresh)
	// 名前フィルタがあるときは全件取ってから絞る（API 側に部分一致の絞り込みが無い）。
	fetch := limit
	if nameFilter != "" {
		fetch = 0
	}
	channels, err := sess.Client.Channels(context.Background(), types, fetch)
	after, err := splitListErr(err)
	if err != nil {
		return err
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

	return finishList(cfg.JSON, channels, func() string { return channelColumns.Render(channels, cols, !noHeader) }, after)
}
