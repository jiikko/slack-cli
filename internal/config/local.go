package config

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"gopkg.in/yaml.v3"
)

// LocalName はカレントディレクトリに置くローカル設定のファイル名。
// 書いてあるキーだけが共通の config.yml より優先される（キーごとの上書き）。
//
// 🚨 信頼確認（direnv allow のような仕組み）は入れていない。clone した repo に置かれた
// .slack-cli.yml で、読むワークスペース・使う Chrome プロファイルは変わりうる。それでも
// 接続先は ValidateWorkspace を通した <workspace>.slack.com に限られ（internal/slack が接続時にも
// 再検証する）、読んだ内容が出るのは利用者自身の端末だけで、ファイルを置いた第三者へ渡る
// 経路はこのツールの中に無い。そのため stderr の通知（LocalNotice）だけにしている（issue 004）。
// 読み取り結果を外部へ送る機能を足すときは、この判断を見直すこと。
const LocalName = ".slack-cli.yml"

// localHeader はローカル設定を書き出すときの見出し。
const localHeader = "# slack-cli のローカル設定（このディレクトリでだけ、共通の config.yml より優先される）\n" +
	"# workspace:     対象ワークスペースのサブドメイン（https://<workspace>.slack.com）\n" +
	"# profile:       使用する Chrome プロファイル名（auto でログイン済みを自動検出）\n" +
	"# default_count: 検索・取得の既定件数\n"

// localMaxSize はローカル設定として読む上限。設定ファイルとしてありえない大きさのものは読まない。
const localMaxSize = 1 << 20

// localLayer はローカル設定の読み込み結果。
type localLayer struct {
	layer
	path    string // 表示用のパス（読み書きは相対パス LocalName で行う）
	present bool   // 検査に通って読み込んだ
	ignored string // 検査に通らず無視した理由（「〜ため」の「〜」の形。空なら無視していない）
}

// readLocal はカレントディレクトリの .slack-cli.yml を読む。親ディレクトリへはさかのぼらない。
//
// 🚨 絶対パスでなく相対パス（LocalName）で開く。絶対パスだと、祖先のディレクトリに実行権限が無いだけで
// 開けず（EACCES）、ローカル設定が無いのに全コマンドが止まる。相対パスなら Getwd にも依存しない。
//
// 🚨 置き場所のディレクトリの検査は、ファイルを開く前に行う。開けないファイル（他人の 0600 等）を先に
// 「読めない = 中止」に分類すると、誰でも書ける /tmp に置かれた 1 ファイルで、そこで実行した全員が止まる。
//
// 🚨 ファイルの検査は「開いた fd」に対して行う。パスで検査してから開き直すと、その間に差し替えられる。
// O_NOFOLLOW で最後の要素のシンボリックリンクを拒み、O_NONBLOCK で FIFO に置き換えられていても
// 止まらない（その後の fstat で通常のファイルでないとして無視する）。
func readLocal() localLayer {
	l := localLayer{path: localDisplayPath()}
	if _, err := os.Lstat(LocalName); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			// 🚨 「無い」に丸めない（ローカル設定を黙って捨てる）。中を調べられないディレクトリ（r だけで x が無い等）
			// では、あるかどうか分からないので、無視したことを警告として出す。
			l.ignored = "置き場所のディレクトリを調べられない"
		}
		return l
	}
	if reason := unsafeDirReason("."); reason != "" {
		l.ignored = reason
		return l
	}
	f, err := os.OpenFile(LocalName, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		switch {
		case errors.Is(err, fs.ErrNotExist): // Lstat の後に消えた
		case errors.Is(err, syscall.ELOOP):
			l.ignored = "シンボリックリンクである"
		default:
			l.err = fmt.Errorf("%s を開けません: %w", l.path, err)
		}
		return l
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		l.err = fmt.Errorf("%s を調べられません: %w", l.path, err)
		return l
	}
	if reason := unsafeReason(fi, false); reason != "" {
		l.ignored = reason
		return l
	}
	data, err := io.ReadAll(io.LimitReader(f, localMaxSize+1))
	if err != nil {
		l.err = fmt.Errorf("%s を読めません: %w", l.path, err)
		return l
	}
	if len(data) > localMaxSize {
		l.ignored = "大きすぎる"
		return l
	}
	var fc File
	if err := yaml.Unmarshal(data, &fc); err != nil {
		l.err = fmt.Errorf("%s の解析に失敗しました: %w", l.path, err)
		return l
	}
	l.file = fc
	l.present = true
	return l
}

// localDisplayPath はローカル設定の表示用のパスを返す（読み書きは相対パス LocalName で行う）。
func localDisplayPath() string {
	if cwd, err := os.Getwd(); err == nil {
		return filepath.Join(cwd, LocalName)
	}
	return "./" + LocalName
}

