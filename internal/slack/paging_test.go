package slack

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// threadFake は cursor ページングする conversations.history / replies を模す。
//
// 受け側（Slack）の挙動に合わせる: 1 回に返すのは limit 件まで、続きがあれば
// has_more=true と next_cursor を返す。limit が API の上限（1000）を超えたら拒否する
// （実 API は 1000 超を黙って既定値に丸めるが、テストでは取り違えを見えるように拒否する）。
// ts は 1..total。newestFirst なら新しい順（history）、そうでなければ古い順（replies）。
type threadFake struct {
	total       int
	newestFirst bool
	// repeatParent は 2 ページ目以降の先頭に親メッセージ（ts=1）を再掲する（replies の報告された挙動）。
	repeatParent bool
	// pageCap > 0 なら、limit が大きくても 1 回に最大 pageCap 件しか返さない（短いページ）。
	pageCap int
	// dropCursorAfter > 0 なら、その回数目の応答で has_more=true のまま next_cursor を空にする。
	dropCursorAfter int
	// failAt > 0 なら、その回数目の呼び出しを 429 で失敗させる。
	failAt int

	mu    sync.Mutex
	calls int
}

func (f *threadFake) handle(method string, form url.Values) (int, string) {
	f.mu.Lock()
	f.calls++
	call := f.calls
	f.mu.Unlock()
	if f.failAt > 0 && call == f.failAt {
		return 429, ""
	}
	limit, err := strconv.Atoi(form.Get("limit"))
	if err != nil || limit <= 0 || limit > 1000 {
		return 200, `{"ok":false,"error":"invalid_limit"}`
	}
	n := limit
	if f.pageCap > 0 && n > f.pageCap {
		n = f.pageCap
	}
	offset := 0
	if c := form.Get("cursor"); c != "" {
		offset, _ = strconv.Atoi(strings.TrimPrefix(c, "off"))
	}
	msg := func(ts int) string { return fmt.Sprintf(`{"ts":"%d.000000","text":"m%d"}`, ts, ts) }
	var msgs []string
	if f.repeatParent && offset > 0 {
		msgs = append(msgs, msg(1))
		n-- // 再掲した親も limit に数えられる
	}
	for i := offset; i < offset+n && i < f.total; i++ {
		ts := i + 1
		if f.newestFirst {
			ts = f.total - i
		}
		msgs = append(msgs, msg(ts))
	}
	next := offset + len(msgs)
	if f.repeatParent && offset > 0 {
		next-- // 再掲分は位置を進めない
	}
	meta := `"has_more":false`
	if next < f.total {
		cursor := fmt.Sprintf("off%d", next)
		if f.dropCursorAfter > 0 && call >= f.dropCursorAfter {
			cursor = ""
		}
		meta = fmt.Sprintf(`"has_more":true,"response_metadata":{"next_cursor":%q}`, cursor)
	}
	return 200, `{"ok":true,"messages":[` + strings.Join(msgs, ",") + `],` + meta + `}`
}

func uniqueTs(msgs []Message) int {
	seen := map[string]bool{}
	for _, m := range msgs {
		seen[m.Ts] = true
	}
	return len(seen)
}

// 🚨 -n が 1 回の応答を超えるときは cursor を追って -n 件まで取ること。
//
// 1 回で終わると、スレッドは古い順に返るので**新しい側の返信**が警告なしに欠ける（rc=0）。
func TestRepliesPagesUntilLimit(t *testing.T) {
	f := &threadFake{total: 2500}
	rt := &recordingTransport{handle: f.handle}
	c := newTestClient(t, rt)
	msgs, err := c.Replies(context.Background(), "C0123456789", "1.000000", 2500)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2500 {
		t.Fatalf("件数: got %d, want 2500", len(msgs))
	}
	if got := msgs[len(msgs)-1].Ts; got != "2500.000000" {
		t.Errorf("最新の返信が欠けている: 末尾 ts=%s", got)
	}
	for i, fm := range rt.forms {
		if n, _ := strconv.Atoi(fm.Get("limit")); n > messagePageLimit {
			t.Errorf("%d 回目の limit=%d が 1 回の上限を超えている", i+1, n)
		}
		if fm.Get("ts") != "1.000000" {
			t.Errorf("%d 回目で ts が引き継がれていない: %q", i+1, fm.Get("ts"))
		}
	}
	// -n が総数より少なければ、ちょうど -n 件で止まる（打ち切りではない）。
	rt2 := &recordingTransport{handle: (&threadFake{total: 2500}).handle}
	msgs, err = newTestClient(t, rt2).Replies(context.Background(), "C0123456789", "1.000000", 1500)
	if err != nil || len(msgs) != 1500 {
		t.Fatalf("-n 1500: got %d 件, err=%v", len(msgs), err)
	}
}

