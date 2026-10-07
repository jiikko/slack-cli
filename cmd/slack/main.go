// Command slack は Chrome にログイン済みの Slack セッションを流用して、
// **設定した 1 つのワークスペースだけ**を読み取る CLI。
//
// 読み取り専用: メッセージ投稿・編集・リアクション・ファイルアップロード等の
// 副作用のある API は実装しない（internal/slack/method.go の allowlist を参照）。
//
// 認証: Chrome の Cookie（d）を Keychain 経由で復号し、Local Storage から xoxc トークンを
// 取り出す。トークンの手動管理は不要。パスは HOME 基準で解決する。カレントディレクトリに
// 依存するのはローカル設定（.slack-cli.yml。internal/config/local.go）の読み書きだけ。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jiikko/slack-cli/internal/auth"
	"github.com/jiikko/slack-cli/internal/config"
	"github.com/jiikko/slack-cli/internal/slack"
)

// topUsage は `slack` / `slack --help` の出力。概要とサブコマンドの一覧だけを持ち、詳細は各サブコマンドの --help に置く。
const topUsage = `slack - Slack ワークスペースを読む CLI（読み取り専用 / Chrome のログインで認証）

使い方:  slack <サブコマンド> [オプション] [引数]
  Chrome にログイン済みの Slack のセッションを使い、設定した 1 つのワークスペースを検索・閲覧する。

サブコマンド:
  search      メッセージを検索する
  channels    チャンネルの一覧を出す
  history     チャンネルのメッセージを出す
  thread      スレッドの返信を出す
  users       ユーザーの一覧を出す
  whoami      接続中のユーザーとワークスペースを出す
  config      設定ファイル（config.yml / .slack-cli.yml）を表示・編集する
  setup       対話式で初期設定する（ワークスペース・Chrome のプロファイル）
  help        このヘルプ

各サブコマンドの詳細（オプション・終了コード・安全のための制約）:  slack <サブコマンド> --help
はじめて使うとき:  slack setup
`

// commonOptionsHelp は Slack に問い合わせるサブコマンドの help に共通のオプションの説明（正本はここだけ）。
const commonOptionsHelp = `
共通オプション:
  -workspace <name>  対象ワークスペースのサブドメイン。必須（` + config.EnvWorkspace + ` / .slack-cli.yml・config.yml の workspace でも可）
  -profile <name>    Chrome のプロファイル。既定 auto=自動検出（` + config.EnvProfile + `）
  -token <xoxc-...>  トークンを明示指定（` + config.EnvToken + `）。指定してもワークスペースの一致は検証する
  -json              JSON で出力
  優先順位: コマンドラインフラグ > 環境変数 > .slack-cli.yml（カレントディレクトリ） > config.yml > 既定
            （詳細は slack config --help。.slack-cli.yml で workspace / profile が決まったときは stderr に 1 行出る）
`

