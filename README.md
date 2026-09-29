# slack-cli

Chrome にログイン済みの Slack セッションを流用して、**設定した 1 つのワークスペースだけ**を
コマンドラインから読むための CLI。API トークンを自分で発行・管理する必要はない。

[esa-cli](https://github.com/jiikko/esa-cli) と同じ操作感（TSV / `-json` / `config` / `setup`）。

> **このツールは読み取り専用です。** メッセージの投稿・編集・削除、リアクション、
> ファイルアップロード、チャンネル作成など、副作用のある操作は一切実装していません。

```console
$ slack channels -name dev
→ workspace: acme (Acme Inc / alice)
id          name          private  members  topic
C0123456789 #dev          false    42       開発の雑談
C0123456790 #dev-release  false    18       リリース連絡

$ slack search -in dev -n 5 'デプロイ 失敗'
$ slack history '#dev' -n 100
$ slack thread '#dev' 1725000000.123456
$ slack users -name tanaka
$ slack whoami
```

## インストール

```sh
# Homebrew
brew install jiikko/tap/chrome-slack-cli

# go install
go install github.com/jiikko/slack-cli/cmd/slack@latest

# 手動ビルド（cgo 不要）
CGO_ENABLED=0 go build -o slack ./cmd/slack
```

macOS 専用。Chrome（Google Chrome）専用。

> **formula 名が `chrome-slack-cli` なのは意図的です。** `slack-cli` は Homebrew 公式 cask
> （Slack 社の [Slack CLI](https://docs.slack.dev/tools/slack-cli/)）と衝突するため、別名にしてあります。
> インストールされるコマンド名は `slack` です。
>
> formula の正本は [jiikko/homebrew-tap](https://github.com/jiikko/homebrew-tap) の
> `Formula/chrome-slack-cli.rb` **のみ**です（このリポジトリには写しを置きません。
> 2 箇所に同じものがあると、片方だけ直したときに静かにずれるため）。

## セットアップ

```sh
slack setup          # 対話式（ワークスペースとプロファイルを選んで保存）
# または
slack config init    # 自動検出して保存（候補が 1 つなら確認なしで確定）
slack config         # 現在の設定と、その出所を表示
```

初回は macOS の Keychain 許可ダイアログが出る。**「常に許可」**を選ぶと以後は出ない。
ターミナルに「フルディスクアクセス」が必要な場合がある
（システム設定 → プライバシーとセキュリティ → フルディスクアクセス）。

### 設定ファイル

`$XDG_CONFIG_HOME/slack-cli/config.yml`（未設定なら `~/.config/slack-cli/config.yml`）

```yaml
workspace: acme       # 必須。https://<workspace>.slack.com の <workspace>
profile: auto         # Chrome プロファイル名。auto で自動検出
default_count: 20     # 検索・取得の既定件数
```

優先順位は **コマンドラインフラグ > 環境変数 > config.yml > 既定値**。
環境変数は `SLACK_CLI_WORKSPACE` / `SLACK_CLI_CHROME_PROFILE` / `SLACK_CLI_TOKEN`。

> `workspace` は `team` という別名でも読める。保存時は `workspace` に正規化される。
> **資格情報（cookie / トークン）は config.yml に保存しない**（保存するキーも用意していない）。

## コマンド

| コマンド | 内容 | Slack API |
|---|---|---|
| `slack search <クエリ>` | メッセージ検索 | `search.messages` |
| `slack channels` | チャンネル一覧 | `conversations.list` |
| `slack history <channel>` | チャンネルのメッセージ | `conversations.history` |
| `slack thread <channel> <ts>` | スレッドの返信 | `conversations.replies` |
| `slack users` | ユーザー一覧 | `users.list` |
| `slack whoami` | 接続中のユーザー/ワークスペース | `auth.test` |
| `slack config` / `slack setup` | 設定 | （通信なし / 確認の 1 回のみ） |

`slack --help` は概要とサブコマンドの一覧だけ。オプション・共通フラグ（`-workspace` / `-profile` / `-token` / `-json`）・終了コード・
安全のための制約の詳細は `slack <コマンド> --help` に出る。

出力は既定で TSV。`-c` / `-columns` で列を選び、`-no-header` でヘッダを抑制できるので、
`awk -F'\t'` や `cut` にそのまま流せる。`-json` は機械可読な JSON。

```sh
slack search -no-header -c permalink 'キーワード' | head -20
slack channels -json | jq -r '.[] | select(.is_private) | .name'
```

検索クエリは Slack の検索構文をそのまま透過する（`in:` / `from:` / `before:` / `after:` / `has:` など）。
`-in` / `-from` は `in:#channel` / `from:@user` を付けるショートカット。

終了コードは `0` 成功 / `1` 実行時エラー（認証切れ・ネットワーク等）/ `2` 使い方の誤り。

## 仕組み（認証）

Slack の内部 API を叩くには 2 つの資格情報が要る。どちらも Chrome から自動で取り出す。

| 資格情報 | 形式 | 取得元 |
|---|---|---|
| セッションクッキー | `d=xoxd-…` | Chrome の暗号化 Cookie DB を Keychain 経由で復号 |
| API トークン | `xoxc-…` | Chrome の Local Storage (leveldb) |

- Cookie の復号・一時コピーの後始末・プロファイルの列挙は、esa-cli / newrelic-nrql-cli と共有する
  [`github.com/jiikko/dotfiles/src/chromecookie`](https://github.com/jiikko/dotfiles/tree/master/src/chromecookie) が持つ。
  **直すときはあちらを直し**、`go get github.com/jiikko/dotfiles/src/chromecookie@master` で取り込み直す（tag は打たない）。
- Cookie の復号は PBKDF2-HMAC-SHA1（salt=`saltysalt` / 1003 回 / 16 バイト）→ AES-128-CBC（IV = 空白 16 バイト）。
  Chrome の `meta.version >= 24` では復号結果の先頭 32 バイトが `SHA256(host_key)` であることを照合してから落とす
  （一致しなければ復号失敗。鍵違いのゴミを cookie として使わない）。
- トークンが leveldb の圧縮ブロックに入っていて取り出せないことがある。その場合は
  **Chrome を完全に終了（cmd+Q）してから**再実行するか、`-token xoxc-…` で明示指定する。

## 安全のための制約

このツールは「任意のワークスペースを自分のアカウントで読む」用途を想定しているため、
事故を**構造で**防ぐようにしてある。

1. **ワークスペース限定** — 接続先ホストは `config` の `workspace` から組み立てた
   `https://<workspace>.slack.com` に固定。抽出したトークンは `auth.test` の結果が
   その workspace と一致したものだけを採用し、一致しなければ停止する。
   `-token` で明示指定したトークンも同じ検証を通る（フラグ 1 つで無効化できない）。
   リダイレクトは一切追わない。
2. **読み取り専用 allowlist** — 呼べるメソッドは型で閉じてあり、allowlist 外は
   **呼び出しコードがコンパイルできない**。加えて送信直前にも名前を突き合わせる。
3. **資格情報を残さない** — cookie / トークンの生値は表示も保存もしない（表示はマスクのみ）。
   Chrome のデータは作業領域（`~/Library/Caches/slack-cli/extract`、0700）へコピーしてから読むが、
   そのコピーは ①`defer` ②シグナル（Ctrl-C / Ctrl-\ 等）③次回起動時の掃除、の 3 経路で必ず削除する。
   削除はいずれも「検証済みの作業領域を開いたディレクトリ fd」経由で行い、パス文字列を
   `os.RemoveAll` に渡さない（途中がシンボリックリンクに差し替えられていた場合にリンク先を消すため）。
   `$TMPDIR` は使わない（呼び出し側が場所を動かせるうえ、未設定時の `/tmp` は誰でも書ける）。
   作業領域（とその親）がシンボリックリンク・他人の所有・group / other に権限がある状態なら使わずに止める。
4. **TSV の無害化** — メッセージ本文のタブ・改行は空白に潰し、制御文字（ESC 等）は落とす。

いずれもテストで固定してあり、それぞれの防御を外す変異を当てて赤くなることを確認している。

### 運用上の注意

- `d` cookie は**なりすましログインが可能な資格情報**。ファイル・ログ・シェル履歴に残さないこと。
- 組織のワークスペースで使う場合は、その組織のポリシーと Slack の利用規約に反しない範囲で使うこと
  （自分のアカウントで読める範囲の自動化が前提）。
- `-token` で渡す場合、シェル履歴とプロセス一覧に載る。`SLACK_CLI_TOKEN` 環境変数の方が安全。

## 開発

```sh
go test ./...                              # 単体テスト
go vet ./...
CGO_ENABLED=0 go build -o slack ./cmd/slack

python3 scripts/mutation_check.py          # 安全装置の変異検証（テストが実際に効いているか）
python3 scripts/mutation_check.py --list   # 当てる変異の一覧
```

`scripts/mutation_check.py` は、安全装置（ワークスペース限定 / allowlist / 無害化 / Local Storage の読み取り）を
壊す変異を 1 つずつ当てて、対応するテストが赤くなることを確かめる。後始末（3 経路の削除）と Cookie の復号の変異は
chromecookie 側の `mutation_check.py` にある。
**安全装置かそのテストを触ったら必ず走らせること**（green は「正しい」ではなく
「その書き方では壊せなかった」でしかないため）。作業ツリーは触らず、コピーに対して変異を当てる。

構成:

```
cmd/slack/              サブコマンド分岐・フラグ・出力
internal/auth/          d cookie の選択 / xoxc 抽出 / ワークスペース検出（Cookie 復号と後始末は chromecookie に委譲）
internal/slack/         HTTP クライアント（allowlist・ワークスペース限定）/ API ラッパ / 接続解決
internal/config/        config.yml の読み書きと優先順位解決
internal/output/        TSV / JSON 整形
```

## リリース

0. `git fetch --tags` して `git tag --sort=-v:refname | head -3` で最新の版を確かめてから次の版を決める（別の作業が先に出していることがある）
1. `main` の CI が緑であることを確かめてから tag を打つ（`git tag -a vX.Y.Z -m "..." && git push origin vX.Y.Z`）
2. `curl -sL https://github.com/jiikko/slack-cli/archive/refs/tags/vX.Y.Z.tar.gz | shasum -a 256` の値で、
   [jiikko/homebrew-tap](https://github.com/jiikko/homebrew-tap) の `Formula/chrome-slack-cli.rb` の `url` と `sha256` を更新して push する
3. **手元で入れ直して疎通を確かめる**（tag・tap・ソースからのビルドのどれかが壊れていても、ここまで来ないと分からない）

```sh
brew update
brew uninstall jiikko/tap/chrome-slack-cli
brew install jiikko/tap/chrome-slack-cli
readlink -f "$(command -v slack)"   # 新しい版の Cellar を指しているか
slack whoami                        # 認証（Keychain → d cookie / xoxc）とワークスペースの確認
slack channels | wc -l               # API を実際に叩く読み取り
ls ~/Library/Caches/slack-cli/extract   # 空であること（Cookie DB の一時コピーが残っていない）
```

## ライセンス

MIT
