package config

import "fmt"

// UsageError は「引数・設定の指定ミス」を表す。main はこれを終了コード 2 として扱う
// （仕様 §8: 0 成功 / 1 実行時エラー / 2 使い方エラー）。
type UsageError struct{ Msg string }

func (e *UsageError) Error() string { return e.Msg }

// Usagef は UsageError を作る補助。
func Usagef(format string, args ...any) *UsageError {
	return &UsageError{Msg: fmt.Sprintf(format, args...)}
}
