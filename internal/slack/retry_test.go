package slack

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jiikko/slack-cli/internal/auth"
)

// 🚨 テストでは実時間の待ちに入らない。差し替えずに待ちへ入ったら（Resolve を通る既存のテストが
// 429 を返したなど）、30 秒黙って待つ代わりにその場で落とす。
var realRetrySleep = retrySleep

func TestMain(m *testing.M) {
	retrySleep = func(context.Context, time.Duration) error {
		panic("retrySleep を差し替えずに待ちへ入った（テストが実時間を待つ）")
	}
	os.Exit(m.Run())
}

// fakeSleep は待ちを記録するだけの偽物を挿し、記録を返す。
func fakeSleep(t *testing.T) *[]time.Duration {
	t.Helper()
	var waits []time.Duration
	orig := retrySleep
	retrySleep = func(ctx context.Context, d time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		waits = append(waits, d)
		return nil
	}
	t.Cleanup(func() { retrySleep = orig })
	return &waits
}

// scriptTransport は呼び出しごとに決めた応答（status / Retry-After / 本文）を順に返す。尽きたら最後の応答を返し続ける。
type scriptTransport struct {
	mu    sync.Mutex
	steps []scriptStep
	calls int
	forms []url.Values
}

type scriptStep struct {
	status     int
	retryAfter string
	body       string
}

func (st *scriptTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	b, _ := io.ReadAll(req.Body)
	form, _ := url.ParseQuery(string(b))
	st.mu.Lock()
	i := st.calls
	if i >= len(st.steps) {
		i = len(st.steps) - 1
	}
	s := st.steps[i]
	st.calls++
	st.forms = append(st.forms, form)
	st.mu.Unlock()
	h := http.Header{"Content-Type": []string{"application/json"}}
	if s.retryAfter != "" {
		h.Set("Retry-After", s.retryAfter)
	}
	return &http.Response{StatusCode: s.status, Header: h, Body: io.NopCloser(strings.NewReader(s.body)), Request: req}, nil
}

func rateLimited(retryAfter string) scriptStep {
	return scriptStep{status: 429, retryAfter: retryAfter}
}
func okAuth() scriptStep {
	return scriptStep{status: 200, body: authTestBody("alpha", "Alpha", "alice")}
}

