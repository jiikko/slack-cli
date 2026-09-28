package slack

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/jiikko/slack-cli/internal/auth"
	"github.com/jiikko/slack-cli/internal/config"
)

// Credentials は資格情報の供給元。既定は Chrome（ChromeCredentials）。
// テストではここを差し替えて、Chrome にも Keychain にも触れずに解決経路を回す。
type Credentials interface {
	// Profiles は候補となる Chrome プロファイルを返す。
	Profiles() []auth.Profile
	// Cookie は profile から host 宛ての d cookie を取り出す。
	Cookie(profile, host string) (string, error)
	// Tokens は profile から xoxc トークン候補を「確からしい順」に返す。
	Tokens(profile, workspace string) ([]string, error)
}

// ChromeCredentials は Chrome から資格情報を取り出す既定の実装。
type ChromeCredentials struct{}

func (ChromeCredentials) Profiles() []auth.Profile { return auth.ListProfiles() }

func (ChromeCredentials) Cookie(profile, host string) (string, error) {
	return auth.SlackDCookie(profile, host)
}

func (ChromeCredentials) Tokens(profile, workspace string) ([]string, error) {
	cands, err := auth.ExtractTokens(profile, workspace)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.Token)
	}
	return out, nil
}

// Session は解決済みの接続（クライアント + 使用プロファイル + auth.test の結果）。
type Session struct {
	Client  *Client
	Profile string
	Auth    *AuthTest
}

// Resolve は config から「対象ワークスペースに接続できるクライアント」を組み立てる。
//
// 🚨 これがワークスペース限定（仕様 §4）の本体。守っている不変条件は 2 つ:
//
//  1. 接続先ホストは常に config の workspace から作る（候補トークンが別ワークスペースの
//     ものであっても、リクエストが飛ぶ先は設定したワークスペースだけ）。
//  2. 採用するトークンは auth.test の url が config の workspace と一致したものだけ。
//     一致しなければ停止し、見つかったワークスペース一覧を案内する。
//
// 明示指定のトークン（-token / SLACK_CLI_TOKEN）も同じ検証を通す。ここを免除すると、
// 安全装置の中心がフラグ 1 つで無効化される。
func Resolve(ctx context.Context, cfg config.Config, creds Credentials, stderr io.Writer, opts ...Option) (*Session, error) {
	ws := config.NormalizeWorkspace(cfg.Workspace)
	if ws == "" {
		return nil, &config.UsageError{Msg: "エラー: ワークスペース(workspace)が未設定です。https://<workspace>.slack.com の <workspace> を指定してください:\n" +
			"  slack config set workspace <name>   （推奨: 一度設定すれば以後不要）\n" +
			"  export " + config.EnvWorkspace + "=<name>\n" +
			"  slack <コマンド> -workspace <name> ...\n" +
			"  対話式に設定するなら: slack setup"}
	}
	if err := config.ValidateWorkspace(ws); err != nil {
		return nil, &config.UsageError{Msg: "エラー: " + err.Error()}
	}
	host := ws + ".slack.com"

	if creds == nil {
		creds = ChromeCredentials{}
	}

	profiles := candidateProfiles(cfg, ws, creds)
	fi := failureInfo{
		ws:            ws,
		seenElsewhere: map[string]string{},
		profiles:      profiles,
		// プロファイルが固定されているか（固定だと「そのプロファイルの中だけ」を探す）。
		profileFixed: strings.TrimSpace(cfg.Profile) != "" && cfg.Profile != auth.ProfileAuto,
	}

	for _, profile := range profiles {
		cookie, err := creds.Cookie(profile, host)
		if err != nil {
			if err := fi.record(profile, err); err != nil {
				return nil, err
			}
			continue // その プロファイルには Slack の cookie が無い / 読めない
		}

		tokens, err := tokensFor(cfg, creds, profile, ws)
		if err != nil {
			if err := fi.record(profile, err); err != nil {
				return nil, err
			}
			continue
		}
		if len(tokens) == 0 {
			fi.lastErr = &auth.ErrNoToken{Profile: profile}
			continue
		}

		for _, token := range tokens {
			c, err := New(ws, token, cookie, opts...)
			if err != nil {
				fi.lastErr = err
				continue
			}
			fi.triedToken++
			at, err := c.AuthTest(ctx)
			if err != nil {
				if isCandidateRejection(err) {
					// そのトークン / cookie が無効なだけかもしれないので次の候補へ。
					fi.lastErr = err
					continue
				}
				// ネットワーク障害等は候補を変えても直らないので即座に返す。
				return nil, err
			}
			domain := at.TeamDomain()
			if domain == ws {
				if cfg.Profile == "" || cfg.Profile == auth.ProfileAuto {
					config.WriteProfileCache(ws, profile)
				}
				if stderr != nil {
					fmt.Fprintf(stderr, "→ workspace: %s (%s / %s)\n", ws, at.Team, at.User)
				}
				return &Session{Client: c, Profile: profile, Auth: at}, nil
			}
			if domain != "" {
				fi.seenElsewhere[domain] = at.Team
			}
		}
	}

	return nil, fi.failure()
}

