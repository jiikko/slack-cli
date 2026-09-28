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

- `openVerifiedChild` の `!ok`（型アサーション失敗）分岐を fail-open にする変異 — **等価変異**。
  実測（darwin / os パッケージ由来の FileInfo）ではこの分岐に到達しない（動的型は常に
  `*syscall.Stat_t`）。テストでは守れないので、コード側にその旨を書いてある。

- `RunAllCleanups` の `filepath.Dir(p) != root` ガードの削除 — **等価変異**。
  到達経路を数え直した結果、削除は `removeVerified` が検証済み root からの相対名で行うため、
  このガードを外しても作業領域の外は消えない（実測でも緑）。単独で担っているのは警告の出力だけ。
  コード側にその旨をコメントしてある。

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
    ("allowlist の実行時ガードを外す", "internal/slack/client.go",
     "\tif !isAllowed(m.name) {", "\tif false {",
     "./internal/slack/", "TestDoRejectsMethodOutsideAllowlist"),

    ("ワークスペース一致の判定を常に真にする", "internal/slack/resolve.go",
     "\t\t\tif domain == ws {", "\t\t\tif true {",
     "./internal/slack/", "TestResolveRejectsMismatchedWorkspace"),

    ("明示指定トークンを検証せず採用する（tokensFor をすり抜け）", "internal/slack/resolve.go",
     '\tif t := strings.TrimSpace(cfg.Token); t != "" {\n\t\treturn []string{t}, nil\n\t}',
     '\tif t := strings.TrimSpace(cfg.Token); t != "" && false {\n\t\treturn []string{t}, nil\n\t}',
     "./internal/slack/", "TestExplicitTokenIsStillVerified"),

    ("token をボディでなく URL に載せる", "internal/slack/client.go",
     '\tendpoint := &url.URL{Scheme: "https", Host: c.host, Path: "/api/" + m.name}',
     '\tendpoint := &url.URL{Scheme: "https", Host: c.host, Path: "/api/" + m.name, RawQuery: "token=" + c.token}',
     "./internal/slack/", "TestTokenGoesInBodyNotURL"),

    ("token を URL に載せる（漏えい検査側）", "internal/slack/client.go",
     '\tendpoint := &url.URL{Scheme: "https", Host: c.host, Path: "/api/" + m.name}',
     '\tendpoint := &url.URL{Scheme: "https", Host: c.host, Path: "/api/" + m.name, RawQuery: "token=" + c.token}',
     "./internal/slack/", "TestNoSecretsInStderrOrErrors"),

    ("リダイレクト拒否を外す（既定の追従に戻す）", "internal/slack/client.go",
     "\t\tCheckRedirect: func(req *http.Request, via []*http.Request) error {\n\t\t\treturn fmt.Errorf(\"API がリダイレクト（%s）を返しました。資格情報を送らずに中止します\", req.URL.Redacted())\n\t\t},",
     "",
     "./internal/slack/", "TestRedirectsAreRefused"),

    ("Cookie に d 以外も載せる", "internal/slack/client.go",
     '\treq.Header.Set("Cookie", "d="+c.cookie)',
     '\treq.Header.Set("Cookie", "d="+c.cookie+"; extra=1")',
     "./internal/slack/", "TestOnlyDCookieIsSent"),

    ("HTTP リクエストの発行を do 以外にも作る", "internal/slack/client.go",
     "func (c *Client) call(ctx context.Context, m Method, params url.Values, v any) (json.RawMessage, error) {",
     "func (c *Client) call(ctx context.Context, m Method, params url.Values, v any) (json.RawMessage, error) {\n\tif false {\n\t\t_, _ = c.http.Do(nil)\n\t}",
     "./internal/slack/", "TestOnlyDoIssuesHTTPRequests"),

    ("v11 を平文として素通しする", "internal/auth/cookie.go",
     '\tcase "v10", "v11":', '\tcase "v10":',
     "./internal/auth/", "TestDecryptValueHandlesV10AndV11"),

    ("meta>=24 のハッシュ 32 バイト除去をやめる", "internal/auth/cookie.go",
     "\tif metaVersion >= 24 {", "\tif false {",
     "./internal/auth/", "TestDecryptValueStripsHashPrefixByMetaVersion"),

    ("PKCS7 を最終バイトだけで判定する", "internal/auth/cookie.go",
     "\tfor _, b := range data[len(data)-pad:] {\n\t\tif int(b) != pad {\n\t\t\treturn nil, errors.New(\"PKCS7: パディングバイトが揃っていません\")\n\t\t}\n\t}",
     "",
     "./internal/auth/", "TestPKCS7UnpadValidatesWholePadding"),

    ("Cookie ドメイン判定を素の suffix 比較にする", "internal/auth/cookie.go",
     '\tif strings.HasPrefix(hostKey, ".") {\n\t\td := hostKey[1:] // domain cookie\n\t\treturn reqHost == d || strings.HasSuffix(reqHost, "."+d)\n\t}\n\treturn hostKey == reqHost // host-only cookie は完全一致のみ',
     '\treturn strings.HasSuffix(reqHost, strings.TrimPrefix(hostKey, "."))',
     "./internal/auth/", "TestCookieHostMatches"),


    ("一時ディレクトリをシグナル経路に登録しない", "internal/auth/cleanup.go",
     "\tcleanupPaths[d] = struct{}{}\n", "",
     "./internal/auth/", "TestTempDirIsRemovedByBothPaths"),

    ("作業領域の名前を他ツールと共有する", "internal/auth/cleanup.go",
     '\ttempRootParentName = "slack-cli"',
     '\ttempRootParentName = "esa-cli"',
     "./internal/auth/", "TestTempRootIsToolSpecific"),

    ("main のシグナル後始末を外す", "cmd/slack/main.go",
     "\tauth.InstallCleanupOnSignal()", "",
     "./cmd/slack/", "TestCleanupIsWiredInMain"),

    ("TSV の無害化をやめる", "internal/output/output.go",
     "\t\t\tvs[j] = Sanitize(r.cols[c].Value(items[i]))", "\t\t\tvs[j] = r.cols[c].Value(items[i])",
     "./internal/output/", "TestRenderSanitizesCells"),

    ("ワークスペース名の正規化を常に最初の / で切る", "internal/config/config.go",
     "\tif hadScheme || strings.Contains(strings.ToLower(s), \".slack.com\") {", "\tif hadScheme || true {",
     "./internal/config/", "TestNormalizeWorkspace"),

    ("ワークスペース名の文字種検証を外す", "internal/config/config.go",
     "\t\tdefault:\n\t\t\treturn fmt.Errorf(\"ワークスペース名に使えない文字が含まれています（英小文字・数字・ハイフンのみ）: %q\", ws)",
     "\t\tdefault:",
     "./internal/config/", "TestValidateWorkspace"),

    ("設定キーの保存を取りこぼす（default_count を書かない）", "internal/config/config.go",
     "\t\tfc.DefaultCount = n", "\t\t_ = n",
     "./internal/config/", "TestEveryKeyRoundTrips"),

    ("ページング打ち切りを無音で握り潰す（チャンネル）", "internal/slack/api.go",
     '\treturn out, &TruncatedError{Method: MethodConversationsList.String(), Pages: maxPages, Count: len(out), Hint: truncHintChannels}',
     '\treturn out, nil',
     "./internal/slack/", "TestChannelsReportsTruncation"),

    ("打ち切りと「存在しない」を混同する", "internal/slack/api.go",
     '\tif truncated {\n\t\t// \U0001f6a8 「見つからなかった」と「探しきれなかった」を混同しない。',
     '\tif truncated \u0026\u0026 false {\n\t\t// \U0001f6a8 「見つからなかった」と「探しきれなかった」を混同しない。',
     "./internal/slack/", "TestResolveChannelDistinguishesTruncationFromNotFound"),

    ("-json からチャンネルを落とす", "internal/slack/types.go",
     '\tChannelID string `json:"channel_id,omitempty"`', '\tChannelID string `json:"-"`',
     "./internal/slack/", "TestHistoryFillsChannelID"),

    ("カラム誤りを実行時エラー(rc=1)にする", "cmd/slack/columns.go",
     '\t\treturn nil, &config.UsageError{Msg: "エラー: " + err.Error()}',
     '\t\t_ = (*config.UsageError)(nil)\n\t\treturn nil, err',
     "./cmd/slack/", "TestParseColsReturnsUsageError"),

    ("名前解決で全ページ取得してから探す（429 を踏む形に戻す）", "internal/slack/api.go",
     '\t\t\t\tfound = ch.ID\n\t\t\t\treturn false // 見つかったので以降のページは取らない',
     '\t\t\t\tfound = ch.ID',
     "./internal/slack/", "TestResolveChannelStopsAtFirstMatch"),

    ("SIGQUIT(Ctrl-\\) を捕まえるのをやめる", "internal/auth/cleanup.go",
     'var cleanupSignals = []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT}',
     'var cleanupSignals = []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP}',
     "./internal/auth/", "TestSignalCleanupRemovesTempDirs"),

    ("③の掃除が検証を通らずに root を開く（symlink 先を消す形に戻す）", "internal/auth/cleanup.go",
     '\tr, err := openVerifiedTempRootWith(uid, afterLstat)',
     '\t_ = uid\n\t_ = afterLstat\n\tr, err := os.OpenRoot(tempRoot())',
     "./internal/auth/", "TestSweepRefusesUnverifiedRoot"),

    ("③の掃除を main から外す（資格情報の経路でしか走らない形に戻す）", "cmd/slack/main.go",
     '\tauth.SweepStaleTempDirs()', '',
     "./cmd/slack/", "TestCleanupIsWiredInMain"),

    ("pid 名の往復一致を外す（掃除の母集合を広げる）", "internal/auth/cleanup.go",
     '\tif strconv.Itoa(pid) != head {\n\t\treturn 0, false\n\t}', '',
     "./internal/auth/", "TestPidFromTempDirName"),



    # 「main のどこかで呼ばれている」だけを見る検査は、呼び出しを末尾へ動かす退行を検出できない。
    ("③の掃除をサブコマンド分岐より後ろへ動かす（slack help で走らなくなる）", "cmd/slack/main.go",
     'func main() {\n\t// Chrome の Cookie DB / Local Storage の一時コピーを、Ctrl-C でも残さないようにする。\n\tauth.InstallCleanupOnSignal()\n\tdefer auth.RunAllCleanups()\n\n\t// 🚨 前回の実行が SIGKILL 等で残した一時コピーを、**起動時に**片付ける。\n\t// ここに置かないと「Chrome から資格情報を読む経路を通ったときだけ」の掃除になり、\n\t// slack help / slack config では残骸が残ったままになる（実測で踏んだ）。\n\tauth.SweepStaleTempDirs()\n\n\tif len(os.Args) < 2 {\n\t\tfmt.Fprint(os.Stderr, topUsage)\n\t\tos.Exit(2)\n\t}\n\n\tcmd := os.Args[1]\n\targs := os.Args[2:]\n\n\tvar err error\n\tswitch cmd {\n\tcase "search":\n\t\terr = cmdSearch(args)\n\tcase "channels":\n\t\terr = cmdChannels(args)\n\tcase "history":\n\t\terr = cmdHistory(args)\n\tcase "thread":\n\t\terr = cmdThread(args)\n\tcase "users":\n\t\terr = cmdUsers(args)\n\tcase "whoami", "auth-test":\n\t\terr = cmdWhoami(args)\n\tcase "config":\n\t\terr = cmdConfig(args)\n\tcase "setup":\n\t\terr = cmdSetup(args)\n\tcase "help", "-h", "--help":\n\t\tfmt.Fprint(os.Stdout, topUsage)\n\t\treturn\n\tdefault:\n\t\tfmt.Fprintf(os.Stderr, "不明なコマンド: %q\\n\\n%s", cmd, topUsage)\n\t\tos.Exit(2)\n\t}\n\n\tif err != nil {\n\t\t// 一時コピーを残さずに終了する（os.Exit は defer を走らせない）。\n\t\tauth.RunAllCleanups()\n\t\tcode := exitCodeFor(err)\n\t\tif code == 2 {\n\t\t\tfmt.Fprintln(os.Stderr, err.Error()) // 使い方の誤りはメッセージをそのまま\n\t\t} else {\n\t\t\tfmt.Fprintln(os.Stderr, "エラー: "+err.Error())\n\t\t}\n\t\tos.Exit(code)\n\t}\n}\n',
     'func main() {\n\t// Chrome の Cookie DB / Local Storage の一時コピーを、Ctrl-C でも残さないようにする。\n\tauth.InstallCleanupOnSignal()\n\tdefer auth.RunAllCleanups()\n\n\t// 🚨 前回の実行が SIGKILL 等で残した一時コピーを、**起動時に**片付ける。\n\t// ここに置かないと「Chrome から資格情報を読む経路を通ったときだけ」の掃除になり、\n\t// slack help / slack config では残骸が残ったままになる（実測で踏んだ）。\n\n\tif len(os.Args) < 2 {\n\t\tfmt.Fprint(os.Stderr, topUsage)\n\t\tos.Exit(2)\n\t}\n\n\tcmd := os.Args[1]\n\targs := os.Args[2:]\n\n\tvar err error\n\tswitch cmd {\n\tcase "search":\n\t\terr = cmdSearch(args)\n\tcase "channels":\n\t\terr = cmdChannels(args)\n\tcase "history":\n\t\terr = cmdHistory(args)\n\tcase "thread":\n\t\terr = cmdThread(args)\n\tcase "users":\n\t\terr = cmdUsers(args)\n\tcase "whoami", "auth-test":\n\t\terr = cmdWhoami(args)\n\tcase "config":\n\t\terr = cmdConfig(args)\n\tcase "setup":\n\t\terr = cmdSetup(args)\n\tcase "help", "-h", "--help":\n\t\tfmt.Fprint(os.Stdout, topUsage)\n\t\treturn\n\tdefault:\n\t\tfmt.Fprintf(os.Stderr, "不明なコマンド: %q\\n\\n%s", cmd, topUsage)\n\t\tos.Exit(2)\n\t}\n\n\tif err != nil {\n\t\t// 一時コピーを残さずに終了する（os.Exit は defer を走らせない）。\n\t\tauth.RunAllCleanups()\n\t\tcode := exitCodeFor(err)\n\t\tif code == 2 {\n\t\t\tfmt.Fprintln(os.Stderr, err.Error()) // 使い方の誤りはメッセージをそのまま\n\t\t} else {\n\t\t\tfmt.Fprintln(os.Stderr, "エラー: "+err.Error())\n\t\t}\n\t\tos.Exit(code)\n\t}\n\tauth.SweepStaleTempDirs()\n}\n',
     "./cmd/slack/", "TestCleanupIsWiredInMain"),

    ("掃除が生きているプロセスのものまで消す", "internal/auth/cleanup.go",
     '\t\tif processAlive(pid) && !isStale(e) {\n\t\t\tcontinue // 生きているプロセスの、新しいものは触らない（並行実行）\n\t\t}',
     '',
     "./internal/auth/", "TestSweepRemovesOnlyDeadOwnDirs"),

    ("作業領域の symlink 検査と同一性検査を両方外す", "internal/auth/cleanup.go",
     '\tif want.Mode()&os.ModeSymlink != 0 {\n\t\treturn nil, fmt.Errorf("%s がシンボリックリンクです（削除してください）: %s", name, display)\n\t}\n\tif afterLstat != nil {\n\t\tafterLstat(name) // テストが「検証中の差し替え」を再現するための窓\n\t}\n\tchild, err := parent.OpenRoot(name)\n\tif err != nil {\n\t\treturn nil, err\n\t}\n\tgot, err := child.Stat(".")\n\tif err != nil {\n\t\t_ = child.Close()\n\t\treturn nil, err\n\t}\n\tif !os.SameFile(want, got) {\n\t\t_ = child.Close()\n\t\treturn nil, fmt.Errorf("%s が検証中に差し替えられました: %s", name, display)\n\t}\n\t// 🚨 実測（darwin / os パッケージ由来の FileInfo）ではこの分岐に到達しない\n\t// （動的型は常に *syscall.Stat_t）。つまりテストでは守れない。それでも\n\t// 「判定不能なら拒否」を置くのは、別 platform・別実装で型が変わったときに\n\t// 黙って素通りさせないため。テストが無いことを承知で残している。\n',
     '\t_ = want\n\n\tif afterLstat != nil {\n\t\tafterLstat(name)\n\t}\n\tchild, err := parent.OpenRoot(name)\n\tif err != nil {\n\t\treturn nil, err\n\t}\n\tgot, err := child.Stat(".")\n\tif err != nil {\n\t\t_ = child.Close()\n\t\treturn nil, err\n\t}\n',
     "./internal/auth/", "TestSweepRefusesRelativeSymlinkRoot"),

    ("①の後始末をパス文字列の os.RemoveAll に戻す", "internal/auth/cleanup.go",
     '\treturn d, func() { _, _ = removeVerified(filepath.Base(d)) }, nil',
     '\treturn d, func() { os.RemoveAll(d) }, nil',
     "./internal/auth/", "TestDeferCleanupRefusesUnverifiedRoot"),

    ("親ディレクトリの検証を省いて末尾だけ検証する", "internal/auth/cleanup.go",
     '\tr, err := os.OpenRoot(base)\n\tif err != nil {\n\t\treturn nil, err\n\t}\n\t// 🚨 display はホップごとに進める。進めないと 2 ホップ目のエラーが\n\t// 実在しないパスを表示し、差し替えを報告する画面が嘘をつく。\n\tpath := base\n\tfor _, name := range []string{tempRootParentName, tempRootName} {\n\t\tpath = filepath.Join(path, name)\n\t\tchild, err := openVerifiedChild(r, name, path, uid, afterLstat)\n\t\t_ = r.Close() // 子は自前の fd を持つので、親は閉じてよい\n\t\tif err != nil {\n\t\t\treturn nil, err\n\t\t}\n\t\tr = child\n\t}\n',
     '\tr, err := os.OpenRoot(filepath.Join(base, tempRootParentName))\n\tif err != nil {\n\t\treturn nil, err\n\t}\n\tchild, err := openVerifiedChild(r, tempRootName, tempRoot(), uid, afterLstat)\n\t_ = r.Close()\n\tif err != nil {\n\t\treturn nil, err\n\t}\n\tr = child\n',
     "./internal/auth/", "TestParentComponentIsVerified"),


    ("古い残骸の回収をやめる（pid 再利用で永久に残る形へ戻す）", "internal/auth/cleanup.go",
     '\treturn time.Since(info.ModTime()) > staleAge',
     '\t_ = info\n\treturn false',
     "./internal/auth/", "TestSweepRemovesStaleEntriesEvenIfPidAlive"),

    ("作業領域のパーミッションを 0777 にする", "internal/auth/cleanup.go",
     '\tif err := os.MkdirAll(root, 0o700); err != nil {',
     '\tif err := os.MkdirAll(root, 0o777); err != nil {',
     "./internal/auth/", "TestTempRootPermissions"),

    ("相対パスの HOME を受け入れる（cwd 依存になる）", "internal/auth/cleanup.go",
     '\tif !filepath.IsAbs(dir) {\n\t\treturn "", fmt.Errorf("キャッシュディレクトリが絶対パスではありません（HOME を確認してください）: %s", dir)\n\t}',
     '',
     "./internal/auth/", "TestRelativeHomeIsRejected"),

    ("シグナルを受けても終了しない（後始末だけして走り続ける）", "internal/auth/cleanup.go",
     '\tif s, ok := sig.(syscall.Signal); ok {\n\t\texit(128 + int(s)) // シェルの慣習（SIGINT=130 / SIGTERM=143）\n\t\treturn\n\t}\n\texit(1)',
     '\t_ = sig\n\t_ = exit',
     "./internal/auth/", "TestSignalCleanupRemovesTempDirs"),

    ("作業領域を $TMPDIR 基準に戻す", "internal/auth/cleanup.go",
     '\tdir, err := os.UserCacheDir()\n\tif err != nil {\n\t\treturn "", fmt.Errorf("キャッシュディレクトリを決められません: %w", err)\n\t}\n\t// 🚨 相対パスを弾く。HOME が相対だと作業領域が cwd 依存になり、\n\t// 「カレントディレクトリに一切依存しない」という前提が崩れる（実測: HOME="." で再現）。\n\tif !filepath.IsAbs(dir) {\n\t\treturn "", fmt.Errorf("キャッシュディレクトリが絶対パスではありません（HOME を確認してください）: %s", dir)\n\t}\n\treturn filepath.Join(dir, tempRootParentName), nil',
     '\treturn filepath.Join(os.TempDir(), tempRootParentName), nil',
     "./internal/auth/", "TestTempRootIsToolSpecific"),

    ("プロファイル固定時の探索範囲の案内をやめる", "internal/slack/resolve.go",
     '\tif !fixed || len(profiles) == 0 {\n\t\treturn ""\n\t}',
     '\tif true || !fixed || len(profiles) == 0 {\n\t\treturn ""\n\t}',
     "./internal/slack/", "TestFailureTellsProfileScopeWhenFixed"),

    ("--help / フラグ誤りの出力を flag パッケージにも出させる（二重出力へ戻す）", "cmd/slack/main.go",
     '\tfs.SetOutput(io.Discard)\n\tfs.Usage = func() {}',
     '\tfs.SetOutput(os.Stderr)\n\t_ = io.Discard\n\tfs.Usage = func() { fmt.Fprint(os.Stderr, "usage") }',
     "./cmd/slack/", "TestHelpIsPrintedOnce"),
    ("所有者(uid)の確認を外す", "internal/auth/cleanup.go",
     '\tif int(st.Uid) != uid {',
     '\tif false && int(st.Uid) != uid {',
     "./internal/auth/", "TestForeignOwnerIsRejected"),

    ("検証中の差し替え検出(SameFile)を外す", "internal/auth/cleanup.go",
     '\tif !os.SameFile(want, got) {',
     '\tif false && !os.SameFile(want, got) {',
     "./internal/auth/", "TestSwapDuringVerificationIsDetected"),

    ("シグナル受信後に後始末してから既定へ戻す（順序を逆にする）", "internal/auth/cleanup.go",
     '\treset()\n\tcleanup()',
     '\tcleanup()\n\treset()',
     "./internal/auth/", "TestSignalHandlerResetsBeforeCleanup"),

    ("シグナル受信後に既定へ戻さない", "internal/auth/cleanup.go",
     '\treset()\n\tcleanup()',
     '\t_ = reset\n\tcleanup()',
     "./internal/auth/", "TestSignalHandlerResetsBeforeCleanup"),

    ("シグナルハンドラに no-op の reset を渡す", "internal/auth/cleanup.go",
     '\t\t\tresetFunc(func() { signal.Reset(cleanupSignals...) }),',
     '\t\t\tresetFunc(func() {}),',
     "./internal/auth/", "TestSignalHandlerPassesRealReset"),

    # --- 2026-09-28: ページング / 後始末の窓 / 候補探索の分類 ---
    ("replies を 1 回で終わらせる（新しい側の返信が欠ける形へ戻す）", "internal/slack/api.go",
     '\t\tvar hasMore bool\n',
     '\t\tif true {\n\t\t\treturn out, nil\n\t\t}\n\t\tvar hasMore bool\n',
     "./internal/slack/", "TestRepliesPagesUntilLimit"),

    ("history を 1 回で終わらせる", "internal/slack/api.go",
     '\t\tvar hasMore bool\n',
     '\t\tif true {\n\t\t\treturn out, nil\n\t\t}\n\t\tvar hasMore bool\n',
     "./internal/slack/", "TestHistoryPagesUntilLimit"),

    ("短いページを最後のページと早合点する", "internal/slack/api.go",
     '\t\tvar hasMore bool\n',
     '\t\tif len(resp.Messages) < want {\n\t\t\treturn out, nil\n\t\t}\n\t\tvar hasMore bool\n',
     "./internal/slack/", "TestShortPagesAreFollowed"),

    ("1 回あたりの limit を -n のまま送る（API 上限を超える）", "internal/slack/api.go",
     '\t\tparams.Set("limit", strconv.Itoa(min(want, messagePageLimit)))',
     '\t\tparams.Set("limit", strconv.Itoa(want))',
     "./internal/slack/", "TestHistoryPagesUntilLimit"),

    ("1 ページの limit を 200 に戻す（-n 1000 で 5 回呼ぶ）", "internal/slack/api.go",
     'const messagePageLimit = 1000', 'const messagePageLimit = 200',
     "./internal/slack/", "TestSmallLimitIsSingleCall"),

    ("ts の重複除去をやめる（replies の親の再掲を二重に出す）", "internal/slack/api.go",
     '\t\t\t\tif seen[msg.Ts] {\n\t\t\t\t\tcontinue\n\t\t\t\t}\n',
     '',
     "./internal/slack/", "TestRepliesDedupesRepeatedParent"),

    ("2 ページ目以降に 1 件多く頼むのをやめる（親の再掲で空回りする）", "internal/slack/api.go",
     '\t\t\twant++\n', '',
     "./internal/slack/", "TestRepliesDedupesRepeatedParent"),

    ("has_more なのにカーソルが空を完了扱いにする", "internal/slack/api.go",
     '\t\t\tif hasMore {', '\t\t\tif false && hasMore {',
     "./internal/slack/", "TestHasMoreWithoutCursorIsTruncation"),

    ("メッセージのページング打ち切りを無音で握り潰す", "internal/slack/api.go",
     '\treturn out, &TruncatedError{Method: m.String(), Pages: maxPages, Count: len(out), Hint: hint}',
     '\t_ = hint\n\treturn out, nil',
     "./internal/slack/", "TestMessagePagingReportsTruncation"),

    ("途中失敗で取得済みページを捨てる（history / replies）", "internal/slack/api.go",
     '\t\t\tif len(out) > 0 {\n\t\t\t\treturn out, &PartialError{Method: m.String(), Count: len(out), Err: err}\n\t\t\t}\n',
     '',
     "./internal/slack/", "TestMidPagingFailureReturnsPartial"),

    ("途中失敗で取得済みページを捨てる（channels）", "internal/slack/api.go",
     '\t\tif len(out) > 0 {\n\t\t\treturn out, &PartialError{Method: MethodConversationsList.String(), Count: len(out), Err: err}\n\t\t}\n',
     '',
     "./internal/slack/", "TestMidPagingFailureReturnsPartial"),

    ("途中失敗で取得済みページを捨てる（users）", "internal/slack/api.go",
     '\t\t\tif len(out) > 0 {\n\t\t\t\treturn out, &PartialError{Method: MethodUsersList.String(), Count: len(out), Err: err}\n\t\t\t}\n',
     '',
     "./internal/slack/", "TestMidPagingFailureReturnsPartial"),

    ("打ち切り案内から「-name では避けられない」を落とす（効かない回避策の案内へ戻す）", "internal/slack/api.go",
     'Count: len(out), Hint: truncHintChannels}',
     'Count: len(out)}',
     "./internal/slack/", "TestTruncationHintDoesNotRecommendNameFilter"),

    ("打ち切り・途中失敗を振り分けず取得分ごと捨てる（history）", "cmd/slack/history.go",
     '\tmsgs, err := sess.Client.History(ctx, channelID, count, oldest, latest)\n\tafter, err := splitListErr(err)',
     '\tmsgs, err := sess.Client.History(ctx, channelID, count, oldest, latest)\n\tvar after error',
     "./cmd/slack/", "TestListCommandsHandleTruncation"),

    ("途中失敗を完了扱い（rc=0）にする", "cmd/slack/main.go",
     '\tcase slack.IsPartial(err):\n\t\treturn err, nil',
     '\tcase slack.IsPartial(err):\n\t\treturn nil, nil',
     "./cmd/slack/", "TestSplitListErr"),

    ("部分結果を出力した後に途中失敗を返さない", "cmd/slack/main.go",
     '\tfmt.Print(render())\n\treturn after',
     '\tfmt.Print(render())\n\treturn nil',
     "./cmd/slack/", "TestFinishListRendersThenReturnsAfter"),

    ("②で「終了中」を立てない", "internal/auth/cleanup.go",
     '\tcleanupClosing = true\n', '',
     "./internal/auth/", "TestSignalShutdownRefusesNewTempDirs"),

    ("終了中でも newTempDir が作る", "internal/auth/cleanup.go",
     '\tif cleanupClosing {\n\t\tcleanupMu.Unlock()\n\t\treturn "", nil, tempRootError(errCleanupClosing)\n\t}',
     '',
     "./internal/auth/", "TestSignalShutdownRefusesNewTempDirs"),

    ("作成と登録の間でロックを外す", "internal/auth/cleanup.go",
     '\tif afterMkdir != nil {\n\t\tafterMkdir()\n\t}\n\tcleanupPaths[d] = struct{}{}\n\tcleanupMu.Unlock()',
     '\tcleanupMu.Unlock()\n\tif afterMkdir != nil {\n\t\tafterMkdir()\n\t}\n\tcleanupMu.Lock()\n\tcleanupPaths[d] = struct{}{}\n\tcleanupMu.Unlock()',
     "./internal/auth/", "TestTempDirCreationAndRegistrationAreAtomic"),

    ("シグナルハンドラへ RunAllCleanups を渡す（終了中を立てない）", "internal/auth/cleanup.go",
     '\t\t\tcleanupFunc(shutdownCleanups),',
     '\t\t\tcleanupFunc(RunAllCleanups),',
     "./internal/auth/", "TestSignalHandlerPassesShutdownCleanups"),

    ("削除の前に登録簿から消す（失敗したものを再試行しない）", "internal/auth/cleanup.go",
     '\t\tbyName[filepath.Base(p)] = p\n',
     '\t\tbyName[filepath.Base(p)] = p\n\t\tdelete(cleanupPaths, p)\n',
     "./internal/auth/", "TestFailedCleanupIsRetried"),

    ("削除に失敗した名前も removed に入れる", "internal/auth/cleanup.go",
     '\t\t\t\tfirstErr = err\n\t\t\t}\n\t\t\tcontinue\n\t\t}\n\t\tremoved = append(removed, name)',
     '\t\t\t\tfirstErr = err\n\t\t\t}\n\t\t}\n\t\tremoved = append(removed, name)',
     "./internal/auth/", "TestRemoveVerifiedReportsOnlySuccesses"),

    ("ログインページをネットワーク障害と同じく即 return する", "internal/slack/resolve.go",
     '\treturn errors.As(err, &apiErr) || errors.As(err, &notJSON)',
     '\treturn errors.As(err, &apiErr) || (false && errors.As(err, &notJSON))',
     "./internal/slack/", "TestResolveTriesNextProfileAfterLoginPage"),

    ("応答を上限で黙って切り詰める", "internal/slack/client.go",
     '\traw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))',
     '\traw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))',
     "./internal/slack/", "TestOversizedResponseIsReported"),

    ("上限ちょうどの応答を拒否する（境界の取り違え）", "internal/slack/client.go",
     '\tif int64(len(raw)) > maxResponseBytes {',
     '\tif int64(len(raw)) >= maxResponseBytes {',
     "./internal/slack/", "TestOversizedResponseIsReported"),

    ("Retry-After の HTTP-date にも「秒」を付ける", "internal/slack/client.go",
     '\treturn fmt.Sprintf("しばらく（Retry-After: %q）", v)',
     '\treturn v + " 秒"',
     "./internal/slack/", "TestRetryAfterFormats"),

    ("Keychain の失敗で次のプロファイルへ進む（EnvError で止めない）", "internal/slack/resolve.go",
     '\tif auth.IsEnvError(err) {\n\t\treturn err\n\t}\n\tif is, ok',
     '\tif is, ok',
     "./internal/slack/", "TestResolveStopsOnEnvironmentError"),

    ("読み取りの問題で探索全体を止める（後ろの正常なプロファイルを使わない）", "internal/slack/resolve.go",
     '\tif auth.IsEnvError(err) {\n\t\treturn err\n\t}\n\tif is, ok',
     '\tif _, ok := auth.AsProfileIssue(profile, err); auth.IsEnvError(err) || ok {\n\t\treturn err\n\t}\n\tif is, ok',
     "./internal/slack/", "TestResolveContinuesPastUnreadableProfile"),

    ("読めなかったプロファイルを記録しない（全滅時の案内が消える）", "internal/slack/resolve.go",
     '\t\tfi.issues = append(fi.issues, is)\n', '\t\t_ = is\n',
     "./internal/slack/", "TestResolveReportsReadIssuesWhenAllFail"),

    ("Keychain の失敗を EnvError 以外で返す", "internal/auth/keychain_darwin.go",
     '\t\treturn nil, &EnvError{', '\t\treturn nil, &ReadError{Kind: ReadDenied, ',
     "./internal/auth/", "TestKeychainFailureIsEnvError"),

    ("アクセス拒否を EnvError にする（探索全体が止まる形へ戻す）", "internal/auth/enverror.go",
     '\treturn &ReadError{Kind: ReadDenied, Msg:', '\treturn &EnvError{Msg:',
     "./internal/auth/", "TestPermissionDeniedIsReadDeniedNotMissing"),

    ("Cookie DB の Stat のアクセス拒否を「見つからない」にする", "internal/auth/cookie.go",
     '\t\tif os.IsPermission(err) {\n\t\t\treturn "", diskAccessError("Cookie DB ", c, err)\n\t\t}\n',
     '',
     "./internal/auth/", "TestPermissionDeniedIsReadDeniedNotMissing"),

    ("Local Storage の Stat のアクセス拒否を「見つからない」にする", "internal/auth/token.go",
     '\t\tif os.IsPermission(err) {\n\t\t\treturn "", diskAccessError("Local Storage ", dir, err)\n\t\t}\n',
     '',
     "./internal/auth/", "TestPermissionDeniedIsReadDeniedNotMissing"),

    ("Cookie DB コピー時のアクセス拒否を素のエラーに戻す", "internal/auth/cookie.go",
     '\t\t\t\t\treturn "", nil, nil, diskAccessError("Cookie DB ", src, err)',
     '\t\t\t\t\treturn "", nil, nil, fmt.Errorf("Cookie DB を読み取れませんでした（アクセス拒否）: %s", src)',
     "./internal/auth/", "TestCopyPermissionDeniedIsReadDenied"),

    ("Local Storage コピー時のアクセス拒否を素のエラーに戻す", "internal/auth/token.go",
     '\t\t\treturn "", nil, nil, diskAccessError("Local Storage ", src, err)',
     '\t\t\treturn "", nil, nil, fmt.Errorf("Local Storage を読み取れませんでした（アクセス拒否）: %s", src)',
     "./internal/auth/", "TestCopyPermissionDeniedIsReadDenied"),

    ("-wal / -shm の読み取り失敗を黙って捨てる", "internal/auth/cookie.go",
     '\t\t\tskipped.add(err)\n\t\t\tcontinue\n', '\t\t\tcontinue\n',
     "./internal/auth/", "TestUnreadableWALIsRecorded"),

    ("存在しない -shm まで記録する（ENOENT を除かない）", "internal/auth/enverror.go",
     '\tif err == nil || errors.Is(err, fs.ErrNotExist) {', '\tif err == nil || (false && errors.Is(err, fs.ErrNotExist)) {',
     "./internal/auth/", "TestUnreadableWALIsRecorded"),

    ("leveldb の個別ファイルの読み取り失敗を黙って捨てる", "internal/auth/token.go",
     '\t\t\tskipped.add(err)\n\t\t\tcontinue\n', '\t\t\tcontinue\n',
     "./internal/auth/", "TestUnreadableLevelDBFilesAreReported"),

    ("トークンが無いとき、読めなかったファイルを添えない", "internal/auth/token.go",
     '\t\tif err := skipped.asError("Slack のトークン（xoxc-…）"); err != nil {\n\t\t\treturn nil, err\n\t\t}\n',
     '',
     "./internal/auth/", "TestUnreadableLevelDBFilesAreReported"),

    ("トークンが取れていても読めないファイルがあれば失敗にする", "internal/auth/token.go",
     '\tout := tc.result()\n\tif len(out) == 0 {',
     '\tout := tc.result()\n\tif true {',
     "./internal/auth/", "TestUnreadableLevelDBFilesAreReported"),

    ("ワークスペースの痕跡が無いとき、読めなかったファイルを添えない", "internal/auth/discover.go",
     '\t\tif err := skipped.asError("ワークスペースの痕跡"); err != nil {\n\t\t\treturn nil, err\n\t\t}\n',
     '',
     "./internal/auth/", "TestUnreadableLevelDBFilesAreReported"),

    ("復号の全件失敗を検出しない（cookie が無いに化ける）", "internal/auth/cookie.go",
     'func (d decryptStats) allFailed() bool { return d.tried > 0 && d.failed == d.tried }',
     'func (d decryptStats) allFailed() bool { return false }',
     "./internal/auth/", "TestDecryptFailuresAreReported"),

    ("1 件の復号失敗で全件失敗扱いにする", "internal/auth/cookie.go",
     'func (d decryptStats) allFailed() bool { return d.tried > 0 && d.failed == d.tried }',
     'func (d decryptStats) allFailed() bool { return d.failed > 0 }',
     "./internal/auth/", "TestDecryptFailuresAreReported"),

    ("d cookie が無いとき、読めなかったファイルを添えない", "internal/auth/cookie.go",
     '\tif err := skipped.asError(fmt.Sprintf("%s 宛ての %q cookie ", host, slackCookieName)); err != nil {\n\t\treturn err\n\t}\n',
     '',
     "./internal/auth/", "TestDecryptFailuresAreReported"),

    ("v24 の SHA256(host_key) 照合を外す（鍵違いのゴミが 1/256 で通る）", "internal/auth/cookie.go",
     '\t\tif subtle.ConstantTimeCompare(plain[:sha256.Size], want[:]) != 1 {',
     '\t\tif false && subtle.ConstantTimeCompare(plain[:sha256.Size], want[:]) != 1 {',
     "./internal/auth/", "TestV24HashPrefixDetectsWrongKeyAtScale"),

    ("照合の入力の host_key から先頭ドットを落とす（正常な cookie が全部失敗する）", "internal/auth/cookie.go",
     '\t\twant := sha256.Sum256([]byte(hostKey))',
     '\t\twant := sha256.Sum256([]byte(strings.TrimPrefix(hostKey, ".")))',
     "./internal/auth/", "TestV24HashPrefixDetectsWrongKeyAtScale"),

    ("作業領域の異常を素のエラーで返す（setup が「ログインしてから」に化ける）", "internal/auth/cleanup.go",
     '\t\treturn "", nil, tempRootError(err)\n\t}\n\tcleanupMu.Lock()',
     '\t\treturn "", nil, err\n\t}\n\tcleanupMu.Lock()',
     "./internal/auth/", "TestTempRootFailureIsEnvError"),

    ("終了処理中を素のエラーで返す", "internal/auth/cleanup.go",
     '\t\treturn "", nil, tempRootError(errCleanupClosing)',
     '\t\treturn "", nil, errCleanupClosing',
     "./internal/auth/", "TestTempRootFailureIsEnvError"),

    ("ワークスペース検出で作業領域の異常を候補なしに畳む", "cmd/slack/config_cmd.go",
     '\t\t\tif auth.IsEnvError(err) {\n\t\t\t\treturn nil, nil, err\n\t\t\t}\n',
     '',
     "./cmd/slack/", "TestDiscoveryStopsOnTempRootFailure"),

    ("JSON 出力のときに途中失敗を返さない", "cmd/slack/main.go",
     '\t\tif err := printJSON(items); err != nil {\n\t\t\treturn err\n\t\t}\n\t\treturn after',
     '\t\tif err := printJSON(items); err != nil {\n\t\t\treturn err\n\t\t}\n\t\treturn nil',
     "./cmd/slack/", "TestFinishListRendersThenReturnsAfter"),

    ("0 件のときに途中失敗を返さない", "cmd/slack/main.go",
     '\t\tfmt.Fprintln(os.Stderr, "0 件")\n\t\treturn after',
     '\t\tfmt.Fprintln(os.Stderr, "0 件")\n\t\treturn nil',
     "./cmd/slack/", "TestFinishListRendersThenReturnsAfter"),

    ("コマンドが finishList に after を渡さない（channels）", "cmd/slack/channels.go",
     '\treturn finishList(cfg.JSON, channels, func() string { return channelColumns.Render(channels, cols, !noHeader) }, after)',
     '\t_ = after\n\treturn finishList(cfg.JSON, channels, func() string { return channelColumns.Render(channels, cols, !noHeader) }, nil)',
     "./cmd/slack/", "TestListCommandsHandleTruncation"),

    ("別ワークスペースが見つかった分岐で読み取りの問題の案内を落とす", "internal/slack/resolve.go",
     '\t\tb.WriteString(scope)',
     '\t\tb.WriteString(profileScopeNote(profiles, profileFixed))',
     "./internal/slack/", "TestResolveReportsReadIssuesWhenAllFail"),

    ("ワークスペース検出で読めないプロファイルを記録しない", "cmd/slack/config_cmd.go",
     '\t\t\t\tissues = append(issues, is)\n', '\t\t\t\t_ = is\n',
     "./cmd/slack/", "TestDiscoverySkipsUnreadableProfile"),

    ("ワークスペース検出で読めないプロファイルに当たったら止める", "cmd/slack/config_cmd.go",
     '\t\t\tcontinue // Local Storage が無い等は「候補なし」',
     '\t\t\tbreak',
     "./cmd/slack/", "TestDiscoverySkipsUnreadableProfile"),

]


# COMPILE_GUARDED は「型で閉じている」ことを確かめる変異。
#
# 🚨 こちらは red ではなく **build-error が期待値**。テストで守るのではなく
# コンパイラで守っている性質なので、判定の向きが逆になる。
# （名前: ファイル, 置換前, 置換後）
COMPILE_GUARDED = [
    ("シグナルハンドラの reset と cleanup を入れ替える", "internal/auth/cleanup.go",
     '\t\t\tresetFunc(func() { signal.Reset(cleanupSignals...) }),\n\t\t\tcleanupFunc(shutdownCleanups),',
     '\t\t\tcleanupFunc(shutdownCleanups),\n\t\t\tresetFunc(func() { signal.Reset(cleanupSignals...) }),'),
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
