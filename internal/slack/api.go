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
	// Hint は「実際に効く回避策」の案内（メソッドごとに違う）。
	//
	// 🚨 効かない回避策を勧めないこと。たとえば channels / users の -name は
	// **全件を取得してから**絞り込むので、同じ上限に当たる（勧めても同じ警告が出るだけ）。
	Hint string
	// Stalled は「has_more=true なのに next_cursor が空」で続きを取れなかったことを表す
	// （安全上限ではなく、API の応答のせいで止まった）。
	Stalled bool
}

func (e *TruncatedError) Error() string {
	msg := fmt.Sprintf(
		"%s の取得が %d ページ（%d 件）で打ち切られました。まだ続きがあります。\n"+
			"  結果は不完全です。",
		e.Method, e.Pages, e.Count)
	if e.Stalled {
		msg = fmt.Sprintf(
			"%s の取得が %d 件で止まりました。API は続きがある（has_more）と返しましたが、次のページのカーソルがありませんでした。\n"+
				"  結果は不完全です。",
			e.Method, e.Count)
	}
	if e.Hint != "" {
		msg += "\n  " + e.Hint
	}
	return msg
}

// 打ち切り時の案内（TruncatedError.Hint）。
const (
	truncHintChannels = "-name は全件を取得してから絞り込むため、この上限は避けられません。\n" +
		"  -types public_channel / -types private_channel のように種別を分けて取得すると上限に収まることがあります。\n" +
		"  特定のチャンネルを使うだけなら、チャンネル ID（C…）を直接指定してください。"
	truncHintUsers   = "users.list には API 側の絞り込みが無く、-name も全件を取得してから絞り込むため、この上限は避けられません。"
	truncHintHistory = "-oldest / -latest で期間を絞るか、-n を減らしてください。"
	truncHintReplies = "-n を減らしてください（スレッドは古い順に取得します）。"
)

// IsTruncated は err が打ち切り（結果は使えるが不完全）かを返す。
func IsTruncated(err error) bool {
	var te *TruncatedError
	return errors.As(err, &te)
}

// PartialError はページングの途中（2 ページ目以降）で API 呼び出しが失敗したことを表す。
// 呼び出し側には取得済みの分が返る。
//
// 🚨 TruncatedError と区別すること。打ち切り（安全上限）は「完了扱いで警告」だが、
// 途中失敗（429 / 5xx / 通信断）は**完了ではない**ので、部分結果を出したうえで非 0 で終わる。
// また途中失敗で `return nil, err` にしないこと。取得済みページを捨てると、ページングで
// 呼び出し回数が増えた分だけ、1 回で取っていた頃より悪くなる。
type PartialError struct {
	Method string
	Count  int // 取得済みの件数
	Err    error
}

func (e *PartialError) Error() string {
	return fmt.Sprintf("%s の取得が途中で失敗しました（取得済み %d 件。結果は不完全です）: %v", e.Method, e.Count, e.Err)
}

func (e *PartialError) Unwrap() error { return e.Err }

