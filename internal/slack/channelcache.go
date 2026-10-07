package slack

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// channelCacheTTL はチャンネル一覧のキャッシュを使う期間（issue 007 で 1 時間に決めた）。
const channelCacheTTL = time.Hour

// channelCacheVersion はキャッシュの形式の版。形式を変えたら上げる（古い形式は読まずに取り直す）。
const channelCacheVersion = 1

// ChannelCache は conversations.list の全件取得の結果を、しばらく手元に置く。
//
// 🚨 正しさをキャッシュに預けない。読めない・壊れている・古い・キーが合わないときは使わずに API から取る。
// チャンネル名 → ID の解決では、キャッシュの ID を conversations.info で確かめてから使う（ResolveChannel）。
// 中身はチャンネルの一覧（プライベートチャンネルの名前・トピックを含む）だけで、cookie / トークンは入れない。
type ChannelCache struct {
	dir       string
	workspace string
	userID    string
	refresh   bool      // true なら読まずに取り直す（書くのは今どおり）
	notify    io.Writer // キャッシュを使ったことを知らせる先（stderr）。nil なら知らせない
	now       func() time.Time
}

// NewChannelCache はキャッシュを作る。キーに使う値がファイル名に使えない形なら nil を返す（キャッシュを使わない）。
// nil の ChannelCache はどのメソッドでも「キャッシュなし」として振る舞う。
func NewChannelCache(dir, workspace, userID string, refresh bool, notify io.Writer) *ChannelCache {
	if dir == "" || !isCacheKeyPart(workspace, false) || !isCacheKeyPart(userID, true) {
		return nil
	}
	return &ChannelCache{dir: dir, workspace: workspace, userID: userID, refresh: refresh, notify: notify, now: time.Now}
}

// UseChannelCache はチャンネル一覧の取得と名前の解決にキャッシュを使うようにする（nil なら使わない）。
func (c *Client) UseChannelCache(cc *ChannelCache) { c.channelCache = cc }

// isCacheKeyPart は、ファイル名に入れてよい文字だけでできているかを返す。
// workspace は英小文字・数字・ハイフン（ValidateWorkspace 済み）、user ID は英大文字・数字。
func isCacheKeyPart(s string, upper bool) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch >= '0' && ch <= '9':
		case upper && ch >= 'A' && ch <= 'Z':
		case !upper && (ch >= 'a' && ch <= 'z' || ch == '-'):
		default:
			return false
		}
	}
	return true
}

// normalizeTypes は -types をキャッシュのキー（canonicalTypes の , を + にしたもの）にする。空ならキャッシュを使わない。
func normalizeTypes(types string) string {
	return strings.ReplaceAll(canonicalTypes(types), ",", "+")
}

// canonicalTypes は -types の空白と重複を除いて並べ替えた形（"private_channel,public_channel"）にする。
// 英小文字と _ 以外を含むなら空を返す。
//
// 🚨 キャッシュのキーと API に送る types は、どちらもここから作る（forEachChannelPage）。別々に作ると、
// "public_channel, private_channel" のように空白入りの指定で、API が受け取った種別とキーの種別が食い違いうる
// （Slack が空白入りの種別を捨てるなら、プライベートを欠いた一覧が既定の種別のキーで保存される。issue 007 の敵対的レビュー）。
func canonicalTypes(types string) string {
	if types == "" {
		types = defaultChannelTypes
	}
	seen := map[string]bool{}
	var parts []string
	for _, p := range strings.Split(types, ",") {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		for i := 0; i < len(p); i++ {
			if !(p[i] >= 'a' && p[i] <= 'z' || p[i] == '_') {
				return ""
			}
		}
		seen[p] = true
		parts = append(parts, p)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

type channelCacheFile struct {
	Version   int       `json:"version"`
	Workspace string    `json:"workspace"`
	UserID    string    `json:"user_id"`
	Types     string    `json:"types"`
	FetchedAt time.Time `json:"fetched_at"`
	Channels  []Channel `json:"channels"`
}

func (cc *ChannelCache) path(typesKey string) string {
	return filepath.Join(cc.dir, fmt.Sprintf("channels-%s-%s-%s.json", cc.workspace, cc.userID, typesKey))
}

// load は新しいキャッシュがあれば返す（無い・読めない・壊れている・古い・キーが合わないなら ok=false）。
func (cc *ChannelCache) load(types string) (chs []Channel, age time.Duration, ok bool) {
	if cc == nil || cc.refresh {
		return nil, 0, false
	}
	key := normalizeTypes(types)
	if key == "" {
		return nil, 0, false
	}
	data, err := os.ReadFile(cc.path(key))
	if err != nil {
		return nil, 0, false
	}
	var f channelCacheFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, 0, false
	}
	if f.Version != channelCacheVersion || f.Workspace != cc.workspace || f.UserID != cc.userID || f.Types != key {
		return nil, 0, false
	}
	age = cc.now().Sub(f.FetchedAt)
	if age < 0 || age > channelCacheTTL { // 未来の時刻（時計のずれ・改ざん）は古い扱い
		return nil, 0, false
	}
	return f.Channels, age, true
}

// save は全件の取得結果を書く。失敗しても取得の結果には影響させない（キャッシュは補助の経路）。
//
// 一時ファイル + rename で書く（同時に走った別のプロセスに、書きかけのファイルを読ませない）。
func (cc *ChannelCache) save(types string, chs []Channel) {
	if cc == nil {
		return
	}
	key := normalizeTypes(types)
	if key == "" {
		return
	}
	data, err := json.Marshal(channelCacheFile{
		Version: channelCacheVersion, Workspace: cc.workspace, UserID: cc.userID, Types: key,
		FetchedAt: cc.now(), Channels: chs,
	})
	if err != nil {
		return
	}
	if err := os.MkdirAll(cc.dir, 0o700); err != nil {
		return
	}
	tmp, err := os.CreateTemp(cc.dir, "channels-*.tmp") // 0600 で作られる
	if err != nil {
		return
	}
	name := tmp.Name()
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr != nil || cerr != nil || os.Rename(name, cc.path(key)) != nil {
		os.Remove(name)
	}
}

// noteUsed はキャッシュを使ったことを stderr に知らせる。
func (cc *ChannelCache) noteUsed(age time.Duration) {
	if cc == nil || cc.notify == nil {
		return
	}
	fmt.Fprintf(cc.notify, "チャンネル一覧はキャッシュ（%d 分前に取得）を使いました。最新にするには -refresh を付けてください\n", int(age/time.Minute))
}
