# 006 (bug): レート制限（429）を受けたら Retry-After だけ待って続きを取る

起票日: 2026-10-07

## 概要

大きなワークスペースでは、`slack channels` を数分のうちに 2 回実行すると、途中のページで 429 になって不完全な結果で終わる。
Slack は 429 に `Retry-After`（秒）を付けて返すので、その秒数だけ待って同じリクエストをやり直せば、1 回の実行の中で最後まで取れる。

## 現状（2026-10-07、v0.2.0 で確認）

- 実測（ubiregi、約 3500 チャンネル）: `slack channels` を数分のうちに 4 回実行し、2〜4 回目が
  「conversations.list の取得が途中で失敗しました（取得済み 3447 / 1774 / 1194 件）: レート制限（429）… 30 秒待って再実行してください」で rc=1。
  5 分空けた 5 回目は rc=0（3504 行）。案内の「30 秒」は Slack が返した `Retry-After: 30`
- `conversations.list` は 1 ページ 200 件のカーソルページング（`internal/slack/api.go` の `forEachChannelPage`、上限 `maxPages` = 50）。
  3500 件で 1 回あたり約 18 リクエスト
- 429 の処理は `internal/slack/client.go` の `do` で、型の無い `fmt.Errorf` を返すだけ。再試行は無い。
  途中のページで失敗したときは `PartialError` になり、取れた分を出して rc=1（`cmd/slack/main.go` の `splitListErr`）
- すべての API 呼び出しは `call` → `do` を通る。`do` は HTTP を送る唯一の場所（`wiring_test.go` が固定）。呼べるのは読み取り専用の
  メソッドだけ（allowlist）なので、同じリクエストをやり直しても副作用は無い

## 対応方針

- 429 を型で返す（`RateLimitError`。`Retry-After` を秒として読めたかと、その秒数を持つ）。案内の文言は今と同じ
- `call` で、429 を受けたら `Retry-After` の秒数だけ待って同じリクエストをやり直す。`do` は HTTP を送る唯一の場所のまま変えない
- 際限なく待たない（上限を超えたら今と同じく 429 のエラーを返し、途中まで取れた分は今どおり `PartialError` で出す）
  - `Retry-After` が整数の秒として読めないとき（無い・HTTP-date）は待たない（待つ時間を推測しない）
  - 1 回の待ちの上限 60 秒、1 リクエストあたり 3 回まで、クライアント全体での待ちの合計 180 秒まで
- 待つときは stderr に 1 行出す（例: `レート制限（429）: conversations.list。30 秒待って再試行します（1/3）`）。stdout（`-json`）には出さない。
  通知の出し先は `Resolve` が受け取っている stderr を使う
- 待ちは差し替えられるようにし、テストは実時間を待たない（偽の時計で「何秒待ったか・何回やり直したか」を見る）。待っている間も
  context の取り消しで抜ける

## 受け入れ条件

- [ ] 429（`Retry-After: N`）の後に 200 が返るとき、N 秒待って同じリクエストをやり直し、成功として返す
- [ ] ページングの途中で 429 が出ても、待ってから続きのページを取り、全件がそろう（`PartialError` にならない）
- [ ] `Retry-After` が無い・整数でない・上限（60 秒）を超えるときは待たず、今と同じ 429 のエラーになる
- [ ] 1 リクエストで 3 回やり直しても 429 なら、今と同じ 429 のエラーになる。合計 180 秒を超える待ちはしない
- [ ] 待つときの通知は stderr にだけ出る
- [ ] context の取り消しで待ちから抜ける
- [ ] README の該当箇所（あれば）と help の文言を、自動で待つことに合わせる

## 関連ファイル

- `internal/slack/client.go`（`do` / `call` / `retryAfterText` / `Option`）
- `internal/slack/resolve.go`（`Resolve` が `New` を呼ぶ箇所）
- `internal/slack/api.go`（`forEachChannelPage` / `PartialError`）
- `internal/slack/paging_test.go` / `response_test.go` / `wiring_test.go`

## 進捗

- 2026-10-07 起票
