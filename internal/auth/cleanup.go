package auth

// Chrome の Cookie DB / Local Storage は、ロックを避けるため作業領域へコピーしてから読む。
// コピーの中身は「なりすましログインできる資格情報そのもの」なので、**プロセスが終わった
// 時点で必ず消えている**ことを構造で保証する。
//
// 🚨 「必ず消す」を defer だけに任せない。Go の既定ではシグナル（Ctrl-C）でプロセスが
// 即終了して defer が走らず、フルディスクアクセスで守られた領域の完全なコピーが、
// 守られていない $TMPDIR に残る。
//
// 3 段構え（段ごとにテストを持つ。cleanup_test.go）:
//  ① defer による即時削除 — 正常終了・エラー・panic を覆う
//  ② シグナル（SIGINT/SIGTERM/SIGHUP）を捕まえ、登録済みの削除を実行してから終了する
//  ③ ②でも間に合わない終わり方（SIGKILL・強制終了・電源断）に備え、**起動時に**
//     「自分が作った親ディレクトリ直下」「名前が <pid>-… の形」「その pid が生きていない」の
//     3 条件をすべて満たすものだけを消す（呼び出しは cmd/slack/main.go の先頭 1 箇所。
//     資格情報を読む経路に置くと、slack help / slack config では走らない）
//
// ③ は破壊的操作なので、条件に合わないものは一切触らない（母集合を広げない）。

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var (
	cleanupMu    sync.Mutex
	cleanupPaths = map[string]struct{}{}
)

// registerCleanup は「プロセスが終わる前に消すべきパス」を登録する（②が使う）。
func registerCleanup(path string) {
	cleanupMu.Lock()
	defer cleanupMu.Unlock()
	cleanupPaths[path] = struct{}{}
}

// RunAllCleanups は登録済みのパスをすべて削除する。
// ①の defer と②のシグナル経路の両方から呼ばれるが、二重呼び出しは無害。
//
// 🚨 削除は③と同じく「検証済みの作業領域を開いた fd」経由で行う。パス文字列を
// os.RemoveAll に渡すと、途中のディレクトリがシンボリックリンクに差し替えられていた場合に
// **リンク先を再帰削除する**（③だけ塞いで①②に同じ穴を残していた。敵対的レビューが実証）。
//
// 🚨 検証（openVerifiedTempRoot）と fd 経由の削除は冗長な関係にある（変異検証で実測）:
// 検証が先に弾くので、削除だけを os.RemoveAll に戻す変異は現行のテストでは素通りする。
// fd 経由が単独で守るのは「検証を通ってから削除するまでの差し替え」だけで、これは
// 決定論的なテストを書けていない（残っている未検証リスク）。両方を外す変異は red になる。
//
// 🚨 削除ループはロックを保持したまま回す。先に map を空にしてロックを外すと、
// シグナル経路が「消すものは無い」と判断して os.Exit し、**削除途中のコピーが残る**。
func RunAllCleanups() {
	cleanupMu.Lock()
	defer cleanupMu.Unlock()
	if len(cleanupPaths) == 0 {
		return
	}

	root := tempRoot()
	names := make([]string, 0, len(cleanupPaths))
	for p := range cleanupPaths {
		// 想定外の場所が登録されていたら触らない（登録経路は newTempDir だけ）。
		//
		// 🚨 これは**冗長**な検査（変異検証で確認）。削除は removeVerified が
		// 検証済み root からの相対名で行うので、作業領域の外を消すことは構造的に起きない。
		// このガードが単独で担っているのは「気づける形にする」ことだけ（警告を出す）。
		// 冗長だからと外すときは、removeVerified が相対名のままかを必ず確かめること。
		if filepath.Dir(p) != root {
			fmt.Fprintf(os.Stderr, "警告: 想定外の後始末対象を無視しました: %s\n", p)
			continue
		}
		names = append(names, filepath.Base(p))
		delete(cleanupPaths, p)
	}
	if err := removeVerified(names...); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "警告: 一時コピーを削除できませんでした: %v\n", err)
		}
	}
}

