package auth

import (
	"bytes"
	"strings"
	"testing"
)

const (
	tokA = "xoxc-1111111111-aaaaaaaaaa"
	tokB = "xoxc-2222222222-bbbbbbbbbb"
	tokC = "xoxc-3333333333-cccccccccc"
)

// 正規表現がトークンだけを拾うこと（前後のゴミを巻き込まない）。
func TestScanTokensExtractsExactToken(t *testing.T) {
	data := []byte(`{"token":"` + tokA + `","x":1}` + "\x00\x01" + tokB + `"`)
	got := scanTokens(data, "")
	if len(got) != 2 {
		t.Fatalf("2 件見つかるべき: %d 件", len(got))
	}
	if got[0].Token != tokA || got[1].Token != tokB {
		t.Errorf("抽出結果が違う: %q %q", got[0].Token, got[1].Token)
	}
	// 短すぎるもの・別プレフィックスは拾わない。
	for _, s := range []string{"xoxc-short", "xoxb-1111111111-aaaaaaaaaa", "xoxp-1111111111-aaaaaaaaaa"} {
		if len(scanTokens([]byte(s), "")) != 0 {
			t.Errorf("拾ってはいけない: %q", s)
		}
	}
}

// 近傍にワークスペース名があるトークンを先に試すこと（順序のヒント）。
//
// 🚨 これは順序だけの話で、採用の可否ではない。採用は auth.test の一致で決める
// （resolve_test.go が担保）。ここで検査するのは「正しい候補を先に試せるか」。
func TestTokenOrderingPrefersWorkspaceProximity(t *testing.T) {
	tc := newTokenCollector()
	// 1 ファイル目: ワークスペース名と無関係なトークン
	tc.add([]byte(`{"team":"other","token":"`+tokA+`"}`), "alpha")
	// 2 ファイル目: alpha.slack.com の近くにあるトークン
	tc.add([]byte(`{"domain":"alpha","url":"https://alpha.slack.com/","token":"`+tokB+`"}`), "alpha")

	got := tc.result()
	if len(got) != 2 {
		t.Fatalf("2 件になるべき: %d", len(got))
	}
	if got[0].Token != tokB {
		t.Errorf("ワークスペース近傍のトークンを先頭にすべき: got %q", got[0].Token)
	}
	if got[0].Score <= got[1].Score {
		t.Errorf("スコアが順序を説明していない: %d <= %d", got[0].Score, got[1].Score)
	}
}

// 離れた位置にあるワークスペース名は近傍と見なさないこと（窓の外）。
func TestProximityWindowHasBoundary(t *testing.T) {
	far := append([]byte("alpha"), bytes.Repeat([]byte("x"), proximityWindow+100)...)
	far = append(far, []byte(tokA)...)
	if s := scanTokens(far, "alpha")[0].Score; s != 0 {
		t.Errorf("窓の外なのにスコアが付いている: %d", s)
	}
	near := append([]byte(`"alpha"`), bytes.Repeat([]byte("x"), 10)...)
	near = append(near, []byte(tokA)...)
	if s := scanTokens(near, "alpha")[0].Score; s == 0 {
		t.Error("近傍なのにスコアが付いていない")
	}
}

// 同じトークンが複数ファイルに出ても 1 件にまとめ、最大スコアを採ること。
func TestTokenCollectorDedupesAndKeepsBestScore(t *testing.T) {
	tc := newTokenCollector()
	tc.add([]byte(`{"x":"`+tokA+`"}`), "alpha")                                  // score 0
	tc.add([]byte(`{"url":"https://alpha.slack.com/","y":"`+tokA+`"}`), "alpha") // score 2
	tc.add([]byte(`{"z":"`+tokC+`"}`), "alpha")

	got := tc.result()
	if len(got) != 2 {
		t.Fatalf("重複除去できていない: %d 件", len(got))
	}
	if got[0].Token != tokA || got[0].Score != 2 {
		t.Errorf("最大スコアを採っていない: %+v", got[0])
	}
}

// 同点なら検出順を保つこと（順序が乱数で変わると再現性が無くなる）。
func TestTokenCollectorStableOnTie(t *testing.T) {
	for i := 0; i < 20; i++ { // map の走査順は毎回変わるので繰り返して確かめる
		tc := newTokenCollector()
		tc.add([]byte(tokA+" "+tokB+" "+tokC), "")
		got := tc.result()
		if got[0].Token != tokA || got[1].Token != tokB || got[2].Token != tokC {
			t.Fatalf("検出順が保たれていない: %v", got)
		}
	}
}

// トークンが取れなかったときの案内に、次の一手が全部書かれていること。
func TestErrNoTokenGuidance(t *testing.T) {
	msg := (&ErrNoToken{Profile: "Profile 1"}).Error()
	for _, want := range []string{"Profile 1", "完全に終了", "-token"} {
		if !strings.Contains(msg, want) {
			t.Errorf("案内に %q が無い: %s", want, msg)
		}
	}
}