// IsPartial は err が途中失敗（結果は一部だけ）かを返す。
func IsPartial(err error) bool {
	var pe *PartialError
	return errors.As(err, &pe)
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
	if t := canonicalTypes(types); t != "" {
		types = t // キャッシュのキーと同じ正規化を通す（channelcache.go の canonicalTypes）
	} else if types == "" {
		types = defaultChannelTypes
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

// defaultChannelTypes は conversations.list の types の既定値。
const defaultChannelTypes = "public_channel,private_channel"

// Channels は conversations.list を（必要なだけページングして）取得する。
// limit<=0 なら全件。チャンネル一覧のキャッシュ（UseChannelCache）が新しければ、API を呼ばずにそれを返す。
func (c *Client) Channels(ctx context.Context, types string, limit int) ([]Channel, error) {
	if chs, age, ok := c.channelCache.load(types); ok {
		c.channelCache.noteUsed(age)
		if limit > 0 && len(chs) > limit {
			chs = chs[:limit]
		}
		return chs, nil
	}
	var out []Channel
	truncated, err := c.forEachChannelPage(ctx, types, func(page []Channel) bool {
		out = append(out, page...)
		return !(limit > 0 && len(out) >= limit) // 必要数に達したら打ち切る
	})
	if err != nil {
		if len(out) > 0 {
			return out, &PartialError{Method: MethodConversationsList.String(), Count: len(out), Err: err}
		}
		return nil, err
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	if truncated {
		// データは返すが、不完全だと伝える。
		return out, &TruncatedError{Method: MethodConversationsList.String(), Pages: maxPages, Count: len(out), Hint: truncHintChannels}
	}
	// 書くのは全件を最後まで取れたときだけ（途中で失敗・打ち切り・-n で止めた取得を「全件」として使い回さない）。
	if limit <= 0 {
		c.channelCache.save(types, out)
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
	if id := c.cachedChannelID(ctx, name); id != "" {
		return id, nil
	}

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

// cachedChannelID はキャッシュから name のチャンネル ID を引き、conversations.info で今もその名前かを確かめて返す。
// 無い・確かめられない・名前が変わっていたら空を返す（呼び出し側は今どおり API で探す）。
//
// 🚨 確かめずに使わない。改名や名前の再利用があると、キャッシュの古い対応で別のチャンネルを読む。
func (c *Client) cachedChannelID(ctx context.Context, name string) string {
	chs, _, ok := c.channelCache.load("")
	if !ok {
		return ""
	}
	for _, ch := range chs {
		if ch.Name != name {
			continue
		}
		info, err := c.ChannelInfo(ctx, ch.ID)
		if err != nil || info.Name != name {
			return ""
		}
		return ch.ID
	}
	return ""
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

// messagePageLimit は conversations.history / replies の 1 回あたりの limit の上限。
//
// 1000 は API の上限（実 API で確認: 1000 は受け付け、1000 を超えると黙って既定値に丸められる）。
// -n 1000 以下なら 1 回で済む（ページングを入れる前と同じ呼び出し回数）。
const messagePageLimit = 1000

// collectMessages は conversations.history / replies を cursor でページングし、limit 件まで集める。
//
// 🚨 1 回だけ呼んで終わらせないこと。1 回の応答は limit 以下しか返さず、残りは
// next_cursor の先にある。見ないと -n を満たさないまま rc=0 で終わり、
// スレッドなら**新しい側の返信**が警告なしに欠ける（古い順に返るため）。
//
// 安全上限（maxPages）に達してもまだ続きがあるときは、取得できた分と TruncatedError を返す。
// limit に達して止めるのは利用者が件数を指定した結果なので、打ち切りではない。
// 2 ページ目以降の失敗は、取得できた分と PartialError を返す。
//
// ts で重複を除く（件数 -n は重複除去後で数える）。conversations.replies は各ページの先頭に
// 親メッセージを再度含めるという報告がある（未実測）。含めなくても害は無い。
func (c *Client) collectMessages(ctx context.Context, m Method, base url.Values, limit int, channelID, hint string) ([]Message, error) {
	var out []Message
	seen := map[string]bool{}
	cursor := ""
	for page := 0; page < maxPages; page++ {
		params := url.Values{}
		for k, vs := range base {
			params[k] = vs
		}
		want := limit - len(out)
		if page > 0 {
			// 🚨 2 ページ目以降は 1 件多く頼む。親の再掲（上記）で 1 件ぶん消費されると、
			// 残り 1 件のときに「再掲された親だけ」のページが続いて maxPages まで空回りする。
			// 多く取った分は下の out[:limit] で落ちる。
			want++
		}
		params.Set("limit", strconv.Itoa(min(want, messagePageLimit)))
		if cursor != "" {
			params.Set("cursor", cursor)
		}
		var resp struct {
			Messages []Message `json:"messages"`
		}
		raw, err := c.call(ctx, m, params, &resp)
		if err != nil {
			if len(out) > 0 {
				return out, &PartialError{Method: m.String(), Count: len(out), Err: err}
			}
			return nil, err
		}
		for _, msg := range resp.Messages {
			if msg.Ts != "" {
				if seen[msg.Ts] {
					continue
				}
				seen[msg.Ts] = true
			}
			msg.ChannelID = channelID
			out = append(out, msg)
		}
		if len(out) >= limit {
			return out[:limit], nil
		}
		var hasMore bool
		cursor, hasMore = pageState(raw)
		if cursor == "" {
			if hasMore {
				// 🚨 「続きがある」と言われたのに続きを取れない。完了扱いにしない。
				return out, &TruncatedError{Method: m.String(), Pages: page + 1, Count: len(out), Hint: hint, Stalled: true}
			}
			return out, nil
		}
	}
	return out, &TruncatedError{Method: m.String(), Pages: maxPages, Count: len(out), Hint: hint}
}

// History は conversations.history を（-n 件に達するまでページングして）取得する。新しい順。
func (c *Client) History(ctx context.Context, channelID string, limit int, oldest, latest string) ([]Message, error) {
	if limit <= 0 {
		limit = 50
	}
	params := url.Values{"channel": {channelID}}
	if oldest != "" {
		params.Set("oldest", oldest)
	}
	if latest != "" {
		params.Set("latest", latest)
	}
	return c.collectMessages(ctx, MethodConversationsHistory, params, limit, channelID, truncHintHistory)
}

// Replies は conversations.replies を（-n 件に達するまでページングして）取得する（スレッド取得）。
// 古い順（先頭は親メッセージ）。
func (c *Client) Replies(ctx context.Context, channelID, threadTs string, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = 200
	}
	params := url.Values{
		"channel": {channelID},
		"ts":      {threadTs},
	}
	return c.collectMessages(ctx, MethodConversationsReplies, params, limit, channelID, truncHintReplies)
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
			if len(out) > 0 {
				return out, &PartialError{Method: MethodUsersList.String(), Count: len(out), Err: err}
			}
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
	return out, &TruncatedError{Method: MethodUsersList.String(), Pages: maxPages, Count: len(out), Hint: truncHintUsers}
}
