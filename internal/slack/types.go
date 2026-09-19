package slack

import "encoding/json"

// envelope は Slack API の共通レスポンス枠。
type envelope struct {
	OK               bool   `json:"ok"`
	Error            string `json:"error"`
	Warning          string `json:"warning"`
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
// channel は search.messages にはあり、history/replies には無い（呼び出し側で補う）。
type Message struct {
	Type     string `json:"type"`
	User     string `json:"user"`
	Username string `json:"username"`
	BotID    string `json:"bot_id"`
	Ts       string `json:"ts"`
	ThreadTs string `json:"thread_ts"`
	Text     string `json:"text"`
	Perma    string `json:"permalink"`
	Channel  struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"channel"`
	// ChannelID は history/replies のように channel が無いレスポンスで、
	// 呼び出し側が引数のチャンネルを埋めるための欄。
	//
	// 🚨 json:"-" にしないこと。TSV の channel 列はここへフォールバックするのに、
	// -json 出力だけチャンネルが落ちる（jq に流す用途で効く）。
	ChannelID string `json:"channel_id,omitempty"`
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
