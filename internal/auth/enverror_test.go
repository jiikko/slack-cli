package auth

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jiikko/dotfiles/src/chromecookie"
)

// Cookie DB の読み取りの分類は chromecookie 側でテストしている。ここは slack-cli が
// 自分で読む Local Storage(leveldb) と、d cookie が見つからなかったときの案内だけ。

// isReadKind は err が kind の ReadError かを返す。
func isReadKind(err error, kind ReadErrorKind) bool {
	var re *ReadError
	return errors.As(err, &re) && re.Kind == kind
}

// isolateHome は HOME を一時ディレクトリへ向ける（作業領域 ~/Library/Caches/slack-cli もこの下になる）。
func isolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", "") // Linux の os.UserCacheDir を HOME 配下へ向ける
	return home
}

// chromeProfileForTest は隔離した HOME の下に Chrome のプロファイルディレクトリを作って返す。
func chromeProfileForTest(t *testing.T, profile string) string {
	t.Helper()
	dir, err := chromecookie.ProfileDir(profile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "Local Storage", "leveldb"), 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// lockForTest は path のパーミッションを 0 にし、テスト後に戻す。
func lockForTest(t *testing.T, path string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root では権限拒否を作れない")
	}
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o700) })
}

// 🚨 Local Storage のアクセス拒否を「見つからない」に化けさせない。かつ EnvError（探索全体を止める）にもしない。
// 拒否はプロファイル単位でも起きる（1 プロファイルだけ chmod 000 / sudo で起動した Chrome が root 所有にした）。
func TestLocalStoragePermissionDeniedIsReadDenied(t *testing.T) {
	isolateHome(t)
	profileDir := chromeProfileForTest(t, "Default")
	lockForTest(t, profileDir)
	_, err := localStorageDir("Default")
	if !isReadKind(err, ReadDenied) || IsEnvError(err) {
		t.Errorf("ReadDenied であるべき: %T %v", err, err)
	}

	src := t.TempDir()
	leveldb := filepath.Join(src, "leveldb")
	if err := os.Mkdir(leveldb, 0o700); err != nil {
		t.Fatal(err)
	}
	lockForTest(t, leveldb)
	if _, _, _, err := copyLevelDB(leveldb); !isReadKind(err, ReadDenied) || IsEnvError(err) {
		t.Errorf("コピー段: ReadDenied であるべき: %v", err)
	}
	// 失敗経路でも一時コピーが残らないこと。
	caches, err := os.UserCacheDir() // darwin は ~/Library/Caches、Linux は ~/.cache
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(caches, "slack-cli", "extract")
	if entries, err := os.ReadDir(root); err == nil && len(entries) != 0 {
		t.Errorf("失敗経路で一時ディレクトリが残った: %d 件", len(entries))
	}
}

// leveldb の個別ファイルの読み取り失敗を黙って捨てず、目的のもの（トークン / 痕跡）が
// 見つからなかったときに ReadIncomplete として理由を添えること。見つかったなら従来どおり成功。
func TestUnreadableLevelDBFilesAreReported(t *testing.T) {
	isolateHome(t)
	profileDir := chromeProfileForTest(t, "Default")
	ldb := filepath.Join(profileDir, "Local Storage", "leveldb")
	locked := filepath.Join(ldb, "000005.ldb")
	if err := os.WriteFile(locked, []byte("xoxc-1234567890-abcdef acme.slack.com"), 0o600); err != nil {
		t.Fatal(err)
	}
	lockForTest(t, locked)

	if _, err := ExtractTokens("Default", "acme"); !isReadKind(err, ReadIncomplete) {
		t.Errorf("ExtractTokens: 読めないファイルがあるのに理由を添えていない: %v", err)
	}
	if _, err := DiscoverWorkspaces("Default"); !isReadKind(err, ReadIncomplete) {
		t.Errorf("DiscoverWorkspaces: 読めないファイルがあるのに理由を添えていない: %v", err)
	}

	// 別のファイルから取れたなら、1 ファイル読めないだけでは失敗にしない。
	if err := os.WriteFile(filepath.Join(ldb, "000006.log"), []byte("xoxc-0987654321-fedcba acme.slack.com"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := ExtractTokens("Default", "acme"); err != nil || len(got) != 1 {
		t.Errorf("取れたトークンがあるのに失敗した: %v %v", got, err)
	}
	if got, err := DiscoverWorkspaces("Default"); err != nil || len(got) != 1 {
		t.Errorf("取れた痕跡があるのに失敗した: %v %v", got, err)
	}
}

// d cookie が見つからず手がかりも無いときは「ログインしているか確認して」の案内、
// Cookie DB が無いときは slack-cli のフラグ名を添えること。
func TestDCookieNotFoundGuidance(t *testing.T) {
	if err := dCookieNotFound("Default", "acme.slack.com", chromecookie.Result{}); err == nil ||
		!strings.Contains(err.Error(), "ありません") || !strings.Contains(err.Error(), "ログインしているか") {
		t.Errorf("手がかりが無いときはログインの案内であるべき: %v", err)
	}

	isolateHome(t)
	// 🚨 SlackDCookie は呼ばない（本物の Keychain を読み、手元では許可ダイアログが出る）。
	_, err := readDCookie("NoSuch", "acme.slack.com", []byte("testpassword"))
	if !chromecookie.IsMissing(err) || !strings.Contains(err.Error(), "SLACK_CLI_CHROME_PROFILE") {
		t.Errorf("DB 無しに slack-cli のフラグ名を添えていない: %v", err)
	}
}

// 🚨 d cookie が見つからなかったとき、chromecookie が見つけた原因（鍵違いで全件復号に失敗）を
// 「ログインしているか確認して」に化けさせないこと。実物の sqlite の Cookie DB を HOME の下に作る。
func TestDCookieNotFoundKeepsDiagnosis(t *testing.T) {
	isolateHome(t)
	dir := filepath.Join(chromeProfileForTest(t, "Default"), "Network")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "Cookies"))
	if err != nil {
		t.Fatal(err)
	}
	// v10 + 16 バイトの暗号文。どの鍵でも PKCS7 が通らない（または値が壊れる）ので「復号を試して失敗」になる。
	enc := append([]byte("v10"), make([]byte, 16)...)
	for _, q := range []string{
		`CREATE TABLE meta (key TEXT, value TEXT)`,
		`INSERT INTO meta VALUES ('version', '24')`,
		`CREATE TABLE cookies (host_key TEXT, name TEXT, value TEXT, encrypted_value BLOB)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO cookies VALUES ('.slack.com', 'd', '', ?)`, enc); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	_, err = readDCookie("Default", "acme.slack.com", []byte("testpassword"))
	if !isReadKind(err, DecryptFailed) {
		t.Errorf("全件の復号失敗が「cookie が無い」に化けた: %T %v", err, err)
	}
}
