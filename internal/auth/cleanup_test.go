package auth

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// isolateTemp は作業領域をテスト専用にする（本物の ~/Library/Caches を触らない）。
// 作業領域は HOME 基準（os.UserCacheDir）なので、HOME を差し替えれば隔離できる。
// 戻り値は作業領域の親（= tempRoot の 1 つ上）。
func isolateTemp(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	cleanupMu.Lock()
	cleanupPaths = map[string]struct{}{}
	cleanupMu.Unlock()

	parent, err := tempRootParent()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	return parent
}

// deadPID は確実に終了済みのプロセス ID を返す。
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("/usr/bin/true")
	if err := cmd.Run(); err != nil {
		t.Skipf("/usr/bin/true を実行できない: %v", err)
	}
	pid := cmd.Process.Pid
	if processAlive(pid) {
		t.Skipf("pid %d がまだ生きている（再利用の可能性）", pid)
	}
	return pid
}

// 一時ディレクトリは①（defer）でも②（シグナル経路の RunAllCleanups）でも消えること。
// 成功パスのあとに残骸が 0 件であることまで見る。
func TestTempDirIsRemovedByBothPaths(t *testing.T) {
	isolateTemp(t)

	// ①: cleanup 関数で消える
	dir, cleanup, err := newTempDir()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("作られていない: %v", err)
	}
	if !strings.HasPrefix(filepath.Base(dir), strconv.Itoa(os.Getpid())+"-") {
		t.Errorf("ディレクトリ名に pid が入っていない（③の掃除が効かなくなる）: %s", filepath.Base(dir))
	}
	cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("①で消えていない: %v", err)
	}

	// ②: RunAllCleanups（シグナル経路）で消える
	dir2, _, err := newTempDir()
	if err != nil {
		t.Fatal(err)
	}
	RunAllCleanups()
	if _, err := os.Stat(dir2); !os.IsNotExist(err) {
		t.Errorf("②で消えていない: %v", err)
	}

	// 残骸ゼロ（成功パスの後に一時領域が空であること）
	entries, err := os.ReadDir(tempRoot())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("一時領域に残骸がある: %d 件", len(entries))
	}
}

