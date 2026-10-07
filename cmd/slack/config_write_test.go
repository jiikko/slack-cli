package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jiikko/slack-cli/internal/config"
)

// isolateConfig は設定ディレクトリと HOME をテスト専用にする。
// HOME も差し替えるのは、守りが外れたときに本物の Chrome のデータを読みに行かせないため。
func isolateConfig(t *testing.T) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv(config.EnvWorkspace, "")
	t.Setenv(config.EnvProfile, "")
	config.ResetCache()
	t.Cleanup(config.ResetCache)
	dir := filepath.Join(xdg, "slack-cli")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// 🚨 壊れた config.yml を、config.yml を書き直すどの経路でも上書きしないこと。
// config init / setup は空の File に workspace / profile だけを入れて保存するので、
// 書き込めてしまうと他のキーが黙って消える（以前は config set だけが断っていた）。
func TestBrokenConfigIsNotOverwrittenByAnyWriter(t *testing.T) {
	for name, run := range map[string]func() error{
		"config set":  func() error { return cmdConfig([]string{"set", "default_count", "5"}) },
		"config init": func() error { return cmdConfig([]string{"init"}) },
		"setup":       func() error { return cmdSetup(nil) },
	} {
		t.Run(name, func(t *testing.T) {
			dir := isolateConfig(t)
			path := filepath.Join(dir, "config.yml")
			broken := "workspace: [不正\ndefault_count: 50\n"
			if err := os.WriteFile(path, []byte(broken), 0o600); err != nil {
				t.Fatal(err)
			}

			err := run()
			var ue *config.UsageError
			if !errors.As(err, &ue) || !strings.Contains(err.Error(), "書き込みを中止") {
				t.Fatalf("壊れた config.yml への書き込みを断っていない: %v", err)
			}
			data, rerr := os.ReadFile(path)
			if rerr != nil {
				t.Fatal(rerr)
			}
			if string(data) != broken {
				t.Errorf("config.yml が書き換えられた:\n%s", data)
			}
		})
	}
}
