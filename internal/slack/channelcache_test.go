package slack

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// channelsHandler は conversations.list を pages ページ（1 ページ 1 件、C1..Cn）で返し、conversations.info は names の名前で返す。
func channelsHandler(pages int, names map[string]string) func(string, url.Values) (int, string) {
	return func(method string, form url.Values) (int, string) {
		switch method {
		case "conversations.list":
			i := 1
			if c := form.Get("cursor"); c != "" {
				fmt.Sscanf(c, "p%d", &i)
			}
			next := ""
			if i < pages {
				next = fmt.Sprintf("p%d", i+1)
			}
			return 200, fmt.Sprintf(`{"ok":true,"channels":[{"id":"C%d","name":"ch%d"}],"response_metadata":{"next_cursor":%q}}`, i, i, next)
		case "conversations.info":
			id := form.Get("channel")
			name, ok := names[id]
			if !ok {
				return 200, `{"ok":false,"error":"channel_not_found"}`
			}
			return 200, fmt.Sprintf(`{"ok":true,"channel":{"id":%q,"name":%q}}`, id, name)
		}
		return 200, `{"ok":true}`
	}
}

func countMethod(rt *recordingTransport, m string) int {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	n := 0
	for _, x := range rt.methods {
		if x == m {
			n++
		}
	}
	return n
}

// cachedClient はキャッシュ付きのクライアントを作る。now を差し替えて時刻を進められる。
func cachedClient(t *testing.T, rt *recordingTransport, dir string, now *time.Time, refresh bool, notify *bytes.Buffer) *Client {
	t.Helper()
	c := newTestClient(t, rt)
	var w io.Writer // 🚨 nil の *bytes.Buffer をそのまま渡すと、nil でない io.Writer になって書き込みで panic する
	if notify != nil {
		w = notify
	}
	cc := NewChannelCache(dir, "alpha", "U1", refresh, w)
	if cc == nil {
		t.Fatal("キャッシュが作れない")
	}
	cc.now = func() time.Time { return *now }
	c.UseChannelCache(cc)
	return c
}

// 2 回目はキャッシュから返し、conversations.list を呼ばないこと。使ったことは知らせること。
func TestChannelsServedFromCache(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	rt := &recordingTransport{handle: channelsHandler(3, nil)}
	if chs, err := cachedClient(t, rt, dir, &now, false, nil).Channels(context.Background(), "", 0); err != nil || len(chs) != 3 {
		t.Fatalf("1 回目: %v %d", err, len(chs))
	}
	first := countMethod(rt, "conversations.list")

	now = now.Add(59 * time.Minute)
	var notify bytes.Buffer
	chs, err := cachedClient(t, rt, dir, &now, false, &notify).Channels(context.Background(), "", 0)
	if err != nil || len(chs) != 3 {
		t.Fatalf("2 回目: %v %d", err, len(chs))
	}
	if got := countMethod(rt, "conversations.list"); got != first {
		t.Errorf("キャッシュが新しいのに API を呼んだ（%d → %d 回）", first, got)
	}
	if !strings.Contains(notify.String(), "59 分前") || !strings.Contains(notify.String(), "-refresh") {
		t.Errorf("キャッシュを使ったことを知らせていない: %q", notify.String())
	}
}

// 古い・壊れた・形式やキーが合わないキャッシュは使わず、API から取り直すこと。
func TestStaleOrBrokenCacheIsRefetched(t *testing.T) {
	cases := map[string]func(path string, now *time.Time){
		"1 時間を過ぎた":  func(_ string, now *time.Time) { *now = now.Add(61 * time.Minute) },
		"未来の取得時刻":   func(_ string, now *time.Time) { *now = now.Add(-time.Minute) },
		"壊れた JSON":  func(path string, _ *time.Time) { os.WriteFile(path, []byte("{broken"), 0o600) },
		"形式の版が違う":   func(path string, _ *time.Time) { rewrite(path, `"version":1`, `"version":99`) },
		"ユーザーが違う中身": func(path string, _ *time.Time) { rewrite(path, `"user_id":"U1"`, `"user_id":"U2"`) },
	}
	for name, spoil := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
			rt := &recordingTransport{handle: channelsHandler(2, nil)}
			if _, err := cachedClient(t, rt, dir, &now, false, nil).Channels(context.Background(), "", 0); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "channels-alpha-U1-private_channel+public_channel.json")
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("キャッシュが書かれていない: %v", err)
			}
			spoil(path, &now)
			before := countMethod(rt, "conversations.list")
			if _, err := cachedClient(t, rt, dir, &now, false, nil).Channels(context.Background(), "", 0); err != nil {
				t.Fatal(err)
			}
			if countMethod(rt, "conversations.list") == before {
				t.Error("使えないキャッシュを使った（API から取り直していない）")
			}
		})
	}
}

