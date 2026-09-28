// Package auth は Chrome にログイン済みの Slack セッションから、API 呼び出しに必要な
// 資格情報（d cookie / xoxc トークン）を取り出す。
//
// 🚨 ここで扱う値はいずれも「なりすましログインが可能な資格情報」。
// ファイル・ログ・標準出力へ生値を出さないこと（表示は Mask を通す）。
// 一時コピーの後始末は chromecookie の Workspace（chrome.go の ws）に必ず載せる。
package auth

import (
	"fmt"
	"strings"

	"github.com/jiikko/dotfiles/src/chromecookie"
)

// slackCookieName は Slack のセッション Cookie の名前。
const slackCookieName = "d"

// profileHint は Cookie DB / Local Storage が無かったときに添える slack-cli 固有の案内。
const profileHint = "  - プロファイル名が正しいか確認してください（-profile / SLACK_CLI_CHROME_PROFILE）。"

// pickCookie は reqHost 宛ての Cookie のうち、最も具体的な host_key のものを返す。
func pickCookie(cookies []chromecookie.Cookie, reqHost string) (string, bool) {
	best := ""
	bestRank := -1
	for _, c := range cookies {
		if !chromecookie.HostMatches(c.Host, reqHost) || c.Value == "" {
			continue
		}
		rank := len(strings.TrimPrefix(c.Host, "."))
		if !strings.HasPrefix(c.Host, ".") {
			rank += 1000 // host-only を最優先
		}
		if rank > bestRank {
			best, bestRank = c.Value, rank
		}
	}
	return best, bestRank >= 0
}

// SlackDCookie は指定プロファイルから Slack の d cookie（xoxd-…）を取り出す。
//
// 取り出すのは name="d" だけ。全 Cookie を載せる Cookie ヘッダは作らない
// （Slack の内部 API に必要なのは d だけで、他を送るのは露出面を広げるだけ）。
func SlackDCookie(profile, host string) (string, error) {
	password, err := chromecookie.KeychainPassword()
	if err != nil {
		return "", err
	}
	return readDCookie(profile, host, password)
}

// readDCookie は SlackDCookie の Keychain 以降（テストが実物の Keychain に触れずに通す）。
func readDCookie(profile, host string, password []byte) (string, error) {
	res, err := ws.ReadCookies(profile, password, slackCookieName)
	if err != nil {
		if chromecookie.IsMissing(err) {
			return "", fmt.Errorf("%w\n%s", err, profileHint)
		}
		return "", err
	}
	v, ok := pickCookie(res.Cookies, host)
	if !ok {
		return "", dCookieNotFound(profile, host, res)
	}
	return v, nil
}

// dCookieNotFound は d cookie が見つからなかったときのエラーを、原因の手がかりに応じて作る。
//
// 🚨 「ログインしているか確認して」は最後の選択肢。復号が全件失敗した（鍵違い）/
// 読めなかったファイルがある、のに「ログインして」と案内すると、原因にたどり着けない。
func dCookieNotFound(profile, host string, res chromecookie.Result) error {
	if err := res.Diagnose(fmt.Sprintf("%s 宛ての %q cookie ", host, slackCookieName)); err != nil {
		return err
	}
	return fmt.Errorf(
		"%s 宛ての %q cookie がプロファイル %q にありません。\n"+
			"  %s で https://%s にログインしているか確認してください。",
		host, slackCookieName, profile, ChromeName, host)
}

// Mask は資格情報を表示用に短縮する。生値を絶対に返さない。
//
// 🚨 表示・ログ・エラーメッセージで資格情報に触れるときは必ずここを通す。
func Mask(secret string) string {
	if secret == "" {
		return "(なし)"
	}
	// プレフィックス（xoxc- / xoxd-）までは判別に必要なので残し、以降は伏せる。
	prefix := ""
	if i := strings.IndexByte(secret, '-'); i >= 0 && i <= 5 {
		prefix = secret[:i+1]
	}
	rest := strings.TrimPrefix(secret, prefix)
	head := rest
	if len(head) > 4 {
		head = head[:4]
	}
	return fmt.Sprintf("%s%s…(%d 文字)", prefix, head, len(secret))
}