// cleanupSignals は②が捕まえるシグナル。
var cleanupSignals = []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT}

// InstallCleanupOnSignal は②を仕掛ける。main の先頭で 1 回だけ呼ぶ。
//
// 捕まえるのは「人が実際に送れて、プロセスを終わらせるシグナル」:
//
//	SIGINT  Ctrl-C
//	SIGTERM kill の既定
//	SIGHUP  端末が閉じた
//	SIGQUIT Ctrl-\
//
// 🚨 SIGQUIT を外さないこと。**鍵盤から届く**ので Ctrl-C と同じ頻度で起こりうるのに、
// 捕まえていないと復号済みの d cookie を含むコピーが $TMPDIR に残る（実測で踏んだ）。
// 代償として Go 既定の SIGQUIT（goroutine スタックダンプを出して終了）は無くなるが、
// 資格情報のコピーを残さない方を採る。signal_cleanup_test.go が実際にシグナルを撃って
// 4 つすべてを固定している。
//
// SIGKILL / SIGSTOP は捕まえられない（OS の仕様）。そちらは③（起動時の掃除）が受け持つ。
// SIGABRT は意図的に捕まえない — Go ランタイムが致命的エラーで自分に送るシグナルで、
// 横取りするとクラッシュの報告経路を壊す。
func InstallCleanupOnSignal() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, cleanupSignals...)
	go func() {
		sig := <-ch
		// 🚨 受け取ったら即座に既定動作へ戻す。ハンドラは 1 回しか読まないので、
		// ここで戻さないと**後始末が長引いている間、どのシグナルでも止められなくなる**
		// （Ctrl-C を連打しても効かず、SIGQUIT による脱出口も塞がる。実測で踏んだ）。
		// 戻したあとは「1 発目 = 後始末して終了 / 2 発目 = OS 既定」になる。
		// 🚨 ただし 2 発目で必ず止まるとは限らない。起動時に SIGINT が SIG_IGN で
		// 継承されていると（`&` によるバックグラウンド起動・cron・CI）、Reset は
		// その無視を復元するので SIGINT では止まらない（実測）。端末から起動した
		// 場合は Ctrl-C の 2 発目で止まる。
		signal.Reset(cleanupSignals...)
		RunAllCleanups()
		if s, ok := sig.(syscall.Signal); ok {
			os.Exit(128 + int(s)) // シェルの慣習（SIGINT=130 / SIGTERM=143）
		}
		os.Exit(1)
	}()
}

// 作業領域は ~/Library/Caches/slack-cli/extract。
//
// 🚨 $TMPDIR は使わない。未設定時の os.TempDir() は /tmp を返し、/tmp は誰でも書けるので、
// **別ユーザー**が先に同名のシンボリックリンクを作って居座れる（sticky bit は新規作成を妨げない）。
// こちらからは消せないため、資格情報の読み取りが恒久的に壊れる。HOME 配下ならこの形は無い。
//
// 🚨 ただし「環境変数で動かない」わけではない。os.UserCacheDir() は darwin では
// $HOME/Library/Caches を返すので、**HOME を差し替えれば作業領域も破壊的な掃除の母集合も動く**
// （実測。$XDG_CACHE_HOME は darwin では見ない）。HOME 未設定はエラーになる（fail-closed）ので、
// 残る穴は「HOME を任意の場所へ向けられる呼び出し側」だけ。これは $TMPDIR と同じ性質で、
// $TMPDIR を捨てた理由は上の「別ユーザーが居座れる」ほうだけが根拠になる。
// 相対パスの HOME（cwd 依存になる）は tempRootParent が拒否する。
const (
	tempRootParentName = "slack-cli"
	tempRootName       = "extract"
)

