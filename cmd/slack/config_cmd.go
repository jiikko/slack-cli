package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jiikko/slack-cli/internal/auth"
	"github.com/jiikko/slack-cli/internal/config"
	"github.com/jiikko/slack-cli/internal/output"
)

const configHelp = `slack config - 設定ファイル（config.yml / .slack-cli.yml）を表示・編集する

共通の設定:   $XDG_CONFIG_HOME/slack-cli/config.yml（未設定なら ~/.config/slack-cli/config.yml）
ローカル設定: カレントディレクトリの .slack-cli.yml（あれば読み込まれる。親ディレクトリはさかのぼらない）
              書いてあるキーだけが config.yml より優先される（書いていないキーは config.yml の値）。
              workspace / profile がそこで決まったときは stderr に 1 行出る。シンボリックリンク・自分以外が所有・グループか他人が
              書き込めるファイルとディレクトリは無視する（警告を出す）。
設定できるキー: workspace（別名 team） / profile / default_count
優先順位: コマンドラインフラグ > 環境変数 > .slack-cli.yml > config.yml > 組み込み既定

使い方:
  slack config                    現在の有効な設定と、その出所（env/file:<パス>/default）を表示
  slack config path [-local]      config.yml のパスを表示（-local: カレントディレクトリの .slack-cli.yml のパス）
  slack config set [-local] <k> <v>
                                  キーを設定して config.yml に保存（-local: .slack-cli.yml に指定したキーだけを保存）
  slack config get <k>            キーの有効な値（.slack-cli.yml と config.yml を合わせた値）を表示
  slack config init               ログイン済みのワークスペース/プロファイルを自動検出して config.yml に保存

例:
  slack config set workspace acme
  slack config set profile "Profile 3"
  slack config set default_count 50
  slack config set -local workspace other   # このディレクトリでだけ other を読む
  slack config init
`

func cmdConfig(args []string) error {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "", "show":
		if err := checkLocalConfig(); err != nil {
			return err
		}
		return configShow()
	case "path":
		return configPath(args[1:])
	case "get":
		if err := checkLocalConfig(); err != nil {
			return err
		}
		if len(args) < 2 {
			return &config.UsageError{Msg: "エラー: キー名を指定してください。\n使い方: slack config get <workspace|profile|default_count>"}
		}
		v, err := config.Get(config.Effective(), args[1])
		if err != nil {
			return &config.UsageError{Msg: "エラー: " + err.Error()}
		}
		fmt.Println(v)
		return nil
	case "set":
		return configSet(os.Stdout, os.Stderr, args[1:])
	case "init":
		return configInit(args[1:])
	case "-h", "--help", "help":
		fmt.Fprint(os.Stdout, configHelp)
		return nil
	default:
		return &config.UsageError{Msg: fmt.Sprintf("エラー: 不明なサブコマンド %q\n%s", sub, configHelp)}
	}
}

func configPath(args []string) error {
	fs := newFlagSet("config path")
	local := fs.Bool("local", false, "カレントディレクトリの .slack-cli.yml のパスを表示する")
	if done, err := parseArgs(fs, configHelp, args); err != nil || done {
		return err
	}
	// 🚨 -local なしの出力は config.yml のパス 1 行のまま変えない（$(slack config path) で使われる）。
	if *local {
		p := config.LocalPath()
		if p == "" {
			return fmt.Errorf("カレントディレクトリを特定できません")
		}
		fmt.Println(p)
		return nil
	}
	p, err := config.Path()
	if err != nil {
		return err
	}
	fmt.Println(p)
	return nil
}

