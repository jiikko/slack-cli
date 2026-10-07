# 004 (feat): カレントディレクトリのローカル設定 `.slack-cli.yml` を共通の config.yml より優先する

起票日: 2026-10-07

## 概要

共通の設定は今どおり `$XDG_CONFIG_HOME/slack-cli/config.yml`（未設定なら `~/.config/slack-cli/config.yml`）に置く。
カレントディレクトリに `.slack-cli.yml` があれば、そこに書いてあるキーを共通の設定より優先する。
あわせて、ローカル設定に書き込む `-local` オプションを用意する。

🚨 この変更は現在の設計方針「パスは HOME / XDG 基準で解決し、カレントディレクトリに依存しない」
（`internal/config/config.go` の package doc、`cmd/slack/main.go` の冒頭コメント）を変える。
方針を変えることはユーザーが決めた（2026-10-07 のセッション）。

## 現状（2026-10-07 時点のコードで確認）

- 読み込み: `config.Load()` が `config.Path()` の 1 ファイルだけを `sync.Once` で読む
- 書き込み: `config.Save()` の書き込み先は `config.Dir()` に固定。`config set` / `config init` / `setup` に
  書き込み先を変えるオプションは無い
- 設定元の表示: `configShow()`（`cmd/slack/config_cmd.go`）は `ResolveDefaultSource` が返す `env:KEY` / `file` / `default`
  を表示する。`file` が 1 種類しかない
- `slack config path` は共通の config.yml のパスだけを表示する

## 仕様

### 読み込み

- ファイル名は `.slack-cli.yml`。中身は config.yml と同じ YAML（`workspace` / `team` / `profile` / `default_count`）
- 探すのは**カレントディレクトリだけ**。親ディレクトリへはさかのぼらない
- **キーごとに上書き**する。ローカルに書いたキーだけが優先され、書いていないキーは共通側の値を使う
  （ファイル丸ごとの置き換えにはしない）
- 優先順位: コマンドラインフラグ > 環境変数 > **ローカル `.slack-cli.yml`** > 共通 config.yml > 組み込み既定
- `team` を `workspace` の別名として扱う規則はファイル単位で適用してから合わせる
  （ローカルの `team:` は共通の `workspace:` より優先する）
- ローカルのファイルが YAML として壊れていたら警告を出す。共通側へ黙って切り替えたことにしない
  （今の `Problem()` と同じく、書き込みは拒否する）

### 安全面

clone してきた repo などに置かれた `.slack-cli.yml` で、`workspace` / `profile` が気づかないうちに切り替わる経路ができる。
接続先のホストと auth.test によるワークスペースの照合は残るので、設定したワークスペース以外へ接続して読むことはできない。
ただし「どのワークスペースを読むか」はローカル設定で変えられる。

- ローカル設定を使ったときは、stderr に 1 行（例: `ローカル設定 ./.slack-cli.yml を使用`）を出す。
  `-json` の stdout を汚さないよう stdout には出さない
- 自分以外の所有者のファイルと、グループ・他人が書き込めるファイルは無視する（無視したことを警告として出す）

### 書き込み（`-local`）

- `config set` / `config init` / `setup` に `-local` を足す。付けたときはカレントディレクトリの `.slack-cli.yml` に書き、
  付けないときは今どおり共通側に書く
- `-local` で書くのは**指定したキーだけ**。共通側と合わせた結果は書かない
  （共通側の値までローカルに写すと、後から共通側を変えてもローカルの古い値が優先され続ける）
- ローカルのファイルがあれば、それを読んでからキーを更新する。無ければ新しく作る（権限 0600）。
  ローカルのファイルが壊れていたら書き込みを拒否する
- 書き込んだ後に、書いたファイルのパスを表示する（今の「<path> に保存しました: key = value」と同じ形）
- `config set` は今は引数を位置で読んでいる（`args[1]` / `args[2]`）。`-local` の位置（`config set -local workspace foo`）を
  受けられるように引数の解析を直す

### 表示

- `slack config` は、ローカルと共通を合わせた実効値をキーごとに出し、設定元を `file:<ローカルのパス>` /
  `file:<共通のパス>` に分ける。ローカル設定のファイルがあれば、そのパスも冒頭に出す
- `slack config get` はローカルと共通を合わせた実効値を返す
- `slack config path` はローカル設定があればそのパスも出す（出し方は実装時に決める）

### 変えないもの

- 自動検出したプロファイルのキャッシュ（`profileCachePath`）は workspace ごとのキーで持つので変更しない
- 資格情報（cookie / トークン）はローカル設定にも保存しない（キーを用意しない）

## 受け入れ条件

- [ ] ローカル設定があるディレクトリでは、ローカル側の `workspace` が使われる
- [ ] ローカルに無いキーは共通側の値になる
- [ ] フラグと環境変数は今どおりローカルより優先される
- [ ] `slack config` に、どの値がどのファイルから来たかが出る
- [ ] 引数なしの `config set` の結果がローカル設定のファイルに書かれない
- [ ] `slack config set -local workspace foo` を実行すると `./.slack-cli.yml` に `workspace: foo` だけが書かれ、共通の config.yml は変わらない
- [ ] ローカルのファイルが壊れているとき、`config set -local` は拒否し、読み込み時には警告が出る
- [ ] 他人が書き込めるローカル設定は無視され、警告が出る
- [ ] ローカル設定を使ったことが stderr に出て、stdout には出ない
- [ ] README の「設定ファイル」節、`slack config --help` / `setup --help`、package doc と `main.go` の冒頭コメントを新しい方針に合わせる

## 関連ファイル

- `internal/config/config.go`（`Load` / `Save` / `Path` / `Dir` / `Problem` / `Defaults`）
- `cmd/slack/config_cmd.go`（`cmdConfig` / `configShow` / `configSet` / `configInit`）
- `cmd/slack/setup.go`
- `cmd/slack/main.go`（冒頭コメント）
- `README.md`「設定ファイル」節

## 進捗

- 2026-10-07 起票。未着手
