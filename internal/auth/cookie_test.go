package auth

// 復号・ドメイン一致のテストは chromecookie 側にある。ここは slack-cli 固有の部分だけ。

import (
	"strings"
	"testing"

	"github.com/jiikko/dotfiles/src/chromecookie"
)

// 同名 Cookie が複数あるときは、より具体的な host のものを選ぶこと。
func TestPickCookiePrefersSpecificHost(t *testing.T) {
	entries := []chromecookie.Cookie{
		{Host: ".slack.com", Name: "d", Value: "domain-wide"},
		{Host: "alpha.slack.com", Name: "d", Value: "host-only"},
		{Host: ".other.com", Name: "d", Value: "unrelated"},
		{Host: "alpha.slack.com", Name: "d", Value: ""}, // 空は候補にしない
	}
	got, ok := pickCookie(entries, "alpha.slack.com")
	if !ok || got != "host-only" {
		t.Errorf("got %q (ok=%v), want host-only", got, ok)
	}
	if _, ok := pickCookie(entries, "beta.example.com"); ok {
		t.Error("無関係なホストに cookie を返してはいけない")
	}
}

// Mask は生値を返さないこと（デバッグ表示の唯一の口）。
func TestMaskDoesNotRevealSecret(t *testing.T) {
	const secret = "xoxc-1234567890-abcdefghijklmnop"
	got := Mask(secret)
	if strings.Contains(got, "abcdefghijklmnop") || got == secret {
		t.Errorf("生値が残っている: %q", got)
	}
	if !strings.HasPrefix(got, "xoxc-") {
		t.Errorf("種別が分かるプレフィックスは残すべき: %q", got)
	}
	if strings.Contains(got, "7890") {
		t.Errorf("マスクが短すぎる: %q", got)
	}
	if Mask("") != "(なし)" {
		t.Errorf("空の表示: %q", Mask(""))
	}
}