// staleAge はこれより古い残骸を、pid の生死に関わらず消す閾値。
//
// 🚨 pid の生死だけで判定すると、**番号が再利用された残骸は永久に消えない**。
// また $TMPDIR 時代は OS（dirhelper）が数日で回収していたが、~/Library/Caches には
// それに相当する仕組みを見つけられなかったので、寿命の上限は自分で持つ必要がある。
const staleAge = 7 * 24 * time.Hour

// tempRootParent は作業領域の親（~/Library/Caches/slack-cli）を返す。
func tempRootParent() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("キャッシュディレクトリを決められません: %w", err)
	}
	// 🚨 相対パスを弾く。HOME が相対だと作業領域が cwd 依存になり、
	// 「カレントディレクトリに一切依存しない」という前提が崩れる（実測: HOME="." で再現）。
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("キャッシュディレクトリが絶対パスではありません（HOME を確認してください）: %s", dir)
	}
	return filepath.Join(dir, tempRootParentName), nil
}

// tempRoot は作業領域のパスを返す（決められないときは空文字）。
func tempRoot() string {
	parent, err := tempRootParent()
	if err != nil {
		return ""
	}
	return filepath.Join(parent, tempRootName)
}

// openVerifiedChild は parent の直下の name を**検証したうえで**開く。
//
// 検証の中身と、それぞれが何を防いでいるか:
//
//	Lstat で symlink を拒否   os.Root は「ルート内に留まる**相対**シンボリックリンクは追う」
//	                          （絶対リンクだけを拒否する。実測で確認した）
//	SameFile で同一性を確認   Lstat した実体と、実際に開いた実体が同じであることを見る。
//	                          名前を 2 回解決することによる差し替えの窓を閉じる
//	uid を確認（fail-closed） 所有者が自分でなければ使わない。型アサーションに失敗したら
//	                          「判定不能」なので拒否する（素通りさせない）
//
// 🚨 最初の 2 つは**冗長**な関係にある（変異検証で実測）: symlink を張られた場合、
// Lstat はリンク自身・Stat(".") はリンク先を指すので、symlink 判定を外しても
// SameFile 側が食い違いを検出して拒否する。片方だけを外す変異は素通りするため、
// 外すときは「もう片方が本当に同じものを守るか」を確かめること。意図的に両方残す
// （symlink 判定は意図が読めるエラーメッセージを出せる、という別の役目もある）。
//
// ディレクトリかどうかは別途見ない — 通常ファイルに対して OpenRoot が
// "not a directory" を返すため（実測）。冗長な検査は置かない。
func openVerifiedChild(parent *os.Root, name, display string) (*os.Root, error) {
	// Lstat なのでシンボリックリンクを追わない（追ってから調べたのでは遅い）。
	want, err := parent.Lstat(name)
	if err != nil {
		return nil, err // 存在しない = まだ一度も使っていない。これは正常
	}
	if want.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s がシンボリックリンクです（削除してください）: %s", name, display)
	}
	child, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	got, err := child.Stat(".")
	if err != nil {
		_ = child.Close()
		return nil, err
	}
	if !os.SameFile(want, got) {
		_ = child.Close()
		return nil, fmt.Errorf("%s が検証中に差し替えられました: %s", name, display)
	}
	st, ok := got.Sys().(*syscall.Stat_t)
	if !ok {
		_ = child.Close()
		return nil, fmt.Errorf("%s の所有者を判定できません: %s", name, display)
	}
	if int(st.Uid) != os.Getuid() {
		_ = child.Close()
		return nil, fmt.Errorf("%s の所有者が自分ではありません（削除してください）: %s", name, display)
	}
	return child, nil
}