// unsafeReason は、他人が中身を差し替えられるファイル・ディレクトリなら理由を返す。
// 所有者が自分でない、またはグループか他人が書き込めるなら差し替えられる。
func unsafeReason(fi os.FileInfo, wantDir bool) string {
	switch {
	case wantDir && !fi.IsDir():
		return "ディレクトリでない"
	case !wantDir && !fi.Mode().IsRegular():
		return "通常のファイルでない"
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return "所有者を確認できない"
	}
	if int(st.Uid) != os.Geteuid() {
		return "所有者が自分でない"
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return "グループか他人が書き込める"
	}
	return ""
}

// unsafeDirReason は、ローカル設定を置くディレクトリを他人が書き換えられるなら理由を返す
// （/tmp 直下のように誰でもファイルを置ける場所では、ファイル自体の検査に意味がない）。
func unsafeDirReason(dir string) string {
	fi, err := os.Stat(dir)
	if err != nil {
		return "置き場所のディレクトリを確認できない"
	}
	if reason := unsafeReason(fi, true); reason != "" {
		return "置き場所のディレクトリの" + reason
	}
	return ""
}

// LocalPath はカレントディレクトリのローカル設定の表示用のパスを返す（無くても返す）。
func LocalPath() string {
	load()
	return local.path
}

// LocalFile はローカル設定単体の内容を返す（無い・無視したならゼロ値）。SaveLocal に渡す値はここから作る。
func LocalFile() File {
	load()
	if !local.present {
		return File{}
	}
	return local.file
}

// LocalProblem はローカル設定を読めなかった（開けない・解析に失敗した）ならその理由を返す。
//
// 🚨 これを無視して続行しない。ローカル設定の値を黙って捨てると、共通の config.yml にある
// 別のワークスペースで動いてしまう。
func LocalProblem() error {
	load()
	return local.err
}

// LocalIgnored はローカル設定を検査で無視したならその理由を返す。
func LocalIgnored() string {
	load()
	return local.ignored
}

// LocalHas はローカル設定にそのキーが書いてあるかを返す。
func LocalHas(key string) bool {
	v, err := Get(LocalFile(), key)
	return err == nil && v != ""
}

// LocalPresent はローカル設定を読み込んだかを返す（キーが 1 つも無い空のファイルでも true）。
func LocalPresent() bool {
	load()
	return local.present
}

// Origin はそのキーの値をどの設定ファイルから取ったかを返す（どちらにも無ければ空）。
func Origin(key string) string {
	if LocalHas(key) {
		return LocalPath()
	}
	if v, err := Get(GlobalFile(), key); err == nil && v != "" {
		p, _ := Path()
		return p
	}
	return ""
}

// LocalNotice は、ローカル設定で workspace / profile が決まったときに stderr へ出す 1 行を返す。
// default_count は通知しない（読む対象・使うアカウントを変えないため）。
// ws / profile は解決後の値。フラグ・環境変数で別の値に上書きされたならローカルの値と一致しないので、
// 一致したキーだけを「ローカル設定で決まった」とみなす（同じ値をフラグで渡したときも通知するが、結果は同じ）。
//
// 🚨 stdout には出さない（-json の出力を壊す）。
func LocalNotice(ws, profile string) string {
	lf := LocalFile()
	var used []string
	if v := lf.workspaceValue(); v != "" && v == ws {
		used = append(used, "workspace="+v)
	}
	if v := lf.Profile; v != "" && v == profile {
		used = append(used, "profile="+v)
	}
	if len(used) == 0 {
		return ""
	}
	return fmt.Sprintf("ローカル設定 %s を使用: %s", LocalPath(), strings.Join(used, " "))
}

// SaveLocal はローカル設定を書き出し、書いたパスを返す。fc は LocalFile() から作ること
// （共通の config.yml の値まで写すと、後から共通側を変えてもローカルの古い値が優先され続ける）。
//
// 🚨 os.WriteFile で既存のパスへ書かない。シンボリックリンクをたどり、0600 も新規作成のときしか効かない。
// 同じディレクトリに一時ファイルを作って rename で置き換える（rename はリンクそのものを置き換え、たどらない）。
func SaveLocal(fc File) (string, error) {
	load()
	switch {
	case local.err != nil:
		return "", local.err
	case local.ignored != "":
		return "", fmt.Errorf("ローカル設定 %s は%sため書き込みません", local.path, local.ignored)
	}
	if reason := unsafeDirReason("."); reason != "" {
		return "", fmt.Errorf("ローカル設定 %s は%sため書き込みません", local.path, reason)
	}
	data, err := marshalFile(fc, localHeader)
	if err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(".", LocalName+".tmp-*") // 0600 で作られる
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return "", err
	}
	if err := os.Rename(tmpName, LocalName); err != nil {
		os.Remove(tmpName)
		return "", err
	}
	return local.path, nil
}
