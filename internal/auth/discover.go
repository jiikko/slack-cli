package auth

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
)

// domainRe は Local Storage 内に現れる <sub>.slack.com を拾う。
var domainRe = regexp.MustCompile(`\b([a-z0-9][a-z0-9-]{0,62})\.slack\.com\b`)

// nonWorkspaceSubdomains は Slack 自身が使うサブドメイン（ワークスペース名ではないもの）。
//
// 🚨 これは**候補の並べ替えと明らかなノイズ除去のためのヒューリスティック**であり、
// 正しさの保証ではない。ここに載っていない Slack のインフラ用サブドメインが候補に
// 混ざることはありうる。混ざっても実害は「setup の候補一覧に余計な行が出る」だけで、
// 実際の接続は auth.test の一致確認を通ったものだけ（resolve.go）。
var nonWorkspaceSubdomains = map[string]bool{
	"a": true, "api": true, "app": true, "ca": true, "cdn": true,
	"downloads": true, "edgeapi": true, "emoji": true, "files": true,
	"hooks": true, "my": true, "slack": true, "slack-redir": true,
	"slackb": true, "status": true, "www": true,
	"wss-primary": true, "wss-backup": true, "wss-mobile": true,
}

// WorkspaceHint は Local Storage から見つかったワークスペース候補。
type WorkspaceHint struct {
	Domain string // サブドメイン（例: acme）
	Hits   int    // 出現回数（多いほど「よく使っている」ヒント）
}

// DiscoverWorkspaces は指定プロファイルの Local Storage から、ログインしていそうな
// ワークスペースのサブドメインを列挙する（出現回数の多い順）。
//
// 🚨 ここはネットワークに一切出ない。「どのワークスペースにログインしているか」を
// 調べるために別ワークスペースへ API を投げると、仕様 §4（設定した対象以外に接続しない）
// を自分で破ることになる。候補はローカルの痕跡だけから作り、接続は設定確定後に行う。
func DiscoverWorkspaces(profile string) ([]WorkspaceHint, error) {
	src, err := localStorageDir(profile)
	if err != nil {
		return nil, err
	}
	dir, cleanup, err := copyLevelDB(src)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	counts := map[string]int{}
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		for _, m := range domainRe.FindAllSubmatch(data, -1) {
			sub := string(m[1])
			if nonWorkspaceSubdomains[sub] {
				continue
			}
			counts[sub]++
		}
		return nil
	})

	out := make([]WorkspaceHint, 0, len(counts))
	for d, n := range counts {
		out = append(out, WorkspaceHint{Domain: d, Hits: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Hits != out[j].Hits {
			return out[i].Hits > out[j].Hits
		}
		return out[i].Domain < out[j].Domain
	})
	return out, nil
}