func retryClient(t *testing.T, st *scriptTransport, b *RetryBudget) *Client {
	t.Helper()
	c, err := New("alpha", "xoxc-test", "xoxd-test", WithHTTPClient(&http.Client{Transport: st}), WithRetry(b))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// 429（Retry-After: N）の後に 200 が返るなら、N 秒待って同じリクエストをやり直し、成功として返すこと。
func TestRetryWaitsRetryAfterThenSucceeds(t *testing.T) {
	waits := fakeSleep(t)
	st := &scriptTransport{steps: []scriptStep{rateLimited("30"), okAuth()}}
	var notify bytes.Buffer
	if _, err := retryClient(t, st, NewRetryBudget(&notify)).AuthTest(context.Background()); err != nil {
		t.Fatalf("待ってやり直せば成功するのに失敗した: %v", err)
	}
	if st.calls != 2 || len(*waits) != 1 || (*waits)[0] != 30*time.Second {
		t.Errorf("呼び出し %d 回 / 待ち %v（want 2 回 / [30s]）", st.calls, *waits)
	}
	if !strings.Contains(notify.String(), "30 秒待って再試行します") {
		t.Errorf("待つことを知らせていない: %q", notify.String())
	}
}

// ページングの 2 ページ目で 429 が出ても、待ってから続きを取り、全件がそろうこと（PartialError にならない）。
func TestRetryCompletesPagingAfterRateLimit(t *testing.T) {
	waits := fakeSleep(t)
	page := func(id, next string) scriptStep {
		return scriptStep{status: 200, body: fmt.Sprintf(`{"ok":true,"channels":[{"id":%q,"name":%q}],"response_metadata":{"next_cursor":%q}}`, id, id, next)}
	}
	st := &scriptTransport{steps: []scriptStep{page("C1", "c2"), rateLimited("30"), page("C2", "c3"), rateLimited("30"), page("C3", "")}}
	chs, err := retryClient(t, st, NewRetryBudget(nil)).Channels(context.Background(), "", 0)
	if err != nil {
		t.Fatalf("待てば取りきれるのに失敗した: %v", err)
	}
	if len(chs) != 3 || len(*waits) != 2 {
		t.Errorf("件数 %d（want 3）/ 待ち %v（want 2 回）", len(chs), *waits)
	}
	// やり直しは同じカーソルで送ること（ページを飛ばさない）。
	if st.forms[1].Get("cursor") != "c2" || st.forms[2].Get("cursor") != "c2" {
		t.Errorf("やり直しのカーソル: %q → %q（want c2 → c2）", st.forms[1].Get("cursor"), st.forms[2].Get("cursor"))
	}
}

// 待つ時間が分からない・長すぎるときは待たず、今までと同じ 429 のエラーにすること。
func TestRetryDoesNotWaitWithoutUsableRetryAfter(t *testing.T) {
	// 9300000000 / 18446744074 は秒 → Duration の掛け算で桁があふれる値（負 / 290ms になって上限を素通りしていた）。
	for _, ra := range []string{"", "Wed, 21 Oct 2015 07:28:00 GMT", "-5", "61", "9300000000", "18446744074"} {
		t.Run(ra, func(t *testing.T) {
			waits := fakeSleep(t)
			st := &scriptTransport{steps: []scriptStep{rateLimited(ra), okAuth()}}
			_, err := retryClient(t, st, NewRetryBudget(nil)).AuthTest(context.Background())
			var rl *RateLimitError
			if !errors.As(err, &rl) || !strings.Contains(err.Error(), "待って再実行してください") {
				t.Fatalf("今までと同じ 429 のエラーであるべき: %v", err)
			}
			if st.calls != 1 || len(*waits) != 0 {
				t.Errorf("待つべきでないのに待った: 呼び出し %d 回 / 待ち %v", st.calls, *waits)
			}
		})
	}
}

// 1 リクエストで 3 回やり直しても 429 なら諦めること。
func TestRetryGivesUpAfterMaxAttempts(t *testing.T) {
	waits := fakeSleep(t)
	st := &scriptTransport{steps: []scriptStep{rateLimited("1")}}
	_, err := retryClient(t, st, NewRetryBudget(nil)).AuthTest(context.Background())
	var rl *RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("429 のエラーであるべき: %v", err)
	}
	if st.calls != 1+defaultRetryMaxAttempts || len(*waits) != defaultRetryMaxAttempts {
		t.Errorf("呼び出し %d 回 / 待ち %d 回（want %d / %d）", st.calls, len(*waits), 1+defaultRetryMaxAttempts, defaultRetryMaxAttempts)
	}
}

// 待ちの合計は上限（180 秒）を超えないこと。上限はクライアントをまたいで共有されること。
func TestRetryTotalBudgetIsSharedAcrossClients(t *testing.T) {
	waits := fakeSleep(t)
	b := NewRetryBudget(nil)
	// 60 秒 × 3 で上限に達する。4 回目の待ちは別のクライアントでも行わない。
	for i := 0; i < 2; i++ {
		st := &scriptTransport{steps: []scriptStep{rateLimited("60"), rateLimited("60"), okAuth()}}
		_, _ = retryClient(t, st, b).AuthTest(context.Background())
	}
	var total time.Duration
	for _, w := range *waits {
		total += w
	}
	if total != defaultRetryTotal {
		t.Errorf("待ちの合計 %v（want %v）: %v", total, defaultRetryTotal, *waits)
	}
}