// -n が 1 回の上限（1000）以下なら、呼び出しは 1 回だけ（ページングを入れる前と同じ）。
func TestSmallLimitIsSingleCall(t *testing.T) {
	for name, call := range map[string]func(*Client) ([]Message, error){
		"history": func(c *Client) ([]Message, error) {
			return c.History(context.Background(), "C0123456789", 1000, "", "")
		},
		"replies": func(c *Client) ([]Message, error) {
			return c.Replies(context.Background(), "C0123456789", "1.000000", 1000)
		},
	} {
		rt := &recordingTransport{handle: (&threadFake{total: 5000}).handle}
		msgs, err := call(newTestClient(t, rt))
		if err != nil || len(msgs) != 1000 {
			t.Fatalf("%s: got %d 件, err=%v", name, len(msgs), err)
		}
		if len(rt.forms) != 1 {
			t.Errorf("%s: -n 1000 で %d 回呼んでいる（1 回で取れる）", name, len(rt.forms))
		}
	}
}

func TestHistoryPagesUntilLimit(t *testing.T) {
	rt := &recordingTransport{handle: (&threadFake{total: 2500, newestFirst: true}).handle}
	c := newTestClient(t, rt)
	msgs, err := c.History(context.Background(), "C0123456789", 1500, "10.000000", "9999.000000")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1500 || uniqueTs(msgs) != 1500 {
		t.Fatalf("件数: got %d（重複除去後 %d）, want 1500", len(msgs), uniqueTs(msgs))
	}
	if msgs[0].ChannelID != "C0123456789" || msgs[len(msgs)-1].ChannelID != "C0123456789" {
		t.Error("2 ページ目以降でチャンネルが埋まっていない")
	}
	if len(rt.forms) != 2 {
		t.Fatalf("1500 件は 2 回で取れるはず: %d 回", len(rt.forms))
	}
	for i, fm := range rt.forms {
		if n, _ := strconv.Atoi(fm.Get("limit")); n > messagePageLimit {
			t.Errorf("%d 回目の limit=%d が 1 回の上限を超えている", i+1, n)
		}
		if fm.Get("oldest") != "10.000000" || fm.Get("latest") != "9999.000000" {
			t.Errorf("%d 回目で期間指定が引き継がれていない: %v", i+1, fm)
		}
	}
}

// replies が各ページの先頭に親メッセージを再掲しても、重複させず、-n は重複除去後で数えること。
func TestRepliesDedupesRepeatedParent(t *testing.T) {
	rt := &recordingTransport{handle: (&threadFake{total: 2500, repeatParent: true}).handle}
	msgs, err := newTestClient(t, rt).Replies(context.Background(), "C0123456789", "1.000000", 2500)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2500 || uniqueTs(msgs) != 2500 {
		t.Errorf("件数: got %d（重複除去後 %d）, want 2500", len(msgs), uniqueTs(msgs))
	}
	if got := msgs[len(msgs)-1].Ts; got != "2500.000000" {
		t.Errorf("再掲分を件数に数えて、最新の返信を取りこぼしている: 末尾 ts=%s", got)
	}
}

