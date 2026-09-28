#!/usr/bin/env python3
"""安全装置の変異検証（mutation check）。

このリポジトリの「安全装置」— ワークスペース限定 / 読み取り専用 allowlist /
資格情報を残さない後始末 / 出力の無害化 — は、テストが**実際に効いている**ことまで
確かめないと「在るだけ」になる。green は「正しい」ではなく「その書き方では壊せなかった」。

このスクリプトは、各安全装置を壊す変異を 1 つずつ当てて、対象テストが red になることを
確かめる。安全装置やそのテストを触ったら実行すること。

    python3 scripts/mutation_check.py          # 全件
    python3 scripts/mutation_check.py --list   # 変異の一覧だけ表示

判定は 4 値で出す（2 値に丸めない）:
  red         期待どおりテストが落ちた = そのテストは効いている
  GREEN       変異したのにテストが緑 = **テストが何も守っていない**（要修正）
  build-error 変異でコンパイルが通らなかった = 判定不能（変異の書き方を直す）
  not-applied 置換対象が見つからない / 1 箇所でない = 判定不能（実装が動いた）

手順として次を必ず通す（どれを飛ばしても誤診する）:
  1. 変異前後でファイルが実際に変わったことを確認する（当たっていない緑を防ぐ）
  2. go build が通ることを確認する（ビルド不能の緑を「検知できなかった」と誤読しない）
  3. 対象テストを名指しで実行し、「--- FAIL: <テスト名>」の有無で判定する
     （rc だけ・件数だけでは「1 本も走らなかった」と区別できない）
  4. 作業ツリーは触らず、リポジトリのコピーに対して変異を当てる

🚨 **red は「その変異でそのテストが落ちた」以上のことを意味しない。**
どの assert が落ちたかまでは見ていないので、「seam を消す正当な整理」のような
挙動中立の変更も red になりうる。red を「そのガードが効いている証拠」として読むなら、
少なくとも 1 度は `-v` で**意図した assert が落ちているか**を目視すること。

## 意図的に載せていない変異（等価変異と判定したもの）

後始末（openVerifiedChild / RunAllCleanups）の等価変異の記録は chromecookie の mutation_check.py へ移した。

## 過去にこのスクリプトが見つけたテストの弱さ（再発防止のため記録）

- `TestTokenGoesInBodyNotURL` が URL の **Host しか見ておらず**、token をクエリ文字列に
  載せる変異を素通しした → 完全な URL を記録して検査するよう修正
- `TestCookieHostMatches` の fixture が弱く、ドット境界を無視した **素の suffix 比較**でも
  全ケース通った → `slack.com` / `ack.com` / `lpha.slack.com` を追加
"""
import argparse
import json
import os
import shutil
import subprocess
import sys
import tempfile

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
MUTATIONS = [
    # (名前, ファイル, 置換前, 置換後, テスト対象パッケージ, 期待して red になるテスト名)
    #
    # Cookie の復号・作業領域の後始末・エラーの分類の変異は chromecookie へ移した
    # (github.com/jiikko/dotfiles の src/chromecookie/mutation_check.py)。ここは slack-cli に残るコードだけ。
    ('allowlist の実行時ガードを外す', 'internal/slack/client.go', '\tif !isAllowed(m.name) {', '\tif false {', './internal/slack/', 'TestDoRejectsMethodOutsideAllowlist'),
    ('ワークスペース一致の判定を常に真にする', 'internal/slack/resolve.go', '\t\t\tif domain == ws {', '\t\t\tif true {', './internal/slack/', 'TestResolveRejectsMismatchedWorkspace'),
    ('明示指定トークンを検証せず採用する（tokensFor をすり抜け）', 'internal/slack/resolve.go', '\tif t := strings.TrimSpace(cfg.Token); t != "" {\n\t\treturn []string{t}, nil\n\t}', '\tif t := strings.TrimSpace(cfg.Token); t != "" && false {\n\t\treturn []string{t}, nil\n\t}', './internal/slack/', 'TestExplicitTokenIsStillVerified'),
    ('token をボディでなく URL に載せる', 'internal/slack/client.go', '\tendpoint := &url.URL{Scheme: "https", Host: c.host, Path: "/api/" + m.name}', '\tendpoint := &url.URL{Scheme: "https", Host: c.host, Path: "/api/" + m.name, RawQuery: "token=" + c.token}', './internal/slack/', 'TestTokenGoesInBodyNotURL'),
    ('token を URL に載せる（漏えい検査側）', 'internal/slack/client.go', '\tendpoint := &url.URL{Scheme: "https", Host: c.host, Path: "/api/" + m.name}', '\tendpoint := &url.URL{Scheme: "https", Host: c.host, Path: "/api/" + m.name, RawQuery: "token=" + c.token}', './internal/slack/', 'TestNoSecretsInStderrOrErrors'),
    ('リダイレクト拒否を外す（既定の追従に戻す）', 'internal/slack/client.go', '\t\tCheckRedirect: func(req *http.Request, via []*http.Request) error {\n\t\t\treturn fmt.Errorf("API がリダイレクト（%s）を返しました。資格情報を送らずに中止します", req.URL.Redacted())\n\t\t},', '', './internal/slack/', 'TestRedirectsAreRefused'),
    ('Cookie に d 以外も載せる', 'internal/slack/client.go', '\treq.Header.Set("Cookie", "d="+c.cookie)', '\treq.Header.Set("Cookie", "d="+c.cookie+"; extra=1")', './internal/slack/', 'TestOnlyDCookieIsSent'),
    ('HTTP リクエストの発行を do 以外にも作る', 'internal/slack/client.go', 'func (c *Client) call(ctx context.Context, m Method, params url.Values, v any) (json.RawMessage, error) {', 'func (c *Client) call(ctx context.Context, m Method, params url.Values, v any) (json.RawMessage, error) {\n\tif false {\n\t\t_, _ = c.http.Do(nil)\n\t}', './internal/slack/', 'TestOnlyDoIssuesHTTPRequests'),
    ('main のシグナル後始末を外す', 'cmd/slack/main.go', '\tauth.InstallCleanupOnSignal()', '', './cmd/slack/', 'TestCleanupIsWiredInMain'),
    ('TSV の無害化をやめる', 'internal/output/output.go', '\t\t\tvs[j] = Sanitize(r.cols[c].Value(items[i]))', '\t\t\tvs[j] = r.cols[c].Value(items[i])', './internal/output/', 'TestRenderSanitizesCells'),
    ('ワークスペース名の正規化を常に最初の / で切る', 'internal/config/config.go', '\tif hadScheme || strings.Contains(strings.ToLower(s), ".slack.com") {', '\tif hadScheme || true {', './internal/config/', 'TestNormalizeWorkspace'),
    ('ワークスペース名の文字種検証を外す', 'internal/config/config.go', '\t\tdefault:\n\t\t\treturn fmt.Errorf("ワークスペース名に使えない文字が含まれています（英小文字・数字・ハイフンのみ）: %q", ws)', '\t\tdefault:', './internal/config/', 'TestValidateWorkspace'),
    ('設定キーの保存を取りこぼす（default_count を書かない）', 'internal/config/config.go', '\t\tfc.DefaultCount = n', '\t\t_ = n', './internal/config/', 'TestEveryKeyRoundTrips'),
    ('ページング打ち切りを無音で握り潰す（チャンネル）', 'internal/slack/api.go', '\treturn out, &TruncatedError{Method: MethodConversationsList.String(), Pages: maxPages, Count: len(out), Hint: truncHintChannels}', '\treturn out, nil', './internal/slack/', 'TestChannelsReportsTruncation'),
    ('打ち切りと「存在しない」を混同する', 'internal/slack/api.go', '\tif truncated {\n\t\t// 🚨 「見つからなかった」と「探しきれなかった」を混同しない。', '\tif truncated && false {\n\t\t// 🚨 「見つからなかった」と「探しきれなかった」を混同しない。', './internal/slack/', 'TestResolveChannelDistinguishesTruncationFromNotFound'),
    ('-json からチャンネルを落とす', 'internal/slack/types.go', '\tChannelID string `json:"channel_id,omitempty"`', '\tChannelID string `json:"-"`', './internal/slack/', 'TestHistoryFillsChannelID'),
    ('カラム誤りを実行時エラー(rc=1)にする', 'cmd/slack/columns.go', '\t\treturn nil, &config.UsageError{Msg: "エラー: " + err.Error()}', '\t\t_ = (*config.UsageError)(nil)\n\t\treturn nil, err', './cmd/slack/', 'TestParseColsReturnsUsageError'),
    ('名前解決で全ページ取得してから探す（429 を踏む形に戻す）', 'internal/slack/api.go', '\t\t\t\tfound = ch.ID\n\t\t\t\treturn false // 見つかったので以降のページは取らない', '\t\t\t\tfound = ch.ID', './internal/slack/', 'TestResolveChannelStopsAtFirstMatch'),
    ('③の掃除を main から外す（資格情報の経路でしか走らない形に戻す）', 'cmd/slack/main.go', '\tauth.SweepStaleTempDirs()', '', './cmd/slack/', 'TestCleanupIsWiredInMain'),
    ('③の掃除をサブコマンド分岐より後ろへ動かす（slack help で走らなくなる）', 'cmd/slack/main.go', 'func main() {\n\t// Chrome の Cookie DB / Local Storage の一時コピーを、Ctrl-C でも残さないようにする。\n\tauth.InstallCleanupOnSignal()\n\tdefer auth.RunAllCleanups()\n\n\t// 🚨 前回の実行が SIGKILL 等で残した一時コピーを、**起動時に**片付ける。\n\t// ここに置かないと「Chrome から資格情報を読む経路を通ったときだけ」の掃除になり、\n\t// slack help / slack config では残骸が残ったままになる（実測で踏んだ）。\n\tauth.SweepStaleTempDirs()\n\n\tif len(os.Args) < 2 {\n\t\tfmt.Fprint(os.Stderr, topUsage)\n\t\tos.Exit(2)\n\t}\n\n\tcmd := os.Args[1]\n\targs := os.Args[2:]\n\n\tvar err error\n\tswitch cmd {\n\tcase "search":\n\t\terr = cmdSearch(args)\n\tcase "channels":\n\t\terr = cmdChannels(args)\n\tcase "history":\n\t\terr = cmdHistory(args)\n\tcase "thread":\n\t\terr = cmdThread(args)\n\tcase "users":\n\t\terr = cmdUsers(args)\n\tcase "whoami", "auth-test":\n\t\terr = cmdWhoami(args)\n\tcase "config":\n\t\terr = cmdConfig(args)\n\tcase "setup":\n\t\terr = cmdSetup(args)\n\tcase "help", "-h", "--help":\n\t\tfmt.Fprint(os.Stdout, topUsage)\n\t\treturn\n\tdefault:\n\t\tfmt.Fprintf(os.Stderr, "不明なコマンド: %q\\n\\n%s", cmd, topUsage)\n\t\tos.Exit(2)\n\t}\n\n\tif err != nil {\n\t\t// 一時コピーを残さずに終了する（os.Exit は defer を走らせない）。\n\t\tauth.RunAllCleanups()\n\t\tcode := exitCodeFor(err)\n\t\tif code == 2 {\n\t\t\tfmt.Fprintln(os.Stderr, err.Error()) // 使い方の誤りはメッセージをそのまま\n\t\t} else {\n\t\t\tfmt.Fprintln(os.Stderr, "エラー: "+err.Error())\n\t\t}\n\t\tos.Exit(code)\n\t}\n}\n', 'func main() {\n\t// Chrome の Cookie DB / Local Storage の一時コピーを、Ctrl-C でも残さないようにする。\n\tauth.InstallCleanupOnSignal()\n\tdefer auth.RunAllCleanups()\n\n\t// 🚨 前回の実行が SIGKILL 等で残した一時コピーを、**起動時に**片付ける。\n\t// ここに置かないと「Chrome から資格情報を読む経路を通ったときだけ」の掃除になり、\n\t// slack help / slack config では残骸が残ったままになる（実測で踏んだ）。\n\n\tif len(os.Args) < 2 {\n\t\tfmt.Fprint(os.Stderr, topUsage)\n\t\tos.Exit(2)\n\t}\n\n\tcmd := os.Args[1]\n\targs := os.Args[2:]\n\n\tvar err error\n\tswitch cmd {\n\tcase "search":\n\t\terr = cmdSearch(args)\n\tcase "channels":\n\t\terr = cmdChannels(args)\n\tcase "history":\n\t\terr = cmdHistory(args)\n\tcase "thread":\n\t\terr = cmdThread(args)\n\tcase "users":\n\t\terr = cmdUsers(args)\n\tcase "whoami", "auth-test":\n\t\terr = cmdWhoami(args)\n\tcase "config":\n\t\terr = cmdConfig(args)\n\tcase "setup":\n\t\terr = cmdSetup(args)\n\tcase "help", "-h", "--help":\n\t\tfmt.Fprint(os.Stdout, topUsage)\n\t\treturn\n\tdefault:\n\t\tfmt.Fprintf(os.Stderr, "不明なコマンド: %q\\n\\n%s", cmd, topUsage)\n\t\tos.Exit(2)\n\t}\n\n\tif err != nil {\n\t\t// 一時コピーを残さずに終了する（os.Exit は defer を走らせない）。\n\t\tauth.RunAllCleanups()\n\t\tcode := exitCodeFor(err)\n\t\tif code == 2 {\n\t\t\tfmt.Fprintln(os.Stderr, err.Error()) // 使い方の誤りはメッセージをそのまま\n\t\t} else {\n\t\t\tfmt.Fprintln(os.Stderr, "エラー: "+err.Error())\n\t\t}\n\t\tos.Exit(code)\n\t}\n\tauth.SweepStaleTempDirs()\n}\n', './cmd/slack/', 'TestCleanupIsWiredInMain'),
    ('プロファイル固定時の探索範囲の案内をやめる', 'internal/slack/resolve.go', '\tif !fixed || len(profiles) == 0 {\n\t\treturn ""\n\t}', '\tif true || !fixed || len(profiles) == 0 {\n\t\treturn ""\n\t}', './internal/slack/', 'TestFailureTellsProfileScopeWhenFixed'),
    ('--help / フラグ誤りの出力を flag パッケージにも出させる（二重出力へ戻す）', 'cmd/slack/main.go', '\tfs.SetOutput(io.Discard)\n\tfs.Usage = func() {}', '\tfs.SetOutput(os.Stderr)\n\t_ = io.Discard\n\tfs.Usage = func() { fmt.Fprint(os.Stderr, "usage") }', './cmd/slack/', 'TestHelpIsPrintedOnce'),
    ('replies を 1 回で終わらせる（新しい側の返信が欠ける形へ戻す）', 'internal/slack/api.go', '\t\tvar hasMore bool\n', '\t\tif true {\n\t\t\treturn out, nil\n\t\t}\n\t\tvar hasMore bool\n', './internal/slack/', 'TestRepliesPagesUntilLimit'),
    ('history を 1 回で終わらせる', 'internal/slack/api.go', '\t\tvar hasMore bool\n', '\t\tif true {\n\t\t\treturn out, nil\n\t\t}\n\t\tvar hasMore bool\n', './internal/slack/', 'TestHistoryPagesUntilLimit'),
    ('短いページを最後のページと早合点する', 'internal/slack/api.go', '\t\tvar hasMore bool\n', '\t\tif len(resp.Messages) < want {\n\t\t\treturn out, nil\n\t\t}\n\t\tvar hasMore bool\n', './internal/slack/', 'TestShortPagesAreFollowed'),
    ('1 回あたりの limit を -n のまま送る（API 上限を超える）', 'internal/slack/api.go', '\t\tparams.Set("limit", strconv.Itoa(min(want, messagePageLimit)))', '\t\tparams.Set("limit", strconv.Itoa(want))', './internal/slack/', 'TestHistoryPagesUntilLimit'),
    ('1 ページの limit を 200 に戻す（-n 1000 で 5 回呼ぶ）', 'internal/slack/api.go', 'const messagePageLimit = 1000', 'const messagePageLimit = 200', './internal/slack/', 'TestSmallLimitIsSingleCall'),
    ('ts の重複除去をやめる（replies の親の再掲を二重に出す）', 'internal/slack/api.go', '\t\t\t\tif seen[msg.Ts] {\n\t\t\t\t\tcontinue\n\t\t\t\t}\n', '', './internal/slack/', 'TestRepliesDedupesRepeatedParent'),
    ('2 ページ目以降に 1 件多く頼むのをやめる（親の再掲で空回りする）', 'internal/slack/api.go', '\t\t\twant++\n', '', './internal/slack/', 'TestRepliesDedupesRepeatedParent'),
    ('has_more なのにカーソルが空を完了扱いにする', 'internal/slack/api.go', '\t\t\tif hasMore {', '\t\t\tif false && hasMore {', './internal/slack/', 'TestHasMoreWithoutCursorIsTruncation'),
    ('メッセージのページング打ち切りを無音で握り潰す', 'internal/slack/api.go', '\treturn out, &TruncatedError{Method: m.String(), Pages: maxPages, Count: len(out), Hint: hint}', '\t_ = hint\n\treturn out, nil', './internal/slack/', 'TestMessagePagingReportsTruncation'),
    ('途中失敗で取得済みページを捨てる（history / replies）', 'internal/slack/api.go', '\t\t\tif len(out) > 0 {\n\t\t\t\treturn out, &PartialError{Method: m.String(), Count: len(out), Err: err}\n\t\t\t}\n', '', './internal/slack/', 'TestMidPagingFailureReturnsPartial'),
    ('途中失敗で取得済みページを捨てる（channels）', 'internal/slack/api.go', '\t\tif len(out) > 0 {\n\t\t\treturn out, &PartialError{Method: MethodConversationsList.String(), Count: len(out), Err: err}\n\t\t}\n', '', './internal/slack/', 'TestMidPagingFailureReturnsPartial'),
    ('途中失敗で取得済みページを捨てる（users）', 'internal/slack/api.go', '\t\t\tif len(out) > 0 {\n\t\t\t\treturn out, &PartialError{Method: MethodUsersList.String(), Count: len(out), Err: err}\n\t\t\t}\n', '', './internal/slack/', 'TestMidPagingFailureReturnsPartial'),
    ('打ち切り案内から「-name では避けられない」を落とす（効かない回避策の案内へ戻す）', 'internal/slack/api.go', 'Count: len(out), Hint: truncHintChannels}', 'Count: len(out)}', './internal/slack/', 'TestTruncationHintDoesNotRecommendNameFilter'),
    ('打ち切り・途中失敗を振り分けず取得分ごと捨てる（history）', 'cmd/slack/history.go', '\tmsgs, err := sess.Client.History(ctx, channelID, count, oldest, latest)\n\tafter, err := splitListErr(err)', '\tmsgs, err := sess.Client.History(ctx, channelID, count, oldest, latest)\n\tvar after error', './cmd/slack/', 'TestListCommandsHandleTruncation'),
    ('途中失敗を完了扱い（rc=0）にする', 'cmd/slack/main.go', '\tcase slack.IsPartial(err):\n\t\treturn err, nil', '\tcase slack.IsPartial(err):\n\t\treturn nil, nil', './cmd/slack/', 'TestSplitListErr'),
    ('部分結果を出力した後に途中失敗を返さない', 'cmd/slack/main.go', '\tfmt.Print(render())\n\treturn after', '\tfmt.Print(render())\n\treturn nil', './cmd/slack/', 'TestFinishListRendersThenReturnsAfter'),
    ('ログインページをネットワーク障害と同じく即 return する', 'internal/slack/resolve.go', '\treturn errors.As(err, &apiErr) || errors.As(err, &notJSON)', '\treturn errors.As(err, &apiErr) || (false && errors.As(err, &notJSON))', './internal/slack/', 'TestResolveTriesNextProfileAfterLoginPage'),
    ('応答を上限で黙って切り詰める', 'internal/slack/client.go', '\traw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))', '\traw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))', './internal/slack/', 'TestOversizedResponseIsReported'),
    ('上限ちょうどの応答を拒否する（境界の取り違え）', 'internal/slack/client.go', '\tif int64(len(raw)) > maxResponseBytes {', '\tif int64(len(raw)) >= maxResponseBytes {', './internal/slack/', 'TestOversizedResponseIsReported'),
    ('Retry-After の HTTP-date にも「秒」を付ける', 'internal/slack/client.go', '\treturn fmt.Sprintf("しばらく（Retry-After: %q）", v)', '\treturn v + " 秒"', './internal/slack/', 'TestRetryAfterFormats'),
    ('Keychain の失敗で次のプロファイルへ進む（EnvError で止めない）', 'internal/slack/resolve.go', '\tif auth.IsEnvError(err) {\n\t\treturn err\n\t}\n\tif is, ok', '\tif is, ok', './internal/slack/', 'TestResolveStopsOnEnvironmentError'),
    ('読み取りの問題で探索全体を止める（後ろの正常なプロファイルを使わない）', 'internal/slack/resolve.go', '\tif auth.IsEnvError(err) {\n\t\treturn err\n\t}\n\tif is, ok', '\tif _, ok := auth.AsProfileIssue(profile, err); auth.IsEnvError(err) || ok {\n\t\treturn err\n\t}\n\tif is, ok', './internal/slack/', 'TestResolveContinuesPastUnreadableProfile'),
    ('読めなかったプロファイルを記録しない（全滅時の案内が消える）', 'internal/slack/resolve.go', '\t\tfi.issues = append(fi.issues, is)\n', '\t\t_ = is\n', './internal/slack/', 'TestResolveReportsReadIssuesWhenAllFail'),
    ('leveldb の個別ファイルの読み取り失敗を黙って捨てる', 'internal/auth/token.go', '\t\t\tskipped.Add(err)\n\t\t\tcontinue\n', '\t\t\tcontinue\n', './internal/auth/', 'TestUnreadableLevelDBFilesAreReported'),
    ('トークンが無いとき、読めなかったファイルを添えない', 'internal/auth/token.go', '\t\tif err := skipped.AsError("Slack のトークン（xoxc-…）"); err != nil {\n\t\t\treturn nil, err\n\t\t}\n', '', './internal/auth/', 'TestUnreadableLevelDBFilesAreReported'),
    ('トークンが取れていても読めないファイルがあれば失敗にする', 'internal/auth/token.go', '\tout := tc.result()\n\tif len(out) == 0 {', '\tout := tc.result()\n\tif true {', './internal/auth/', 'TestUnreadableLevelDBFilesAreReported'),
    ('ワークスペースの痕跡が無いとき、読めなかったファイルを添えない', 'internal/auth/discover.go', '\t\tif err := skipped.AsError("ワークスペースの痕跡"); err != nil {\n\t\t\treturn nil, err\n\t\t}\n', '', './internal/auth/', 'TestUnreadableLevelDBFilesAreReported'),
    ('ワークスペース検出で作業領域の異常を候補なしに畳む', 'cmd/slack/config_cmd.go', '\t\t\tif auth.IsEnvError(err) {\n\t\t\t\treturn nil, nil, err\n\t\t\t}\n', '', './cmd/slack/', 'TestDiscoveryStopsOnTempRootFailure'),
    ('JSON 出力のときに途中失敗を返さない', 'cmd/slack/main.go', '\t\tif err := printJSON(items); err != nil {\n\t\t\treturn err\n\t\t}\n\t\treturn after', '\t\tif err := printJSON(items); err != nil {\n\t\t\treturn err\n\t\t}\n\t\treturn nil', './cmd/slack/', 'TestFinishListRendersThenReturnsAfter'),
    ('0 件のときに途中失敗を返さない', 'cmd/slack/main.go', '\t\tfmt.Fprintln(os.Stderr, "0 件")\n\t\treturn after', '\t\tfmt.Fprintln(os.Stderr, "0 件")\n\t\treturn nil', './cmd/slack/', 'TestFinishListRendersThenReturnsAfter'),
    ('コマンドが finishList に after を渡さない（channels）', 'cmd/slack/channels.go', '\treturn finishList(cfg.JSON, channels, func() string { return channelColumns.Render(channels, cols, !noHeader) }, after)', '\t_ = after\n\treturn finishList(cfg.JSON, channels, func() string { return channelColumns.Render(channels, cols, !noHeader) }, nil)', './cmd/slack/', 'TestListCommandsHandleTruncation'),
    ('別ワークスペースが見つかった分岐で読み取りの問題の案内を落とす', 'internal/slack/resolve.go', '\t\tb.WriteString(scope)', '\t\tb.WriteString(profileScopeNote(profiles, profileFixed))', './internal/slack/', 'TestResolveReportsReadIssuesWhenAllFail'),
    ('ワークスペース検出で読めないプロファイルを記録しない', 'cmd/slack/config_cmd.go', '\t\t\t\tissues = append(issues, is)\n', '\t\t\t\t_ = is\n', './cmd/slack/', 'TestDiscoverySkipsUnreadableProfile'),
    ('ワークスペース検出で読めないプロファイルに当たったら止める', 'cmd/slack/config_cmd.go', '\t\t\tcontinue // Local Storage が無い等は「候補なし」', '\t\t\tbreak', './cmd/slack/', 'TestDiscoverySkipsUnreadableProfile'),
    ('Local Storage の Stat のアクセス拒否を「見つからない」にする', 'internal/auth/token.go', '\t\tif os.IsPermission(err) {\n\t\t\treturn "", chromecookie.ReadFailure("Local Storage ", dir, err)\n\t\t}\n', '', './internal/auth/', 'TestLocalStoragePermissionDeniedIsReadDenied'),
    ('Local Storage コピー時のアクセス拒否を素のエラーに戻す', 'internal/auth/token.go', '\t\t\treturn "", nil, nil, chromecookie.ReadFailure("Local Storage ", src, err)', '\t\t\treturn "", nil, nil, fmt.Errorf("Local Storage を読み取れませんでした（アクセス拒否）: %s", src)', './internal/auth/', 'TestLocalStoragePermissionDeniedIsReadDenied'),
    ('d cookie が無いとき、chromecookie の診断（鍵違い・読めないファイル）を捨てる', 'internal/auth/cookie.go', '\tif err := res.Diagnose(fmt.Sprintf("%s 宛ての %q cookie ", host, slackCookieName)); err != nil {\n\t\treturn err\n\t}\n', '', './internal/auth/', 'TestDCookieNotFoundKeepsDiagnosis'),
    ('Cookie DB が無いときに slack-cli のフラグ名を添えない', 'internal/auth/cookie.go', '\t\tif chromecookie.IsMissing(err) {\n\t\t\treturn "", fmt.Errorf("%w\\n%s", err, profileHint)\n\t\t}\n', '', './internal/auth/', 'TestDCookieNotFoundGuidance'),
]


