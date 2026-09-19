// Package auth は Chrome にログイン済みの Slack セッションから、API 呼び出しに必要な
// 資格情報（d cookie / xoxc トークン）を取り出す。
//
// 🚨 ここで扱う値はいずれも「なりすましログインが可能な資格情報」。
// ファイル・ログ・標準出力へ生値を出さないこと（表示は Mask を通す）。
// 一時コピーの後始末は cleanup.go の 3 段構えに必ず載せる。
package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/sha1"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// 🚨 このツールは Google Chrome 専用。
//
// esa-cli と同じ方針（issue 003）。対応表の値（Keychain のサービス名・Application
// Support 配下のディレクトリ名）は実機で確認しないと正しいか分からないため、
// 手元で確認できる Chrome だけに絞る。未確認の値を並べると「動くように見えて
// 別ブラウザの領域を読みに行く」形の事故になる。
const (
	chromeKeychainAccount = "Chrome"              // security -a
	chromeKeychainService = "Chrome Safe Storage" // security -s
	chromeSupportSubdir   = "Google/Chrome"       // ~/Library/Application Support 配下
	ChromeName            = "Google Chrome"       // エラーメッセージ用の表示名
)

// slackCookieName は Slack のセッション Cookie の名前。
const slackCookieName = "d"

// cookieEntry は復号済みの 1 Cookie。
type cookieEntry struct {
	host  string // host_key（先頭ドットを含む場合がある）
	name  string
	value string
}

// deriveKey は PBKDF2-SHA1（macOS: 1003 回, 16 バイト）で AES-128 鍵を導出する。
func deriveKey(password []byte) ([]byte, error) {
	return pbkdf2.Key(sha1.New, string(password), []byte("saltysalt"), 1003, 16)
}

