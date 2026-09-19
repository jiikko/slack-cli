// Command slack は Chrome にログイン済みの Slack セッションを流用して、
// **設定した 1 つのワークスペースだけ**を読み取る CLI。
//
// 読み取り専用: メッセージ投稿・編集・リアクション・ファイルアップロード等の
// 副作用のある API は実装しない（internal/slack/method.go の allowlist を参照）。
//
// 認証: Chrome の Cookie（d）を Keychain 経由で復号し、Local Storage から xoxc トークンを
// 取り出す。トークンの手動管理は不要。パスはすべて HOME 基準で解決し、カレント
// ディレクトリに一切依存しない。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/jiikko/slack-cli/internal/auth"
	"github.com/jiikko/slack-cli/internal/config"
	"github.com/jiikko/slack-cli/internal/slack"
)

const topUsage = `slack - Slack ワークスペースを読む CLI（読み取り専用 / Chrome cookie 認証）

概要:
  Chrome にログイン済みの Slack セッションを流用して、設定した 1 つのワークスペースを
  検索・閲覧する。トークンの手動管理は不要。書き込み系の操作は一切持たない。

サブコマンド:
  search      メッセージ検索（search.messages）
  channels    チャンネル一覧（conversations.list）
  history     チャンネルのメッセージ取得（conversations.history）
  thread      スレッドの返信取得（conversations.replies）
  users       ユーザー一覧（users.list）
  whoami      接続中のユーザー/ワークスペースを表示（auth.test）
  config      設定ファイル(config.yml)の表示・編集
  setup       対話式の初期セットアップ
  help        このヘルプ

各サブコマンドの詳細:  slack <サブコマンド> --help   （例: slack search --help）

共通オプション:
  -workspace <name>  対象ワークスペースのサブドメイン。必須（` + config.EnvWorkspace + ` / config.yml workspace）
  -profile <name>    Chrome のプロファイル。既定 auto=自動検出（` + config.EnvProfile + `）
  -token <xoxc-...>  トークンを明示指定（` + config.EnvToken + `）。指定してもワークスペース一致は検証する
  -json              JSON で出力

設定の優先順位: コマンドラインフラグ > 環境変数 > config.yml > 既定
  はじめての場合:  slack setup   （対話式にワークスペースとプロファイルを設定）

終了コード: 0=成功 / 1=実行時エラー(認証切れ・ネットワーク等) / 2=使い方の誤り

安全のための制約:
  - 設定した workspace 以外のホストへは API を投げない。
  - 読み取り専用メソッドの allowlist 外は呼べない（型と実行時の 2 段で拒否）。
  - cookie / トークンの生値は表示・保存しない。Chrome からの一時コピーは終了時に必ず消す。
`

func main() {
	// Chrome の Cookie DB / Local Storage の一時コピーを、Ctrl-C でも残さないようにする。
	auth.InstallCleanupOnSignal()
	defer auth.RunAllCleanups()

	// 🚨 前回の実行が SIGKILL 等で残した一時コピーを、**起動時に**片付ける。
	// ここに置かないと「Chrome から資格情報を読む経路を通ったときだけ」の掃除になり、
	// slack help / slack config では残骸が残ったままになる（実測で踏んだ）。
	auth.SweepStaleTempDirs()

	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, topUsage)
		os.Exit(2)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	var err error
	switch cmd {
	case "search":
		err = cmdSearch(args)
	case "channels":
		err = cmdChannels(args)
	case "history":
		err = cmdHistory(args)
	case "thread":
		err = cmdThread(args)
	case "users":
		err = cmdUsers(args)
	case "whoami", "auth-test":
		err = cmdWhoami(args)
	case "config":
		err = cmdConfig(args)
	case "setup":
		err = cmdSetup(args)
	case "help", "-h", "--help":
		fmt.Fprint(os.Stdout, topUsage)
		return
	default:
		fmt.Fprintf(os.Stderr, "不明なコマンド: %q\n\n%s", cmd, topUsage)
		os.Exit(2)
	}

	if err != nil {
		// 一時コピーを残さずに終了する（os.Exit は defer を走らせない）。
		auth.RunAllCleanups()
		code := exitCodeFor(err)
		if code == 2 {
			fmt.Fprintln(os.Stderr, err.Error()) // 使い方の誤りはメッセージをそのまま
		} else {
			fmt.Fprintln(os.Stderr, "エラー: "+err.Error())
		}
		os.Exit(code)
	}
}

