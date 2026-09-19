// Package config は ~/.config/slack-cli/config.yml の読み書きと、
// 「コマンドラインフラグ > 環境変数 > config.yml > 組み込み既定」の優先順位解決を担う。
//
// パスはすべて HOME / XDG 基準で解決し、カレントディレクトリに一切依存しない。
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// 環境変数名（仕様 §5）。
const (
	EnvWorkspace = "SLACK_CLI_WORKSPACE"
	EnvProfile   = "SLACK_CLI_CHROME_PROFILE"
	EnvToken     = "SLACK_CLI_TOKEN"
)

// 組み込み既定。
const (
	DefaultProfile = "auto"
	DefaultCount   = 20
)

// File は config.yml の内容。すべて任意項目。
//
// 🚨 `team` は `workspace` の別名（読み込み専用）。保存時は必ず `workspace` に正規化して
// 書き出すため、`slack config set` / `slack setup` を一度でも実行すると既存の `team:` 行は
// ファイルから消える（値は workspace へ引き継がれる）。
type File struct {
	Workspace    string `yaml:"workspace,omitempty"`
	Team         string `yaml:"team,omitempty"`
	Profile      string `yaml:"profile,omitempty"`
	DefaultCount int    `yaml:"default_count,omitempty"`
}

// workspaceValue は workspace / team の別名を吸収した実効値を返す。
func (f File) workspaceValue() string {
	if f.Workspace != "" {
		return f.Workspace
	}
	return f.Team
}

// Config は解決済みの実行設定（フラグ解析後の値）。
type Config struct {
	Workspace string // 対象ワークスペースのサブドメイン（例: acme）。必須
	Profile   string // Chrome プロファイル名。auto で自動検出
	Token     string // xoxc トークンの明示指定（任意）
	JSON      bool   // 機械可読な JSON で出力する
	Count     int    // 既定の取得件数
}

// Host は接続先ホスト（<workspace>.slack.com）を返す。
func (c Config) Host() string { return c.Workspace + ".slack.com" }

// Dir は設定ディレクトリ（$XDG_CONFIG_HOME/slack-cli、無ければ ~/.config/slack-cli）を返す。
func Dir() (string, error) {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "slack-cli"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "slack-cli"), nil
}

// Path は config.yml のパスを返す。
func Path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.yml"), nil
}

var (
	loadOnce   sync.Once
	loadCached File
	loadErr    error // 解析に失敗したときの理由（config set はこれを見て書き込みを拒む）
)

// Problem は config.yml の解析に失敗していればその理由を返す。
func Problem() error {
	Load()
	return loadErr
}

// Load は config.yml を読む（無ければゼロ値）。プロセス内で 1 回だけ読む。
func Load() File {
	loadOnce.Do(func() {
		path, err := Path()
		if err != nil {
			return
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return // 無い場合はゼロ値
		}
		var fc File
		if err := yaml.Unmarshal(data, &fc); err != nil {
			loadErr = fmt.Errorf("%s の解析に失敗しました: %w", path, err)
			fmt.Fprintf(os.Stderr, "警告: %v\n", loadErr)
			return
		}
		loadCached = fc
	})
	return loadCached
}

// Save は config.yml を書き出す（ディレクトリごと作成）。
func Save(fc File) error {
	dir, err := Dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// team は別名なので workspace へ正規化してから書く（両方が残ると出典が二重になる）。
	if fc.Workspace == "" && fc.Team != "" {
		fc.Workspace = fc.Team
	}
	fc.Team = ""

	data, err := yaml.Marshal(fc)
	if err != nil {
		return err
	}
	header := "# slack-cli 設定ファイル（slack config set で更新できます）\n" +
		"# workspace:     対象ワークスペースのサブドメイン（https://<workspace>.slack.com）。必須\n" +
		"# profile:       使用する Chrome プロファイル名（auto でログイン済みを自動検出）\n" +
		"# default_count: 検索・取得の既定件数\n"
	path := filepath.Join(dir, "config.yml")
	return os.WriteFile(path, append([]byte(header), data...), 0o600)
}

// ResolveDefault は「環境変数 > config.yml > 組み込み既定」の順で既定値を決める。
// これを flag の既定値に使うことで、-flag 明示指定が最優先になる（flag > env > file > 既定）。
func ResolveDefault(envKey, fileVal, builtin string) string {
	v, _ := ResolveDefaultSource(envKey, fileVal, builtin)
	return v
}

// ResolveDefaultSource は値とその出所（env:KEY / file / default）を返す。
func ResolveDefaultSource(envKey, fileVal, builtin string) (value, source string) {
	if envKey != "" {
		if v := os.Getenv(envKey); v != "" {
			return v, "env:" + envKey
		}
	}
	if fileVal != "" {
		return fileVal, "file"
	}
	return builtin, "default"
}

// ResolveDefaultInt は数値項目の「環境変数 > config.yml > 既定」。
// 環境変数が数値として読めない場合は無視して次の候補へ落ちる。
func ResolveDefaultInt(envKey string, fileVal, builtin int) (value int, source string) {
	if envKey != "" {
		if v := os.Getenv(envKey); v != "" {
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
				return n, "env:" + envKey
			}
		}
	}
	if fileVal > 0 {
		return fileVal, "file"
	}
	return builtin, "default"
}

