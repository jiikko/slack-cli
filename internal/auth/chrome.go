package auth

// Chrome のプロファイルからの読み取り（Cookie の復号・作業領域の後始末・エラーの分類・
// プロファイルの列挙）は github.com/jiikko/dotfiles/src/chromecookie が持つ
// （esa-cli / newrelic-nrql-cli と共有。直すときはあちらを直し、ここへコピーを戻さない）。
// このファイルはパッケージ内外の呼び出し側が使う名前を、そちらへつなぐだけ。

import "github.com/jiikko/dotfiles/src/chromecookie"

// ws は slack-cli の作業領域（~/Library/Caches/slack-cli/extract）。
// Cookie DB と Local Storage(leveldb) の一時コピーは必ずここを通す（後始末の 3 段構えに載せる）。
var ws = chromecookie.NewWorkspace("slack-cli")

// ChromeName はエラーメッセージ用の表示名。
const ChromeName = chromecookie.ChromeName

// ProfileAuto は「ログイン済みプロファイルを自動検出する」ことを示す予約値。
const ProfileAuto = "auto"

// Profile は Chrome のプロファイル 1 件。
type Profile = chromecookie.Profile

// ListProfiles は Local State からプロファイルを列挙する（直近に使ったものが先頭）。
func ListProfiles() []Profile { return chromecookie.ListProfiles() }

// エラーの分類（EnvError = 即停止 / ReadError = 記録して次へ）。意味は chromecookie の package doc。
type (
	EnvError      = chromecookie.EnvError
	ReadError     = chromecookie.ReadError
	ReadErrorKind = chromecookie.ReadErrorKind
	ProfileIssue  = chromecookie.ProfileIssue
)

const (
	ReadDenied     = chromecookie.ReadDenied
	ReadIncomplete = chromecookie.ReadIncomplete
	DecryptFailed  = chromecookie.DecryptFailed
	ReadBroken     = chromecookie.ReadBroken
)

// IsEnvError は err が EnvError（プロファイルに依存しない環境の問題）かを返す。
func IsEnvError(err error) bool { return chromecookie.IsEnvError(err) }

// AsProfileIssue は err が ReadError なら記録にして返す。
func AsProfileIssue(profile string, err error) (ProfileIssue, bool) {
	return chromecookie.AsProfileIssue(profile, err)
}

// IssueNote は全候補が失敗したときの案内に添える文。
func IssueNote(issues []ProfileIssue) string { return chromecookie.IssueNote(issues) }

// InstallCleanupOnSignal は②（シグナルで終わっても一時コピーを消す）を仕掛ける。main の先頭で 1 回。
func InstallCleanupOnSignal() { ws.InstallCleanupOnSignal() }

// SweepStaleTempDirs は③（前回の実行が SIGKILL 等で残した一時コピーを消す）。main の先頭で 1 回。
func SweepStaleTempDirs() { ws.SweepStaleTempDirs() }

// RunAllCleanups は登録済みの一時コピーをすべて消す（main の defer とエラー経路）。
func RunAllCleanups() { ws.RunAllCleanups() }
