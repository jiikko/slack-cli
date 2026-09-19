package auth

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// tokenRe は Slack の Web クライアント用トークン（xoxc-…）を拾う正規表現。
var tokenRe = regexp.MustCompile(`xoxc-[0-9A-Za-z-]{10,}`)

// proximityWindow は「トークンの近くにワークスペース名があるか」を見る前後のバイト数。
// leveldb のレコードは 1 件が数 KB になることがあるため、やや広めに取る。
const proximityWindow = 4096

// TokenCandidate は抽出した xoxc トークン候補。
type TokenCandidate struct {
	Token string
	// Score は「設定中のワークスペースのものらしさ」のヒント（大きいほど確からしい）。
	//
	// 🚨 これは**順序付けのヒントにすぎない**。採用の可否は必ず auth.test の
	// 結果（url / team）で決めること。Slack の localStorage の構造は公開仕様ではなく、
	// 手元で確認できていない（Chrome の資格情報領域を読む探索が環境の制限で行えなかった）。
	// スコアを採用条件にすると、構造が変わった日に「別ワークスペースのトークンを
	// 正しいものとして使う」形で静かに壊れる。
	Score int
}

// localStorageDir は Local Storage の leveldb ディレクトリを返す。
func localStorageDir(profile string) (string, error) {
	base, err := chromeProfileDir(profile)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "Local Storage", "leveldb")
	if _, err := os.Stat(dir); err != nil {
		return "", fmt.Errorf(
			"Local Storage が見つかりませんでした（プロファイル=%q）。探した場所:\n  %s\n"+
				"  - プロファイル名が正しいか確認してください（-profile / SLACK_CLI_CHROME_PROFILE）。",
			profile, dir)
	}
	return dir, nil
}

// copyLevelDB は leveldb ディレクトリを一時領域へコピーする。
//
// 🚨 コピーの中身には xoxc トークンが含まれる。Cookie DB と同じ後始末の機構
// （cleanup.go の 3 段構え）に必ず載せること。別経路で os.MkdirTemp すると、
// その残骸はシグナル経路にも起動時の掃除にも拾われない。
func copyLevelDB(src string) (string, func(), error) {
	tmpdir, cleanup, err := newTempDir()
	if err != nil {
		return "", nil, err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		cleanup()
		if os.IsPermission(err) {
			return "", nil, fmt.Errorf(
				"Local Storage を読み取れませんでした（アクセス拒否）。\n"+
					"  ターミナル（またはこのツールを起動しているアプリ）に「フルディスクアクセス」を付与してください:\n"+
					"    システム設定 → プライバシーとセキュリティ → フルディスクアクセス\n"+
					"  対象: %s", src)
		}
		return "", nil, fmt.Errorf("Local Storage の読み取りに失敗: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue // leveldb は平坦。サブディレクトリは読まない
		}
		data, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			continue // ロック中のファイル等は飛ばす（取れたものだけで走査する）
		}
		if err := os.WriteFile(filepath.Join(tmpdir, e.Name()), data, 0o600); err != nil {
			cleanup()
			return "", nil, err
		}
	}
	return tmpdir, cleanup, nil
}

// scanTokens は 1 ファイル分のバイト列から xoxc トークンを拾い、
// 近傍に workspace 名があるかでスコアを付ける。
func scanTokens(data []byte, workspace string) []TokenCandidate {
	locs := tokenRe.FindAllIndex(data, -1)
	if len(locs) == 0 {
		return nil
	}
	out := make([]TokenCandidate, 0, len(locs))
	for _, loc := range locs {
		token := string(data[loc[0]:loc[1]])
		out = append(out, TokenCandidate{Token: token, Score: proximityScore(data, loc[0], loc[1], workspace)})
	}
	return out
}

// proximityScore はトークンの前後 proximityWindow バイトに workspace 名が
// 現れるかを見て 0〜2 のヒントを返す。
func proximityScore(data []byte, start, end int, workspace string) int {
	if workspace == "" {
		return 0
	}
	lo := start - proximityWindow
	if lo < 0 {
		lo = 0
	}
	hi := end + proximityWindow
	if hi > len(data) {
		hi = len(data)
	}
	window := string(data[lo:hi])
	switch {
	case strings.Contains(window, workspace+".slack.com"):
		return 2 // 同じレコード内にワークスペースの URL がある
	case strings.Contains(window, `"`+workspace+`"`):
		return 2 // JSON の値として現れている
	case strings.Contains(window, workspace):
		return 1
	default:
		return 0
	}
}

// tokenCollector は複数ファイルにまたがる走査結果をまとめる。
// （ファイル読み取りから切り離してあるので、合成データで単体テストできる）
type tokenCollector struct {
	best  map[string]int // トークン -> 最大スコア
	order map[string]int // トークン -> 初出順
	seq   int
}

func newTokenCollector() *tokenCollector {
	return &tokenCollector{best: map[string]int{}, order: map[string]int{}}
}

// add は 1 ファイル分のバイト列を走査結果へ取り込む。
func (tc *tokenCollector) add(data []byte, workspace string) {
	for _, c := range scanTokens(data, workspace) {
		if s, ok := tc.best[c.Token]; !ok || c.Score > s {
			tc.best[c.Token] = c.Score
		}
		if _, ok := tc.order[c.Token]; !ok {
			tc.order[c.Token] = tc.seq
			tc.seq++
		}
	}
}

// result は重複除去済みの候補を「スコアの高い順、同点なら検出順」で返す。
func (tc *tokenCollector) result() []TokenCandidate {
	out := make([]TokenCandidate, 0, len(tc.best))
	for t, s := range tc.best {
		out = append(out, TokenCandidate{Token: t, Score: s})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return tc.order[out[i].Token] < tc.order[out[j].Token]
	})
	return out
}

// ExtractTokens は指定プロファイルの Local Storage から xoxc トークン候補を返す。
// workspace はスコア付け（順序）にのみ使い、絞り込みには使わない。
//
// 戻り値は重複除去済みで、スコアの高い順（同点なら検出順）。
func ExtractTokens(profile, workspace string) ([]TokenCandidate, error) {
	src, err := localStorageDir(profile)
	if err != nil {
		return nil, err
	}
	dir, cleanup, err := copyLevelDB(src)
	if err != nil {
		return nil, err
	}
	defer cleanup() // ①: 正常終了・エラー・panic を覆う

	tc := newTokenCollector()
	walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		tc.add(data, workspace)
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	return tc.result(), nil
}

// ErrNoToken は leveldb から 1 つも xoxc トークンを取り出せなかったことを表す。
type ErrNoToken struct{ Profile string }

func (e *ErrNoToken) Error() string {
	return fmt.Sprintf(
		"Chrome プロファイル %q の Local Storage から Slack のトークン（xoxc-…）を取り出せませんでした。\n"+
			"  考えられる原因と対処:\n"+
			"  1. そのプロファイルで Slack を開いていない → %s で対象ワークスペースを一度開いてください。\n"+
			"  2. トークンが leveldb の圧縮ブロックに入っていて読めない\n"+
			"     → Chrome を完全に終了（cmd+Q）してから、もう一度実行してください。\n"+
			"  3. 上記でも取れない場合は、トークンを明示指定できます:\n"+
			"       slack -token xoxc-... <コマンド>   または   export SLACK_CLI_TOKEN=xoxc-...\n"+
			"     （明示指定したトークンも auth.test でワークスペース一致を確認します）",
		e.Profile, ChromeName)
}
