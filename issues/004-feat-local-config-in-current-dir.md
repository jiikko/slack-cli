# 004 (feat): カレントディレクトリのローカル設定 `.slack-cli.yml` を共通の config.yml より優先する

起票日: 2026-10-07

## 概要

共通の設定は今どおり `$XDG_CONFIG_HOME/slack-cli/config.yml`（未設定なら `~/.config/slack-cli/config.yml`）に置く。
カレントディレクトリに `.slack-cli.yml` があれば、そこに書いてあるキーを共通の設定より優先する。
あわせて、ローカル設定に書き込む `config set -local` を用意する。

🚨 この変更は現在の設計方針「カレントディレクトリに依存しない」を変える
（`internal/config/config.go` の package doc は「HOME / XDG 基準」、`cmd/slack/main.go` の冒頭コメントは「HOME 基準」と書いている）。
方針を変えることはユーザーが決めた（2026-10-07 のセッション）。

## 現状（2026-10-07 時点のコードで確認）

- 読み込み: `config.Load()` が `config.Path()` の 1 ファイルだけを `sync.Once` で読む。読めない (ReadFile 失敗) ときは黙ってゼロ値。
  YAML の解析に失敗したときだけ `loadErr` に入り、`config.Problem()` で返る（1 ファイル分の変数しかない）
- 実効値の入口: `cmd/slack/main.go` の `registerCommon` が `config.Defaults()` を呼び、全サブコマンドのフラグ既定値にする
- 書き込み: `config.Save()` の書き込み先は `config.Dir()` に固定。`config set` / `config init` / `setup` に
  書き込み先を変えるオプションは無い
- 🚨 `configSet` / `configInit` / `setup` はどれも `config.Load()` の結果に値を足してそのまま `Save` する。
  `Load` を「ローカルと共通を合わせた値」に変えると、`Save` が合わせた値を書いてしまう
- 🚨 **既存の不具合**: `Problem()` を見て書き込みを拒否しているのは `config set` だけ。`config init`（`configInit`）と
  `setup` は見ないので、共通の config.yml が YAML として壊れていると、空の `File` に workspace / profile だけを入れて上書きし、
  他のキーが消える。本 issue の前提として先に直す（下の「対応方針」1）
- 設定元の表示: `configShow()`（`cmd/slack/config_cmd.go`）は `env:KEY` / `file` / `default`（workspace 未設定時は `none`）を出す。`file` は 1 種類
- `slack config path` は共通の config.yml のパスだけを出す
- `config set` は引数を位置で読む（`args[1]` / `args[2]`）。`Problem()` の確認は引数の個数の確認より先
- 接続先のホストは `ValidateWorkspace` で接続時に再検証される（`internal/slack/client.go` / `internal/slack/resolve.go`）。
  workspace の値でホストを乗っ取ることはできない（レビューで壊せなかった）

## 仕様

### 読み込み

- ファイル名は `.slack-cli.yml`。中身は config.yml と同じ YAML（`workspace` / `team` / `profile` / `default_count`）
- 探すのは**カレントディレクトリだけ**。親ディレクトリへはさかのぼらない
- **キーごとに上書き**する。ローカルに書いたキーだけが優先され、書いていないキーは共通側の値を使う
- 優先順位: コマンドラインフラグ > 環境変数 > **ローカル `.slack-cli.yml`** > 共通 config.yml > 組み込み既定
- `team` を `workspace` の別名として扱う規則は、ファイルごとに適用してから合わせる
  （ローカルの `team:` は共通の `workspace:` より優先する。同じファイルに両方あれば今どおり `workspace` が優先）
- 「未設定」は今どおり空文字列 / 0。ローカルの `default_count` が 0 以下なら未設定として扱い、共通側へ落ちる
  （負の値で共通の値を隠したうえで組み込み既定に落ちる、ということは起こさない）
- **共通とローカルの読み込み結果と `Problem` をファイルごとに分けて持つ**（今の `loadCached` / `loadErr` は 1 組だけ）

### 壊れたファイルの扱い

| 状態 | 読み込み（全コマンド） | `config set`（共通に書く） | `config set -local` |
|---|---|---|---|
| ローカルが壊れている | 警告を出して実行を止める（ローカルの値を黙って捨てると、共通側の別のワークスペースで動いてしまう） | 拒否 | 拒否 |
| 共通が壊れている | 今どおり警告を出して続行 | 拒否 | 書ける（ローカルだけを読み書きするため） |