// openVerifiedTempRoot は作業領域を**検証したうえで**ディレクトリ fd として開く。
// 検証に失敗したら開かない（呼び出し側は 1 件も触らないこと）。何も作らない。
//
// 🚨 破壊的操作を行う側がこの検証を通ること。検証が作成側にしか無いと、
// 掃除が作業領域のシンボリックリンクを追って**リンク先を再帰削除する**（実証済み）。
//
// 🚨 検証は**最後の 1 コンポーネントだけでは足りない**。途中のディレクトリ
// （~/Library/Caches/slack-cli）を差し替えれば、検証済みの root ごと任意の場所へ移せる。
// os.UserCacheDir() を起点に 1 コンポーネントずつ降りる。
func openVerifiedTempRoot() (*os.Root, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return nil, fmt.Errorf("キャッシュディレクトリを決められません: %w", err)
	}
	if !filepath.IsAbs(base) {
		return nil, fmt.Errorf("キャッシュディレクトリが絶対パスではありません（HOME を確認してください）: %s", base)
	}
	r, err := os.OpenRoot(base)
	if err != nil {
		return nil, err
	}
	for _, name := range []string{tempRootParentName, tempRootName} {
		child, err := openVerifiedChild(r, name, filepath.Join(base, name))
		_ = r.Close() // 子は自前の fd を持つので、親は閉じてよい
		if err != nil {
			return nil, err
		}
		r = child
	}
	return r, nil
}

// removeVerified は検証済みの作業領域から names（いずれも 1 コンポーネント）を削除する。
//
// 🚨 削除の実装をここ 1 本に寄せる。①（defer）と②（シグナル経路）で別々に書くと、
// 片方だけがパス文字列の os.RemoveAll のまま取り残される（実際に起きた）。
func removeVerified(names ...string) error {
	if len(names) == 0 {
		return nil
	}
	r, err := openVerifiedTempRoot()
	if err != nil {
		return err
	}
	defer r.Close()
	var firstErr error
	for _, name := range names {
		if name != filepath.Base(name) || name == "." || name == ".." {
			// 呼び出し側の組み立てミス。作業領域の外を指しうるので触らない。
			if firstErr == nil {
				firstErr = fmt.Errorf("削除対象が作業領域の直下ではありません: %s", name)
			}
			continue
		}
		if err := r.RemoveAll(name); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// ensureTempRoot は作業領域を 0700 で用意し、検証して返す。
func ensureTempRoot() (string, error) {
	root := tempRoot()
	if root == "" {
		return "", fmt.Errorf("作業領域のパスを決められません")
	}
	// 🚨 0700。作業領域には復号済みの資格情報のコピーが入る。
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	r, err := openVerifiedTempRoot()
	if err != nil {
		return "", err
	}
	_ = r.Close()
	return root, nil
}

// SweepStaleTempDirs は③。過去の実行が SIGKILL 等で残したものだけを消す。
// 条件に 1 つでも合わなければ触らない（判断できないものは残す方へ倒す）。
func SweepStaleTempDirs() {
	r, err := openVerifiedTempRoot()
	if err != nil {
		// 存在しない = まだ何も残していない（正常）。それ以外は検証に失敗したということなので、
		// **1 件も消さずに**黙って引き下がる。ただし沈黙はしない（何が起きたか分からなくなる）。
		if !errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "警告: 作業領域を検証できないため掃除を中止しました: %v\n", err)
		}
		return
	}
	defer r.Close()

	dir, err := r.Open(".")
	if err != nil {
		return
	}
	entries, err := dir.ReadDir(-1)
	_ = dir.Close()
	if err != nil {
		return
	}

	self := os.Getpid()
	for _, e := range entries {
		if !e.IsDir() {
			continue // ディレクトリ以外は対象外
		}
		pid, ok := pidFromTempDirName(e.Name())
		if !ok || pid == self {
			// 名前の形が違う / 自分のものは触らない。
			// 🚨 この !ok と processAlive の pid<=0 ガードは現状「互いを覆う」冗長な関係にある
			//（形が違う → pid 0 → 判定不能 → 消さない）。片方を外す変異は素通りするので、
			// 外すときは「もう片方が本当に同じものを守るか」を確かめること。意図的に両方残す。
			continue
		}
		// 🚨 pid の生死だけで判定しない。番号が再利用されると永久に消えなくなる。
		// 十分に古いものは「持ち主はもう居ない」とみなす（名前の条件は緩めない）。
		if processAlive(pid) && !isStale(e) {
			continue // 生きているプロセスの、新しいものは触らない（並行実行）
		}
		// 🚨 削除は fd 起点（r.RemoveAll）で行う。パスを組み直して os.RemoveAll に渡すと、
		// 検証した root とは別の場所を消しうる。
		if err := r.RemoveAll(e.Name()); err != nil {
			// 失敗を無音にしない（残り続けている事実に気づけなくなる）。
			fmt.Fprintf(os.Stderr, "警告: 一時コピーの残骸を削除できませんでした (%s): %v\n", e.Name(), err)
		}
	}
}