// commonTailHelp は Slack に問い合わせるサブコマンドの help の末尾に付ける、終了コードと安全のための制約（正本はここだけ）。
const commonTailHelp = `
終了コード: 0=成功 / 1=実行時エラー（認証切れ・ネットワーク等） / 2=使い方の誤り

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

// checkLocalConfig は、カレントディレクトリのローカル設定が読めないなら止める。
// 呼ぶのは parseArgs（フラグ解析が成功した後）と、フラグを持たない config / config get。
//
// 🚨 読めなかったローカル設定を黙って捨てて続行しない。共通の config.yml にある別のワークスペースで
// 動いてしまい、利用者は「このディレクトリの設定で読んだ」と思ったまま別の結果を受け取る。
//
// 🚨 「help だけは通す」を、引数を自前で走査して判定しない。flag パッケージの解析（値を取るフラグが
// 次の語を消費する等）を真似きれず、`channels -name -h` のように値の位置の -h で検査を抜けられた
// （issue 004 の敵対的レビュー 2 周目）。本物の解析の結果（ErrHelp）で help を見分ける。
func checkLocalConfig() error {
	err := config.LocalProblem()
	if err == nil {
		return nil
	}
	return &config.UsageError{Msg: fmt.Sprintf(
		"エラー: ローカル設定を読めないため中止しました。\n  %v\n"+
			"  ファイルを直すか削除してから、もう一度実行してください: %s", err, config.LocalPath())}
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
// 既定値は「環境変数 > 設定ファイル（.slack-cli.yml > config.yml） > 組み込み既定」で解決し、-flag 明示指定が最優先になる。
func registerCommon(fs *flag.FlagSet, cfg *config.Config) {
	ws, profile, count := config.Defaults()
	fs.StringVar(&cfg.Workspace, "workspace", ws, "対象ワークスペースのサブドメイン（必須）/ "+config.EnvWorkspace+" / .slack-cli.yml・config.yml の workspace")
	fs.StringVar(&cfg.Workspace, "w", ws, "-workspace の別名")
	fs.StringVar(&cfg.Profile, "profile", profile, "Chrome のプロファイル名。既定 auto（自動検出）/ "+config.EnvProfile)
	fs.StringVar(&cfg.Token, "token", os.Getenv(config.EnvToken), "xoxc トークンを明示指定（任意）/ "+config.EnvToken)
	fs.BoolVar(&cfg.JSON, "json", false, "機械可読な JSON で出力する")
	cfg.Count = count
}

// newFlagSet はサブコマンド用の FlagSet を作る。
//
// 🚨 flag パッケージ自身には何も出力させない。ContinueOnError の Parse は
// 失敗時に「エラー文を Output へ」+「Usage を呼ぶ」を自分で行うため、
// 既定のままだと --help で help が 2 回（Usage 経由と parseArgs 経由）、
// フラグの誤りで「flag の生エラー + help + こちらのエラー」が重なって出る。
// 出力は parseArgs に一本化する。
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	return fs
}

// parseArgs は Parse の結果を 3 つに分ける。
//
//   - --help: 明示的な要求なので stdout へ出して正常終了する（パイプで読める）
//   - フラグの誤り: usage は stderr（stdout に混ざるとパイプが壊れる）。rc=2
//   - 正常: カレントディレクトリのローカル設定が読めなければ止める（checkLocalConfig）。読めれば続行
func parseArgs(fs *flag.FlagSet, help string, args []string) (helpRequested bool, err error) {
	if e := fs.Parse(args); e != nil {
		if errors.Is(e, flag.ErrHelp) {
			fmt.Fprint(os.Stdout, help)
			return true, nil
		}
		return false, &config.UsageError{Msg: fmt.Sprintf(
			"エラー: %v\n使い方は  slack %s --help  を参照してください。", e, fs.Name())}
	}
	if err := checkLocalConfig(); err != nil {
		return false, err
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
//
// ローカル設定で workspace / profile が決まったなら、ここで stderr に 1 行出す
// （Slack に問い合わせるコマンドは全部ここを通る。stdout は -json の出力なので使わない）。
func openSession(cfg config.Config) (*slack.Session, error) {
	if n := config.LocalNotice(cfg.Workspace, cfg.Profile); n != "" {
		fmt.Fprintln(os.Stderr, n)
	}
	return slack.Resolve(context.Background(), cfg, nil, os.Stderr)
}

// splitListErr は一覧取得のエラーを振り分ける。
//
//   - 打ち切り（安全上限・has_more なのにカーソル無し）: 警告を stderr に出し、取得分を表示して rc=0
//   - 途中失敗（PartialError）: 取得分を表示してから after として返す（rc≠0。完了ではない）
//   - それ以外: err として返す（何も表示しない）
//
// 🚨 打ち切りを「エラーで終了（取得分を捨てる）」にも「無音で完全な一覧に見せる」にもしない。
// 途中失敗を rc=0 にしない（スクリプトから見て、欠けた結果が完了に見える）。
func splitListErr(err error) (after, fatal error) { return splitListErrTo(os.Stderr, err) }

func splitListErrTo(w io.Writer, err error) (after, fatal error) {
	switch {
	case err == nil:
		return nil, nil
	case slack.IsPartial(err):
		return err, nil
	case slack.IsTruncated(err):
		fmt.Fprintf(w, "警告: %v\n", err)
		return nil, nil
	default:
		return nil, err
	}
}

// finishList は一覧を出力し、出力後に返すべきエラー（after = 途中失敗）を返す。
// render は TSV の組み立て（JSON のときは呼ばない）。
func finishList[T any](asJSON bool, items []T, render func() string, after error) error {
	if asJSON {
		if err := printJSON(items); err != nil {
			return err
		}
		return after
	}
	if len(items) == 0 {
		fmt.Fprintln(os.Stderr, "0 件")
		return after
	}
	fmt.Print(render())
	return after
}