// failureInfo は探索の途中経過（全候補が失敗したときの案内の材料）。
type failureInfo struct {
	ws            string
	seenElsewhere map[string]string // 別ワークスペースのトークンが見つかった記録: domain -> 表示名
	triedToken    int
	lastErr       error
	issues        []auth.ProfileIssue // 資格情報を読めなかったプロファイル
	profiles      []string
	profileFixed  bool
}

// record は資格情報の取り出しの失敗を記録する。戻り値が非 nil なら探索を止めて返す。
//
// 🚨 即座に止めるのは EnvError（Keychain の暗号鍵を取れない = 全プロファイル共通）だけ。
// 読み取り拒否・復号失敗・読めないファイルは**プロファイル単位でも起きる**ので、記録して
// 次へ進む（止めると後ろの正常なプロファイルが使えない）。全滅したら failure が案内に添える。
func (fi *failureInfo) record(profile string, err error) error {
	if auth.IsEnvError(err) {
		return err
	}
	if is, ok := auth.AsProfileIssue(profile, err); ok {
		fi.issues = append(fi.issues, is)
	}
	fi.lastErr = err
	return nil
}

// failure は失敗時の案内を組み立てる。
func (fi *failureInfo) failure() error {
	return resolveFailure(fi.ws, fi.seenElsewhere, fi.triedToken, fi.lastErr, fi.issues, fi.profiles, fi.profileFixed)
}

// isCandidateRejection は「この候補（トークン / cookie）が受け付けられなかった」エラーかを返す。
// true なら次の候補へ進む。false（ネットワーク障害・5xx 等）は候補を変えても直らないので即座に返す。
//
// 🚨 JSON ではない応答（ログインページ）は候補側の問題として扱う。その cookie のセッションが
// 切れているだけで、後ろのプロファイルには有効なセッションがありうる。
func isCandidateRejection(err error) bool {
	var apiErr *APIError
	var notJSON *NotJSONError
	return errors.As(err, &apiErr) || errors.As(err, &notJSON)
}

// tokensFor は候補トークンを返す。明示指定があればそれだけ（ただし検証は同じ）。
func tokensFor(cfg config.Config, creds Credentials, profile, ws string) ([]string, error) {
	if t := strings.TrimSpace(cfg.Token); t != "" {
		return []string{t}, nil
	}
	return creds.Tokens(profile, ws)
}

// candidateProfiles は試すプロファイルの順序を決める。
// 明示指定があればそれだけ。auto のときは「前回の検出結果 → 全プロファイル」。
func candidateProfiles(cfg config.Config, ws string, creds Credentials) []string {
	if p := strings.TrimSpace(cfg.Profile); p != "" && p != auth.ProfileAuto {
		return []string{p}
	}
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	add(config.ReadProfileCache(ws))
	for _, p := range creds.Profiles() {
		add(p.Dir)
	}
	if len(out) == 0 {
		out = append(out, "Default")
	}
	return out
}