func rewrite(path, old, repl string) {
	b, _ := os.ReadFile(path)
	os.WriteFile(path, []byte(strings.Replace(string(b), old, repl, 1)), 0o600)
}

// -types・ユーザー・workspace が違えば別のキャッシュになること。
func TestCacheKeySeparatesTypesAndUsers(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	rt := &recordingTransport{handle: channelsHandler(1, nil)}
	if _, err := cachedClient(t, rt, dir, &now, false, nil).Channels(context.Background(), "public_channel", 0); err != nil {
		t.Fatal(err)
	}
	before := countMethod(rt, "conversations.list")
	if _, err := cachedClient(t, rt, dir, &now, false, nil).Channels(context.Background(), "private_channel", 0); err != nil {
		t.Fatal(err)
	}
	if countMethod(rt, "conversations.list") == before {
		t.Error("-types が違うのに同じキャッシュを使った")
	}
	// 種別ごとに別に持つこと（1 つのファイルを上書きし合うと、交互に取るたびにキャッシュが効かない）。
	before = countMethod(rt, "conversations.list")
	if _, err := cachedClient(t, rt, dir, &now, false, nil).Channels(context.Background(), "public_channel", 0); err != nil {
		t.Fatal(err)
	}
	if countMethod(rt, "conversations.list") != before {
		t.Error("別の種別を取った後に、先に取った種別のキャッシュが消えている")
	}
	if normalizeTypes("public_channel, private_channel") != normalizeTypes("private_channel,public_channel,public_channel") {
		t.Error("同じ種別の並びが別のキーになる")
	}
	a := NewChannelCache(dir, "alpha", "U1", false, nil).path("x")
	if a == NewChannelCache(dir, "alpha", "U2", false, nil).path("x") || a == NewChannelCache(dir, "beta", "U1", false, nil).path("x") {
		t.Error("ユーザー / workspace が違うのに同じファイルになる")
	}
	for _, bad := range [][2]string{{"../x", "U1"}, {"alpha", "u1/../"}, {"alpha", ""}} {
		if NewChannelCache(dir, bad[0], bad[1], false, nil) != nil {
			t.Errorf("ファイル名に使えないキー %v でキャッシュを作った", bad)
		}
	}
	if normalizeTypes("public_channel,../x") != "" {
		t.Error("ファイル名に使えない -types をキーにした")
	}
}

// 途中で失敗した・打ち切られた・-n で止めた取得はキャッシュに書かないこと。
func TestIncompleteFetchIsNotCached(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	check := func(t *testing.T, rt *recordingTransport, limit int) {
		t.Helper()
		dir := t.TempDir()
		_, _ = cachedClient(t, rt, dir, &now, false, nil).Channels(context.Background(), "", limit)
		if left, _ := filepath.Glob(filepath.Join(dir, "*")); len(left) != 0 {
			t.Errorf("不完全な取得をキャッシュに書いた: %v", left)
		}
	}
	t.Run("途中の 429", func(t *testing.T) {
		calls := 0
		check(t, &recordingTransport{handle: func(m string, f url.Values) (int, string) {
			calls++
			if calls == 2 {
				return 429, ""
			}
			return channelsHandler(3, nil)(m, f)
		}}, 0)
	})
	t.Run("上限で打ち切り", func(t *testing.T) { check(t, &recordingTransport{handle: channelsHandler(maxPages+5, nil)}, 0) })
	t.Run("-n で止めた", func(t *testing.T) { check(t, &recordingTransport{handle: channelsHandler(3, nil)}, 1) })
}

// -refresh はキャッシュを読まずに取り直し、書き直すこと。
func TestRefreshBypassesAndRewritesCache(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	rt := &recordingTransport{handle: channelsHandler(1, nil)}
	if _, err := cachedClient(t, rt, dir, &now, false, nil).Channels(context.Background(), "", 0); err != nil {
		t.Fatal(err)
	}
	now = now.Add(10 * time.Minute)
	before := countMethod(rt, "conversations.list")
	if _, err := cachedClient(t, rt, dir, &now, true, nil).Channels(context.Background(), "", 0); err != nil {
		t.Fatal(err)
	}
	if countMethod(rt, "conversations.list") == before {
		t.Error("-refresh なのにキャッシュを使った")
	}
	var notify bytes.Buffer
	if _, err := cachedClient(t, rt, dir, &now, false, &notify).Channels(context.Background(), "", 0); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(notify.String(), "0 分前") {
		t.Errorf("-refresh で書き直していない（取得時刻が古いまま）: %q", notify.String())
	}
}