### 安全面

clone した repo などに置かれた `.slack-cli.yml` で、`workspace` / `profile` を切り替えられるようになる。
README「安全のための制約」の「ワークスペース限定」は**設定した**ワークスペースに限るという意味で、ローカル設定もこの「設定」に入る。
したがって、ローカル設定に `workspace: other` と書いてあれば、利用者がそのワークスペースに Chrome でログインしている限り、
auth.test の照合も通り、`other` が読まれる。`profile` を書けば別の Chrome プロファイル（別アカウント）の cookie が使われる。

- 脅威の評価: 読んだ内容が出るのは利用者自身の端末で、接続先は `*.slack.com` に限られる（ホストは再検証される）。
  ファイルを置いた第三者が読み取り結果を受け取る経路は、このツールの中には無い。危ないのは「別のワークスペースを読んでいると
  気づかずに、出力をスクリプトや他のツールへ流す」こと
- ローカル設定で workspace / profile が決まったときは、stderr に 1 行（例: `ローカル設定 ./.slack-cli.yml を使用: workspace=other`）を出す。
  `-json` の stdout を汚さないよう stdout には出さない。出すのはネットワークへ出るコマンドと `slack config` だけ（`config path` / `--help` では出さない）
- **信頼確認の仕組み（`direnv allow` のような形）は入れない**（2026-10-07 ユーザー決定。stderr の通知だけにする）。
  パイプにつないで使うと stderr の 1 行は見落とされやすいが、上の脅威の評価のとおり、読み取り結果が第三者へ渡る経路は無い。
  この理由と「出力を外部へ流す機能を足すときは再評価する」を、ローカル設定を読む関数の直近にコメントで残す
- ファイルの検査（読み込み）:
  - `O_NOFOLLOW` で開き、**開いた fd を fstat して**検査する（検査してからパスで開き直さない。TOCTOU を作らない）。
    `.slack-cli.yml` がシンボリックリンクなら開けないので、無視して警告する
  - 通常のファイルでない、所有者が自分でない、グループか他人が書き込める、のどれかなら無視して警告する
  - カレントディレクトリ自体が自分の所有でない、またはグループか他人が書き込めるなら（`/tmp` 直下など）、ローカル設定は読まない
- ファイルの検査（`-local` の書き込み）:
  - 既存のファイルにも読み込みと同じ検査をする。検査に通らないファイルには書かない（`os.WriteFile` はシンボリックリンクをたどり、
    0600 も新規作成のときしか効かないため）
  - 同じディレクトリに一時ファイルを作り、書いてから rename で置き換える

### 書き込み（`-local`）

- **`-local` は `config set` だけに足す**。付けたときはカレントディレクトリの `.slack-cli.yml` に書き、付けないときは今どおり共通側に書く
- `config init` / `setup` には足さない。どちらも自動検出した `profile`（マシンごとに違う Chrome プロファイル名）を必ず書くので、
  ローカルに書くと repo に commit されやすい。必要になったら workspace だけを書く形で別に検討する
- `-local` で書くのは**指定したキーだけ**。ローカルのファイルだけを読んでキーを更新して書く（共通と合わせた値は書かない。
  共通側の値までローカルに写すと、後から共通側を変えてもローカルの古い値が優先され続ける）
- そのために `Load` を「合わせた実効値」と「ファイル単体の内容」に分け、`Save` は必ずファイル単体の内容に対して行う
  （`configSet` / `configInit` / `setup` の 3 か所すべて）
- 書き込んだ後に、書いたファイルのパスを表示する（今の「<path> に保存しました: key = value」と同じ形）
- `-local` なしの `config set` で書いたキーがローカル設定で上書きされているときは、「ローカル設定 ./.slack-cli.yml が優先されるため、
  このディレクトリでは反映されません」と警告する
- ローカルからキーを消すコマンドは今回は作らない（ファイルを直接編集する）
- `config set` の引数の解析を直し、`config set -local workspace foo` を受けられるようにする

### 表示

- `slack config` は、ローカルと共通を合わせた実効値をキーごとに出し、設定元を `file:<ローカルのパス>` /
  `file:<共通のパス>` に分ける。ローカル設定のファイルがあれば（無視した場合はその理由も）冒頭に出す
- `slack config get` はローカルと共通を合わせた実効値を返す
- `slack config path` はローカル設定があればそのパスも出す（出し方は実装時に決める）