// profileScopeNote は「どこまで探したか」を案内に足す。
//
// 🚨 プロファイルを固定していると、探索は**そのプロファイルの中だけ**で終わる。
// これを書かないと「Chrome から見つかったのは次のワークスペースです」という一覧が
// 「Chrome 全体を探した結果」に読めてしまい、実際には別プロファイルにログイン
// している対象を「無い」と誤診する（実際に誤診した）。
func profileScopeNote(profiles []string, fixed bool) string {
	if !fixed || len(profiles) == 0 {
		return ""
	}
	return fmt.Sprintf(
		"\n  なお、探したのは Chrome プロファイル %q だけです（-profile / config.yml で固定されているため）。\n"+
			"  別のプロファイルにログインしているかもしれません。全プロファイルを探すには:\n"+
			"    slack <コマンド> -profile auto ...     （その場限り）\n"+
			"    slack config set profile auto          （恒久）\n"+
			"  どのプロファイルに何があるかは  slack setup  で一覧できます。", profiles[0])
}

// resolveFailure は失敗時の案内を組み立てる（仕様 §8）。
func resolveFailure(ws string, seenElsewhere map[string]string, triedToken int, lastErr error, issues []auth.ProfileIssue, profiles []string, profileFixed bool) error {
	// 🚨 読み取れなかったプロファイルの記録を、どの分岐でも落とさない。
	// 別の候補の失敗（認証が通らない等）で案内を組むと、本当の原因（権限・復号）が消える。
	scope := auth.IssueNote(issues) + profileScopeNote(profiles, profileFixed)
	if len(seenElsewhere) > 0 {
		domains := make([]string, 0, len(seenElsewhere))
		for d := range seenElsewhere {
			domains = append(domains, d)
		}
		sort.Strings(domains)
		var b strings.Builder
		fmt.Fprintf(&b, "ワークスペース %q のトークンが見つかりませんでした。\n", ws)
		b.WriteString("  Chrome から見つかったのは次のワークスペースです:\n")
		for _, d := range domains {
			name := seenElsewhere[d]
			if name == "" {
				name = "-"
			}
			fmt.Fprintf(&b, "    %-20s %s\n", d, name)
		}
		fmt.Fprintf(&b, "  対象を変えるなら:  slack config set workspace %s\n", domains[0])
		fmt.Fprintf(&b, "  %q を使うなら Chrome で https://%s.slack.com にログインしてください。", ws, ws)
		b.WriteString(scope)
		return errors.New(b.String())
	}
	if triedToken > 0 {
		return fmt.Errorf(
			"Slack のトークンは見つかりましたが、いずれも認証が通りませんでした（%d 件試行）。\n"+
				"  Chrome で https://%s.slack.com にログインし直してから再実行してください。\n"+
				"  直前のエラー: %v%s", triedToken, ws, lastErr, scope)
	}
	if lastErr != nil {
		if _, ok := auth.AsProfileIssue("", lastErr); ok {
			// 直前のエラー自体が読み取りの問題なら、それは一覧（scope）に載っているので本文に重ねない。
			return &wrappedError{
				msg: fmt.Sprintf("%s から Slack の資格情報を取り出せませんでした。%s", auth.ChromeName, scope),
				err: lastErr,
			}
		}
		if scope != "" {
			return fmt.Errorf("%w%s", lastErr, scope)
		}
		return lastErr
	}
	return fmt.Errorf(
		"Chrome から Slack の資格情報を取り出せませんでした。\n"+
			"  %s で https://%s.slack.com にログインしているか確認してください。%s",
		auth.ChromeName, ws, scope)
}

// wrappedError は案内文を差し替えつつ、元のエラーの型（errors.As）を保つ。
type wrappedError struct {
	msg string
	err error
}

func (e *wrappedError) Error() string { return e.msg }
func (e *wrappedError) Unwrap() error { return e.err }

// 型の取り違えを防ぐための静的チェック（ChromeCredentials が Credentials を満たすこと）。
var _ Credentials = ChromeCredentials{}
