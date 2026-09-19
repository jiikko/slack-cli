package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/jiikko/slack-cli/internal/auth"
	"github.com/jiikko/slack-cli/internal/config"
	"github.com/jiikko/slack-cli/internal/output"
)

const configHelp = `slack config - 設定ファイル(config.yml)を表示・編集する

config.yml の場所: $XDG_CONFIG_HOME/slack-cli/config.yml（未設定なら ~/.config/slack-cli/config.yml）
設定できるキー: workspace（別名 team） / profile / default_count
優先順位: コマンドラインフラグ > 環境変数 > config.yml > 組み込み既定

使い方:
  slack config              現在の有効な設定と、その出所（flag/env/file/default）を表示
  slack config path         config.yml のパスを表示
  slack config set <k> <v>  キーを設定して保存（例: slack config set workspace acme）
  slack config get <k>      config.yml のキーの値を表示
  slack config init         ログイン済みのワークスペース/プロファイルを自動検出して保存

例:
  slack config set workspace acme
  slack config set profile "Profile 3"
  slack config set default_count 50
  slack config init
`

func cmdConfig(args []string) error {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "", "show":
		return configShow()
	case "path":
		p, err := config.Path()
		if err != nil {
			return err
		}
		fmt.Println(p)
		return nil
	case "get":
		if len(args) < 2 {
			return &config.UsageError{Msg: "エラー: キー名を指定してください。\n使い方: slack config get <workspace|profile|default_count>"}
		}
		v, err := config.Get(config.Load(), args[1])
		if err != nil {
			return &config.UsageError{Msg: "エラー: " + err.Error()}
		}
		fmt.Println(v)
		return nil
	case "set":
		// 🚨 読めなかったファイルを「読めたこと」にして上書きしない。
		// 解析に失敗したまま書き直すと、他の設定が黙って消える。
		if err := config.Problem(); err != nil {
			path, _ := config.Path()
			return &config.UsageError{Msg: fmt.Sprintf(
				"エラー: 設定ファイルを読めないため書き込みを中止しました。\n  %v\n"+
					"  ファイルを直すか削除してから、もう一度実行してください: %s", err, path)}
		}
		if len(args) < 3 {
			return &config.UsageError{Msg: "エラー: キーと値を指定してください。\n使い方: slack config set <workspace|profile|default_count> <値>\n例:     slack config set workspace acme"}
		}
		return configSet(args[1], args[2])
	case "init":
		return configInit(args[1:])
	case "-h", "--help", "help":
		fmt.Fprint(os.Stdout, configHelp)
		return nil
	default:
		return &config.UsageError{Msg: fmt.Sprintf("エラー: 不明なサブコマンド %q\n%s", sub, configHelp)}
	}
}

func configShow() error {
	path, _ := config.Path()
	fc := config.Load()
	exists := false
	if _, err := os.Stat(path); err == nil {
		exists = true
	}

	ws, wsSrc := config.ResolveDefaultSource(config.EnvWorkspace, mustGet(fc, "workspace"), "")
	profile, profileSrc := config.ResolveDefaultSource(config.EnvProfile, fc.Profile, config.DefaultProfile)
	count, countSrc := config.ResolveDefaultInt("", fc.DefaultCount, config.DefaultCount)

	note := ""
	if !exists {
		note = "  (未作成)"
	}
	fmt.Printf("config file: %s%s\n", path, note)
	fmt.Println("有効な設定（コマンドラインフラグ指定時はそれが最優先）:")
	if ws == "" {
		ws, wsSrc = "(未設定)", "none"
	}
	// 🚨 値の列で全角（"(未設定)"）と半角（"acme"）が縦に並ぶため、桁揃えはしない。
	// 表示幅を合わせても全角セルの中で半角文字が左へ寄り、目には揃わない。
	fmt.Printf("  %-14s %s (%s)\n", "workspace:", ws, wsSrc)
	fmt.Printf("  %-14s %s (%s)\n", "profile:", profile, profileSrc)
	fmt.Printf("  %-14s %d (%s)\n", "default_count:", count, countSrc)
	// 🚨 トークンは値を出さない（設定されているかだけを示す）。
	tokenState := "(未設定)"
	if t := os.Getenv(config.EnvToken); t != "" {
		tokenState = auth.Mask(t)
	}
	fmt.Printf("  %-14s %s (%s)\n", "token:", tokenState, "env:"+config.EnvToken)

	if ws == "(未設定)" {
		fmt.Println("\nヒント: まずは  slack setup  （対話式）か  slack config init  で初期設定できます。")
	}
	return nil
}

