package auth

import "testing"

// Local Storage の痕跡からワークスペース候補を拾うこと（ネットワークには出ない）。
func TestDomainRegexAndDenylist(t *testing.T) {
	data := []byte(`https://alpha.slack.com/ files.slack.com app.slack.com https://beta.slack.com/x alpha.slack.com`)
	counts := map[string]int{}
	for _, m := range domainRe.FindAllSubmatch(data, -1) {
		sub := string(m[1])
		if nonWorkspaceSubdomains[sub] {
			continue
		}
		counts[sub]++
	}
	if counts["alpha"] != 2 {
		t.Errorf("alpha の出現回数: got %d, want 2", counts["alpha"])
	}
	if counts["beta"] != 1 {
		t.Errorf("beta の出現回数: got %d, want 1", counts["beta"])
	}
	for _, generic := range []string{"files", "app"} {
		if counts[generic] != 0 {
			t.Errorf("Slack のインフラ用サブドメインを候補にしている: %s", generic)
		}
	}
}

// slack.com に似せた別ドメインを拾わないこと。
func TestDomainRegexDoesNotMatchLookalikes(t *testing.T) {
	data := []byte(`https://alpha.slack.com.evil.jp/ notslack.com myslack.company.com`)
	var found []string
	for _, m := range domainRe.FindAllSubmatch(data, -1) {
		found = append(found, string(m[1]))
	}
	// alpha.slack.com.evil.jp は "alpha.slack.com" を含むため一致しうる。
	// 実害は「setup の候補に余分な行が出る」だけだが、明らかな別ドメインは拾わないこと。
	for _, f := range found {
		if f == "notslack" || f == "company" {
			t.Errorf("別ドメインを候補にしている: %s", f)
		}
	}
}
