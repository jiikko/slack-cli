package auth

import (
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

// EnvError は「どのプロファイルを試しても同じ結果になる」環境の問題を表す。
// 該当するのは Keychain から暗号鍵を取れないこと（と macOS 以外での実行）だけ。
// 暗号鍵（Chrome Safe Storage）は全プロファイルで共通なので、候補を変えても直らない。
//
// 🚨 ファイルの読み取り拒否（フルディスクアクセス不足）をここに入れないこと。
// 拒否は**プロファイル単位でも起きる**（1 プロファイルだけ chmod 000 / sudo で起動した
// Chrome が root 所有にしたディレクトリ）。EnvError にすると探索全体がそこで止まり、
// 後ろの正常なプロファイルが使えなくなる（実際に退行させた）。読み取りの失敗は
// ReadError として記録して次へ進み、全滅したときにだけ案内する。
type EnvError struct {
	Msg string
	Err error // 元エラー（無ければ nil）
}

func (e *EnvError) Error() string {
	if e.Err != nil {
		return e.Msg + "\n  元エラー: " + e.Err.Error()
	}
	return e.Msg
}

func (e *EnvError) Unwrap() error { return e.Err }

// IsEnvError は err が EnvError（プロファイルに依存しない環境の問題）かを返す。
func IsEnvError(err error) bool {
	var ee *EnvError
	return errors.As(err, &ee)
}

// ReadErrorKind は ReadError の種類（案内の出し分けに使う）。
type ReadErrorKind int

const (
	// ReadDenied はアクセス拒否（フルディスクアクセス不足 / ディレクトリの権限・所有者）。
	ReadDenied ReadErrorKind = iota + 1
	// ReadIncomplete は一部のファイルを読めず、目的のもの（cookie / トークン）が見つからなかった。
	ReadIncomplete
	// DecryptFailed は cookie を 1 件以上試して、すべて復号に失敗した（鍵が合っていない可能性）。
	DecryptFailed
)

// ReadError は 1 プロファイルの資格情報を読めなかったことを表す。
// プロファイル固有でありうるので、呼び出し側は記録して次の候補へ進み、
// どの候補も成功しなかったときに IssueNote で原因と対処を添える。
type ReadError struct {
	Kind ReadErrorKind
	Msg  string
	Err  error
}

func (e *ReadError) Error() string {
	if e.Err != nil {
		return e.Msg + "（" + e.Err.Error() + "）"
	}
	return e.Msg
}

func (e *ReadError) Unwrap() error { return e.Err }

// hint は種類ごとの対処。
func (k ReadErrorKind) hint() string {
	switch k {
	case ReadDenied:
		return "アクセス拒否: ターミナル（またはこのツールを起動しているアプリ）の「フルディスクアクセス」" +
			"（システム設定 → プライバシーとセキュリティ → フルディスクアクセス）、または" +
			"そのプロファイルのディレクトリのパーミッション／所有者（sudo で起動した Chrome が root 所有にしていないか）を確認してください。"
	case ReadIncomplete:
		return "読み取れなかったファイルがあります。上のエラーの権限・所有者を確認するか、Chrome を完全に終了（cmd+Q）してから再実行してください。"
	case DecryptFailed:
		return "cookie の復号に失敗しました（Keychain の暗号鍵が合っていない可能性）。" +
			"Keychain の「Chrome Safe Storage」が今の Chrome のものか確認してください。"
	}
	return ""
}

// diskAccessError はアクセス拒否を ReadError にする（Cookie DB / Local Storage 共通）。
func diskAccessError(what, target string, err error) error {
	return &ReadError{Kind: ReadDenied, Msg: fmt.Sprintf("%sを読み取れませんでした（アクセス拒否）: %s", what, target), Err: err}
}

// ProfileIssue は「どのプロファイルで何が読めなかったか」の記録。
type ProfileIssue struct {
	Profile string
	Err     *ReadError
}

// AsProfileIssue は err が ReadError なら記録にして返す（そうでなければ ok=false）。
func AsProfileIssue(profile string, err error) (ProfileIssue, bool) {
	var re *ReadError
	if !errors.As(err, &re) {
		return ProfileIssue{}, false
	}
	return ProfileIssue{Profile: profile, Err: re}, true
}

// IssueNote は全候補が失敗したときの案内に添える文（記録が無ければ空文字）。
// 各プロファイルで何が起きたかと、種類ごとの対処を 1 回ずつ出す。
func IssueNote(issues []ProfileIssue) string {
	if len(issues) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n  次のプロファイルは資格情報を読み取れませんでした:")
	kinds := map[ReadErrorKind]bool{}
	for _, is := range issues {
		fmt.Fprintf(&b, "\n    %s: %v", is.Profile, is.Err)
		kinds[is.Err.Kind] = true
	}
	ks := make([]int, 0, len(kinds))
	for k := range kinds {
		ks = append(ks, int(k))
	}
	sort.Ints(ks)
	for _, k := range ks {
		if h := ReadErrorKind(k).hint(); h != "" {
			b.WriteString("\n  → " + h)
		}
	}
	return b.String()
}

// skippedReads は「読めずに飛ばしたファイル」の記録。
//
// 🚨 読み取りの失敗を黙って捨てないこと。捨てると、目的のもの（WAL にだけある cookie /
// トークン）が見つからなかったときに「ログインしていない」という別の案内に化ける。
// 存在しない（ENOENT）は正常なので記録しない。
type skippedReads []string

func (s *skippedReads) add(err error) {
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return
	}
	*s = append(*s, err.Error())
}

// asError は「目的のものが見つからなかった」ときに、読めなかったファイルがあれば ReadError にする。
// 記録が無ければ nil（= 本当に無かった）。
func (s skippedReads) asError(what string) error {
	if len(s) == 0 {
		return nil
	}
	return &ReadError{Kind: ReadIncomplete, Msg: fmt.Sprintf(
		"%sが見つかりませんでした。読み取れなかったファイルがあります: %s", what, strings.Join(s, " / "))}
}
