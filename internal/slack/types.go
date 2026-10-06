package slack

import (
	"encoding/json"
	"fmt"
)

// envelope は Slack API の共通レスポンス枠。
type envelope struct {
	OK      bool   `json:"ok"`
	Error   string `json:"error"`
	Warning string `json:"warning"`
	// HasMore は history / replies が返す「続きがある」。next_cursor と食い違うことがあるので両方見る。
	HasMore          bool `json:"has_more"`
	ResponseMetadata struct {
		NextCursor string   `json:"next_cursor"`
		Messages   []string `json:"messages"`
	} `json:"response_metadata"`
}

// AuthTest は auth.test のレスポンス。
type AuthTest struct {
	URL    string `json:"url"`     // https://<workspace>.slack.com/
	Team   string `json:"team"`    // 表示名
	TeamID string `json:"team_id"` // T...
	User   string `json:"user"`
	UserID string `json:"user_id"`
	BotID  string `json:"bot_id"`
}

// Message は search.messages / conversations.history / conversations.replies の 1 件。
//
// channel は search.messages にはオブジェクトであり、history/replies には基本的に無い（呼び出し側で補う）。
// history でも一部のメッセージは文字列で持つ（MessageChannel を参照）。
type Message struct {
	Type     string         `json:"type"`
	User     string         `json:"user"`
	Username string         `json:"username"`
	BotID    string         `json:"bot_id"`
	Ts       string         `json:"ts"`
	ThreadTs string         `json:"thread_ts"`
	Text     string         `json:"text"`
	Perma    string         `json:"permalink"`
	Channel  MessageChannel `json:"channel"`
	// ChannelID は history/replies のように channel が無いレスポンスで、
	// 呼び出し側が引数のチャンネルを埋めるための欄。
	//
	// 🚨 json:"-" にしないこと。TSV の channel 列はここへフォールバックするのに、
	// -json 出力だけチャンネルが落ちる（jq に流す用途で効く）。
	ChannelID string `json:"channel_id,omitempty"`
}

// MessageChannel は Message の channel 欄。
//
// search.messages は {"id": ..., "name": ...} のオブジェクトで返すが、conversations.history の一部の
// メッセージは "C..." の文字列 (チャンネル ID) で返す (2026-10-06 実測。オブジェクト前提で受けると、
// その 1 件のせいでページ全体の解析が失敗して history が rc=1 で終わる)。どちらの形も受ける。
// 出力 (-json) は常にオブジェクトの形にする。
type MessageChannel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// UnmarshalJSON は、オブジェクト・文字列・null のどれでも受ける。それ以外の形は誤りとして返す。
func (c *MessageChannel) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		return nil
	}
	var id string
	if err := json.Unmarshal(b, &id); err == nil {
		*c = MessageChannel{ID: id}
		return nil
	}
	type plain MessageChannel // UnmarshalJSON を持たない型にして、自分を再帰で呼ばないようにする
	var v plain
	if err := json.Unmarshal(b, &v); err != nil {
		return fmt.Errorf("channel がオブジェクト・文字列・null のどれでもない (%s): %w", b, err)
	}
	*c = MessageChannel(v)
	return nil
}

// SearchResult は search.messages の結果。
type SearchResult struct {
	Total    int
	Page     int
	Pages    int
	Matches  []Message
	RawPaged json.RawMessage `json:"-"`
}

// Channel は conversations.list / conversations.info の 1 件。
type Channel struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	IsPrivate  bool   `json:"is_private"`
	IsArchived bool   `json:"is_archived"`
	IsMember   bool   `json:"is_member"`
	NumMembers int    `json:"num_members"`
	Topic      struct {
		Value string `json:"value"`
	} `json:"topic"`
	Purpose struct {
		Value string `json:"value"`
	} `json:"purpose"`
}

// User は users.list / users.info の 1 件。
type User struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	RealName string `json:"real_name"`
	Deleted  bool   `json:"deleted"`
	IsBot    bool   `json:"is_bot"`
	Profile  struct {
		Email       string `json:"email"`
		RealName    string `json:"real_name"`
		DisplayName string `json:"display_name"`
	} `json:"profile"`
}

// Email は取得できたメールアドレスを返す（権限が無ければ空）。
func (u User) Email() string { return u.Profile.Email }

// DisplayName は表示に使う名前を返す。
func (u User) DisplayName() string {
	if u.RealName != "" {
		return u.RealName
	}
	if u.Profile.RealName != "" {
		return u.Profile.RealName
	}
	return u.Profile.DisplayName
}
