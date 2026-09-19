package slack

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// AuthTest は auth.test を呼ぶ。接続先ワークスペースの確認に使う。
func (c *Client) AuthTest(ctx context.Context) (*AuthTest, error) {
	var resp AuthTest
	if _, err := c.call(ctx, MethodAuthTest, url.Values{}, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// TeamDomain は auth.test の url（https://<domain>.slack.com/）からサブドメインを取り出す。
//
// 🚨 ワークスペース一致判定の要。url が空・想定外の形のときは空文字を返し、
// 呼び出し側で「一致しなかった」として扱う（判定不能を「一致」に丸めない）。
func (a *AuthTest) TeamDomain() string {
	u, err := url.Parse(strings.TrimSpace(a.URL))
	if err != nil || u.Host == "" {
		return ""
	}
	host := strings.ToLower(u.Host)
	if !strings.HasSuffix(host, ".slack.com") {
		return ""
	}
	return strings.TrimSuffix(host, ".slack.com")
}

// Search は search.messages を呼ぶ。
func (c *Client) Search(ctx context.Context, query string, count, page int) (*SearchResult, error) {
	if count <= 0 {
		count = 20
	}
	if page <= 0 {
		page = 1
	}
	params := url.Values{
		"query":     {query},
		"count":     {strconv.Itoa(count)},
		"page":      {strconv.Itoa(page)},
		"highlight": {"false"},
		"sort":      {"timestamp"},
		"sort_dir":  {"desc"},
	}
	var resp struct {
		Messages struct {
			Total   int       `json:"total"`
			Matches []Message `json:"matches"`
			Paging  struct {
				Count int `json:"count"`
				Total int `json:"total"`
				Page  int `json:"page"`
				Pages int `json:"pages"`
			} `json:"paging"`
		} `json:"messages"`
	}
	if _, err := c.call(ctx, MethodSearchMessages, params, &resp); err != nil {
		return nil, err
	}
	return &SearchResult{
		Total:   resp.Messages.Total,
		Page:    resp.Messages.Paging.Page,
		Pages:   resp.Messages.Paging.Pages,
		Matches: resp.Messages.Matches,
	}, nil
}

// maxPages は cursor ページングの安全上限（無限ループ防止）。
const maxPages = 50

// TruncatedError は cursor ページングが安全上限に達して打ち切られたことを表す。
//
// 🚨 打ち切りを無音で握り潰さないこと。握り潰すと、実在するチャンネルに対して
// ResolveChannel が「見つかりませんでした」を返す（存在しないのではなく、
// 列挙が足りなかっただけ）。呼び出し側が「不完全な一覧」と知れる形で返す。
type TruncatedError struct {
	Method string
	Pages  int
	Count  int
}

func (e *TruncatedError) Error() string {
	return fmt.Sprintf(
		"%s の一覧が %d ページ（%d 件）で打ち切られました。まだ続きがあります。\n"+
			"  結果は不完全です。-name での絞り込みか、ID の直接指定を使ってください。",
		e.Method, e.Pages, e.Count)
}

// IsTruncated は err が打ち切り（結果は使えるが不完全）かを返す。
func IsTruncated(err error) bool {
	var te *TruncatedError
	return errors.As(err, &te)
}

// forEachChannelPage は conversations.list を 1 ページずつ取り出して fn に渡す。
// fn が false を返したらそこで打ち切る（「見つかった時点でやめる」用）。
//
// 戻り値 truncated は「安全上限に達したのに、まだ続きがある」ことを表す。
//
// 🚨 全ページを集めてから探す形にしないこと。大きなワークスペースでは
// `slack history '#name'` のたびに最大 maxPages 回のリクエストを投げ、
// すぐレート制限（429）に当たる（実測で踏んだ）。
func (c *Client) forEachChannelPage(ctx context.Context, types string, fn func([]Channel) bool) (truncated bool, err error) {
	if types == "" {
		types = "public_channel,private_channel"
	}
	cursor := ""
	for page := 0; page < maxPages; page++ {
		params := url.Values{
			"types":            {types},
			"limit":            {"200"},
			"exclude_archived": {"false"},
		}
		if cursor != "" {
			params.Set("cursor", cursor)
		}
		var resp struct {
			Channels []Channel `json:"channels"`
		}
		raw, err := c.call(ctx, MethodConversationsList, params, &resp)
		if err != nil {
			return false, err
		}
		if !fn(resp.Channels) {
			return false, nil // 呼び出し側が「もう十分」と言った = 打ち切りではない
		}
		cursor = nextCursor(raw)
		if cursor == "" {
			return false, nil
		}
	}
	return true, nil
}

// Channels は conversations.list を（必要なだけページングして）取得する。
// limit<=0 なら全件。
func (c *Client) Channels(ctx context.Context, types string, limit int) ([]Channel, error) {
	var out []Channel
	truncated, err := c.forEachChannelPage(ctx, types, func(page []Channel) bool {
		out = append(out, page...)
		return !(limit > 0 && len(out) >= limit) // 必要数に達したら打ち切る
	})
	if err != nil {
		return nil, err
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	if truncated {
		// データは返すが、不完全だと伝える。
		return out, &TruncatedError{Method: MethodConversationsList.String(), Pages: maxPages, Count: len(out)}
	}
	return out, nil
}

// ChannelInfo は conversations.info を呼ぶ。
func (c *Client) ChannelInfo(ctx context.Context, channelID string) (*Channel, error) {
	var resp struct {
		Channel Channel `json:"channel"`
	}
	if _, err := c.call(ctx, MethodConversationsInfo, url.Values{"channel": {channelID}}, &resp); err != nil {
		return nil, err
	}
	return &resp.Channel, nil
}

// ResolveChannel は "#name" / "name" / "C0123..." をチャンネル ID へ解決する。
//
// ID 形式（C/G/D で始まる英数字）はそのまま返し、それ以外は conversations.list を
// **1 ページずつ**見て、見つかった時点で打ち切る。
func (c *Client) ResolveChannel(ctx context.Context, spec string) (string, error) {
	s := strings.TrimSpace(spec)
	if s == "" {
		return "", fmt.Errorf("チャンネルが指定されていません")
	}
	if looksLikeChannelID(s) {
		return s, nil
	}
	name := strings.TrimPrefix(s, "#")

	found := ""
	scanned := 0
	truncated, err := c.forEachChannelPage(ctx, "", func(page []Channel) bool {
		scanned += len(page)
		for _, ch := range page {
			if ch.Name == name {
				found = ch.ID
				return false // 見つかったので以降のページは取らない
			}
		}
		return true
	})
	if err != nil {
		return "", err
	}
	if found != "" {
		return found, nil
	}
	if truncated {
		// 🚨 「見つからなかった」と「探しきれなかった」を混同しない。
		return "", fmt.Errorf(
			"チャンネル %q は、取得できた %d 件の中に見つかりませんでした。\n"+
				"  ただしチャンネル一覧が多すぎて打ち切られているため、存在しないとは限りません。\n"+
				"  チャンネル ID（C… の形）を直接指定してください。", spec, scanned)
	}
	return "", fmt.Errorf("チャンネル %q が見つかりませんでした（slack channels -name %s で確認してください）", spec, name)
}

// looksLikeChannelID は Slack のチャンネル ID の形かを判定する。
func looksLikeChannelID(s string) bool {
	if len(s) < 9 {
		return false
	}
	switch s[0] {
	case 'C', 'G', 'D':
	default:
		return false
	}
	for i := 1; i < len(s); i++ {
		ch := s[i]
		if !(ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9') {
			return false
		}
	}
	return true
}

// History は conversations.history を呼ぶ。
func (c *Client) History(ctx context.Context, channelID string, limit int, oldest, latest string) ([]Message, error) {
	if limit <= 0 {
		limit = 50
	}
	params := url.Values{
		"channel": {channelID},
		"limit":   {strconv.Itoa(limit)},
	}
	if oldest != "" {
		params.Set("oldest", oldest)
	}
	if latest != "" {
		params.Set("latest", latest)
	}
	var resp struct {
		Messages []Message `json:"messages"`
	}
	if _, err := c.call(ctx, MethodConversationsHistory, params, &resp); err != nil {
		return nil, err
	}
	for i := range resp.Messages {
		resp.Messages[i].ChannelID = channelID
	}
	return resp.Messages, nil
}

// Replies は conversations.replies を呼ぶ（スレッド取得）。
func (c *Client) Replies(ctx context.Context, channelID, threadTs string, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = 200
	}
	params := url.Values{
		"channel": {channelID},
		"ts":      {threadTs},
		"limit":   {strconv.Itoa(limit)},
	}
	var resp struct {
		Messages []Message `json:"messages"`
	}
	if _, err := c.call(ctx, MethodConversationsReplies, params, &resp); err != nil {
		return nil, err
	}
	for i := range resp.Messages {
		resp.Messages[i].ChannelID = channelID
	}
	return resp.Messages, nil
}

// Users は users.list を（必要なだけページングして）取得する。limit<=0 なら全件。
func (c *Client) Users(ctx context.Context, limit int) ([]User, error) {
	var out []User
	cursor := ""
	for page := 0; page < maxPages; page++ {
		params := url.Values{"limit": {"200"}}
		if cursor != "" {
			params.Set("cursor", cursor)
		}
		var resp struct {
			Members []User `json:"members"`
		}
		raw, err := c.call(ctx, MethodUsersList, params, &resp)
		if err != nil {
			return nil, err
		}
		out = append(out, resp.Members...)
		if limit > 0 && len(out) >= limit {
			return out[:limit], nil
		}
		cursor = nextCursor(raw)
		if cursor == "" {
			return out, nil
		}
	}
	return out, &TruncatedError{Method: MethodUsersList.String(), Pages: maxPages, Count: len(out)}
}