// Defaults は現在の config.yml / 環境変数から、各項目の既定値を返す。
func Defaults() (workspace, profile string, count int) {
	fc := Load()
	workspace = ResolveDefault(EnvWorkspace, fc.workspaceValue(), "")
	profile = ResolveDefault(EnvProfile, fc.Profile, DefaultProfile)
	count, _ = ResolveDefaultInt("", fc.DefaultCount, DefaultCount)
	return
}

// Keys は config set / get で指定できるキー。
//
// 🚨 ここへキーを足すときは File にフィールドも足すこと。足さないと
// 「保存しました」と表示しながら何も書かれない（keys_test.go が守る）。
var Keys = []string{"workspace", "profile", "default_count"}

// IsKey は key が設定可能かを返す（team は workspace の別名として受ける）。
func IsKey(key string) bool {
	if key == "team" {
		return true
	}
	for _, k := range Keys {
		if k == key {
			return true
		}
	}
	return false
}

// Get は config.yml 上の値を文字列で返す。
func Get(fc File, key string) (string, error) {
	switch key {
	case "workspace", "team":
		return fc.workspaceValue(), nil
	case "profile":
		return fc.Profile, nil
	case "default_count":
		if fc.DefaultCount == 0 {
			return "", nil
		}
		return strconv.Itoa(fc.DefaultCount), nil
	default:
		return "", fmt.Errorf("不明なキー %q（指定可能: %s）", key, strings.Join(Keys, ", "))
	}
}

// Set は File のキーを更新する。
func Set(fc *File, key, value string) error {
	switch key {
	case "workspace", "team":
		ws := NormalizeWorkspace(value)
		if err := ValidateWorkspace(ws); err != nil {
			return err
		}
		fc.Workspace = ws
		fc.Team = ""
	case "profile":
		fc.Profile = value
	case "default_count":
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || n <= 0 {
			return fmt.Errorf("default_count には 1 以上の整数を指定してください（指定値: %q）", value)
		}
		fc.DefaultCount = n
	default:
		return fmt.Errorf("不明なキー %q（指定可能: %s）", key, strings.Join(Keys, ", "))
	}
	return nil
}

// NormalizeWorkspace は URL 形式やフルホスト名で渡された値をサブドメインへ正規化する。
// 例: https://acme.slack.com/messages/C1 → acme / acme.slack.com → acme
//
// 🚨 「URL に見える入力」だけを切り詰める。素の文字列（alpha/../beta のような値）は
// そのまま返し、ValidateWorkspace に弾かせる。何でも最初の / で切ると、
// 明らかにおかしい入力を黙って別の値へ読み替えてしまう。
func NormalizeWorkspace(v string) string {
	s := strings.TrimSpace(v)
	hadScheme := false
	for _, p := range []string{"https://", "http://"} {
		if strings.HasPrefix(strings.ToLower(s), p) {
			s = s[len(p):]
			hadScheme = true
			break
		}
	}
	if hadScheme || strings.Contains(strings.ToLower(s), ".slack.com") {
		s = strings.TrimSuffix(s, "/")
		if i := strings.IndexByte(s, '/'); i >= 0 {
			s = s[:i]
		}
		s = strings.TrimSuffix(strings.ToLower(s), ".slack.com")
	}
	return strings.ToLower(s)
}

// ValidateWorkspace はサブドメインとして妥当かを検証する。
//
// 🚨 これは「接続先ホストの組み立て」に直接効く検証。ここを緩めると、
// workspace に "evil.example.com/" のような値を入れられたときに、
// 組み立てた URL の接続先が別ホストになりうる（safety net は client 側にもあるが、
// 入口で弾くのが本命）。
func ValidateWorkspace(ws string) error {
	if ws == "" {
		return fmt.Errorf("ワークスペース名が空です")
	}
	if len(ws) > 63 {
		return fmt.Errorf("ワークスペース名が長すぎます（63 文字以内）: %q", ws)
	}
	for i := 0; i < len(ws); i++ {
		c := ws[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
		default:
			return fmt.Errorf("ワークスペース名に使えない文字が含まれています（英小文字・数字・ハイフンのみ）: %q", ws)
		}
	}
	if ws[0] == '-' || ws[len(ws)-1] == '-' {
		return fmt.Errorf("ワークスペース名の先頭・末尾にハイフンは使えません: %q", ws)
	}
	return nil
}

// --- 自動検出結果のキャッシュ（cwd 非依存: 設定ディレクトリ配下） ---

func profileCachePath(workspace string) (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	cacheDir := filepath.Join(dir, "cache")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(cacheDir, "profile-"+workspace), nil
}

// ReadProfileCache は前回の自動検出結果（プロファイル名）を返す。
//
// 🚨 キャッシュするのはプロファイル名だけ。トークンや cookie は絶対に保存しない。
func ReadProfileCache(workspace string) string {
	p, err := profileCachePath(workspace)
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// WriteProfileCache は自動検出結果を保存する（失敗しても無視する）。
func WriteProfileCache(workspace, profile string) {
	p, err := profileCachePath(workspace)
	if err != nil {
		return
	}
	_ = os.WriteFile(p, []byte(profile+"\n"), 0o600)
}