### help・メッセージの修正箇所（2026-10-07 に grep で列挙）

`config.yml` / `優先順位` を grep して、利用者に見える文言を挙げた。どれもローカル設定と `-local` を反映する。

| 箇所 | 今の文言 | 直す内容 |
|---|---|---|
| `cmd/slack/config_cmd.go` の `configHelp` | 場所は config.yml だけ / 優先順位にローカルが無い / `path`・`get` は「config.yml の」 | `.slack-cli.yml`（カレントディレクトリだけ・キーごとに上書き）の説明を足す。優先順位にローカルを入れる。`set -local` の使い方と例、`get` は合わせた実効値を返すこと、`path` の出力を書く。ローカル設定を使うと stderr に通知が出ることも書く |
| 同 `config set` の引数エラー（`使い方: slack config set …`） | `-local` が無い | `slack config set [-local] <key> <値>` にする |
| `cmd/slack/main.go` の `commonOptionsHelp` | `-workspace` の説明が「config.yml の workspace でも可」 / 優先順位にローカルが無い | 「.slack-cli.yml / config.yml の workspace でも可」と、ローカル入りの優先順位にする（全サブコマンドの help がここを使う） |
| `cmd/slack/main.go` の `registerCommon` のフラグの説明（`-workspace`） | `config.yml workspace` | ローカル設定も書く |
| `cmd/slack/search.go` の `-n` の説明 | 「既定は config.yml の default_count」 | ローカル設定も書く |
| `cmd/slack/setup.go` の `setupHelp` | 「config.yml に保存する」 | 共通の config.yml に保存し、`-local` は無いこと、ローカル設定があるディレクトリでは保存した値がそちらに隠れうることを書く |
| `cmd/slack/main.go` の `topUsage` の `config` 行 | 「設定ファイル（config.yml）」 | 1 行のままローカル設定も含む表現にする。root には優先順位などの詳細を書かない（`help_layout_test.go` の `TestTopUsageIsSummaryOnly` が守る） |
| `internal/slack/resolve.go` のプロファイル固定時のエラー | 「-profile / config.yml で固定されているため」 | どの設定元で固定されたか（フラグ / 環境変数 / ローカル / 共通）を出す |

- help の文言は `help_layout_test.go` に「`commonOptionsHelp` と `configHelp` が `.slack-cli.yml` に触れている」検査を足して固定する

### README の修正箇所（2026-10-07 に grep で列挙）

実装と同じ commit で直す（実装より先に書くと、まだ無い機能を説明することになる）。

| 節 | 直す内容 |
|---|---|
| 「設定ファイル」 | **カレントディレクトリに `.slack-cli.yml` があれば読み込まれ、書いてあるキーが共通の config.yml より優先される**ことを、共通の config.yml の場所の直後に書く。補足として次も書く: カレントディレクトリだけを見る（親はさかのぼらない）/ キーごとに上書き（書いていないキーは共通側）/ 書式は config.yml と同じ / `slack config set -local <key> <値>` で書ける / 使ったときは stderr に 1 行出る / シンボリックリンク・自分以外の所有・グループか他人が書き込めるファイルとディレクトリは無視される。ローカル設定の例（`workspace` だけを書いた YAML）を 1 つ載せる |
| 同節の優先順位 | **コマンドラインフラグ > 環境変数 > `.slack-cli.yml` > config.yml > 既定値** |
| 同節の資格情報の注記 | 「config.yml」を「config.yml / `.slack-cli.yml`」にする |
| 「安全のための制約」1. ワークスペース限定 | 「設定した workspace」にはローカル設定も含まれること、clone した repo の `.slack-cli.yml` で読む対象のワークスペース・プロファイルが変わりうること、通知は stderr の 1 行だけで信頼確認は無いことを書く |
| 構成（`internal/config/` の行） | 「config.yml / `.slack-cli.yml` の読み書きと優先順位解決」にする |

### 変えないもの

- 自動検出したプロファイルのキャッシュ（`profileCachePath`。`internal/slack/resolve.go` が読み書きする）は workspace ごとのキーなので変更しない
- 資格情報（cookie / トークン）はローカル設定にも保存しない（キーを用意しない）

## 対応方針（順序）

1. 既存の不具合を先に直す: `config init` / `setup` も `Problem()` を見て、壊れた共通の config.yml を上書きしない（単独 commit、回帰テストつき）
2. `Load` / `Problem` をファイルごとに分け、`Save` をファイル単体の内容に対して行う形にする（挙動は変えない refactor）
3. ローカル設定の読み込み・検査・表示
4. `config set -local`

