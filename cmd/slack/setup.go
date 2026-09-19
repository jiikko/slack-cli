package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/jiikko/slack-cli/internal/auth"
	"github.com/jiikko/slack-cli/internal/config"
)

const setupHelp = `slack setup - 対話式セットアップウィザード

Chrome のログイン状況から「対象ワークスペース」と「使用プロファイル」を決めて config.yml に保存する。
検出はローカル（Chrome のデータ）だけで行い、接続はワークスペースを決めた後の確認 1 回だけ。

使い方:
  slack setup                対話的に設定して保存
  slack setup -workspace X   既定値を渡して開始（プロンプトで上書き可）

非対話（パイプ/入力なし）で実行した場合は各項目とも既定値を採用する。
`

// promptDefault は 1 行入力を求める。空入力/EOF なら def を返す。
func promptDefault(r *bufio.Reader, label, def string) string {
	if def != "" {
		fmt.Printf("%s [%s]: ", label, def)
	} else {
		fmt.Printf("%s: ", label)
	}
	line, err := r.ReadString('\n')
	line = strings.TrimRight(line, "\r\n")
	if strings.TrimSpace(line) == "" {
		if err != nil { // EOF（非対話）
			fmt.Println()
		}
		return def
	}
	return line
}

func cmdSetup(args []string) error {
	var cfg config.Config
	fs := newFlagSet("setup")
	registerCommon(fs, &cfg) // 現在の既定（env/config.yml）を初期値として使う
	if done, err := parseArgs(fs, setupHelp, args); err != nil || done {
		return err
	}

	in := bufio.NewReader(os.Stdin)
	fmt.Println("=== slack-cli セットアップ ===")
	fmt.Println("Chrome にログイン済みの Slack セッションを使います（読み取り専用）。")
	fmt.Println()

	// 1. プロファイルごとにワークスペース候補を集める（ローカルのみ）。
	fmt.Printf("%s のプロファイルを調べています...\n", auth.ChromeName)
	type cand struct {
		profile string
		email   string
		hints   []auth.WorkspaceHint
	}
	var cands []cand
	for _, p := range auth.ListProfiles() {
		hints, err := auth.DiscoverWorkspaces(p.Dir)
		if err != nil || len(hints) == 0 {
			continue
		}
		cands = append(cands, cand{profile: p.Dir, email: p.Email, hints: hints})
	}
	if len(cands) == 0 {
		return fmt.Errorf(
			"Slack にログイン済みの %s プロファイルが見つかりませんでした。\n"+
				"  %s で対象の Slack ワークスペースを開いてから、もう一度実行してください。",
			auth.ChromeName, auth.ChromeName)
	}

	fmt.Println("\n見つかったプロファイルとワークスペース:")
	type choice struct {
		profile   string
		workspace string
	}
	var choices []choice
	for _, c := range cands {
		email := c.email
		if email == "" {
			email = "-"
		}
		for _, h := range c.hints {
			choices = append(choices, choice{profile: c.profile, workspace: h.Domain})
			fmt.Printf("  [%d] %-14s %-30s %s\n", len(choices), c.profile, email, h.Domain)
		}
	}

	def := "1"
	if cfg.Workspace != "" {
		for i, ch := range choices {
			if ch.workspace == cfg.Workspace {
				def = strconv.Itoa(i + 1)
				break
			}
		}
	}
	sel := strings.TrimSpace(promptDefault(in, "使用する組み合わせの番号", def))
	idx, err := strconv.Atoi(sel)
	if err != nil || idx < 1 || idx > len(choices) {
		return &config.UsageError{Msg: fmt.Sprintf("エラー: 番号 %q が不正です（1〜%d を指定）", sel, len(choices))}
	}
	chosen := choices[idx-1]
	cfg.Workspace = chosen.workspace
	cfg.Profile = chosen.profile

	// 2. 実際に接続して確認する（ここが最初で唯一のネットワーク接続）。
	fmt.Printf("\n%s への接続を確認しています...\n", cfg.Host())
	sess, err := openSession(cfg)
	if err != nil {
		return err
	}

	// 3. 保存
	fc := config.Load()
	if err := config.Set(&fc, "workspace", sess.Client.Workspace()); err != nil {
		return err
	}
	fc.Profile = sess.Profile
	if err := config.Save(fc); err != nil {
		return err
	}
	path, _ := config.Path()

	fmt.Printf("\n保存しました: %s\n", path)
	fmt.Printf("  workspace=%s  profile=%s\n", sess.Client.Workspace(), sess.Profile)
	fmt.Printf("  接続確認: %s (%s)\n", sess.Auth.Team, sess.Auth.User)
	fmt.Println("\n準備完了。次のように使えます:")
	fmt.Println("  slack channels")
	fmt.Println("  slack search 'キーワード'")
	fmt.Println("  slack history '#general'")
	return nil
}