// ③: 起動時の掃除は「自分が作った親の直下 / <pid>- 形式 / その pid が死んでいる」の
// 3 条件を満たすものだけを消すこと。1 つでも欠けたら触らない。
func TestSweepRemovesOnlyDeadOwnDirs(t *testing.T) {
	isolateTemp(t)
	root, err := ensureTempRoot()
	if err != nil {
		t.Fatal(err)
	}

	dead := filepath.Join(root, strconv.Itoa(deadPID(t))+"-abc")
	mine := filepath.Join(root, strconv.Itoa(os.Getpid())+"-abc")
	alive := filepath.Join(root, "1-abc") // pid 1 (launchd) は生きている
	weird := filepath.Join(root, "notapid-abc")
	// 🚨 「死んだ pid」の名前にすること。適当な数字（1234 等）だと、その pid が
	// たまたま生きている機械では processAlive に守られてしまい、
	// 「ディレクトリ以外は対象外」の検査が効いているかを確かめられない。
	plainFile := filepath.Join(root, strconv.Itoa(deadPID(t))+"-file")
	for _, d := range []string{dead, mine, alive, weird} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(plainFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	SweepStaleTempDirs()

	if _, err := os.Stat(dead); !os.IsNotExist(err) {
		t.Errorf("死んだプロセスの残骸が消えていない: %v", err)
	}
	for _, keep := range []string{mine, alive, weird, plainFile} {
		if _, err := os.Stat(keep); err != nil {
			t.Errorf("消してはいけないものを消した: %s (%v)", keep, err)
		}
	}
}

// 一時領域の名前は他ツール（esa-cli の esa-cookie 等）と共有しないこと。
//
// 🚨 共有すると③の掃除の母集合に別ツールの実行中ディレクトリが入る。
func TestTempRootIsToolSpecific(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := tempRoot()
	if !strings.Contains(root, "slack") {
		t.Errorf("このツール専用だと分かるパスにすべき: %q", root)
	}
	if strings.Contains(root, "esa") {
		t.Errorf("他ツールと同じ名前を使っている: %q", root)
	}
	// 🚨 $TMPDIR 配下に置かないこと（呼び出し側が場所を動かせるうえ、
	// 未設定時の /tmp は誰でも書けるので別ユーザーに居座られる）。
	if !strings.HasPrefix(root, home) {
		t.Errorf("HOME 基準の固定パスにすべき: %q (HOME=%q)", root, home)
	}
	t.Setenv("TMPDIR", filepath.Join(home, "elsewhere"))
	if got := tempRoot(); got != root {
		t.Errorf("TMPDIR で作業領域が動いている: %q -> %q", root, got)
	}
}

// 🚨 ③（破壊的な掃除）が、検証を通らない一時領域に一切触れないこと。
//
// 発火条件: $TMPDIR/slack-cli-extract を別ディレクトリへのシンボリックリンクにし、
// その先に「死んだ pid の名前」のディレクトリを置く。以前の実装は検証（ensureTempRoot）を
// 通らず os.ReadDir → os.RemoveAll していたため、**リンク先を再帰削除した**。
func TestSweepRefusesUnverifiedRoot(t *testing.T) {
	parent := isolateTemp(t)

	victim := filepath.Join(parent, "victim")
	stale := filepath.Join(victim, strconv.Itoa(deadPID(t))+"-abc")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	important := filepath.Join(stale, "important.txt")
	if err := os.WriteFile(important, []byte("消えてはいけない"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, tempRoot()); err != nil {
		t.Skipf("シンボリックリンクを作れない: %v", err)
	}

	SweepStaleTempDirs()

	if _, err := os.Stat(important); err != nil {
		t.Errorf("作業領域の外のファイルが削除された: %v", err)
	}
	if _, err := os.Stat(stale); err != nil {
		t.Errorf("作業領域の外のディレクトリが削除された: %v", err)
	}
}

// 🚨 **相対**シンボリックリンクでも拒否すること。
//
// os.Root が拒否するのは「ルートの外へ出る」リンクだけで、**ルート内に留まる相対リンクは追う**
// （実測で確認）。つまり絶対リンクだけを試すテストは、検証本体を一度も通らずに緑になる。
// 1 周目のテストがまさにその形で、検証を全部消しても緑のままだった。
func TestSweepRefusesRelativeSymlinkRoot(t *testing.T) {
	parent := isolateTemp(t)

	victim := filepath.Join(parent, "victim")
	stale := filepath.Join(victim, strconv.Itoa(deadPID(t))+"-abc")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	important := filepath.Join(stale, "important.txt")
	if err := os.WriteFile(important, []byte("消えてはいけない"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 相対リンク（親ディレクトリの中に留まるので os.Root は追ってしまう）
	if err := os.Symlink("victim", tempRoot()); err != nil {
		t.Skipf("シンボリックリンクを作れない: %v", err)
	}

	SweepStaleTempDirs()

	if _, err := os.Stat(important); err != nil {
		t.Errorf("相対シンボリックリンクの先が削除された: %v", err)
	}
}

// 🚨 ①（newTempDir が返す cleanup。defer で呼ぶ）も③と同じ検証を通ること。
//
// ①の窓は「一時ディレクトリを作ってから defer が走るまで」＝作業中ずっとなので、
// ②③より構造的に広い。ここだけパス文字列の os.RemoveAll に戻すと穴が復活する。
func TestDeferCleanupRefusesUnverifiedRoot(t *testing.T) {
	parent := isolateTemp(t)

	dir, cleanup, err := newTempDir()
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Base(dir)

	victim := filepath.Join(parent, "victim")
	if err := os.MkdirAll(filepath.Join(victim, name), 0o700); err != nil {
		t.Fatal(err)
	}
	important := filepath.Join(victim, name, "important.txt")
	if err := os.WriteFile(important, []byte("消えてはいけない"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(tempRoot()); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, tempRoot()); err != nil {
		t.Skipf("シンボリックリンクを作れない: %v", err)
	}

	cleanup()

	if _, err := os.Stat(important); err != nil {
		t.Errorf("①がシンボリックリンクの先を削除した: %v", err)
	}
}

// 🚨 検証は最後の 1 コンポーネントだけでは足りない。
// 途中のディレクトリ（~/Library/Caches/slack-cli）を差し替えれば、
// 検証済みの root ごと任意の場所へ移せる。
func TestParentComponentIsVerified(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cleanupMu.Lock()
	cleanupPaths = map[string]struct{}{}
	cleanupMu.Unlock()

	caches := filepath.Join(home, "Library", "Caches")
	if err := os.MkdirAll(caches, 0o700); err != nil {
		t.Fatal(err)
	}
	// 攻撃者が用意した先。中身は「掃除の条件に合う」形にしておく。
	attacker := filepath.Join(home, "attacker")
	stale := filepath.Join(attacker, "extract", strconv.Itoa(deadPID(t))+"-abc")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	important := filepath.Join(stale, "important.txt")
	if err := os.WriteFile(important, []byte("消えてはいけない"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 親（slack-cli）自体を symlink にする。
	if err := os.Symlink(attacker, filepath.Join(caches, "slack-cli")); err != nil {
		t.Skipf("シンボリックリンクを作れない: %v", err)
	}

	if _, err := openVerifiedTempRoot(); err == nil {
		t.Error("親がシンボリックリンクでも検証を通ってしまう")
	}

	SweepStaleTempDirs()
	if _, err := os.Stat(important); err != nil {
		t.Errorf("作業領域の外が削除された: %v", err)
	}
}

// 登録経路が壊れて作業領域の外のパスが登録されても、触らないこと。
func TestRunAllCleanupsIgnoresPathsOutsideRoot(t *testing.T) {
	parent := isolateTemp(t)
	if _, err := ensureTempRoot(); err != nil {
		t.Fatal(err)
	}

	outside := filepath.Join(parent, "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(outside, "keep.txt")
	if err := os.WriteFile(keep, []byte("消えてはいけない"), 0o600); err != nil {
		t.Fatal(err)
	}
	registerCleanup(outside)

	RunAllCleanups()

	if _, err := os.Stat(keep); err != nil {
		t.Errorf("作業領域の外に登録されたパスを削除した: %v", err)
	}
}

// 🚨 pid の生死だけで判定しない。番号が再利用されると永久に消えなくなるため、
// 十分に古いものは生存判定に関わらず消すこと。
func TestSweepRemovesStaleEntriesEvenIfPidAlive(t *testing.T) {
	isolateTemp(t)
	root, err := ensureTempRoot()
	if err != nil {
		t.Fatal(err)
	}

	// pid 1 (launchd) は常に生きている = 生存判定だけなら永久に残る名前。
	old := filepath.Join(root, "1-ancient")
	if err := os.MkdirAll(old, 0o700); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-staleAge - time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	// 同じく生きている pid だが、新しいもの（並行実行中かもしれない）。
	fresh := filepath.Join(root, "1-fresh")
	if err := os.MkdirAll(fresh, 0o700); err != nil {
		t.Fatal(err)
	}

	SweepStaleTempDirs()

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("古い残骸が消えていない（pid 再利用で永久に残る）: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("生きている pid の新しいディレクトリを消した: %v", err)
	}
}

// 作業領域のパーミッションは自分だけ（0700）であること。
func TestTempRootPermissions(t *testing.T) {
	isolateTemp(t)
	root, err := ensureTempRoot()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("作業領域のパーミッション: got %o, want 700", perm)
	}
}

// HOME が相対パスだと作業領域が cwd 依存になるので拒否すること。
func TestRelativeHomeIsRejected(t *testing.T) {
	t.Setenv("HOME", "relative/home")
	if _, err := tempRootParent(); err == nil {
		t.Error("相対パスの HOME を受け入れてはいけない（cwd 依存になる）")
	}
}

// 🚨 ①②（defer / シグナル経路）も③と同じ検証を通ること。
//
// ③だけを塞いで①②に os.RemoveAll(パス文字列) を残すと、同じ差し替えで
// リンク先を消せる。しかも①②は「資格情報を読むたび」に走るので窓はむしろ広い。
func TestRunAllCleanupsRefusesUnverifiedRoot(t *testing.T) {
	parent := isolateTemp(t)

	// 正規の手順で作業領域と一時ディレクトリを作り、後始末に登録する。
	dir, _, err := newTempDir()
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Base(dir)

	// 作業領域を victim へ差し替える（同名のディレクトリを用意しておく）。
	victim := filepath.Join(parent, "victim")
	if err := os.MkdirAll(filepath.Join(victim, name), 0o700); err != nil {
		t.Fatal(err)
	}
	important := filepath.Join(victim, name, "important.txt")
	if err := os.WriteFile(important, []byte("消えてはいけない"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(tempRoot()); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, tempRoot()); err != nil {
		t.Skipf("シンボリックリンクを作れない: %v", err)
	}

	RunAllCleanups()

	if _, err := os.Stat(important); err != nil {
		t.Errorf("後始末がシンボリックリンクの先を削除した: %v", err)
	}
}

// エントリ側がシンボリックリンクでも、その先を消さないこと。
func TestSweepIgnoresSymlinkEntries(t *testing.T) {
	parent := isolateTemp(t)
	root, err := ensureTempRoot()
	if err != nil {
		t.Fatal(err)
	}

	victim := filepath.Join(parent, "victim")
	if err := os.MkdirAll(victim, 0o700); err != nil {
		t.Fatal(err)
	}
	important := filepath.Join(victim, "important.txt")
	if err := os.WriteFile(important, []byte("消えてはいけない"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 死んだ pid の名前でリンクを張る（掃除の条件に合致する名前にする）。
	link := filepath.Join(root, strconv.Itoa(deadPID(t))+"-link")
	if err := os.Symlink(victim, link); err != nil {
		t.Skipf("シンボリックリンクを作れない: %v", err)
	}

	SweepStaleTempDirs()

	if _, err := os.Stat(important); err != nil {
		t.Errorf("リンク先のファイルが削除された: %v", err)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Errorf("ディレクトリ以外は対象外のはずが、リンク自身が削除された: %v", err)
	}
}

// 親ディレクトリが自分のものでない（シンボリックリンク等）なら使わないこと。
func TestEnsureTempRootRejectsSymlink(t *testing.T) {
	parent := isolateTemp(t)
	other := filepath.Join(parent, "elsewhere")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, tempRoot()); err != nil {
		t.Skipf("シンボリックリンクを作れない: %v", err)
	}
	if _, err := ensureTempRoot(); err == nil {
		t.Error("シンボリックリンクの一時領域を受け入れてはいけない")
	}
}

// pid の形式判定（③の母集合を決める）。
func TestPidFromTempDirName(t *testing.T) {
	cases := map[string]int{
		"123-abc": 123,
		"1-x":     1,
		"abc-1":   0,
		"-1-x":    0,
		"0-x":     0,
		"123":     0,
		"":        0,
		// 🚨 Atoi は受けるが MkdirTemp が作らない形。掃除の母集合を広げないよう拒否する。
		"+1-x":               0,
		"007-x":              0,
		"0000000000000001-x": 0,
		" 1-x":               0,
	}
	for name, want := range cases {
		got, ok := pidFromTempDirName(name)
		if want == 0 {
			if ok {
				t.Errorf("%q: 対象外にすべき（got %d）", name, got)
			}
			continue
		}
		if !ok || got != want {
			t.Errorf("%q: got %d (ok=%v), want %d", name, got, ok, want)
		}
	}
}

// pid<=0 を kill(2) に渡さないこと（0 はプロセスグループ全体を意味する）。
func TestProcessAliveGuardsNonPositivePID(t *testing.T) {
	// 🚨 {0, -1, -1234} だけだと、ガードが無くても kill(2) が成功して true になるため
	// 検出力がゼロ（しかも -1234 の結果はその機械にプロセスグループ 1234 が在るかに依存する）。
	// 確実に存在しないプロセスグループを混ぜる。
	for _, pid := range []int{0, -1, -1234, -99999} {
		if !processAlive(pid) {
			t.Errorf("pid=%d は「判定不能 = 消さない」に倒すべき", pid)
		}
	}
	if !processAlive(os.Getpid()) {
		t.Error("自分自身は生きている判定になるべき")
	}
}

// 🚨 所有者が自分でない作業領域は使わないこと。
//
// 実環境では他ユーザー所有のディレクトリを作れないため、uid の取得を seam にして検査する。
// seam が無いとこの検査は永久に無検査のまま残る（3 周の敵対的レビューで指摘され続けた）。
func TestForeignOwnerIsRejected(t *testing.T) {
	isolateTemp(t)
	root, err := ensureTempRoot()
	if err != nil {
		t.Fatal(err)
	}
	// 掃除の条件に合う残骸を置く（検査が効いていなければ消えるはず）。
	stale := filepath.Join(root, strconv.Itoa(deadPID(t))+"-abc")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}

	orig := currentUID
	currentUID = func() int { return orig() + 1 } // 自分以外の所有に見せる
	t.Cleanup(func() { currentUID = orig })

	if _, err := openVerifiedTempRoot(); err == nil {
		t.Error("所有者が自分でない作業領域を受け入れてはいけない")
	}
	SweepStaleTempDirs()
	if _, err := os.Stat(stale); err != nil {
		t.Errorf("所有者を確認できない領域の中身を削除した: %v", err)
	}
}

// 🚨 所有者を判定できないとき（型アサーション失敗）は拒否すること（fail-closed）。
//
// 判定不能を「自分のもの」に丸めると、確認できていないものを消しにいくことになる。
func TestOwnerCheckFailsClosed(t *testing.T) {
	isolateTemp(t)
	if _, err := ensureTempRoot(); err != nil {
		t.Fatal(err)
	}
	// currentUID が「ありえない値」を返す = 一致しない → 拒否される、が期待。
	orig := currentUID
	currentUID = func() int { return -1 }
	t.Cleanup(func() { currentUID = orig })
	if _, err := openVerifiedTempRoot(); err == nil {
		t.Error("所有者が一致しないのに受け入れた")
	}
}

// 🚨 Lstat した実体と、実際に開いた実体が違うなら拒否すること。
//
// 「名前を 2 回解決する」構造には必ず窓がある。実時間のレースは再現できないので、
// 窓の内側（Lstat と OpenRoot の間）に seam を置いて決定論的に差し替える。
func TestSwapDuringVerificationIsDetected(t *testing.T) {
	parent := isolateTemp(t)
	if _, err := ensureTempRoot(); err != nil {
		t.Fatal(err)
	}
	// すり替え先（別 inode のディレクトリ）。中身は掃除の条件に合う形にしておく。
	victim := filepath.Join(parent, "victim")
	stale := filepath.Join(victim, strconv.Itoa(deadPID(t))+"-abc")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	important := filepath.Join(stale, "important.txt")
	if err := os.WriteFile(important, []byte("消えてはいけない"), 0o600); err != nil {
		t.Fatal(err)
	}

	swapped := false
	afterLstatHook = func(name string) {
		if name != tempRootName || swapped {
			return
		}
		swapped = true
		// 検証の途中で、同じ名前に別のディレクトリを置く。
		if err := os.Rename(tempRoot(), filepath.Join(parent, "moved-away")); err != nil {
			t.Error(err)
			return
		}
		if err := os.Rename(victim, tempRoot()); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { afterLstatHook = nil })

	_, err := openVerifiedTempRoot()
	if !swapped {
		t.Fatal("seam が呼ばれていない（窓の内側に無い）")
	}
	if err == nil {
		t.Fatal("検証中に差し替えられたのに受け入れた")
	}
	if !strings.Contains(err.Error(), "差し替え") {
		t.Errorf("差し替えだと分かるメッセージにすべき: %v", err)
	}
}

// 🚨 シグナル受信後の順序: 既定へ戻す → 後始末 → 終了。
//
// 逆順だと、後始末が長引いている間どのシグナルでも止められなくなる。
// 実シグナルでは「後始末が長い」状況を壁時計なしに作れないので、順序そのものを検査する。
func TestSignalHandlerResetsBeforeCleanup(t *testing.T) {
	var order []string
	exited := 0
	handleCleanupSignal(
		syscall.SIGINT,
		func() { order = append(order, "reset") },
		func() { order = append(order, "cleanup") },
		func(code int) { order = append(order, "exit"); exited = code },
	)
	if strings.Join(order, ",") != "reset,cleanup,exit" {
		t.Errorf("順序が違う: %v（reset,cleanup,exit であるべき）", order)
	}
	if exited != 128+int(syscall.SIGINT) {
		t.Errorf("終了コード: got %d, want %d", exited, 128+int(syscall.SIGINT))
	}
	// exit は 1 回だけ（os.Exit は戻らないが、戻る実装で二重に呼ばない）。
	if n := strings.Count(strings.Join(order, ","), "exit"); n != 1 {
		t.Errorf("exit の呼び出しが %d 回", n)
	}
}
