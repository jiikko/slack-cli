package main

import (
	"strings"

	"github.com/jiikko/slack-cli/internal/config"
	"github.com/jiikko/slack-cli/internal/output"
	"github.com/jiikko/slack-cli/internal/slack"
)

// messageColumns は search / history / thread が共有するカラム定義。
var messageColumns = output.NewRegistry[slack.Message](
	"datetime,channel,user,text",
	[]string{"ts", "datetime", "channel", "user", "text", "permalink"},
	map[string]output.Column[slack.Message]{
		"ts":       {Header: "ts", Value: func(m slack.Message) string { return m.Ts }},
		"datetime": {Header: "日時", Value: func(m slack.Message) string { return output.FormatTs(m.Ts) }},
		"channel": {Header: "チャンネル", Value: func(m slack.Message) string {
			if m.Channel.Name != "" {
				return "#" + m.Channel.Name
			}
			if m.Channel.ID != "" {
				return m.Channel.ID
			}
			return m.ChannelID
		}},
		"user": {Header: "ユーザー", Value: func(m slack.Message) string {
			if m.Username != "" {
				return m.Username
			}
			if m.User != "" {
				return m.User
			}
			return m.BotID
		}},
		"text":      {Header: "本文", Value: func(m slack.Message) string { return m.Text }},
		"permalink": {Header: "permalink", Value: func(m slack.Message) string { return m.Perma }},
	},
)

// channelColumns は channels のカラム定義。
var channelColumns = output.NewRegistry[slack.Channel](
	"id,name,is_private,num_members,topic",
	[]string{"id", "name", "is_private", "is_archived", "num_members", "topic", "purpose"},
	map[string]output.Column[slack.Channel]{
		"id":          {Header: "id", Value: func(c slack.Channel) string { return c.ID }},
		"name":        {Header: "name", Value: func(c slack.Channel) string { return "#" + c.Name }},
		"is_private":  {Header: "private", Value: func(c slack.Channel) string { return output.Bool(c.IsPrivate) }},
		"is_archived": {Header: "archived", Value: func(c slack.Channel) string { return output.Bool(c.IsArchived) }},
		"num_members": {Header: "members", Value: func(c slack.Channel) string { return output.Int(c.NumMembers) }},
		"topic":       {Header: "topic", Value: func(c slack.Channel) string { return c.Topic.Value }},
		"purpose":     {Header: "purpose", Value: func(c slack.Channel) string { return c.Purpose.Value }},
	},
)

// userColumns は users のカラム定義。
var userColumns = output.NewRegistry[slack.User](
	"id,name,real_name,is_bot",
	[]string{"id", "name", "real_name", "email", "is_bot", "deleted"},
	map[string]output.Column[slack.User]{
		"id":        {Header: "id", Value: func(u slack.User) string { return u.ID }},
		"name":      {Header: "name", Value: func(u slack.User) string { return u.Name }},
		"real_name": {Header: "real_name", Value: func(u slack.User) string { return u.DisplayName() }},
		"email":     {Header: "email", Value: func(u slack.User) string { return u.Email() }},
		"is_bot":    {Header: "bot", Value: func(u slack.User) string { return output.Bool(u.IsBot) }},
		"deleted":   {Header: "deleted", Value: func(u slack.User) string { return output.Bool(u.Deleted) }},
	},
)

// containsFold は部分一致フィルタ（大文字小文字を無視）。
func containsFold(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}

// parseCols はカラム指定を検証し、誤りを「使い方エラー」(rc=2) として返す。
//
// 🚨 カラム名の間違いは引数の指定ミスなので rc=2。ここを通さず生の error を返すと
// rc=1（実行時エラー）になり、スクリプトから「Slack 側の問題」と区別できなくなる。
func parseCols[T any](r *output.Registry[T], spec string) ([]string, error) {
	cols, err := r.Parse(spec)
	if err != nil {
		return nil, &config.UsageError{Msg: "エラー: " + err.Error()}
	}
	return cols, nil
}