# COMPILE_GUARDED は「型で閉じている」ことを確かめる変異。
#
# 🚨 こちらは red ではなく **build-error が期待値**。テストで守るのではなく
# コンパイラで守っている性質なので、判定の向きが逆になる。
# （名前: ファイル, 置換前, 置換後）
COMPILE_GUARDED = [
    # シグナルハンドラの引数の型で閉じている性質は chromecookie へ移した。
]


def run(cmd, cwd):
    p = subprocess.run(cmd, cwd=cwd, shell=True, capture_output=True, text=True)
    return p.returncode, p.stdout, p.stderr


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--list", action="store_true", help="変異の一覧だけ表示する")
    ap.add_argument("--json", action="store_true", help="結果を JSON で出す")
    args = ap.parse_args()

    if args.list:
        for i, m in enumerate(MUTATIONS, 1):
            print(f"{i:2d}. {m[0]}  ({m[1]} -> {m[5]})")
        return 0

    # 🚨 作業ツリーには当てない。コピーへ当てる（変異が残ったまま commit される事故を防ぐ）。
    work = tempfile.mkdtemp(prefix="slack-cli-mutation-")
    root = os.path.join(work, "repo")
    try:
        # 🚨 ignore_patterns("slack") は internal/slack と cmd/slack まで巻き込む
        # （パターンは階層を問わずディレクトリ名に一致する）。除外はリポジトリ直下だけに限る。
        def ignore(dirpath, names):
            if os.path.abspath(dirpath) != REPO:
                return set()
            return {n for n in names if n in {".git", "tmp", "dist", "slack"}}

        shutil.copytree(REPO, root, ignore=ignore)

        rc, out, err = run("go test -count=1 ./...", root)
        if rc != 0:
            print("baseline が緑ではない。先にテストを通すこと:\n" + (out or err), file=sys.stderr)
            return 2

        results = []
        for name, path, old, new, pkg, testname in MUTATIONS:
            full = os.path.join(root, path)
            src = open(full, encoding="utf-8").read()
            if src.count(old) != 1:
                results.append((name, "not-applied", f"置換対象が {src.count(old)} 箇所（1 箇所であるべき）"))
                continue
            open(full, "w", encoding="utf-8").write(src.replace(old, new, 1))
            try:
                if open(full, encoding="utf-8").read() == src:
                    results.append((name, "not-applied", "ファイルが変わっていない"))
                    continue
                rc, out, err = run("go build ./...", root)
                if rc != 0:
                    results.append((name, "build-error", (err or out).strip().splitlines()[0][:160]))
                    continue
                rc, out, err = run(f"go test -count=1 -run '^{testname}$' {pkg}", root)
                combined = out + err
                if f"--- FAIL: {testname}" in combined:
                    verdict, detail = "red", ""
                elif "no tests to run" in combined:
                    verdict, detail = "not-run", f"{testname} が実行されていない"
                elif rc == 0:
                    verdict, detail = "GREEN", "変異したのにテストが緑（テストが守っていない）"
                else:
                    verdict, detail = "red(別要因の可能性)", combined.strip().splitlines()[-1][:160]
                results.append((name, verdict, detail))
            finally:
                open(full, "w", encoding="utf-8").write(src)
        # 型で閉じている性質: 変異がコンパイルエラーになることを確かめる
        for name, path, old, new in COMPILE_GUARDED:
            full = os.path.join(root, path)
            src = open(full, encoding="utf-8").read()
            if src.count(old) != 1:
                results.append((name, "not-applied", f"置換対象が {src.count(old)} 箇所（1 箇所であるべき）"))
                continue
            open(full, "w", encoding="utf-8").write(src.replace(old, new, 1))
            try:
                rc, out, err = run("go build ./...", root)
                if rc != 0:
                    results.append((name, "red", "コンパイルエラー（型で閉じている）"))
                else:
                    results.append((name, "GREEN", "コンパイルが通ってしまう（型で閉じていない）"))
            finally:
                open(full, "w", encoding="utf-8").write(src)
    finally:
        shutil.rmtree(work, ignore_errors=True)

    bad = [r for r in results if r[1] != "red"]
    if args.json:
        print(json.dumps([{"変異": n, "判定": v, "備考": d} for n, v, d in results], ensure_ascii=False, indent=1))
    else:
        for n, v, d in results:
            mark = "OK  " if v == "red" else "NG  "
            print(f"{mark}{v:<24} {n}" + (f"  -- {d}" if d else ""))
    print(f"\n変異 {len(results)} 件 / red {len(results) - len(bad)} 件 / 要確認 {len(bad)} 件")
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