// 🚨 #name の解決は、キャッシュの ID を conversations.info で確かめてから使うこと。名前が変わっていたら API で探し直す。
func TestResolveChannelVerifiesCachedID(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	for name, c := range map[string]struct {
		info     map[string]string // conversations.info が返す今の名前
		wantID   string
		wantList bool // conversations.list で探し直すか
	}{
		"名前が一致": {info: map[string]string{"C2": "ch2"}, wantID: "C2", wantList: false},
		"改名された（再利用の可能性）": {info: map[string]string{"C2": "renamed", "C9": "ch2"}, wantID: "C2", wantList: true},
		"確かめられない":        {info: map[string]string{}, wantID: "C2", wantList: true},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			// キャッシュを作る（C1..C3 = ch1..ch3）。
			warm := &recordingTransport{handle: channelsHandler(3, nil)}
			if _, err := cachedClient(t, warm, dir, &now, false, nil).Channels(context.Background(), "", 0); err != nil {
				t.Fatal(err)
			}
			rt := &recordingTransport{handle: channelsHandler(3, c.info)}
			id, err := cachedClient(t, rt, dir, &now, false, nil).ResolveChannel(context.Background(), "#ch2")
			if err != nil || id != c.wantID {
				t.Fatalf("got %q %v, want %q", id, err, c.wantID)
			}
			if got := countMethod(rt, "conversations.list") > 0; got != c.wantList {
				t.Errorf("conversations.list で探し直したか: got %v want %v", got, c.wantList)
			}
			if countMethod(rt, "conversations.info") != 1 {
				t.Error("キャッシュの ID を conversations.info で確かめていない")
			}
		})
	}
}

// キャッシュのファイルは 0600。cookie / トークンが入らないこと。
func TestCacheFileIsPrivateAndHasNoSecrets(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	rt := &recordingTransport{handle: channelsHandler(1, nil)}
	if _, err := cachedClient(t, rt, dir, &now, false, nil).Channels(context.Background(), "", 0); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*"))
	if len(files) != 1 {
		t.Fatalf("キャッシュのファイル: %v（一時ファイルが残っていないか）", files)
	}
	fi, _ := os.Stat(files[0])
	di, _ := os.Stat(dir)
	if fi.Mode().Perm() != 0o600 || di.Mode().Perm() != 0o700 {
		t.Errorf("パーミッション: ファイル %o / ディレクトリ %o", fi.Mode().Perm(), di.Mode().Perm())
	}
	data, _ := os.ReadFile(files[0])
	if strings.Contains(string(data), "xoxc") || strings.Contains(string(data), "xoxd") {
		t.Error("資格情報がキャッシュに入っている")
	}
}

// キャッシュを付けないクライアント（nil）は今どおり毎回 API から取ること。
func TestNilCacheKeepsOldBehavior(t *testing.T) {
	rt := &recordingTransport{handle: channelsHandler(1, nil)}
	c := newTestClient(t, rt)
	c.UseChannelCache(nil)
	for i := 0; i < 2; i++ {
		if _, err := c.Channels(context.Background(), "", 0); err != nil {
			t.Fatal(err)
		}
	}
	if countMethod(rt, "conversations.list") != 2 {
		t.Error("キャッシュなしなのに API を呼ばなかった")
	}
}

// API に送る types とキャッシュのキーは同じ正規化から作ること（空白入りの指定で食い違わない）。
func TestTypesSentToAPIMatchCacheKey(t *testing.T) {
	rt := &recordingTransport{handle: channelsHandler(1, nil)}
	if _, err := newTestClient(t, rt).Channels(context.Background(), " public_channel , private_channel,public_channel", 0); err != nil {
		t.Fatal(err)
	}
	got := rt.forms[0].Get("types")
	if got != "private_channel,public_channel" || strings.ReplaceAll(got, ",", "+") != normalizeTypes(" public_channel , private_channel") {
		t.Errorf("API に送った types %q がキャッシュのキー %q と対応しない", got, normalizeTypes(" public_channel , private_channel"))
	}
}