// mustGet は config.Get の値だけを取り出す（キーは定数なのでエラーは起きない）。
func mustGet(fc config.File, key string) string {
	v, err := config.Get(fc, key)
	if err != nil {
		return ""
	}
	return v
}

func configSet(key, value string) error {
	fc := config.Load()
	if err := config.Set(&fc, key, value); err != nil {
		return &config.UsageError{Msg: "エラー: " + err.Error()}
	}
	if err := config.Save(fc); err != nil {
		return err
	}
	path, _ := config.Path()
	saved, _ := config.Get(fc, key)
	fmt.Printf("%s に保存しました: %s = %s\n", path, key, saved)
	return nil
}

// configInit はログイン済みのワークスペースとプロファイルを検出して保存する。
//
// 🚨 検出はローカル（Chrome の Local Storage の痕跡）だけで行い、ネットワークには
// 出ない。「どのワークスペースにログインしているか」を調べるために各ワークスペースへ
// API を投げると、仕様 §4（設定した対象以外に接続しない）を自分で破ることになる。
// 接続が起きるのは workspace が確定した後の検証（auth.test）だけ。
func configInit(args []string) error {
	var cfg config.Config
	fs := newFlagSet("config init")
	registerCommon(fs, &cfg)
	if done, err := parseArgs(fs, configHelp, args); err != nil || done {
		return err
	}

	fc := config.Load()

	// 1. workspace が未設定なら、ローカルの痕跡から候補を出す。
	if strings.TrimSpace(cfg.Workspace) == "" {
		hints, profile := discoverWorkspaces()
		switch {
		case len(hints) == 0:
			return fmt.Errorf(
				"ログイン済みのワークスペースを検出できませんでした。\n"+
					"  %s で対象の Slack ワークスペースを開いてから、もう一度実行してください。\n"+
					"  分かっている場合は直接指定できます: slack config set workspace <name>", auth.ChromeName)
		case len(hints) == 1:
			cfg.Workspace = hints[0].Domain
			fmt.Printf("ワークスペースを検出しました: %s（プロファイル %s）\n", cfg.Workspace, profile)
		default:
			fmt.Println("複数のワークスペースが見つかりました:")
			for _, h := range hints {
				fmt.Printf("  %-24s (%s 内の出現 %d 回)\n", h.Domain, profile, h.Hits)
			}
			return &config.UsageError{Msg: fmt.Sprintf(
				"エラー: 対象を 1 つ選んでください。\n  slack config set workspace %s", hints[0].Domain)}
		}
	}

	// 2. 実際に接続して検証し、使われたプロファイルを保存する。
	sess, err := openSession(cfg)
	if err != nil {
		return err
	}
	if err := config.Set(&fc, "workspace", sess.Client.Workspace()); err != nil {
		return err
	}
	fc.Profile = sess.Profile
	if err := config.Save(fc); err != nil {
		return err
	}
	path, _ := config.Path()
	fmt.Printf("%s に保存しました: workspace = %s / profile = %s\n", path, sess.Client.Workspace(), sess.Profile)
	fmt.Printf("接続確認: %s (%s)\n", sess.Auth.Team, sess.Auth.User)
	return nil
}

// discoverWorkspaces は全プロファイルを走査して、ワークスペース候補を集める。
// 返り値の 2 つ目は、候補が見つかったプロファイル名。
func discoverWorkspaces() ([]auth.WorkspaceHint, string) {
	for _, p := range auth.ListProfiles() {
		hints, err := auth.DiscoverWorkspaces(p.Dir)
		if err != nil || len(hints) == 0 {
			continue
		}
		return hints, p.Dir
	}
	return nil, ""
}

// printJSON は JSON 出力の共通口。
func printJSON(v any) error { return output.PrintJSON(os.Stdout, v) }
