package auth

import (
	"fmt"
	"os/exec"
	"strings"
)

// getKeychainPassword は Keychain から "Chrome Safe Storage" のパスワードを取得する。
//
// 🚨 取得した値はプロセス内だけで使い、ファイルにもログにも出さない。
func getKeychainPassword() ([]byte, error) {
	cmd := exec.Command("security", "find-generic-password",
		"-w",
		"-a", chromeKeychainAccount,
		"-s", chromeKeychainService)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf(
			"Keychain から暗号化キーを取得できませんでした（service=%q account=%q）。\n"+
				"  - ターミナルで次を実行し、表示される許可ダイアログで「常に許可」を押してください:\n"+
				"      security find-generic-password -w -a %q -s %q\n"+
				"  - %s がインストールされ、ログインしているか確認してください（このツールは Chrome 専用です）。\n"+
				"  元エラー: %w",
			chromeKeychainService, chromeKeychainAccount,
			chromeKeychainAccount, chromeKeychainService, ChromeName, err)
	}
	return []byte(strings.TrimRight(string(out), "\n")), nil
}