func configShow() error {
	path, _ := config.Path()
	fc := config.Effective()
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
	switch {
	case config.LocalIgnored() != "":
		fmt.Printf("local file:  %s  (無視: %s)\n", config.LocalPath(), config.LocalIgnored())
	case config.LocalPresent():
		fmt.Printf("local file:  %s\n", config.LocalPath())
	}
	fmt.Println("有効な設定（コマンドラインフラグ指定時はそれが最優先）:")
	wsSrc = fileSource(wsSrc, "workspace")
	profileSrc = fileSource(profileSrc, "profile")
	countSrc = fileSource(countSrc, "default_count")
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

// refuseWriteIfBroken は config.yml を書き直す全経路（config set / config init / setup）の入口で呼ぶ。
//
// 🚨 読めなかったファイルを「読めたこと」にして上書きしない。
// 解析に失敗したまま書き直すと、ゼロ値に書いたキーだけが残り、他の設定が黙って消える。
func refuseWriteIfBroken() error {
	err := config.Problem()
	if err == nil {
		return nil
	}
	path, _ := config.Path()
	return &config.UsageError{Msg: fmt.Sprintf(
		"エラー: 設定ファイルを読めないため書き込みを中止しました。\n  %v\n"+
			"  ファイルを直すか削除してから、もう一度実行してください: %s", err, path)}
}

// ignoreLocalDefaults は、config.yml に保存するコマンド（config init / setup）で、フラグで明示されていない
// workspace / profile の初期値からローカル設定を外す（環境変数 > config.yml > 既定 に戻す）。
//
// 🚨 外さないと、cd した先の .slack-cli.yml（clone した repo の作者が書いたもの）の値が、検出を飛ばして
// そのまま config.yml に書き写され、どのディレクトリでも効く値になる。
func ignoreLocalDefaults(fs *flag.FlagSet, cfg *config.Config) {
	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	g := config.GlobalFile()
	if !explicit["workspace"] && !explicit["w"] {
		cfg.Workspace = config.ResolveDefault(config.EnvWorkspace, mustGet(g, "workspace"), "")
	}
	if !explicit["profile"] {
		cfg.Profile = config.ResolveDefault(config.EnvProfile, g.Profile, config.DefaultProfile)
	}
}

// fileSource は出所 "file" を、どのファイルかが分かる "file:<パス>" にする。
func fileSource(src, key string) string {
	if src != "file" {
		return src
	}
	if p := config.Origin(key); p != "" {
		return "file:" + p
	}
	return src
}

// warnShadowed は、config.yml に保存したキーがこのディレクトリではローカル設定に隠れるなら警告する。
func warnShadowed(w io.Writer, keys ...string) {
	for _, k := range keys {
		if config.LocalHas(k) {
			fmt.Fprintf(w, "警告: ローカル設定 %s に %s が書いてあるため、このディレクトリでは保存した値より %s の値が優先されます。\n",
				config.LocalPath(), k, config.LocalName)
		}
	}
}

// mustGet は config.Get の値だけを取り出す（キーは定数なのでエラーは起きない）。
func mustGet(fc config.File, key string) string {
	v, err := config.Get(fc, key)
	if err != nil {
		return ""
	}
	return v
}

// configSet は config set [-local] <key> <value> を実行する。
//
// -local のときはローカル設定単体の内容だけを読んで書く（共通の config.yml の値を写さない）。
// 共通の config.yml が壊れていても -local は書ける（ローカルしか読み書きしないため）。
func configSet(stdout, stderr io.Writer, args []string) error {
	fs := newFlagSet("config set")
	local := fs.Bool("local", false, "カレントディレクトリの .slack-cli.yml に保存する")
	if done, err := parseArgs(fs, configHelp, args); err != nil || done {
		return err
	}
	rest := fs.Args()
	if err := checkNoTrailingFlags(fs, rest); err != nil {
		return err
	}
	if !*local {
		if err := refuseWriteIfBroken(); err != nil {
			return err
		}
	}
	if len(rest) < 2 {
		return &config.UsageError{Msg: "エラー: キーと値を指定してください。\n使い方: slack config set [-local] <workspace|profile|default_count> <値>\n例:     slack config set workspace acme"}
	}
	key, value := rest[0], rest[1]

	if *local {
		fc := config.LocalFile()
		if err := config.Set(&fc, key, value); err != nil {
			return &config.UsageError{Msg: "エラー: " + err.Error()}
		}
		path, err := config.SaveLocal(fc)
		if err != nil {
			return err
		}
		saved, _ := config.Get(fc, key)
		fmt.Fprintf(stdout, "%s に保存しました: %s = %s\n", path, key, saved)
		return nil
	}

	fc := config.GlobalFile()
	if err := config.Set(&fc, key, value); err != nil {
		return &config.UsageError{Msg: "エラー: " + err.Error()}
	}
	if err := config.Save(fc); err != nil {
		return err
	}
	path, _ := config.Path()
	saved, _ := config.Get(fc, key)
	fmt.Fprintf(stdout, "%s に保存しました: %s = %s\n", path, key, saved)
	warnShadowed(stderr, key)
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
	ignoreLocalDefaults(fs, &cfg)
	// 検出・接続の前に断る（接続してから保存で失敗すると、確認 1 回分が無駄になる）。
	if err := refuseWriteIfBroken(); err != nil {
		return err
	}

	fc := config.GlobalFile()

	// 1. workspace が未設定なら、ローカルの痕跡から候補を出す。
	if strings.TrimSpace(cfg.Workspace) == "" {
		found, issues, err := scanProfiles(true)
		if err != nil {
			return err
		}
		var hints []auth.WorkspaceHint
		var profile string
		if len(found) > 0 {
			hints, profile = found[0].hints, found[0].profile
		}
		switch {
		case len(hints) == 0:
			return fmt.Errorf(
				"ログイン済みのワークスペースを検出できませんでした。\n"+
					"  %s で対象の Slack ワークスペースを開いてから、もう一度実行してください。\n"+
					"  分かっている場合は直接指定できます: slack config set workspace <name>%s", auth.ChromeName, auth.IssueNote(issues))
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
	warnShadowed(os.Stderr, "workspace", "profile")
	fmt.Printf("接続確認: %s (%s)\n", sess.Auth.Team, sess.Auth.User)
	return nil
}

// profileHints は 1 プロファイル分のワークスペース候補。
type profileHints struct {
	profile string
	email   string
	hints   []auth.WorkspaceHint
}

// scanProfiles は全プロファイルを走査してワークスペース候補を集める（ローカルのみ）。
// firstOnly なら候補が見つかった最初のプロファイルで止める。
//
// 🚨 読み取りの失敗（アクセス拒否・読めないファイル）は記録して次のプロファイルへ進む。
// 止めると、1 プロファイルだけ読めない（chmod 000 / sudo で起動した Chrome が root 所有にした）
// ときに、後ろの正常なプロファイルが使えない。握り潰すと、全滅したときに
// 「ログイン済みのプロファイルが無い」という別の案内に化ける。全滅時は IssueNote を添えること。
//
// 🚨 EnvError（作業領域の異常など、全プロファイル共通の問題）だけはその場で返す。
// 捨てると全プロファイルが同じ理由で失敗し、「ログインしてから」という別の案内に化ける。
func scanProfiles(firstOnly bool) ([]profileHints, []auth.ProfileIssue, error) {
	var out []profileHints
	var issues []auth.ProfileIssue
	for _, p := range auth.ListProfiles() {
		hints, err := auth.DiscoverWorkspaces(p.Dir)
		if err != nil {
			if auth.IsEnvError(err) {
				return nil, nil, err
			}
			if is, ok := auth.AsProfileIssue(p.Dir, err); ok {
				issues = append(issues, is)
			}
			continue // Local Storage が無い等は「候補なし」
		}
		if len(hints) == 0 {
			continue
		}
		out = append(out, profileHints{profile: p.Dir, email: p.Email, hints: hints})
		if firstOnly {
			break
		}
	}
	return out, issues, nil
}

// printJSON は JSON 出力の共通口。
func printJSON(v any) error { return output.PrintJSON(os.Stdout, v) }
