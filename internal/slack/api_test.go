package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

// endlessCursorHandler は「いくらでも続きがある」Slack を模す。
func endlessCursorHandler(namePrefix string, key string) func(string, url.Values) (int, string) {
	n := 0
	return func(method string, form url.Values) (int, string) {
		n++
		return 200, fmt.Sprintf(`{"ok":true,%q:[{"id":"C%09d","name":"%s%d"}],"response_metadata":{"next_cursor":"cur%d"}}`,
			key, n, namePrefix, n, n)
	}
}

// 🚨 ページングの安全上限に達したら、黙って打ち切らずに「不完全」と伝えること。
//
// 無音で切ると、実在するチャンネルに対して「見つかりませんでした」が出る
// （存在しないのではなく、列挙が足りなかっただけ）。
func TestChannelsReportsTruncation(t *testing.T) {
	rt := &recordingTransport{handle: endlessCursorHandler("ch", "channels")}
	c := newTestClient(t, rt)

	got, err := c.Channels(context.Background(), "", 0)
	if err == nil {
		t.Fatal("打ち切りを伝えていない（不完全な一覧を完全なものとして返している）")
	}
	if !IsTruncated(err) {
		t.Fatalf("打ち切りとして分類されていない: %T %v", err, err)
	}
	if len(got) != maxPages {
		t.Errorf("取得できた分は返すべき: %d 件", len(got))
	}
	if len(rt.hosts) != maxPages {
		t.Errorf("安全上限を超えて呼び続けている: %d 回", len(rt.hosts))
	}
	if !strings.Contains(err.Error(), "不完全") {
		t.Errorf("不完全だと分かる案内にすべき: %v", err)
	}
}

func TestUsersReportsTruncation(t *testing.T) {
	rt := &recordingTransport{handle: endlessCursorHandler("user", "members")}
	c := newTestClient(t, rt)

	got, err := c.Users(context.Background(), 0)
	if !IsTruncated(err) {
		t.Fatalf("打ち切りとして分類されていない: %T %v", err, err)
	}
	if len(got) != maxPages {
		t.Errorf("取得できた分は返すべき: %d 件", len(got))
	}
}

// 打ち切られた一覧で名前が見つからなかったとき、「存在しない」と断定しないこと。
func TestResolveChannelDistinguishesTruncationFromNotFound(t *testing.T) {
	rt := &recordingTransport{handle: endlessCursorHandler("ch", "channels")}
	c := newTestClient(t, rt)

	_, err := c.ResolveChannel(context.Background(), "#nosuch")
	if err == nil {
		t.Fatal("エラーになるべき")
	}
	if !strings.Contains(err.Error(), "存在しないとは限りません") {
		t.Errorf("打ち切りを踏まえた案内になっていない: %v", err)
	}

	// 打ち切られていても、取得済みの中に在れば解決できること。
	rt2 := &recordingTransport{handle: endlessCursorHandler("ch", "channels")}
	c2 := newTestClient(t, rt2)
	id, err := c2.ResolveChannel(context.Background(), "#ch3")
	if err != nil {
		t.Fatalf("取得済みの中に在るのに解決できない: %v", err)
	}
	if id == "" {
		t.Error("ID が空")
	}

	// 一覧が完全（打ち切りなし）なら、従来どおり「見つかりませんでした」。
	rt3 := &recordingTransport{handle: func(method string, form url.Values) (int, string) {
		return 200, `{"ok":true,"channels":[{"id":"C000000001","name":"general"}]}`
	}}
	c3 := newTestClient(t, rt3)
	if _, err := c3.ResolveChannel(context.Background(), "#nosuch"); err == nil {
		t.Fatal("エラーになるべき")
	} else if strings.Contains(err.Error(), "存在しないとは限りません") {
		t.Errorf("打ち切っていないのに曖昧な案内を出している: %v", err)
	}
}

// 名前解決は「見つかった時点で」打ち切ること。
//
// 🚨 全ページを集めてから探すと、大きなワークスペースでは
// `slack history '#name'` のたびに最大 maxPages 回のリクエストを投げ、429 に当たる。
func TestResolveChannelStopsAtFirstMatch(t *testing.T) {
	rt := &recordingTransport{handle: endlessCursorHandler("ch", "channels")}
	c := newTestClient(t, rt)

	// endlessCursorHandler は n ページ目に "ch<n>" を 1 件返す（続きは無限にある）。
	id, err := c.ResolveChannel(context.Background(), "#ch2")
	if err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Fatal("ID が空")
	}
	if len(rt.hosts) != 2 {
		t.Errorf("2 ページ目で見つかるのに %d 回リクエストしている（全件取得してから探している）", len(rt.hosts))
	}
}

// ID 形式はそのまま使い、一覧取得を走らせないこと（無駄な API 呼び出しを避ける）。
func TestResolveChannelSkipsListForIDs(t *testing.T) {
	rt := &recordingTransport{}
	c := newTestClient(t, rt)
	id, err := c.ResolveChannel(context.Background(), "C0123456789")
	if err != nil || id != "C0123456789" {
		t.Fatalf("got %q, %v", id, err)
	}
	if len(rt.hosts) != 0 {
		t.Errorf("ID 指定なのに API を呼んでいる: %v", rt.methods)
	}
}

// -n 指定での早期打ち切りは「不完全」ではない（利用者が件数を指定している）。
func TestExplicitLimitIsNotTruncation(t *testing.T) {
	rt := &recordingTransport{handle: endlessCursorHandler("ch", "channels")}
	c := newTestClient(t, rt)
	got, err := c.Channels(context.Background(), "", 3)
	if err != nil {
		t.Fatalf("-n 指定は打ち切り扱いにしない: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("件数: %d", len(got))
	}
}

// history / replies は引数のチャンネルを各メッセージへ埋めること（-json でも落とさない）。
func TestHistoryFillsChannelID(t *testing.T) {
	rt := &recordingTransport{handle: func(method string, form url.Values) (int, string) {
		return 200, `{"ok":true,"messages":[{"ts":"1725000000.000100","text":"hi"}]}`
	}}
	c := newTestClient(t, rt)
	msgs, err := c.History(context.Background(), "C0123456789", 10, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].ChannelID != "C0123456789" {
		t.Errorf("チャンネルが埋まっていない: %+v", msgs)
	}
	// -json 出力にチャンネルが載ること（TSV だけ載って JSON で落ちるのを防ぐ）。
	b, err := json.Marshal(msgs[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"channel_id":"C0123456789"`) {
		t.Errorf("-json 出力にチャンネルが載っていない: %s", b)
	}

	replies, err := c.Replies(context.Background(), "C0123456789", "1725000000.000100", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(replies) != 1 || replies[0].ChannelID != "C0123456789" {
		t.Errorf("スレッド側でチャンネルが埋まっていない: %+v", replies)
	}
}