// decryptValue は encrypted_value を復号する。
// v10 / v11 プレフィックスなら AES-128-CBC（IV=0x20*16, PKCS7）で復号し、
// metaVersion>=24 なら復号後の先頭 32 バイト（ホスト名の SHA256 ハッシュ）を落とす。
//
// 🚨 v11 を「プレフィックス無し = 平文」として素通しさせないこと。素通しすると
// 復号されないバイト列がそのまま Cookie ヘッダへ載り、原因の分からない 401 になる。
func decryptValue(enc, key []byte, metaVersion int) (string, error) {
	if len(enc) == 0 {
		return "", nil
	}
	if len(enc) < 3 {
		return string(enc), nil
	}
	switch string(enc[:3]) {
	case "v10", "v11":
		// 復号へ進む
	default:
		// 古い Chrome の平文データ
		return string(enc), nil
	}
	ciphertext := enc[3:]
	if len(ciphertext) == 0 || len(ciphertext)%aes.BlockSize != 0 {
		return "", errors.New("暗号文の長さが不正です")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	iv := []byte("                ") // 0x20 * 16
	mode := cipher.NewCBCDecrypter(block, iv)
	plain := make([]byte, len(ciphertext))
	mode.CryptBlocks(plain, ciphertext)

	plain, err = pkcs7Unpad(plain, aes.BlockSize)
	if err != nil {
		return "", err
	}
	if metaVersion >= 24 {
		if len(plain) < 32 {
			return "", errors.New("復号結果がハッシュプレフィックスより短いです")
		}
		plain = plain[32:]
	}
	return string(plain), nil
}

func pkcs7Unpad(data []byte, blockSize int) ([]byte, error) {
	if len(data) == 0 || len(data)%blockSize != 0 {
		return nil, errors.New("PKCS7: データ長が不正です")
	}
	pad := int(data[len(data)-1])
	if pad == 0 || pad > blockSize || pad > len(data) {
		return nil, errors.New("PKCS7: パディングが不正です")
	}
	// 🚨 最終バイトだけでなく、パディング全体が同じ値であることを確かめる。
	// 最終バイトしか見ない実装は 1,2,3,4 のような不正なパディングを通し、
	// 復号結果の末尾にゴミが残った Cookie 値をそのまま送ることになる。
	for _, b := range data[len(data)-pad:] {
		if int(b) != pad {
			return nil, errors.New("PKCS7: パディングバイトが揃っていません")
		}
	}
	return data[:len(data)-pad], nil
}

// chromeProfileDir は ~/Library/Application Support/Google/Chrome/<profile> を返す。
func chromeProfileDir(profile string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Application Support", chromeSupportSubdir, profile), nil
}

// cookieDBSourcePath は Cookie DB の絶対パスを返す（cwd 非依存: HOME 起点）。
func cookieDBSourcePath(profile string) (string, error) {
	base, err := chromeProfileDir(profile)
	if err != nil {
		return "", err
	}
	// 新しい Chrome は Cookies を Network/ サブディレクトリに置く。両方を候補にする。
	candidates := []string{
		filepath.Join(base, "Network", "Cookies"),
		filepath.Join(base, "Cookies"),
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf(
		"Cookie DB が見つかりませんでした（プロファイル=%q）。探した場所:\n  %s\n"+
			"  - プロファイル名が正しいか確認してください（-profile / SLACK_CLI_CHROME_PROFILE）。\n"+
			"  - ~/Library/Application Support/%s/ 配下のディレクトリ名がプロファイル名です（既定は Default）。",
		profile, strings.Join(candidates, "\n  "), chromeSupportSubdir)
}

// copyCookieDB は Cookie DB を一時ディレクトリへコピーする。
// WAL に未反映のセッション Cookie を取りこぼさないよう、-wal / -shm も同名でコピーする。
// 返り値: 一時 DB パスと後始末関数（① の defer で必ず呼ぶ）。
func copyCookieDB(src string) (string, func(), error) {
	tmpdir, cleanup, err := newTempDir()
	if err != nil {
		return "", nil, err
	}

	for _, suffix := range []string{"", "-wal", "-shm"} {
		s := src + suffix
		data, err := os.ReadFile(s)
		if err != nil {
			if suffix == "" {
				cleanup()
				if os.IsPermission(err) {
					return "", nil, fmt.Errorf(
						"Cookie DB を読み取れませんでした（アクセス拒否）。\n"+
							"  ターミナル（またはこのツールを起動しているアプリ）に「フルディスクアクセス」を付与してください:\n"+
							"    システム設定 → プライバシーとセキュリティ → フルディスクアクセス\n"+
							"  対象ファイル: %s", src)
				}
				return "", nil, fmt.Errorf("Cookie DB の読み取りに失敗: %w", err)
			}
			continue // -wal / -shm は存在しないこともある
		}
		dst := filepath.Join(tmpdir, "Cookies"+suffix)
		if err := os.WriteFile(dst, data, 0o600); err != nil {
			cleanup()
			return "", nil, err
		}
	}
	return filepath.Join(tmpdir, "Cookies"), cleanup, nil
}

// extractCookies は指定プロファイル（Chrome）から name の Cookie を復号して返す。
// name が空なら全件。Slack の用途では "d" だけを取る（露出面を最小にする）。
func extractCookies(profile, name string) ([]cookieEntry, error) {
	password, err := getKeychainPassword()
	if err != nil {
		return nil, err
	}
	key, err := deriveKey(password)
	if err != nil {
		return nil, err
	}

	src, err := cookieDBSourcePath(profile)
	if err != nil {
		return nil, err
	}
	dbPath, cleanup, err := copyCookieDB(src)
	if err != nil {
		return nil, err
	}
	defer cleanup() // ①: 正常終了・エラー・panic を覆う

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	var metaVersion int
	if err := db.QueryRow(`SELECT value FROM meta WHERE key = 'version'`).Scan(&metaVersion); err != nil {
		// meta が読めない場合は 0 扱い（トリム無し）で続行
		metaVersion = 0
	}

	query := `SELECT host_key, name, value, encrypted_value FROM cookies`
	var args []any
	if name != "" {
		query += ` WHERE name = ?`
		args = append(args, name)
	}
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("cookies テーブルの読み取りに失敗: %w", err)
	}
	defer rows.Close()

	var out []cookieEntry
	for rows.Next() {
		var host, cname, plainValue string
		var enc []byte
		if err := rows.Scan(&host, &cname, &plainValue, &enc); err != nil {
			return nil, err
		}
		value := plainValue
		if value == "" && len(enc) > 0 {
			v, derr := decryptValue(enc, key, metaVersion)
			if derr != nil {
				continue // 1 件の復号失敗で全体を止めない
			}
			value = v
		}
		out = append(out, cookieEntry{host: host, name: cname, value: value})
	}
	return out, rows.Err()
}

// cookieHostMatches は Cookie の host_key が対象ホストに送信されるべきか判定する。
// 標準の Cookie ドメインマッチ（ドット境界）を用い、suffix 文字列比較の誤爆を避ける。
//
// 🚨 SQL の LIKE '%slack.com' で絞らないこと。"notslack.com" / "evilslack.com" が
// 一致してしまい、無関係なサイトの Cookie を Slack へ送る経路になる。
func cookieHostMatches(hostKey, reqHost string) bool {
	if hostKey == "" {
		return false
	}
	if strings.HasPrefix(hostKey, ".") {
		d := hostKey[1:] // domain cookie
		return reqHost == d || strings.HasSuffix(reqHost, "."+d)
	}
	return hostKey == reqHost // host-only cookie は完全一致のみ
}

// pickCookie は reqHost 宛ての Cookie のうち、最も具体的な host_key のものを返す。
func pickCookie(cookies []cookieEntry, reqHost string) (string, bool) {
	best := ""
	bestRank := -1
	for _, c := range cookies {
		if !cookieHostMatches(c.host, reqHost) || c.value == "" {
			continue
		}
		rank := len(strings.TrimPrefix(c.host, "."))
		if !strings.HasPrefix(c.host, ".") {
			rank += 1000 // host-only を最優先
		}
		if rank > bestRank {
			best, bestRank = c.value, rank
		}
	}
	return best, bestRank >= 0
}

// SlackDCookie は指定プロファイルから Slack の d cookie（xoxd-…）を取り出す。
//
// 取り出すのは name="d" だけ。全 Cookie を載せる Cookie ヘッダは作らない
// （Slack の内部 API に必要なのは d だけで、他を送るのは露出面を広げるだけ）。
func SlackDCookie(profile, host string) (string, error) {
	cookies, err := extractCookies(profile, slackCookieName)
	if err != nil {
		return "", err
	}
	v, ok := pickCookie(cookies, host)
	if !ok {
		return "", fmt.Errorf(
			"%s 宛ての %q cookie がプロファイル %q にありません。\n"+
				"  %s で https://%s にログインしているか確認してください。",
			host, slackCookieName, profile, ChromeName, host)
	}
	return v, nil
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
