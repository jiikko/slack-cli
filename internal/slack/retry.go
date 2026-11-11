package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// RateLimitError は 429（レート制限）。
//
// 🚨 「その候補（トークン / cookie）が無効」には分類しない（isCandidateRejection に入れない）。
// 入れると、混んでいるだけのときに有効なトークンを捨てて次の候補へ進む。
type RateLimitError struct {
	Method     string
	RetryAfter string        // Retry-After ヘッダの生値（無ければ空）
	Wait       time.Duration // RetryAfter を秒として読めたときの待ち時間
	HasWait    bool          // RetryAfter を 0 以上の整数の秒として読めたか
}

// maxRetryAfterSeconds は Retry-After を秒として読む上限（1 日）。
//
// 🚨 秒のまま比べてから time.Duration に変える。先に掛け算すると巨大な値（9300000000 等）で桁があふれて
// 負や短い値になり、RetryBudget の合計の上限が効かなくなる（issue 006 の敵対的レビュー）。
const maxRetryAfterSeconds = 24 * 60 * 60

func newRateLimitError(method, retryAfter string) *RateLimitError {
	e := &RateLimitError{Method: method, RetryAfter: strings.TrimSpace(retryAfter)}
	if n, err := strconv.Atoi(e.RetryAfter); err == nil && n >= 0 && n <= maxRetryAfterSeconds {
		e.Wait, e.HasWait = time.Duration(n)*time.Second, true
	}
	return e
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("レート制限（429）: %s。%s待って再実行してください", e.Method, retryAfterText(e.RetryAfter))
}

// 429 を受けたときに待つ上限の既定値。
//
// conversations.list（Tier 2、1 分あたり 20 回前後）を約 3500 件 = 約 18 ページ取ると、Slack は
// Retry-After: 30 を返した（issue 006 の実測）。30 秒の待ちを数回はさめば 1 回の取得が最後まで終わる。
// 上限は「人が待てる長さ」で決めている。超えたら今までどおり諦めて、取れた分を出す（PartialError）。
const (
	defaultRetryMaxWait     = 60 * time.Second  // 1 回の待ちの上限（これより長い Retry-After は待たない）
	defaultRetryMaxAttempts = 3                 // 1 リクエストあたりのやり直しの回数
	defaultRetryTotal       = 180 * time.Second // 待ちの合計の上限
)

// retrySleep は待ちの実体。テストは偽物に差し替え、実時間を待たない。
var retrySleep = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// RetryBudget は 429 を受けたときに待つかどうかを決め、待った合計を持つ。
//
// 🚨 Resolve 1 回で 1 つを作り、全候補のクライアントで共有する。Resolve は候補（プロファイル × トークン）ごとに
// New するので、クライアントごとに作ると候補の数だけ上限が増える。
// クライアントは並行に使わない（1 コマンド 1 本の逐次の呼び出し）前提で、排他は持たない。
type RetryBudget struct {
	maxWait     time.Duration
	maxAttempts int
	total       time.Duration
	waited      time.Duration
	notify      io.Writer // 待つことを知らせる先（stderr）。nil なら知らせない
	// lastCall はメソッドごとの最後の呼び出しが終わった時刻（pace が間を空ける）
	lastCall map[string]time.Time
}

// paceGap は同じメソッドを続けて呼ぶとき、前の呼び出しの終わりから空ける間。Slack の Web API のレート制限の段
// （https://docs.slack.dev/apis/web-api/rate-limits）に合わせる: Tier 2 は 1 分に 20 回ほど、Tier 3 は 50 回ほど。
// ページ送り（history・replies・conversations.list の cursor）を間を空けずに打つと、段の上限に当たって 429 を受ける（issue 006 の実測）。
// 受けてから待つのでなく、先に間を空けて上限を超えない（Slack のレート制限の作法）。間は段の上限ちょうどで、余りは呼び出しにかかる時間の分だけ。
// その分、ページの多い取得は遅くなる（conversations.list の 18 ページで待ちが約 51 秒。チャンネルの一覧は channelcache が覚えるので毎回ではない）。
// 速さの前後は未実測。Tier 4 以上（auth.test・users.info）は空けない。
// RetryBudget は Resolve の候補（別のトークン）で共有するので、候補をまたいでも同じメソッドの間を空ける。今の Resolve が候補ごとに呼ぶのは
// auth.test（空けない）だけなので、候補を試す速さは変わらない。
func paceGap(method string) time.Duration {
	switch method {
	case "search.messages", "search.modules", "conversations.list", "users.list", "emoji.list": // Tier 2
		return 3 * time.Second
	case "conversations.history", "conversations.replies", "conversations.info", "users.lookupByEmail", "team.info": // Tier 3
		return 1200 * time.Millisecond
	}
	return 0
}