// exitCodeFor はエラーから終了コードを決める（使い方の誤り=2 / 実行時=1）。
func exitCodeFor(err error) int {
	if err == nil {
		return 0
	}
	var ue *config.UsageError
	if errors.As(err, &ue) {
		return 2
	}
	return 1
}

// registerCommon は全サブコマンド共通のフラグを登録する。
// 既定値は「環境変数 > config.yml > 組み込み既定」で解決し、-flag 明示指定が最優先になる。
func registerCommon(fs *flag.FlagSet, cfg *config.Config) {
	ws, profile, count := config.Defaults()
	fs.StringVar(&cfg.Workspace, "workspace", ws, "対象ワークスペースのサブドメイン（必須）/ "+config.EnvWorkspace+" / config.yml workspace")
	fs.StringVar(&cfg.Workspace, "w", ws, "-workspace の別名")
	fs.StringVar(&cfg.Profile, "profile", profile, "Chrome のプロファイル名。既定 auto（自動検出）/ "+config.EnvProfile)
	fs.StringVar(&cfg.Token, "token", os.Getenv(config.EnvToken), "xoxc トークンを明示指定（任意）/ "+config.EnvToken)
	fs.BoolVar(&cfg.JSON, "json", false, "機械可読な JSON で出力する")
	cfg.Count = count
}

// newFlagSet は共通の Usage（サブコマンド詳細 help）を設定した FlagSet を作る。
func newFlagSet(name, help string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprint(os.Stderr, help) }
	return fs
}

// parseArgs は Parse の結果を 3 つに分ける。
//
//   - --help: 明示的な要求なので stdout へ出して正常終了する（パイプで読める）
//   - フラグの誤り: usage は stderr（stdout に混ざるとパイプが壊れる）。rc=2
//   - 正常: そのまま続行
func parseArgs(fs *flag.FlagSet, help string, args []string) (helpRequested bool, err error) {
	if e := fs.Parse(args); e != nil {
		if errors.Is(e, flag.ErrHelp) {
			fmt.Fprint(os.Stdout, help)
			return true, nil
		}
		return false, &config.UsageError{Msg: "エラー: " + e.Error()}
	}
	return false, nil
}

// checkNoTrailingFlags は「引数の後ろに置かれたフラグ」を検出する。
//
// `slack search 'キーワード' -c text` と書くと flag パッケージはそこで解析を止め、
// -c text が検索クエリの一部になる。Slack 側では単に 0 件として返るため、
// 原因がフラグの位置だと分からない。ここでは「フラグとして定義されている名前と
// 一致する語」だけを弾く（`-語` のような除外検索を誤検出しないため）。
func checkNoTrailingFlags(fs *flag.FlagSet, args []string) error {
	defined := map[string]bool{}
	fs.VisitAll(func(f *flag.Flag) { defined[f.Name] = true })
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			continue
		}
		name := strings.TrimLeft(a, "-")
		if i := strings.IndexByte(name, '='); i >= 0 {
			name = name[:i]
		}
		if defined[name] {
			return &config.UsageError{Msg: fmt.Sprintf(
				"エラー: %q はフラグとして解釈されませんでした（引数の一部になっています）。\n"+
					"  フラグは引数より前に置いてください。\n"+
					"  正: slack %s %s <値> '<引数>'\n"+
					"  誤: slack %s '<引数>' %s <値>", a, fs.Name(), a, fs.Name(), a)}
		}
	}
	return nil
}

// openSession は設定から接続を解決する（ワークスペース限定・allowlist は internal/slack 側）。
func openSession(cfg config.Config) (*slack.Session, error) {
	return slack.Resolve(context.Background(), cfg, nil, os.Stderr)
}
