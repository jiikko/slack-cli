package auth

import (
	"strings"
	"testing"

	"github.com/jiikko/dotfiles/src/chromecookie/chromecookietest"
)

const dcookieTestPassword = "testpassword"

// writeDCookieDB は隔離した HOME の下の Default プロファイルに、v24 の Cookie DB を作る。
func writeDCookieDB(t *testing.T, cookies []chromecookietest.Cookie) {
	t.Helper()
	chromecookietest.WriteCookieDB(t, isolateHome(t), "Default", "24", cookies)
}

// d cookie は送信先（<workspace>.slack.com）に送られうる host_key の行だけから読む。
// host-only の acme.slack.com の d は、送信先を slack.com や別のワークスペースと取り違えると読まれない。
func TestDCookieReadsWorkspaceHostOnlyCookie(t *testing.T) {
	writeDCookieDB(t, []chromecookietest.Cookie{
		{Host: "acme.slack.com", Name: "d", Enc: chromecookietest.Encrypt(t, dcookieTestPassword, "xoxd-acme", "acme.slack.com")},
	})
	got, err := readDCookie("Default", "acme.slack.com", []byte(dcookieTestPassword))
	if err != nil || got != "xoxd-acme" {
		t.Errorf("acme.slack.com の host-only の d を読めない: %q %v", got, err)
	}
}

// 他サイトの d cookie は読まない（復号もしない）。他サイトの行しか無く、それが鍵違いでも、
// 「復号に全件失敗」ではなく「cookie が無い」（ログインの案内）になる。
func TestDCookieDoesNotDecryptOtherSitesCookies(t *testing.T) {
	writeDCookieDB(t, []chromecookietest.Cookie{
		{Host: ".notslack.com", Name: "d", Enc: chromecookietest.Encrypt(t, "wrongpassword", "x", ".notslack.com")},
		{Host: "example.com", Name: "d", Enc: chromecookietest.Encrypt(t, "wrongpassword", "y", "example.com")},
	})
	_, err := readDCookie("Default", "acme.slack.com", []byte(dcookieTestPassword))
	if err == nil || isReadKind(err, DecryptFailed) || !strings.Contains(err.Error(), "ありません") {
		t.Errorf("他サイトの行を復号しようとした（cookie が無いことの案内になるべき）: %T %v", err, err)
	}
}
