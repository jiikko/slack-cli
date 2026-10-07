package main

import (
	"strings"
	"testing"
)

// root の help は概要とサブコマンドの一覧だけにし、詳細（共通オプション・終了コード・安全のための制約）は各サブコマンドの help に置くこと。
// root に詳細を書き足すと、サブコマンドの help と二重になって片方だけ直る。
func TestTopUsageIsSummaryOnly(t *testing.T) {
	for _, sub := range []string{"search", "channels", "history", "thread", "users", "whoami", "config", "setup", "help"} {
		if !strings.Contains(topUsage, "\n  "+sub+" ") {
			t.Errorf("root の help にサブコマンド %q が無い", sub)
		}
	}
	for _, detail := range []string{"終了コード:", "-workspace <name>", "-token <", "安全のための制約:", "優先順位:"} {
		if strings.Contains(topUsage, detail) {
			t.Errorf("root の help に詳細 %q がある（各サブコマンドの help に置く）", detail)
		}
	}
	if n := strings.Count(topUsage, "\n"); n > 20 {
		t.Errorf("root の help が %d 行ある（概要だけにする）", n)
	}
}

// Slack に問い合わせるサブコマンドの help は、それだけで共通オプション・終了コード・安全のための制約が分かること（root を参照させない）。
func TestSubcommandHelpsCarryCommonDetails(t *testing.T) {
	for name, h := range map[string]string{
		"search": searchHelp, "channels": channelsHelp, "history": historyHelp, "thread": threadHelp,
		"users": usersHelp, "whoami": whoamiHelp, "setup": setupHelp,
	} {
		for _, want := range []string{"-workspace <name>", "-profile <name>", "-token <", "終了コード:", "安全のための制約:"} {
			if !strings.Contains(h, want) {
				t.Errorf("slack %s --help に %q が無い", name, want)
			}
		}
		if strings.Contains(h, "slack --help を参照") {
			t.Errorf("slack %s --help が root の help を参照している", name)
		}
	}
}

// help がローカル設定（.slack-cli.yml）と、その優先順位を説明していること。
// config.yml だけを案内すると、ローカル設定で値が変わった理由を help から辿れない。
func TestHelpsMentionLocalConfig(t *testing.T) {
	for name, h := range map[string]string{"共通オプション": commonOptionsHelp, "slack config --help": configHelp} {
		if !strings.Contains(h, ".slack-cli.yml") {
			t.Errorf("%s が .slack-cli.yml に触れていない", name)
		}
		if !strings.Contains(h, "環境変数 > .slack-cli.yml") {
			t.Errorf("%s の優先順位にローカル設定が入っていない", name)
		}
	}
	if !strings.Contains(configHelp, "set [-local]") {
		t.Error("slack config --help に set -local の使い方が無い")
	}
}