## 受け入れ条件

- [ ] 共通の config.yml が壊れているとき、`config init` / `setup` は書き込みを拒否する
- [ ] ローカル設定があるディレクトリでは、ローカル側の `workspace` が使われる
- [ ] 親ディレクトリにある `.slack-cli.yml` は読まれない
- [ ] ローカルに無いキーは共通側の値になる
- [ ] ローカルの `team:` が共通の `workspace:` より優先される
- [ ] フラグと環境変数は今どおりローカルより優先される
- [ ] `slack config` に、どの値がどのファイルから来たかが出る
- [ ] `-local` を付けない `config set` は共通の config.yml に書き、ローカル設定のファイルは変わらない。ローカルで上書きされているキーなら警告が出る
- [ ] `slack config set -local workspace foo` を実行すると `./.slack-cli.yml` に `workspace: foo` だけが書かれ、共通の config.yml は変わらない
- [ ] 共通の config.yml だけが壊れているとき、`config set -local` は書ける
- [ ] ローカルのファイルが壊れているとき、読み込みは警告を出して止まり、`config set` / `config set -local` は拒否する
- [ ] シンボリックリンク / 所有者が自分でない / グループか他人が書き込めるローカル設定は無視され、警告が出る（`-local` の書き込みも行わない）
- [ ] グループか他人が書き込めるカレントディレクトリではローカル設定を読まない
- [ ] ローカル設定を使ったことが stderr に出て、stdout（`-json` を含む）には出ない
- [ ] 「help・メッセージの修正箇所」の表の全行を直し、`help_layout_test.go` の検査で固定する
- [ ] 「README の修正箇所」の表の全行を直す（ローカル設定があれば読み込まれることが「設定ファイル」節に書いてある）
- [ ] package doc と `main.go` の冒頭コメントを新しい方針に合わせる

## 関連ファイル

- `internal/config/config.go`（`Load` / `Save` / `Path` / `Dir` / `Problem` / `Defaults`）と `internal/config/config_test.go`
- `cmd/slack/config_cmd.go`（`cmdConfig` / `configShow` / `configSet` / `configInit`）
- `cmd/slack/setup.go`
- `cmd/slack/main.go`（冒頭コメントと `registerCommon`）
- `README.md`「設定ファイル」節・「安全のための制約」節

## 反証レビュー（2026-10-07）

codex は利用上限のため使えず、観点を分けた読み取り専用のサブエージェント 2 体（事実確認 / 仕様の穴・安全面）で代替した。

- 採用: 実効値の入口 `registerCommon` と関連ファイルの漏れ / `Load` の結果をそのまま `Save` する 3 か所が `-local` の仕様と衝突する /
  `config init`・`setup` が `Problem()` を見ない既存の不具合 / 壊れたファイルの扱いが未定義 / シンボリックリンク・TOCTOU・
  カレントディレクトリの権限・書き込み時の検査 / `setup`・`init` の `-local` がマシン固有の profile を書く / 負の `default_count` /
  共通に書いたキーがローカルで隠れる警告 / main.go の冒頭コメントは「HOME 基準」で XDG とは書いていない / 設定元に `none` もある /
  受け入れ条件の抜け
- 一部採用: 「ローカル設定で読むワークスペースが変えられる」の指摘は初版にも書いてあったが、`profile` で別アカウントの cookie が
  使われる点と、パイプ運用で stderr が見落とされる点を追記した。信頼確認の仕組みはユーザーの判断で入れない（stderr 通知のみ）
- 壊せなかった: workspace の値によるホストの乗っ取り（接続時に `ValidateWorkspace` で再検証される）/ プロファイルキャッシュ

## 進捗

- 2026-10-07 起票。反証レビューの指摘を反映
- 2026-10-07 着手
  - [x] 対応方針 1: `fix(config): config init / setup も壊れた config.yml を上書きしない`。`refuseWriteIfBroken` に寄せ、
    config set / config init / setup の入口で呼ぶ（検出・接続の前）。回帰テスト `TestBrokenConfigIsNotOverwrittenByAnyWriter`。
    変異検証（`mutate-verify`）: init / setup の呼び出しをそれぞれ外すと、該当のサブテストだけが red
  - [ ] 対応方針 2〜4