// context が取り消されたら、待ちから抜けてそれ以上やり直さないこと。
func TestRetryStopsOnContextCancel(t *testing.T) {
	fakeSleep(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	st := &scriptTransport{steps: []scriptStep{rateLimited("30"), okAuth()}}
	_, err := retryClient(t, st, NewRetryBudget(nil)).AuthTest(ctx)
	if err == nil || st.calls > 1 {
		t.Errorf("取り消し後もやり直した: 呼び出し %d 回 / err %v", st.calls, err)
	}
}

// WithRetry を付けない Client は今までどおり待たないこと（素の New は再試行しない）。
func TestPlainClientDoesNotRetry(t *testing.T) {
	waits := fakeSleep(t)
	st := &scriptTransport{steps: []scriptStep{rateLimited("30"), okAuth()}}
	c, err := New("alpha", "xoxc-test", "xoxd-test", WithHTTPClient(&http.Client{Transport: st}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.AuthTest(context.Background()); err == nil || st.calls != 1 || len(*waits) != 0 {
		t.Errorf("素の Client が再試行した: 呼び出し %d 回 / 待ち %v / err %v", st.calls, *waits, err)
	}
}

// Resolve で作ったクライアントは再試行し、429 を「その候補が無効」として次の候補へ進まないこと。
func TestResolveClientRetriesAndDoesNotSkipCandidateOnRateLimit(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	waits := fakeSleep(t)
	st := &scriptTransport{steps: []scriptStep{rateLimited("30"), okAuth()}}
	creds := fakeCreds{
		profiles: []auth.Profile{{Dir: "Profile 1"}},
		cookies:  map[string]string{"Profile 1": sentinelCookie},
		tokens:   map[string][]string{"Profile 1": {tokenForAlpha, tokenForBeta}},
	}
	var stderr bytes.Buffer
	sess, err := Resolve(context.Background(), alphaConfig(), creds, &stderr, WithHTTPClient(&http.Client{Transport: st}))
	if err != nil {
		t.Fatalf("Resolve が 429 の後に待ってやり直していない: %v", err)
	}
	if len(*waits) != 1 || st.forms[1].Get("token") != tokenForAlpha {
		t.Errorf("待ち %v / やり直しのトークンが同じでない（候補を捨てた）", *waits)
	}
	if !strings.Contains(stderr.String(), "待って再試行します") {
		t.Errorf("待つ通知が Resolve の stderr に出ていない: %q", stderr.String())
	}
	if isCandidateRejection(newRateLimitError("auth.test", "30")) {
		t.Error("429 を候補の無効として扱っている")
	}
	_ = sess
}

// 本物の待ちは、context が取り消されたらすぐ抜けること（Retry-After が長くても待ち続けない）。
func TestRealRetrySleepHonorsCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() { done <- realRetrySleep(ctx, time.Hour) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("取り消しの理由を返していない: %v", err)
		}
	case <-time.After(10 * time.Second): // 安全網（合否の基準ではない）
		t.Fatal("取り消された context で待ち続けた")
	}
}

// Resolve は待ちの上限を全候補で共有すること（候補ごとに作ると、候補の数だけ上限が増える）。
func TestResolveSharesRetryBudgetAcrossCandidates(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	waits := fakeSleep(t)
	// 1 つ目の候補: 60 秒の待ちを 2 回してから無効（次の候補へ）。2 つ目の候補: 60 秒の待ちを 2 回してから成功。
	// 共有なら合計 180 秒で止まり、2 つ目の 2 回目の待ちには入らない。
	st := &scriptTransport{steps: []scriptStep{
		rateLimited("60"), rateLimited("60"), {status: 200, body: `{"ok":false,"error":"invalid_auth"}`},
		rateLimited("60"), rateLimited("60"), okAuth(),
	}}
	creds := fakeCreds{
		profiles: []auth.Profile{{Dir: "Profile 1"}},
		cookies:  map[string]string{"Profile 1": sentinelCookie},
		tokens:   map[string][]string{"Profile 1": {tokenInvalidated, tokenForAlpha}},
	}
	_, _ = Resolve(context.Background(), alphaConfig(), creds, nil, WithHTTPClient(&http.Client{Transport: st}))
	var total time.Duration
	for _, w := range *waits {
		total += w
	}
	if total > defaultRetryTotal {
		t.Errorf("候補をまたいだ待ちの合計 %v が上限 %v を超えた: %v", total, defaultRetryTotal, *waits)
	}
}