// isStale は staleAge より古いエントリかを返す。判定できないときは false（消さない方へ倒す）。
func isStale(e os.DirEntry) bool {
	info, err := e.Info()
	if err != nil {
		return false
	}
	return time.Since(info.ModTime()) > staleAge
}

// pidFromTempDirName は "<pid>-<乱数>" 形式のディレクトリ名から pid を取り出す。
// この形式でないものは対象外（false を返す）。
func pidFromTempDirName(name string) (int, bool) {
	i := strings.IndexByte(name, '-')
	if i <= 0 {
		return 0, false
	}
	head := name[:i]
	pid, err := strconv.Atoi(head)
	if err != nil || pid <= 0 {
		return 0, false
	}
	// 🚨 往復一致を要求する。Atoi は "+1" / "007" / "0000000000000001" を受けてしまい、
	// 自分が作らない名前まで掃除の母集合に入る（MkdirTemp が作るのは十進・前置ゼロ無し）。
	if strconv.Itoa(pid) != head {
		return 0, false
	}
	return pid, true
}

// processAlive は pid のプロセスが生きているかを返す。
// 判断できないとき（権限が無い等）は「生きている」に倒す = 消さない方へ倒す。
//
// 🚨 os.FindProcess + Process.Signal を使わないこと。消えたプロセスに対して
// ESRCH ではなく os.ErrProcessDone を返すため、ESRCH だけを見る判定は
// 「常に生きている」に落ちて掃除が 1 件も走らなくなる。
// kill(2) を直接呼び、errno をそのまま判定する。
func processAlive(pid int) bool {
	// 🚨 pid 0 / 負値を kill(2) に渡さない。0 は「自分のプロセスグループ全体」、
	// 負値は「プロセスグループ指定」を意味し、生死判定にならない（成功して
	// 「生きている」に見える）。ここでは判定不能として扱い、消さない方へ倒す。
	if pid <= 0 {
		return true
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true // シグナルを送れた = 生きている
	}
	if errors.Is(err, syscall.ESRCH) {
		return false // そんなプロセスは無い = 死んでいる
	}
	return true // EPERM 等、判断できないときは消さない
}

// newTempDir は「①②③すべての対象になる」一時ディレクトリを作る。
// 戻り値の cleanup は ① として defer で呼ぶこと。
//
// Cookie DB と Local Storage(leveldb) の両方がこの関数を通る。片方だけ別経路で
// os.MkdirTemp すると、その残骸は②③のどちらにも拾われない。
func newTempDir() (dir string, cleanup func(), err error) {
	root, err := ensureTempRoot()
	if err != nil {
		return "", nil, err
	}
	// ディレクトリ名に pid を埋める（③がこれを見て「生きていない実行の残骸」を判定する）。
	d, err := os.MkdirTemp(root, fmt.Sprintf("%d-", os.Getpid()))
	if err != nil {
		return "", nil, err
	}
	registerCleanup(d)
	// 🚨 ①も②③と同じ「検証済みの作業領域を開いた fd」経由で消す。
	// ここだけパス文字列の os.RemoveAll に戻すと、作業領域を差し替えられたときに
	// リンク先を消す経路が復活する（①の窓は「作業中ずっと」なので最も広い）。
	return d, func() { _ = removeVerified(filepath.Base(d)) }, nil
}
