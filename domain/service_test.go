package domain

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/chris64233/go-ad-inventory/clock"
	"github.com/chris64233/go-ad-inventory/money"
	"github.com/chris64233/go-ad-inventory/store"
)

var testStart = time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)

func newTestService(t *testing.T) (*Service, *clock.Fake) {
	t.Helper()
	clk := clock.NewFake(testStart)
	svc, err := NewService(clk, store.NewMemStore())
	if err != nil {
		t.Fatal(err)
	}
	return svc, clk
}

func mustCampaign(t *testing.T, svc *Service, total, daily string) *Campaign {
	t.Helper()
	c, err := svc.CreateCampaign(context.Background(), CreateCampaignParams{
		Name:        "test",
		TotalBudget: money.MustParse(total, "CNY"),
		DailyCap:    money.MustParse(daily, "CNY"),
		Timezone:    "UTC",
		DefaultTTL:  time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func mustReserve(t *testing.T, svc *Service, campaignID, reqID, amount string) *Reservation {
	t.Helper()
	r, err := svc.Reserve(context.Background(), ReserveParams{
		CampaignID: campaignID,
		RequestID:  reqID,
		Amount:     money.MustParse(amount, "CNY"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func balance(t *testing.T, svc *Service, campaignID string) *Balance {
	t.Helper()
	b, err := svc.GetBalance(context.Background(), campaignID)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestReserveSuccess(t *testing.T) {
	svc, clk := newTestService(t)
	c := mustCampaign(t, svc, "100", "50")

	r := mustReserve(t, svc, c.ID, "req-1", "30")
	if r.Status != StatusReserved {
		t.Fatalf("status = %s", r.Status)
	}
	if r.ExpiresAt != testStart.Add(time.Hour) {
		t.Fatalf("expires at %v, want %v", r.ExpiresAt, testStart.Add(time.Hour))
	}
	if r.DayKey != "2026-09-26" {
		t.Fatalf("day key = %s", r.DayKey)
	}
	_ = clk

	b := balance(t, svc, c.ID)
	if b.TotalReserved.String() != "30" || b.TotalAvailable.String() != "70" {
		t.Fatalf("total: reserved=%s available=%s", b.TotalReserved, b.TotalAvailable)
	}
	if b.DailyReserved.String() != "30" || b.DailyAvailable.String() != "20" {
		t.Fatalf("daily: reserved=%s available=%s", b.DailyReserved, b.DailyAvailable)
	}
}

func TestReserveValidation(t *testing.T) {
	svc, _ := newTestService(t)
	c := mustCampaign(t, svc, "100", "50")

	cases := []ReserveParams{
		{CampaignID: "", RequestID: "r", Amount: money.MustParse("1", "CNY")},
		{CampaignID: c.ID, RequestID: "", Amount: money.MustParse("1", "CNY")},
		{CampaignID: c.ID, RequestID: "r", Amount: money.MustParse("0", "CNY")},
		{CampaignID: c.ID, RequestID: "r", Amount: money.MustParse("-1", "CNY")},
		{CampaignID: c.ID, RequestID: "r", Amount: money.MustParse("1", "USD")},
	}
	for i, p := range cases {
		if _, err := svc.Reserve(context.Background(), p); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("case %d: err = %v, want invalid_argument", i, err)
		}
	}
	if _, err := svc.Reserve(context.Background(), ReserveParams{
		CampaignID: "cmp_missing", RequestID: "r", Amount: money.MustParse("1", "CNY"),
	}); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing campaign: want not_found, got %v", err)
	}
}

func TestReserveTotalBudgetExceeded(t *testing.T) {
	svc, _ := newTestService(t)
	c := mustCampaign(t, svc, "100", "1000")

	mustReserve(t, svc, c.ID, "req-1", "80")
	_, err := svc.Reserve(context.Background(), ReserveParams{
		CampaignID: c.ID, RequestID: "req-2", Amount: money.MustParse("30", "CNY"),
	})
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("err = %v, want budget_exceeded", err)
	}
	de, _ := AsError(err)
	if de.Level != "total" {
		t.Fatalf("level = %q, want total", de.Level)
	}
	if de.Available.String() != "20" {
		t.Fatalf("available = %s, want 20", de.Available)
	}
}

func TestReserveDailyCapExceeded(t *testing.T) {
	svc, _ := newTestService(t)
	c := mustCampaign(t, svc, "1000", "50")

	mustReserve(t, svc, c.ID, "req-1", "40")
	_, err := svc.Reserve(context.Background(), ReserveParams{
		CampaignID: c.ID, RequestID: "req-2", Amount: money.MustParse("20", "CNY"),
	})
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("err = %v, want budget_exceeded", err)
	}
	if de, _ := AsError(err); de.Level != "daily" {
		t.Fatalf("level = %q, want daily", de.Level)
	}
}

// TestReserveAtomicAcrossLevels 日预算不足时整体失败，总预算不得留下半笔预占。
func TestReserveAtomicAcrossLevels(t *testing.T) {
	svc, _ := newTestService(t)
	c := mustCampaign(t, svc, "1000", "50")

	mustReserve(t, svc, c.ID, "req-1", "50") // 打满当日
	_, err := svc.Reserve(context.Background(), ReserveParams{
		CampaignID: c.ID, RequestID: "req-2", Amount: money.MustParse("10", "CNY"),
	})
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("err = %v", err)
	}
	b := balance(t, svc, c.ID)
	if b.TotalReserved.String() != "50" {
		t.Fatalf("total reserved = %s, want 50 (no partial hold)", b.TotalReserved)
	}
}

func TestReserveIdempotency(t *testing.T) {
	svc, _ := newTestService(t)
	c := mustCampaign(t, svc, "100", "50")

	r1 := mustReserve(t, svc, c.ID, "req-1", "30")
	// 同编号同内容 → 返回原凭证，不重复占额。
	r2, err := svc.Reserve(context.Background(), ReserveParams{
		CampaignID: c.ID, RequestID: "req-1", Amount: money.MustParse("30", "CNY"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if r2.ID != r1.ID {
		t.Fatalf("idempotent replay returned %s, want %s", r2.ID, r1.ID)
	}
	if b := balance(t, svc, c.ID); b.TotalReserved.String() != "30" {
		t.Fatalf("reserved = %s, want 30", b.TotalReserved)
	}

	// 同编号不同内容 → 幂等冲突。
	_, err = svc.Reserve(context.Background(), ReserveParams{
		CampaignID: c.ID, RequestID: "req-1", Amount: money.MustParse("40", "CNY"),
	})
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("err = %v, want idempotency_conflict", err)
	}
}

func TestCapturePartialReleasesDifference(t *testing.T) {
	svc, _ := newTestService(t)
	c := mustCampaign(t, svc, "100", "50")

	r := mustReserve(t, svc, c.ID, "req-1", "30")
	got, err := svc.Capture(context.Background(), CaptureParams{
		ReservationID: r.ID, ReceiptID: "rcpt-1", Amount: money.MustParse("20", "CNY"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusCaptured || got.Captured.String() != "20" {
		t.Fatalf("status=%s captured=%s", got.Status, got.Captured)
	}
	b := balance(t, svc, c.ID)
	// 核销 20，差额 10 释放：spent=20, reserved=0。
	if b.TotalSpent.String() != "20" || b.TotalReserved.String() != "0" || b.TotalAvailable.String() != "80" {
		t.Fatalf("total: spent=%s reserved=%s available=%s", b.TotalSpent, b.TotalReserved, b.TotalAvailable)
	}
	if b.DailySpent.String() != "20" || b.DailyReserved.String() != "0" || b.DailyAvailable.String() != "30" {
		t.Fatalf("daily: spent=%s reserved=%s available=%s", b.DailySpent, b.DailyReserved, b.DailyAvailable)
	}
}

func TestCaptureValidation(t *testing.T) {
	svc, _ := newTestService(t)
	c := mustCampaign(t, svc, "100", "50")
	r := mustReserve(t, svc, c.ID, "req-1", "30")

	// 超过预占额 → 参数错误。
	if _, err := svc.Capture(context.Background(), CaptureParams{
		ReservationID: r.ID, ReceiptID: "rcpt-x", Amount: money.MustParse("31", "CNY"),
	}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("over-capture: err = %v, want invalid_argument", err)
	}
	// 非正金额。
	if _, err := svc.Capture(context.Background(), CaptureParams{
		ReservationID: r.ID, ReceiptID: "rcpt-y", Amount: money.MustParse("0", "CNY"),
	}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("zero capture: err = %v", err)
	}
	// 不存在的凭证。
	if _, err := svc.Capture(context.Background(), CaptureParams{
		ReservationID: "rsv_missing", ReceiptID: "rcpt-z", Amount: money.MustParse("1", "CNY"),
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing reservation: err = %v", err)
	}
}

func TestReceiptDeduplication(t *testing.T) {
	svc, _ := newTestService(t)
	c := mustCampaign(t, svc, "100", "50")
	r := mustReserve(t, svc, c.ID, "req-1", "30")

	_, err := svc.Capture(context.Background(), CaptureParams{
		ReservationID: r.ID, ReceiptID: "rcpt-1", Amount: money.MustParse("20", "CNY"),
	})
	if err != nil {
		t.Fatal(err)
	}
	// 重复回执（同标识同内容）→ 幂等返回，不重复计费。
	again, err := svc.Capture(context.Background(), CaptureParams{
		ReservationID: r.ID, ReceiptID: "rcpt-1", Amount: money.MustParse("20", "CNY"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != r.ID {
		t.Fatalf("dedup replay returned %s", again.ID)
	}
	if b := balance(t, svc, c.ID); b.TotalSpent.String() != "20" {
		t.Fatalf("spent = %s, want 20 (no double count)", b.TotalSpent)
	}

	// 同回执标识不同内容 → 幂等冲突。
	if _, err := svc.Capture(context.Background(), CaptureParams{
		ReservationID: r.ID, ReceiptID: "rcpt-1", Amount: money.MustParse("25", "CNY"),
	}); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("err = %v, want idempotency_conflict", err)
	}

	// 不同回执打到已核销凭证 → 状态冲突。
	if _, err := svc.Capture(context.Background(), CaptureParams{
		ReservationID: r.ID, ReceiptID: "rcpt-2", Amount: money.MustParse("5", "CNY"),
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want conflict", err)
	}
}

func TestCancelReleasesAll(t *testing.T) {
	svc, _ := newTestService(t)
	c := mustCampaign(t, svc, "100", "50")
	r := mustReserve(t, svc, c.ID, "req-1", "30")

	got, err := svc.Cancel(context.Background(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusCancelled {
		t.Fatalf("status = %s", got.Status)
	}
	b := balance(t, svc, c.ID)
	if b.TotalReserved.String() != "0" || b.TotalAvailable.String() != "100" {
		t.Fatalf("reserved=%s available=%s", b.TotalReserved, b.TotalAvailable)
	}

	// 重复取消 → 幂等。
	if _, err := svc.Cancel(context.Background(), r.ID); err != nil {
		t.Fatalf("second cancel: %v", err)
	}
	// 取消后收到回执 → 状态冲突。
	if _, err := svc.Capture(context.Background(), CaptureParams{
		ReservationID: r.ID, ReceiptID: "rcpt-1", Amount: money.MustParse("10", "CNY"),
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("capture after cancel: err = %v, want conflict", err)
	}
}

func TestExpiryReleasesAll(t *testing.T) {
	svc, clk := newTestService(t)
	c := mustCampaign(t, svc, "100", "50")
	r := mustReserve(t, svc, c.ID, "req-1", "30")

	clk.Advance(time.Hour + time.Second) // 超过默认 TTL

	got, err := svc.GetReservation(context.Background(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusExpired {
		t.Fatalf("status = %s, want expired", got.Status)
	}
	b := balance(t, svc, c.ID)
	if b.TotalReserved.String() != "0" || b.TotalAvailable.String() != "100" {
		t.Fatalf("reserved=%s available=%s", b.TotalReserved, b.TotalAvailable)
	}

	// 失效凭证收到回执 → 明确状态冲突。
	if _, err := svc.Capture(context.Background(), CaptureParams{
		ReservationID: r.ID, ReceiptID: "rcpt-1", Amount: money.MustParse("10", "CNY"),
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("capture after expiry: err = %v, want conflict", err)
	}
	// 过期后取消 → 状态冲突。
	if _, err := svc.Cancel(context.Background(), r.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("cancel after expiry: err = %v, want conflict", err)
	}
}

func TestLateCancelCannotReverseCapture(t *testing.T) {
	svc, _ := newTestService(t)
	c := mustCampaign(t, svc, "100", "50")
	r := mustReserve(t, svc, c.ID, "req-1", "30")

	if _, err := svc.Capture(context.Background(), CaptureParams{
		ReservationID: r.ID, ReceiptID: "rcpt-1", Amount: money.MustParse("20", "CNY"),
	}); err != nil {
		t.Fatal(err)
	}
	// 迟到取消 → 状态冲突，已核销费用不受影响。
	if _, err := svc.Cancel(context.Background(), r.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("late cancel: err = %v, want conflict", err)
	}
	if b := balance(t, svc, c.ID); b.TotalSpent.String() != "20" {
		t.Fatalf("spent = %s, want 20", b.TotalSpent)
	}
}

// TestTerminalStateRace 核销、取消、过期并发竞争时只能落入一个终态，额度不重复释放。
func TestTerminalStateRace(t *testing.T) {
	for trial := 0; trial < 50; trial++ {
		svc, clk := newTestService(t)
		c := mustCampaign(t, svc, "100", "50")
		r := mustReserve(t, svc, c.ID, "req-1", "30")
		clk.Advance(time.Hour + time.Second) // 使过期路径也合法

		var wg sync.WaitGroup
		results := make([]error, 3)
		ops := []func() error{
			func() error {
				_, err := svc.Capture(context.Background(), CaptureParams{
					ReservationID: r.ID, ReceiptID: "rcpt-1", Amount: money.MustParse("20", "CNY"),
				})
				return err
			},
			func() error {
				_, err := svc.Cancel(context.Background(), r.ID)
				return err
			},
			func() error {
				_, err := svc.ExpireSweep(context.Background())
				return err
			},
		}
		wg.Add(3)
		for i, op := range ops {
			go func(i int, op func() error) {
				defer wg.Done()
				results[i] = op()
			}(i, op)
		}
		wg.Wait()

		got, err := svc.GetReservation(context.Background(), r.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Status.IsTerminal() {
			t.Fatalf("trial %d: status = %s, want terminal", trial, got.Status)
		}
		b := balance(t, svc, c.ID)
		if b.TotalReserved.String() != "0" {
			t.Fatalf("trial %d: reserved = %s, want 0 (no double release)", trial, b.TotalReserved)
		}
		// 不变量：spent + reserved + available == total budget。
		sum := b.TotalSpent.Add(b.TotalReserved).Add(b.TotalAvailable)
		if sum.String() != "100" {
			t.Fatalf("trial %d: invariant broken, sum = %s", trial, sum)
		}
	}
}

// TestConcurrentReserveNeverExceedsBudget 并发争抢下两级预算都不被突破。
func TestConcurrentReserveNeverExceedsBudget(t *testing.T) {
	svc, _ := newTestService(t)
	c := mustCampaign(t, svc, "100", "100")

	const workers = 32
	var wg sync.WaitGroup
	var mu sync.Mutex
	succeeded := 0
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			_, err := svc.Reserve(context.Background(), ReserveParams{
				CampaignID: c.ID,
				RequestID:  fmt.Sprintf("req-%d", w),
				Amount:     money.MustParse("10", "CNY"),
			})
			if err == nil {
				mu.Lock()
				succeeded++
				mu.Unlock()
			} else if !errors.Is(err, ErrBudgetExceeded) {
				t.Errorf("unexpected err: %v", err)
			}
		}(w)
	}
	wg.Wait()
	if succeeded != 10 {
		t.Fatalf("succeeded = %d, want exactly 10", succeeded)
	}
	b := balance(t, svc, c.ID)
	if b.TotalReserved.String() != "100" || b.TotalAvailable.String() != "0" {
		t.Fatalf("reserved=%s available=%s", b.TotalReserved, b.TotalAvailable)
	}
}

// TestConcurrentIdempotentReserve 同一请求号并发重放只产生一笔预占。
func TestConcurrentIdempotentReserve(t *testing.T) {
	svc, _ := newTestService(t)
	c := mustCampaign(t, svc, "100", "100")

	const workers = 16
	ids := make([]string, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			r, err := svc.Reserve(context.Background(), ReserveParams{
				CampaignID: c.ID, RequestID: "req-same", Amount: money.MustParse("10", "CNY"),
			})
			if err != nil {
				t.Errorf("reserve: %v", err)
				return
			}
			ids[w] = r.ID
		}(w)
	}
	wg.Wait()
	for _, id := range ids[1:] {
		if id != ids[0] {
			t.Fatalf("idempotent replay diverged: %s vs %s", id, ids[0])
		}
	}
	if b := balance(t, svc, c.ID); b.TotalReserved.String() != "10" {
		t.Fatalf("reserved = %s, want 10", b.TotalReserved)
	}
}

// TestDayBoundaryReset 跨自然日后日预算重置，总预算继续累计。
func TestDayBoundaryReset(t *testing.T) {
	svc, clk := newTestService(t)
	c := mustCampaign(t, svc, "1000", "50")

	// 长 TTL 让预占跨天存活，验证日预算重置而总预算继续累计。
	if _, err := svc.Reserve(context.Background(), ReserveParams{
		CampaignID: c.ID, RequestID: "req-1",
		Amount: money.MustParse("50", "CNY"), TTL: 48 * time.Hour,
	}); err != nil {
		t.Fatal(err)
	}
	clk.Set(time.Date(2026, 9, 27, 0, 0, 1, 0, time.UTC))

	// 新的一天可以再占 50。
	r := mustReserve(t, svc, c.ID, "req-2", "50")
	if r.DayKey != "2026-09-27" {
		t.Fatalf("day key = %s", r.DayKey)
	}
	b := balance(t, svc, c.ID)
	if b.DayKey != "2026-09-27" || b.DailyAvailable.String() != "0" {
		t.Fatalf("day=%s daily available=%s", b.DayKey, b.DailyAvailable)
	}
	if b.TotalReserved.String() != "100" {
		t.Fatalf("total reserved = %s, want 100", b.TotalReserved)
	}
	// 当日再占 → 日预算不足。
	if _, err := svc.Reserve(context.Background(), ReserveParams{
		CampaignID: c.ID, RequestID: "req-3", Amount: money.MustParse("1", "CNY"),
	}); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("err = %v, want budget_exceeded", err)
	}
}

// TestTimezoneDayBoundary 日边界按活动时区判定。
func TestTimezoneDayBoundary(t *testing.T) {
	svc, clk := newTestService(t)
	c, err := svc.CreateCampaign(context.Background(), CreateCampaignParams{
		Name:        "tz",
		TotalBudget: money.MustParse("1000", "CNY"),
		DailyCap:    money.MustParse("50", "CNY"),
		Timezone:    "Asia/Shanghai",
		DefaultTTL:  time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	// UTC 2026-09-26 17:00 = 上海 2026-09-27 01:00。
	clk.Set(time.Date(2026, 9, 26, 17, 0, 0, 0, time.UTC))
	r := mustReserve(t, svc, c.ID, "req-1", "10")
	if r.DayKey != "2026-09-27" {
		t.Fatalf("day key = %s, want 2026-09-27 (Asia/Shanghai)", r.DayKey)
	}
}

// TestPersistenceAcrossRestart 事件重放后状态完整恢复。
func TestPersistenceAcrossRestart(t *testing.T) {
	clk := clock.NewFake(testStart)
	st := store.NewMemStore()

	svc1, err := NewService(clk, st)
	if err != nil {
		t.Fatal(err)
	}
	c := mustCampaign(t, svc1, "100", "50")
	r := mustReserve(t, svc1, c.ID, "req-1", "30")
	if _, err := svc1.Capture(context.Background(), CaptureParams{
		ReservationID: r.ID, ReceiptID: "rcpt-1", Amount: money.MustParse("20", "CNY"),
	}); err != nil {
		t.Fatal(err)
	}
	r2 := mustReserve(t, svc1, c.ID, "req-2", "10")

	// 模拟重启：同一事件存储重建服务。
	svc2, err := NewService(clk, st)
	if err != nil {
		t.Fatal(err)
	}
	b := balance(t, svc2, c.ID)
	if b.TotalSpent.String() != "20" || b.TotalReserved.String() != "10" || b.TotalAvailable.String() != "70" {
		t.Fatalf("spent=%s reserved=%s available=%s", b.TotalSpent, b.TotalReserved, b.TotalAvailable)
	}
	// 幂等索引恢复：重放请求号返回原凭证。
	replay, err := svc2.Reserve(context.Background(), ReserveParams{
		CampaignID: c.ID, RequestID: "req-2", Amount: money.MustParse("10", "CNY"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if replay.ID != r2.ID {
		t.Fatalf("replayed reservation %s, want %s", replay.ID, r2.ID)
	}
	// 回执去重索引恢复：重复回执不重复计费。
	if _, err := svc2.Capture(context.Background(), CaptureParams{
		ReservationID: r.ID, ReceiptID: "rcpt-1", Amount: money.MustParse("20", "CNY"),
	}); err != nil {
		t.Fatal(err)
	}
	if b := balance(t, svc2, c.ID); b.TotalSpent.String() != "20" {
		t.Fatalf("spent = %s after duplicate receipt", b.TotalSpent)
	}
}

func TestExpireSweepExplicit(t *testing.T) {
	svc, clk := newTestService(t)
	c := mustCampaign(t, svc, "100", "50")
	mustReserve(t, svc, c.ID, "req-1", "10")
	mustReserve(t, svc, c.ID, "req-2", "20")

	clk.Advance(2 * time.Hour)
	n, err := svc.ExpireSweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("expired %d, want 2", n)
	}
	// 再次扫描无新增。
	if n, _ := svc.ExpireSweep(context.Background()); n != 0 {
		t.Fatalf("second sweep expired %d", n)
	}
	if b := balance(t, svc, c.ID); b.TotalReserved.String() != "0" {
		t.Fatalf("reserved = %s", b.TotalReserved)
	}
}

func TestCustomTTL(t *testing.T) {
	svc, clk := newTestService(t)
	c := mustCampaign(t, svc, "100", "50")
	r, err := svc.Reserve(context.Background(), ReserveParams{
		CampaignID: c.ID, RequestID: "req-1",
		Amount: money.MustParse("10", "CNY"), TTL: 5 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.ExpiresAt != testStart.Add(5*time.Minute) {
		t.Fatalf("expires at %v", r.ExpiresAt)
	}
	clk.Advance(6 * time.Minute)
	got, _ := svc.GetReservation(context.Background(), r.ID)
	if got.Status != StatusExpired {
		t.Fatalf("status = %s", got.Status)
	}
}