// paceSleep / paceNow は間を空ける待ちと時計。テストは差し替えて実時間を待たない。
var (
	paceSleep = retrySleep
	paceNow   = time.Now
)

// pace は method の前の呼び出しの終わりから paceGap だけ空けてから戻る。
func (b *RetryBudget) pace(ctx context.Context, method string) error {
	last, ok := b.lastCall[method]
	if !ok {
		return nil
	}
	return paceSleep(ctx, last.Add(paceGap(method)).Sub(paceNow()))
}

// called は method の呼び出しが終わった時刻を覚える。
func (b *RetryBudget) called(method string) {
	if b.lastCall == nil {
		b.lastCall = map[string]time.Time{}
	}
	b.lastCall[method] = paceNow()
}

// NewRetryBudget は既定の上限で RetryBudget を作る。notify には stderr を渡す（stdout は -json の出力なので使わない）。
func NewRetryBudget(notify io.Writer) *RetryBudget {
	return &RetryBudget{
		maxWait:     defaultRetryMaxWait,
		maxAttempts: defaultRetryMaxAttempts,
		total:       defaultRetryTotal,
		notify:      notify,
	}
}

// WithRetry は 429 を受けたら Retry-After だけ待ってやり直し、同じメソッドを続けて呼ぶときは間を空ける（pace）ようにする。
// 付けない Client は今までどおり 429 をそのまま返し、間も空けない（Resolve が付ける）。
func WithRetry(b *RetryBudget) Option {
	return func(c *Client) { c.retry = b }
}

// take は attempt 回目のやり直しのために e.Wait を待ってよいかを決め、よければ待った合計に足す。
// Retry-After を秒として読めないときは待たない（待つ時間を推測しない）。
func (b *RetryBudget) take(e *RateLimitError, attempt int) bool {
	if !e.HasWait || e.Wait > b.maxWait || attempt > b.maxAttempts || b.waited+e.Wait > b.total {
		return false
	}
	b.waited += e.Wait
	return true
}

// callRetrying は do を呼び、429 なら RetryBudget の範囲で Retry-After だけ待ってやり直す。
// 呼べるのは読み取り専用のメソッドだけ（allowlist）なので、同じリクエストをやり直しても副作用は無い。
func (c *Client) callRetrying(ctx context.Context, m Method, params url.Values) (json.RawMessage, error) {
	for attempt := 1; ; attempt++ {
		if c.retry != nil {
			if err := c.retry.pace(ctx, m.name); err != nil {
				return nil, err
			}
		}
		raw, err := c.do(ctx, m, params)
		if c.retry != nil {
			c.retry.called(m.name)
		}
		var rl *RateLimitError
		if err == nil || c.retry == nil || !errors.As(err, &rl) || !c.retry.take(rl, attempt) {
			return raw, err
		}
		if c.retry.notify != nil {
			fmt.Fprintf(c.retry.notify, "レート制限（429）: %s。%d 秒待って再試行します（%d/%d）\n",
				m.name, int(rl.Wait/time.Second), attempt, c.retry.maxAttempts)
		}
		if err := retrySleep(ctx, rl.Wait); err != nil {
			return nil, fmt.Errorf("%w（レート制限の待機を中断しました: %v）", rl, err)
		}
	}
}
