package auth

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isReadKind は err が kind の ReadError かを返す。
func isReadKind(err error, kind ReadErrorKind) bool {
	var re *ReadError
	return errors.As(err, &re) && re.Kind == kind
}

// chromeProfileForTest は隔離した HOME の下に Chrome のプロファイルディレクトリを作って返す。
func chromeProfileForTest(t *testing.T, home, profile string) string {
	t.Helper()
	dir := filepath.Join(home, "Library", "Application Support", chromeSupportSubdir, profile)
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

// 🚨 アクセス拒否を「見つからない」に化けさせない。かつ EnvError（探索全体を止める）にもしない。
//
// 拒否はプロファイル単位でも起きる（1 プロファイルだけ chmod 000 / sudo で起動した Chrome が
// root 所有にした）。EnvError にすると後ろの正常なプロファイルが使えなくなる（実際に退行させた）。
func TestPermissionDeniedIsReadDeniedNotMissing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	profileDir := chromeProfileForTest(t, home, "Default")
	if err := os.WriteFile(filepath.Join(profileDir, "Cookies"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	lockForTest(t, profileDir)

	for name, f := range map[string]func() error{
		"Cookie DB":     func() error { _, err := cookieDBSourcePath("Default"); return err },
		"Local Storage": func() error { _, err := localStorageDir("Default"); return err },
	} {
		err := f()
		if !isReadKind(err, ReadDenied) {
			t.Errorf("%s: アクセス拒否として分類されていない: %v", name, err)
		}
		if IsEnvError(err) {
			t.Errorf("%s: プロファイル単位の拒否を EnvError にしている（探索全体が止まる）: %v", name, err)
		}
	}
}

// コピー段（存在は確認できたが読めない）のアクセス拒否も ReadDenied にすること。
func TestCopyPermissionDeniedIsReadDenied(t *testing.T) {
	isolateTemp(t) // 作業領域（コピー先）を隔離する
	src := t.TempDir()

	cookies := filepath.Join(src, "Cookies")
	if err := os.WriteFile(cookies, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	lockForTest(t, cookies)
	leveldb := filepath.Join(src, "leveldb")
	if err := os.Mkdir(leveldb, 0o700); err != nil {
		t.Fatal(err)
	}
	lockForTest(t, leveldb)

	if _, _, _, err := copyCookieDB(cookies); !isReadKind(err, ReadDenied) || IsEnvError(err) {
		t.Errorf("Cookie DB: ReadDenied であるべき: %v", err)
	}
	if _, _, _, err := copyLevelDB(leveldb); !isReadKind(err, ReadDenied) || IsEnvError(err) {
		t.Errorf("Local Storage: ReadDenied であるべき: %v", err)
	}
	// 失敗経路でも一時コピーが残らないこと。
	entries, err := os.ReadDir(tempRoot())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("失敗経路で一時ディレクトリが残った: %d 件", len(entries))
	}
}

// 🚨 -wal / -shm の読み取り失敗を黙って捨てないこと（ENOENT は正常なので記録しない）。
// WAL にだけあるセッション cookie を取りこぼすと「ログインしていない」に化ける。
func TestUnreadableWALIsRecorded(t *testing.T) {
	isolateTemp(t)
	src := t.TempDir()
	cookies := filepath.Join(src, "Cookies")
	for _, p := range []string{cookies, cookies + "-wal"} { // -shm は作らない（ENOENT）
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	lockForTest(t, cookies+"-wal")

	_, cleanup, skipped, err := copyCookieDB(cookies)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if len(skipped) != 1 || !strings.Contains(skipped[0], "Cookies-wal") {
		t.Errorf("読めなかった -wal だけを記録すべき（存在しない -shm は記録しない）: %q", skipped)
	}
}

// leveldb の個別ファイルの読み取り失敗を黙って捨てず、目的のもの（トークン / 痕跡）が
// 見つからなかったときに ReadIncomplete として理由を添えること。見つかったなら従来どおり成功。
func TestUnreadableLevelDBFilesAreReported(t *testing.T) {
	isolateTemp(t) // HOME を差し替える（作業領域もこの下）
	home := os.Getenv("HOME")
	profileDir := chromeProfileForTest(t, home, "Default")
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

// 🚨 d cookie を 1 件以上試してすべて復号に失敗したら、「cookie が無い／ログインを確認」に
// 化けさせず DecryptFailed にすること（Keychain の鍵違いなど）。1 件だけの失敗は従来どおり続行。
func TestDecryptFailuresAreReported(t *testing.T) {
	key := testKey(t)
	wrong := make([]byte, len(key))
	copy(wrong, key)
	wrong[0] ^= 0xff
	good := rawCookie{host: ".slack.com", name: "d", enc: encryptForTest(t, key, "v10", "xoxd-GOOD", "")}
	bad := rawCookie{host: ".other.example", name: "d", enc: encryptForTest(t, wrong, "v10", "xoxd-BAD", "")}

	out, st := decryptCookies([]rawCookie{good, bad}, key, 0)
	if len(out) != 1 || st.allFailed() {
		t.Errorf("1 件だけの失敗で全体を失敗扱いにしている: out=%d stats=%+v", len(out), st)
	}
	third := make([]byte, len(key))
	copy(third, key)
	third[1] ^= 0xff
	_, st = decryptCookies([]rawCookie{good, bad}, third, 0) // どちらの鍵とも違う = 全件失敗
	if !st.allFailed() {
		t.Fatalf("全件失敗を検出していない: %+v", st)
	}
	if err := dCookieNotFound("Default", "acme.slack.com", st, nil); !isReadKind(err, DecryptFailed) {
		t.Errorf("全件の復号失敗が「cookie が無い」に化けた: %v", err)
	}
	// 平文だけ（試していない）は復号失敗ではない。
	_, st = decryptCookies([]rawCookie{{host: ".slack.com", name: "d", plain: "xoxd-PLAIN"}}, wrong, 0)
	if st.allFailed() {
		t.Errorf("復号を試していないのに全件失敗としている: %+v", st)
	}
	// 読めなかったファイルがあれば ReadIncomplete、何も無ければ従来の「ありません」。
	if err := dCookieNotFound("Default", "acme.slack.com", decryptStats{}, skippedReads{"open Cookies-wal: permission denied"}); !isReadKind(err, ReadIncomplete) {
		t.Errorf("読めなかった -wal を案内に添えていない: %v", err)
	}
	if err := dCookieNotFound("Default", "acme.slack.com", decryptStats{}, nil); err == nil || !strings.Contains(err.Error(), "ありません") {
		t.Errorf("手がかりが無いときは従来の案内であるべき: %v", err)
	}
}

// 全滅時の案内に、各プロファイルの問題と種類ごとの対処が載ること。
func TestIssueNoteListsProfilesAndHints(t *testing.T) {
	note := IssueNote([]ProfileIssue{
		{Profile: "Profile 1", Err: &ReadError{Kind: ReadDenied, Msg: "拒否"}},
		{Profile: "Profile 2", Err: &ReadError{Kind: DecryptFailed, Msg: "復号"}},
	})
	for _, want := range []string{"Profile 1", "Profile 2", "フルディスクアクセス", "パーミッション／所有者", "鍵が合っていない"} {
		if !strings.Contains(note, want) {
			t.Errorf("案内に %q が無い: %s", want, note)
		}
	}
	if IssueNote(nil) != "" {
		t.Error("記録が無いときは空であるべき")
	}
}

// 🚨 meta v24 以上では、鍵違いを「先頭 32 バイト = SHA256(host_key)」の照合で確実に検出すること。
//
// PKCS7 の照合だけだと、鍵違いでも末尾が約 1/256 で偶然通る。v24 の値は長い（実物の d cookie は
// 数百バイト）ので 32 バイト落としても何か残り、「復号できた」ことになる。件数が多いと
// 全件失敗の検出（allFailed）が働かない。正しい鍵なら全件が元の値に戻ることも確かめる
// （照合を誤ると正常な cookie が全部失敗になるため、両方向を見る）。
func TestV24HashPrefixDetectsWrongKeyAtScale(t *testing.T) {
	key := testKey(t)
	wrong := make([]byte, len(key))
	copy(wrong, key)
	wrong[0] ^= 0xff

	const n = 2000
	// 🚨 値は 1 件ずつ変える。IV が固定なので、同じ値・同じホストだと暗号文が同一になり、
	// 「偶然通る 1/256」が全件同時に起きるか起きないかの 1 回の試行になってしまう。
	long := func(i int) string { return fmt.Sprintf("xoxd-%06d-%s", i, strings.Repeat("A1b2C3d4", 40)) } // 実物に近い長さ
	rows := make([]rawCookie, 0, n)
	for i := 0; i < n; i++ {
		host := []string{".slack.com", "acme.slack.com", ".example.com"}[i%3]
		rows = append(rows, rawCookie{host: host, name: "d", enc: encryptForTest(t, key, "v10", long(i), host)})
	}

	out, st := decryptCookies(rows, key, 24)
	if len(out) != n || st.failed != 0 {
		t.Fatalf("正しい鍵なのに復号に失敗した（照合の入力を取り違えている）: ok=%d failed=%d", len(out), st.failed)
	}
	for i, c := range out {
		if c.value != long(i) {
			t.Fatalf("%d 件目の復号結果が元の値と違う: len=%d", i, len(c.value))
		}
	}

	out, st = decryptCookies(rows, wrong, 24)
	if !st.allFailed() {
		t.Errorf("鍵違いを %d 件中 %d 件しか検出していない（偶然通ったゴミを復号結果にしている）", st.tried, st.failed)
	}
	if len(out) != 0 {
		t.Errorf("鍵違いのゴミを cookie として返している: %d 件", len(out))
	}

	// 別のホスト宛てに暗号化された値（先頭のハッシュが host_key と合わない）も通さない。
	swapped := rawCookie{host: ".slack.com", name: "d", enc: encryptForTest(t, key, "v10", long(0), ".evil.example")}
	if _, st := decryptCookies([]rawCookie{swapped}, key, 24); !st.allFailed() {
		t.Error("host_key のハッシュが一致しない値を通した")
	}
}
