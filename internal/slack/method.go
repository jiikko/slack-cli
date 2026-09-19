package slack

// 読み取り専用メソッドの allowlist（仕様 §7.2）。
//
// 🚨 これはワークスペース限定と並ぶ安全装置。2 段で守る:
//
//  ① 型で閉じる: Method は非公開フィールドしか持たないため、この package の外では
//     値を作れない。つまり chat.postMessage のような書き込みメソッドは
//     **呼び出しコードがコンパイルできない**（存在しない識別子になる）。
//  ② 実行時に拒否する: package 内でうっかり Method{"chat.postMessage"} と書いた場合に
//     備え、送信の直前に名前を allowlist と突き合わせる（client.go の do）。
//
// ①だけだと package 内の新しいコードが素通りし、②だけだと呼び出し側が任意の文字列を
// 渡せる。method_test.go が①（AST で Method リテラルの出現箇所を固定）と②の両方に
// 変異を当てて red を確認している。

// Method は呼び出してよい Slack API メソッド。
type Method struct{ name string }

// String はメソッド名を返す（URL 組み立てとエラーメッセージ用）。
func (m Method) String() string { return m.name }

// 許可メソッド。ここに無いものは呼べない。
var (
	MethodAuthTest             = Method{"auth.test"}
	MethodSearchMessages       = Method{"search.messages"}
	MethodSearchModules        = Method{"search.modules"}
	MethodConversationsList    = Method{"conversations.list"}
	MethodConversationsHistory = Method{"conversations.history"}
	MethodConversationsReplies = Method{"conversations.replies"}
	MethodConversationsInfo    = Method{"conversations.info"}
	MethodUsersList            = Method{"users.list"}
	MethodUsersInfo            = Method{"users.info"}
	MethodUsersLookupByEmail   = Method{"users.lookupByEmail"}
	MethodTeamInfo             = Method{"team.info"}
	MethodEmojiList            = Method{"emoji.list"}
)

// allowedMethods は②（実行時ガード）が参照する集合。
//
// 上の var 群と同じ名前を二重に持つのは意図的（片方だけを書き換える変異を検出するため）。
// 齟齬は method_test.go が「全 Method 変数が allowedMethods に載っていること」で固定する。
var allowedMethods = map[string]bool{
	"auth.test":             true,
	"search.messages":       true,
	"search.modules":        true,
	"conversations.list":    true,
	"conversations.history": true,
	"conversations.replies": true,
	"conversations.info":    true,
	"users.list":            true,
	"users.info":            true,
	"users.lookupByEmail":   true,
	"team.info":             true,
	"emoji.list":            true,
}

// isAllowed は送信直前の実行時ガード。
func isAllowed(name string) bool { return allowedMethods[name] }