// 🚨 limit より短いページが返っても、has_more と cursor があれば続けること
// （「短いページ = 最後のページ」と早合点しない）。
func TestShortPagesAreFollowed(t *testing.T) {
	rt := &recordingTransport{handle: (&threadFake{total: 30, pageCap: 7}).handle}
	msgs, err := newTestClient(t, rt).Replies(context.Background(), "C0123456789", "1.000000", 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 30 {
		t.Errorf("短いページで止まった: %d 件", len(msgs))
	}
}

// has_more=true なのに next_cursor が空なら、完了扱いにせず TruncatedError（続きを取れない）にする。
func TestHasMoreWithoutCursorIsTruncation(t *testing.T) {
	rt := &recordingTransport{handle: (&threadFake{total: 30, pageCap: 7, dropCursorAfter: 2}).handle}
	msgs, err := newTestClient(t, rt).Replies(context.Background(), "C0123456789", "1.000000", 30)
	if !IsTruncated(err) {
		t.Fatalf("続きがあるのに完了扱いにした: %d 件, err=%v", len(msgs), err)
	}
	if len(msgs) != 14 {
		t.Errorf("取得できた分は返すべき: %d 件", len(msgs))
	}
	if !strings.Contains(err.Error(), "カーソルがありませんでした") {
		t.Errorf("安全上限と区別できる案内になっていない: %v", err)
	}
}

// ページングの安全上限に達したら、取得できた分と TruncatedError を返すこと（無音で切らない）。
func TestMessagePagingReportsTruncation(t *testing.T) {
	for name, call := range map[string]func(*Client) ([]Message, error){
		"history": func(c *Client) ([]Message, error) {
			return c.History(context.Background(), "C0123456789", 1<<30, "", "")
		},
		"replies": func(c *Client) ([]Message, error) {
			return c.Replies(context.Background(), "C0123456789", "1.000000", 1<<30)
		},
	} {
		rt := &recordingTransport{handle: (&threadFake{total: 1 << 30}).handle}
		got, err := call(newTestClient(t, rt))
		if !IsTruncated(err) {
			t.Errorf("%s: 打ち切りとして分類されていない: %v", name, err)
			continue
		}
		if len(rt.hosts) != maxPages {
			t.Errorf("%s: 安全上限を超えて呼び続けている: %d 回", name, len(rt.hosts))
		}
		if len(got) != maxPages*messagePageLimit {
			t.Errorf("%s: 取得できた分は返すべき: %d 件", name, len(got))
		}
	}
}

// 🚨 2 ページ目以降の失敗（429 / 5xx / 通信断）で、取得済みページを捨てないこと。
// 取得分と PartialError（打ち切りとは別）を返す。1 ページ目の失敗は従来どおりエラーだけ。
func TestMidPagingFailureReturnsPartial(t *testing.T) {
	type fetch func(*Client) (int, error)
	cases := map[string]struct {
		handle func(string, url.Values) (int, string)
		call   fetch
		first  int // 1 ページ目の件数
	}{
		"history": {(&threadFake{total: 5000, failAt: 2}).handle, func(c *Client) (int, error) {
			m, err := c.History(context.Background(), "C0123456789", 3000, "", "")
			return len(m), err
		}, 1000},
		"replies": {(&threadFake{total: 5000, failAt: 2}).handle, func(c *Client) (int, error) {
			m, err := c.Replies(context.Background(), "C0123456789", "1.000000", 3000)
			return len(m), err
		}, 1000},
		"channels": {failingAt(2, endlessCursorHandler("ch", "channels")), func(c *Client) (int, error) {
			ch, err := c.Channels(context.Background(), "", 0)
			return len(ch), err
		}, 1},
		"users": {failingAt(2, endlessCursorHandler("user", "members")), func(c *Client) (int, error) {
			u, err := c.Users(context.Background(), 0)
			return len(u), err
		}, 1},
	}
	for name, tc := range cases {
		n, err := tc.call(newTestClient(t, &recordingTransport{handle: tc.handle}))
		if !IsPartial(err) {
			t.Errorf("%s: 途中失敗として分類されていない: %v", name, err)
			continue
		}
		if IsTruncated(err) {
			t.Errorf("%s: 途中失敗を打ち切り（完了扱い）と混同している", name)
		}
		if n != tc.first {
			t.Errorf("%s: 取得済みの %d 件を返すべき: %d 件", name, tc.first, n)
		}
		if !strings.Contains(err.Error(), "レート制限") {
			t.Errorf("%s: 元の失敗理由が失われている: %v", name, err)
		}
	}

	// 1 ページ目で失敗したら、部分結果ではなくただのエラー。
	rt := &recordingTransport{handle: (&threadFake{total: 5000, failAt: 1}).handle}
	m, err := newTestClient(t, rt).History(context.Background(), "C0123456789", 3000, "", "")
	if err == nil || IsPartial(err) || m != nil {
		t.Errorf("1 ページ目の失敗: got %d 件, err=%v", len(m), err)
	}
}

// failingAt は n 回目の呼び出しだけ 429 を返すハンドラにする。
func failingAt(n int, h func(string, url.Values) (int, string)) func(string, url.Values) (int, string) {
	var mu sync.Mutex
	calls := 0
	return func(method string, form url.Values) (int, string) {
		mu.Lock()
		calls++
		c := calls
		mu.Unlock()
		if c == n {
			return 429, ""
		}
		return h(method, form)
	}
}

// 打ち切りの案内が、実際には効かない回避策（-name での絞り込み）を勧めないこと。
// channels / users の -name は全件を取得してから絞り込むので、同じ上限に当たる。
func TestTruncationHintDoesNotRecommendNameFilter(t *testing.T) {
	rt := &recordingTransport{handle: endlessCursorHandler("ch", "channels")}
	_, err := newTestClient(t, rt).Channels(context.Background(), "", 0)
	if !IsTruncated(err) {
		t.Fatalf("前提: 打ち切りになるはず: %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "-name は全件を取得してから絞り込む") {
		t.Errorf("-name では回避できないことを伝えていない: %s", msg)
	}
	if !strings.Contains(msg, "-types") {
		t.Errorf("効く回避策（-types で分ける）を案内していない: %s", msg)
	}

	rt2 := &recordingTransport{handle: endlessCursorHandler("user", "members")}
	_, err = newTestClient(t, rt2).Users(context.Background(), 0)
	if !IsTruncated(err) {
		t.Fatalf("前提: 打ち切りになるはず: %v", err)
	}
	if strings.Contains(err.Error(), "-name での絞り込み") || !strings.Contains(err.Error(), "避けられません") {
		t.Errorf("users の打ち切り案内が効かない回避策を勧めている: %s", err.Error())
	}
}
