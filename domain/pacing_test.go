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

// uniformWeights 返回 24 个权重 1。
func uniformWeights() []int64 {
	w := make([]int64, 24)
	for i := range w {
		w[i] = 1
	}
	return w
}

// curveLinear 返回权重 1..24 的曲线，总权重 300，用于制造非整除目标。
func curveLinear() []int64 {
	w := make([]int64, 24)
	for i := range w {
		w[i] = int64(i + 1)
	}
	return w
}

func mustCampaignWithCurve(t *testing.T, svc *Service, total, daily string, curve []int64) *Campaign {
	t.Helper()
	c, err := svc.CreateCampaign(context.Background(), CreateCampaignParams{
		Name:         "paced",
		TotalBudget:  money.MustParse(total, "CNY"),
		DailyCap:     money.MustParse(daily, "CNY"),
		Timezone:     "UTC",
		DefaultTTL:   time.Hour,
		CurveWeights: curve,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCurveValidation(t *testing.T) {
	if _, err := NewCurve(make([]int64, 23)); err == nil {
		t.Fatal("expected error for 23 weights")
	}
	if _, err := NewCurve(make([]int64, 25)); err == nil {
		t.Fatal("expected error for 25 weights")
	}
	bad := uniformWeights()
	bad[3] = -1
	if _, err := NewCurve(bad); err == nil {
		t.Fatal("expected error for negative weight")
	}
	if _, err := NewCurve(make([]int64, 24)); err == nil {
		t.Fatal("expected error for all-zero curve")
	}
}

func TestCumulativeTargetMonotonicAndCeil(t *testing.T) {
	c, err := NewCurve(curveLinear())
	if err != nil {
		t.Fatal(err)
	}
	cap := money.MustParse("100", "CNY")
	var prev money.Money
	for h := 0; h < 24; h++ {
		target := c.CumulativeTarget(cap, h)
		if h > 0 && target.Cmp(prev) < 0 {
			t.Fatalf("slot %d target %s < prev %s", h, target, prev)
		}
		prev = target
	}
	// slot 0（权重 1）目标 = ceil(100*1/300) = 0.333334；整除截断会得 0.333333。
	if got := c.CumulativeTarget(cap, 0); got.String() != "0.333334" {
		t.Fatalf("slot0 target = %s, want 0.333334 (ceil)", got)
	}
	// 最后一个时段累计目标恰为日预算。
	if got := c.CumulativeTarget(cap, 23); got.Cmp(cap) != 0 {
		t.Fatalf("last target = %s, want %s", got, cap)
	}
}

// TestReservePacingLimited 预占除总预算、日预算外还受当前时段累计目标限制。
func TestReservePacingLimited(t *testing.T) {
	svc, _ := newTestService(t)
	// 测试时钟固定在 2026-09-26 10:00 UTC = slot 10。
	// 均匀曲线：累计目标 = 100*11/24 = 45.833334。
	c := mustCampaignWithCurve(t, svc, "1000", "100", uniformWeights())

	mustReserve(t, svc, c.ID, "req-1", "40")
	_, err := svc.Reserve(context.Background(), ReserveParams{
		CampaignID: c.ID, RequestID: "req-2", Amount: money.MustParse("10", "CNY"),
	})
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("err = %v, want budget_exceeded", err)
	}
	de, _ := AsError(err)
	if de.Level != "slot" {
		t.Fatalf("level = %q, want slot", de.Level)
	}
	if de.Available.String() != "5.833334" {
		t.Fatalf("slot available = %s, want 5.833334", de.Available)
	}
	// 总预算与日预算此时都充足，失败仅来自节奏层。
	b := balance(t, svc, c.ID)
	if b.TotalAvailable.String() != "960" {
		t.Fatalf("total available = %s, want 960", b.TotalAvailable)
	}
	if b.DailyAvailable.String() != "60" {
		t.Fatalf("daily available = %s, want 60", b.DailyAvailable)
	}
	if b.Slot != 10 || b.SlotTarget.String() != "45.833334" || b.SlotAvailable.String() != "5.833334" {
		t.Fatalf("balance pacing: slot=%d target=%s avail=%s", b.Slot, b.SlotTarget, b.SlotAvailable)
	}
}

// TestReserveNoCurveSkipsPacing 未配置曲线时不做节奏限制（slot 目标视为日预算）。
func TestReserveNoCurveSkipsPacing(t *testing.T) {
	svc, _ := newTestService(t)
	c := mustCampaign(t, svc, "1000", "100") // 旧入口：无曲线
	if c.Version != 1 {
		t.Fatalf("initial version = %d, want 1", c.Version)
	}
	mustReserve(t, svc, c.ID, "req-1", "100") // 打满日预算但不触发节奏层
	b := balance(t, svc, c.ID)
	if b.PacingLimited {
		t.Fatal("pacing_limited should be false without curve")
	}
	if b.SlotTarget.String() != "100" || b.SlotAvailable.String() != "0" {
		t.Fatalf("slot target=%s avail=%s, want 100/0", b.SlotTarget, b.SlotAvailable)
	}
}

// TestPacingZeroWeightSlot 曲线只在最后一小时投放：此前所有时段累计目标为 0。
func TestPacingZeroWeightSlot(t *testing.T) {
	svc, clk := newTestService(t)
	// slots 0..22 权重 0，slot 23 权重 24（总权重 24）。
	w := make([]int64, 24)
	w[23] = 24
	c := mustCampaignWithCurve(t, svc, "1000", "100", w)

	clk.Set(time.Date(2026, 9, 26, 15, 0, 0, 0, time.UTC)) // slot 15，累计目标 0
	_, err := svc.Reserve(context.Background(), ReserveParams{
		CampaignID: c.ID, RequestID: "req-1", Amount: money.MustParse("0.000001", "CNY"),
	})
	if !errors.Is(err, ErrBudgetExceeded) || AsErrorMustLevel(t, err) != "slot" {
		t.Fatalf("zero-target slot reserve: %v", err)
	}

	// 进入投放时段 slot 23 后目标恰为日预算 100，可正常预占。
	clk.Set(time.Date(2026, 9, 26, 23, 0, 0, 0, time.UTC))
	mustReserve(t, svc, c.ID, "req-2", "50")
}

func AsErrorMustLevel(t *testing.T, err error) string {
	t.Helper()
	de, ok := AsError(err)
	if !ok {
		t.Fatalf("not domain error: %v", err)
	}
	return de.Level
}

// TestPacingCarriesAcrossSlots 当天已消耗按“累计”口径作用于后续时段。
func TestPacingCarriesAcrossSlots(t *testing.T) {
	svc, clk := newTestService(t)
	c := mustCampaignWithCurve(t, svc, "1000", "100", uniformWeights())

	// slot 10 目标 45.833334，预占 40 并全额核销。
	r := mustReserve(t, svc, c.ID, "req-1", "40")
	if _, err := svc.Capture(context.Background(), CaptureParams{
		ReservationID: r.ID, ReceiptID: "rcpt-1", Amount: money.MustParse("40", "CNY"),
	}); err != nil {
		t.Fatal(err)
	}
	// 推进到 slot 12：目标 = 100*13/24 = 54.166667，尚可消耗 14.166667。
	clk.Set(time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC))
	b := balance(t, svc, c.ID)
	if b.SlotTarget.String() != "54.166667" || b.SlotAvailable.String() != "14.166667" {
		t.Fatalf("slot12 target=%s avail=%s", b.SlotTarget, b.SlotAvailable)
	}
	mustReserve(t, svc, c.ID, "req-2", "14.166667")
	if _, err := svc.Reserve(context.Background(), ReserveParams{
		CampaignID: c.ID, RequestID: "req-3", Amount: money.MustParse("0.000001", "CNY"),
	}); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("over cumulative target: %v", err)
	}
}

// TestLateCaptureStaysInFrozenSlot 迟到回执不会被新曲线/新时间重新归类。
func TestLateCaptureStaysInFrozenSlot(t *testing.T) {
	svc, clk := newTestService(t)
	c := mustCampaignWithCurve(t, svc, "1000", "100", uniformWeights())
	// 长 TTL：推进到次日时凭证仍有效，确保我们测的是“迟到回执”而非“过期”。
	r, err := svc.Reserve(context.Background(), ReserveParams{
		CampaignID: c.ID, RequestID: "req-1",
		Amount: money.MustParse("40", "CNY"), TTL: 72 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	} // 冻结 day=2026-09-26 slot=10

	// 切换曲线为前重后零，并推进到次日（预占 TTL 设长，尚未过期）。
	front := make([]int64, 24)
	for i := 0; i < 12; i++ {
		front[i] = 2
	}
	if _, err := svc.AdjustConfig(context.Background(), AdjustConfigParams{
		CampaignID:      c.ID,
		AdjustmentID:    "adj-curve",
		ExpectedVersion: 1,
		CurveWeights:    front,
	}); err != nil {
		t.Fatal(err)
	}
	clk.Set(time.Date(2026, 9, 27, 15, 0, 0, 0, time.UTC)) // slot 15，新曲线权重 0

	// 迟到回执核销 30：必须仍计入冻结的 09-26 slot 10，而不是当前零权重时段。
	got, err := svc.Capture(context.Background(), CaptureParams{
		ReservationID: r.ID, ReceiptID: "rcpt-late", Amount: money.MustParse("30", "CNY"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.DayKey != "2026-09-26" || got.Slot != 10 {
		t.Fatalf("capture reclassified to %s slot %d", got.DayKey, got.Slot)
	}

	rep, err := svc.GetPacing(context.Background(), c.ID, "2026-09-26")
	if err != nil {
		t.Fatal(err)
	}
	s10 := rep.Slots[10]
	if s10.SlotSpent.String() != "30" {
		t.Fatalf("frozen slot spent = %s, want 30", s10.SlotSpent)
	}
	// 当天累计：核销 30 + 释放 10（不在任何桶里），累计已核销 30。
	if s10.CumulativeSpent.String() != "30" {
		t.Fatalf("cumulative spent = %s, want 30", s10.CumulativeSpent)
	}
	// 次日（新曲线）报告里 slot 15 必须为空，没有被迟到回执污染。
	rep2, err := svc.GetPacing(context.Background(), c.ID, "2026-09-27")
	if err != nil {
		t.Fatal(err)
	}
	if rep2.Slots[15].SlotSpent.String() != "0" {
		t.Fatalf("next-day slot 15 polluted: %s", rep2.Slots[15].SlotSpent)
	}
}

// TestTimezoneSwitchDoesNotReclassify 切换时区后，既有凭证仍按旧时区冻结日入账。
func TestTimezoneSwitchDoesNotReclassify(t *testing.T) {
	svc, clk := newTestService(t)
	// 初始 UTC：2026-09-26 17:00 在上海时区已是 09-27，这里先用 UTC 建活动。
	c := mustCampaignWithCurve(t, svc, "1000", "100", uniformWeights())
	clk.Set(time.Date(2026, 9, 26, 17, 0, 0, 0, time.UTC)) // UTC slot 17
	r := mustReserve(t, svc, c.ID, "req-1", "20")
	if r.DayKey != "2026-09-26" || r.Slot != 17 {
		t.Fatalf("frozen day=%s slot=%d", r.DayKey, r.Slot)
	}

	sh := "Asia/Shanghai"
	if _, err := svc.AdjustConfig(context.Background(), AdjustConfigParams{
		CampaignID:      c.ID,
		AdjustmentID:    "adj-tz",
		ExpectedVersion: 1,
		Timezone:        &sh,
	}); err != nil {
		t.Fatal(err)
	}
	// 此刻在上海为 2026-09-27 01:00，但迟到回执必须落在冻结的 UTC 09-26/slot17。
	got, err := svc.Capture(context.Background(), CaptureParams{
		ReservationID: r.ID, ReceiptID: "rcpt-1", Amount: money.MustParse("20", "CNY"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.DayKey != "2026-09-26" || got.Slot != 17 {
		t.Fatalf("reclassified after tz switch: day=%s slot=%d", got.DayKey, got.Slot)
	}
	rep, err := svc.GetPacing(context.Background(), c.ID, "2026-09-26")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Slots[17].SlotSpent.String() != "20" {
		t.Fatalf("old-tz slot spent = %s", rep.Slots[17].SlotSpent)
	}
}

// TestExpiryReleasesFrozenSlot 过期释放的额度回到冻结时段，节奏统计不串桶。
func TestExpiryReleasedToFrozenSlot(t *testing.T) {
	svc, clk := newTestService(t)
	c := mustCampaignWithCurve(t, svc, "1000", "100", uniformWeights())
	mustReserve(t, svc, c.ID, "req-1", "40") // slot 10
	clk.Set(time.Date(2026, 9, 26, 18, 0, 0, 0, time.UTC))
	if n, err := svc.ExpireSweep(context.Background()); err != nil || n != 1 {
		t.Fatalf("sweep n=%d err=%v", n, err)
	}
	rep, _ := svc.GetPacing(context.Background(), c.ID, "2026-09-26")
	s10 := rep.Slots[10]
	if s10.SlotReserved.String() != "0" || s10.SlotSpent.String() != "0" {
		t.Fatalf("slot10 after expire: reserved=%s spent=%s", s10.SlotReserved, s10.SlotSpent)
	}
	// 释放后当前 slot 18 的节奏额度恢复为目标全额（100*19/24=79.166667）。
	b := balance(t, svc, c.ID)
	if b.Slot != 18 || b.SlotAvailable.String() != "79.166667" {
		t.Fatalf("slot18 avail = %s (slot %d)", b.SlotAvailable, b.Slot)
	}
}

// TestGetPacingReport 逐时段报告的累计值与偏差计算正确。
func TestGetPacingReport(t *testing.T) {
	svc, _ := newTestService(t)
	c := mustCampaignWithCurve(t, svc, "1000", "100", uniformWeights())
	r := mustReserve(t, svc, c.ID, "req-1", "40") // slot 10
	if _, err := svc.Capture(context.Background(), CaptureParams{
		ReservationID: r.ID, ReceiptID: "rcpt-1", Amount: money.MustParse("30", "CNY"),
	}); err != nil {
		t.Fatal(err)
	}

	rep, err := svc.GetPacing(context.Background(), c.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if rep.DayKey != "2026-09-26" || !rep.PacingLimited || rep.Version != 1 {
		t.Fatalf("report header: %+v", rep)
	}
	s := rep.Slots[10]
	if s.Weight != 1 || s.SlotSpent.String() != "30" || s.SlotReserved.String() != "0" ||
		s.CumulativeReserved.String() != "0" {
		t.Fatalf("slot10 = %+v", s)
	}
	if s.CumulativeSpent.String() != "30" || s.CumulativeCommitted.String() != "30" {
		t.Fatalf("cumulative: spent=%s committed=%s", s.CumulativeSpent, s.CumulativeCommitted)
	}
	// 偏差 = 30 - 45.833334 = -15.833334（欠投）。
	if s.Variance.String() != "-15.833334" {
		t.Fatalf("variance = %s", s.Variance)
	}
	// slot 9 累计应为 0（消耗发生在 10）。
	if rep.Slots[9].CumulativeSpent.String() != "0" {
		t.Fatalf("slot9 cumulative spent = %s", rep.Slots[9].CumulativeSpent)
	}

	// 非法日期参数。
	if _, err := svc.GetPacing(context.Background(), c.ID, "2026/09/26"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("bad day key: %v", err)
	}
}

// ---------- 预算调整 ----------

func TestAdjustBudgetRaiseImmediate(t *testing.T) {
	svc, _ := newTestService(t)
	c := mustCampaign(t, svc, "100", "200")
	mustReserve(t, svc, c.ID, "req-1", "80") // 占住总预算 80

	got, err := svc.AdjustBudget(context.Background(), AdjustBudgetParams{
		CampaignID: c.ID, AdjustmentID: "adj-1", ExpectedVersion: 1,
		TotalBudget: money.MustParse("200", "CNY"), DailyCap: money.MustParse("80", "CNY"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 2 || got.TotalBudget.String() != "200" || got.DailyCap.String() != "80" {
		t.Fatalf("campaign after adjust: v=%d total=%s daily=%s", got.Version, got.TotalBudget, got.DailyCap)
	}
	b := balance(t, svc, c.ID)
	if b.Version != 2 || b.TotalAvailable.String() != "120" {
		t.Fatalf("balance v=%d avail=%s", b.Version, b.TotalAvailable)
	}
}

// TestAdjustBudgetLowerBoundaries 降低不得小于已核销，也不能小于已核销+有效预占。
func TestAdjustBudgetLowerBoundaries(t *testing.T) {
	t.Run("below_spent", func(t *testing.T) {
		svc, _ := newTestService(t)
		c := mustCampaign(t, svc, "100", "100")
		r := mustReserve(t, svc, c.ID, "req-1", "40")
		if _, err := svc.Capture(context.Background(), CaptureParams{
			ReservationID: r.ID, ReceiptID: "rcpt-1", Amount: money.MustParse("40", "CNY"),
		}); err != nil {
			t.Fatal(err)
		}
		_, err := svc.AdjustBudget(context.Background(), AdjustBudgetParams{
			CampaignID: c.ID, AdjustmentID: "adj-1", ExpectedVersion: 1,
			TotalBudget: money.MustParse("39.999999", "CNY"), DailyCap: money.MustParse("100", "CNY"),
		})
		if !errors.Is(err, ErrBudgetExceeded) {
			t.Fatalf("err = %v", err)
		}
		de, _ := AsError(err)
		if de.Level != "total" || de.Limit.String() != "39.999999" || de.Committed.String() != "40" {
			t.Fatalf("reject details: level=%s limit=%s committed=%s", de.Level, de.Limit, de.Committed)
		}
	})

	t.Run("below_spent_plus_reserved", func(t *testing.T) {
		svc, _ := newTestService(t)
		c := mustCampaign(t, svc, "100", "100")
		r := mustReserve(t, svc, c.ID, "req-1", "40")
		if _, err := svc.Capture(context.Background(), CaptureParams{
			ReservationID: r.ID, ReceiptID: "rcpt-1", Amount: money.MustParse("20", "CNY"),
		}); err != nil {
			t.Fatal(err)
		}
		mustReserve(t, svc, c.ID, "req-2", "30") // spent 20 + reserved 30 = 50
		_, err := svc.AdjustBudget(context.Background(), AdjustBudgetParams{
			CampaignID: c.ID, AdjustmentID: "adj-1", ExpectedVersion: 1,
			TotalBudget: money.MustParse("49", "CNY"), DailyCap: money.MustParse("100", "CNY"),
		})
		if !errors.Is(err, ErrBudgetExceeded) {
			t.Fatalf("err = %v", err)
		}
		if de, _ := AsError(err); de.Committed.String() != "50" {
			t.Fatalf("committed = %s, want 50", de.Committed)
		}
		// 边界：恰好等于 committed 时允许。
		got, err := svc.AdjustBudget(context.Background(), AdjustBudgetParams{
			CampaignID: c.ID, AdjustmentID: "adj-2", ExpectedVersion: 1,
			TotalBudget: money.MustParse("50", "CNY"), DailyCap: money.MustParse("100", "CNY"),
		})
		if err != nil {
			t.Fatalf("lower to exactly committed should succeed: %v", err)
		}
		if got.Version != 2 || got.TotalBudget.String() != "50" {
			t.Fatalf("got v=%d total=%s", got.Version, got.TotalBudget)
		}
	})

	t.Run("daily_history_day", func(t *testing.T) {
		svc, clk := newTestService(t)
		c := mustCampaign(t, svc, "1000", "100")
		mustReserve(t, svc, c.ID, "req-1", "60") // 09-26 占用 60
		clk.Set(time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC))
		// 降到 50：历史日 09-26 committed 60 > 50，整体拒绝。
		_, err := svc.AdjustBudget(context.Background(), AdjustBudgetParams{
			CampaignID: c.ID, AdjustmentID: "adj-1", ExpectedVersion: 1,
			TotalBudget: money.MustParse("1000", "CNY"), DailyCap: money.MustParse("50", "CNY"),
		})
		if !errors.Is(err, ErrBudgetExceeded) {
			t.Fatalf("err = %v", err)
		}
		de, _ := AsError(err)
		if de.Level != "daily" || de.DayKey != "2026-09-26" || de.Committed.String() != "60" {
			t.Fatalf("daily reject: level=%s day=%s committed=%s", de.Level, de.DayKey, de.Committed)
		}
	})
}

// TestAdjustBudgetRejectDoesNotCancel 调整被拒时凭证原样保留，额度无变化。
func TestAdjustBudgetRejectDoesNotCancel(t *testing.T) {
	svc, _ := newTestService(t)
	c := mustCampaign(t, svc, "100", "100")
	r := mustReserve(t, svc, c.ID, "req-1", "80")

	_, err := svc.AdjustBudget(context.Background(), AdjustBudgetParams{
		CampaignID: c.ID, AdjustmentID: "adj-bad", ExpectedVersion: 1,
		TotalBudget: money.MustParse("10", "CNY"), DailyCap: money.MustParse("100", "CNY"),
	})
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("err = %v", err)
	}
	got, err := svc.GetReservation(context.Background(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusReserved {
		t.Fatalf("reservation status = %s, want reserved (must not be silently cancelled)", got.Status)
	}
	if v := balance(t, svc, c.ID); v.TotalReserved.String() != "80" || v.Version != 1 {
		t.Fatalf("balance after reject: reserved=%s version=%d", v.TotalReserved, v.Version)
	}
}

func TestAdjustBudgetVersionConflict(t *testing.T) {
	svc, _ := newTestService(t)
	c := mustCampaign(t, svc, "100", "100")
	// 先成功升到 v2。
	if _, err := svc.AdjustBudget(context.Background(), AdjustBudgetParams{
		CampaignID: c.ID, AdjustmentID: "adj-1", ExpectedVersion: 1,
		TotalBudget: money.MustParse("200", "CNY"), DailyCap: money.MustParse("100", "CNY"),
	}); err != nil {
		t.Fatal(err)
	}
	// 仍带 expected=1 → 版本冲突，错误带当前版本 2。
	_, err := svc.AdjustBudget(context.Background(), AdjustBudgetParams{
		CampaignID: c.ID, AdjustmentID: "adj-2", ExpectedVersion: 1,
		TotalBudget: money.MustParse("300", "CNY"), DailyCap: money.MustParse("100", "CNY"),
	})
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("err = %v, want version_conflict", err)
	}
	if de, _ := AsError(err); de.CurrentVersion != 2 {
		t.Fatalf("current version = %d, want 2", de.CurrentVersion)
	}
}

// TestAdjustBudgetIdempotent 同调整号幂等；内容变化冲突；重放返回生效版本。
func TestAdjustBudgetIdempotent(t *testing.T) {
	svc, _ := newTestService(t)
	c := mustCampaign(t, svc, "100", "100")
	params := AdjustBudgetParams{
		CampaignID: c.ID, AdjustmentID: "adj-1", ExpectedVersion: 1,
		TotalBudget: money.MustParse("200", "CNY"), DailyCap: money.MustParse("100", "CNY"),
	}
	first, err := svc.AdjustBudget(context.Background(), params)
	if err != nil {
		t.Fatal(err)
	}
	if first.Version != 2 {
		t.Fatalf("version = %d", first.Version)
	}
	// 再做一次别的调整到 v3。
	if _, err := svc.AdjustBudget(context.Background(), AdjustBudgetParams{
		CampaignID: c.ID, AdjustmentID: "adj-2", ExpectedVersion: 2,
		TotalBudget: money.MustParse("300", "CNY"), DailyCap: money.MustParse("100", "CNY"),
	}); err != nil {
		t.Fatal(err)
	}
	// 重放 adj-1：返回该调整生效时的快照 v2/200，而不是当前 v3/300。
	replay, err := svc.AdjustBudget(context.Background(), params)
	if err != nil {
		t.Fatal(err)
	}
	if replay.Version != 2 || replay.TotalBudget.String() != "200" {
		t.Fatalf("replay snapshot v=%d total=%s", replay.Version, replay.TotalBudget)
	}
	if b := balance(t, svc, c.ID); b.TotalBudget.String() != "300" {
		t.Fatalf("current budget changed by replay: %s", b.TotalBudget)
	}
	// 同调整号不同内容 → 幂等冲突。
	params.TotalBudget = money.MustParse("250", "CNY")
	if _, err := svc.AdjustBudget(context.Background(), params); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("err = %v, want idempotency_conflict", err)
	}
}

// TestAdjustBudgetValidation 参数错误。
func TestAdjustBudgetValidation(t *testing.T) {
	svc, _ := newTestService(t)
	c := mustCampaign(t, svc, "100", "100")
	base := func() AdjustBudgetParams {
		return AdjustBudgetParams{
			CampaignID: c.ID, AdjustmentID: "adj-x", ExpectedVersion: 1,
			TotalBudget: money.MustParse("200", "CNY"), DailyCap: money.MustParse("100", "CNY"),
		}
	}
	p := base()
	p.AdjustmentID = ""
	if _, err := svc.AdjustBudget(context.Background(), p); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("missing adjustment id: %v", err)
	}
	p = base()
	p.TotalBudget = money.MustParse("0", "CNY")
	if _, err := svc.AdjustBudget(context.Background(), p); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("zero budget: %v", err)
	}
	p = base()
	p.DailyCap = money.MustParse("200", "USD")
	if _, err := svc.AdjustBudget(context.Background(), p); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("currency mismatch: %v", err)
	}
	p = base()
	p.CampaignID = "cmp_missing"
	if _, err := svc.AdjustBudget(context.Background(), p); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing campaign: %v", err)
	}
}

// TestGetBudgetVersions 版本流完整记录创建与每次调整。
func TestGetBudgetVersions(t *testing.T) {
	svc, _ := newTestService(t)
	c := mustCampaignWithCurve(t, svc, "100", "100", uniformWeights())
	if _, err := svc.AdjustBudget(context.Background(), AdjustBudgetParams{
		CampaignID: c.ID, AdjustmentID: "adj-b", ExpectedVersion: 1,
		TotalBudget: money.MustParse("200", "CNY"), DailyCap: money.MustParse("120", "CNY"),
	}); err != nil {
		t.Fatal(err)
	}
	sh := "Asia/Shanghai"
	if _, err := svc.AdjustConfig(context.Background(), AdjustConfigParams{
		CampaignID: c.ID, AdjustmentID: "adj-c", ExpectedVersion: 2, Timezone: &sh,
	}); err != nil {
		t.Fatal(err)
	}
	versions, err := svc.GetBudgetVersions(context.Background(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 3 {
		t.Fatalf("versions = %d, want 3", len(versions))
	}
	if versions[0].Version != 1 || versions[0].Kind != KindCreate {
		t.Fatalf("v0 = %+v", versions[0])
	}
	if len(versions[0].CurveWeights) != 24 {
		t.Fatalf("create curve weights missing")
	}
	v1 := versions[1]
	if v1.Version != 2 || v1.Kind != KindBudgetAdjust || v1.AdjustmentID != "adj-b" ||
		v1.TotalBudget.String() != "200" || v1.DailyCap.String() != "120" {
		t.Fatalf("v1 = %+v", v1)
	}
	v2 := versions[2]
	if v2.Version != 3 || v2.Kind != KindConfigAdjust || v2.Timezone != "Asia/Shanghai" {
		t.Fatalf("v2 = %+v", v2)
	}
}

// TestAdjustConfigVersioningAndIdempotency 时区/曲线切换的版本与幂等语义。
func TestAdjustConfigVersioningAndIdempotency(t *testing.T) {
	svc, _ := newTestService(t)
	c := mustCampaignWithCurve(t, svc, "1000", "100", uniformWeights())

	sh := "Asia/Shanghai"
	p := AdjustConfigParams{
		CampaignID: c.ID, AdjustmentID: "cfg-1", ExpectedVersion: 1, Timezone: &sh,
		CurveWeights: curveLinear(),
	}
	got, err := svc.AdjustConfig(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 2 || got.Location.String() != "Asia/Shanghai" {
		t.Fatalf("after config: v=%d tz=%s", got.Version, got.Location)
	}
	// 幂等重放。
	if _, err := svc.AdjustConfig(context.Background(), p); err != nil {
		t.Fatalf("idempotent replay: %v", err)
	}
	// 版本号过期 → version_conflict。
	stale := p
	stale.AdjustmentID = "cfg-2"
	stale.ExpectedVersion = 1
	if _, err := svc.AdjustConfig(context.Background(), stale); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale version: %v", err)
	}
	// 同号不同内容 → idempotency_conflict。
	diff := p
	diff.Timezone = nil
	diff.CurveWeights = uniformWeights()
	if _, err := svc.AdjustConfig(context.Background(), diff); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("same id different content: %v", err)
	}
	// 什么都不改 → invalid_argument。
	if _, err := svc.AdjustConfig(context.Background(), AdjustConfigParams{
		CampaignID: c.ID, AdjustmentID: "cfg-3", ExpectedVersion: 2,
	}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("empty adjust: %v", err)
	}
	// 非法时区 / 非法曲线。
	badTZ := "Not/ARealZone"
	if _, err := svc.AdjustConfig(context.Background(), AdjustConfigParams{
		CampaignID: c.ID, AdjustmentID: "cfg-4", ExpectedVersion: 2, Timezone: &badTZ,
	}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("bad timezone: %v", err)
	}
	if _, err := svc.AdjustConfig(context.Background(), AdjustConfigParams{
		CampaignID: c.ID, AdjustmentID: "cfg-5", ExpectedVersion: 2,
		CurveWeights: []int64{1, 2, 3},
	}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("bad curve: %v", err)
	}
}

// TestAdjustConfigRemoveCurve 显式移除曲线后不再做节奏限制。
func TestAdjustConfigRemoveCurve(t *testing.T) {
	svc, _ := newTestService(t)
	c := mustCampaignWithCurve(t, svc, "1000", "100", uniformWeights())
	if _, err := svc.AdjustConfig(context.Background(), AdjustConfigParams{
		CampaignID: c.ID, AdjustmentID: "cfg-rm", ExpectedVersion: 1, RemoveCurve: true,
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := svc.GetCampaign(context.Background(), c.ID)
	if got.Curve != nil || got.Version != 2 {
		t.Fatalf("curve not removed: %+v", got)
	}
	// 可以直接打满日预算，不受 slot 累计目标约束。
	mustReserve(t, svc, c.ID, "req-1", "100")
}

// TestNoDoubleReleaseAcrossRace 配置切换、过期、回执并发时不重复扣减/释放。
func TestConfigSwitchAndTerminalRace(t *testing.T) {
	for trial := 0; trial < 30; trial++ {
		svc, clk := newTestService(t)
		c := mustCampaignWithCurve(t, svc, "1000", "100", uniformWeights())
		r := mustReserve(t, svc, c.ID, "req-1", "40")
		clk.Advance(time.Hour + time.Second)

		nextVersion := int64(1)
		var vmu sync.Mutex
		adjust := func(id string) error {
			vmu.Lock()
			ev := nextVersion
			nextVersion++
			vmu.Unlock()
			_, err := svc.AdjustBudget(context.Background(), AdjustBudgetParams{
				CampaignID: c.ID, AdjustmentID: id, ExpectedVersion: ev,
				TotalBudget: money.MustParse("2000", "CNY"), DailyCap: money.MustParse("200", "CNY"),
			})
			return err
		}
		var wg sync.WaitGroup
		errs := make([]error, 4)
		wg.Add(4)
		go func() { defer wg.Done(); errs[0] = adjust(fmt.Sprintf("adj-%d", trial)) }()
		go func() {
			defer wg.Done()
			_, errs[1] = svc.Capture(context.Background(), CaptureParams{
				ReservationID: r.ID, ReceiptID: "rcpt-1", Amount: money.MustParse("30", "CNY"),
			})
		}()
		go func() { defer wg.Done(); _, errs[2] = svc.Cancel(context.Background(), r.ID) }()
		go func() { defer wg.Done(); _, errs[3] = svc.ExpireSweep(context.Background()) }()
		wg.Wait()

		// 预算调整：恰好一个成功，其余要么版本冲突（本循环串行 ev 下最多一个 ev==1）。
		// 凭证终态唯一。
		got, err := svc.GetReservation(context.Background(), r.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Status.IsTerminal() {
			t.Fatalf("trial %d: not terminal: %s", trial, got.Status)
		}
		b := balance(t, svc, c.ID)
		// 不变量：spent + reserved + available == 当前总预算。
		sum := b.TotalSpent.Add(b.TotalReserved).Add(b.TotalAvailable)
		if sum.Cmp(b.TotalBudget) != 0 {
			t.Fatalf("trial %d: invariant broken: %s vs budget %s (errs=%v)",
				trial, sum, b.TotalBudget, errs)
		}
	}
}

// TestConcurrentReserveNeverExceedsSlotTarget 并发下节奏累计目标也不会被突破。
func TestConcurrentReserveNeverExceedsSlotTarget(t *testing.T) {
	svc, _ := newTestService(t)
	// slot 10 累计目标 45.833334；总预算放到足够大、日预算 100，只让节奏层先成为约束。
	c := mustCampaignWithCurve(t, svc, "1000000", "100", uniformWeights())

	const workers = 64
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
				Amount:     money.MustParse("1", "CNY"),
			})
			if err == nil {
				mu.Lock()
				succeeded++
				mu.Unlock()
			} else if !errors.Is(err, ErrBudgetExceeded) || AsErrorMustLevel(t, err) != "slot" {
				t.Errorf("unexpected err: %v", err)
			}
		}(w)
	}
	wg.Wait()
	if succeeded != 45 {
		t.Fatalf("succeeded = %d, want 45 (target 45.833334)", succeeded)
	}
	b := balance(t, svc, c.ID)
	if b.DailyReserved.String() != "45" {
		t.Fatalf("reserved = %s, want 45", b.DailyReserved)
	}
	if b.SlotAvailable.String() != "0.833334" {
		t.Fatalf("slot available = %s, want 0.833334", b.SlotAvailable)
	}
}

// TestPersistenceAcrossRestartWithPacing 节奏与调整状态在事件重放后完整恢复。
func TestPersistenceAcrossRestartWithPacing(t *testing.T) {
	clk := clock.NewFake(testStart)
	st := store.NewMemStore()

	svc1, err := NewService(clk, st)
	if err != nil {
		t.Fatal(err)
	}
	c := mustCampaignWithCurve(t, svc1, "1000", "100", uniformWeights())
	r := mustReserve(t, svc1, c.ID, "req-1", "40")
	if _, err := svc1.Capture(context.Background(), CaptureParams{
		ReservationID: r.ID, ReceiptID: "rcpt-1", Amount: money.MustParse("30", "CNY"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc1.AdjustBudget(context.Background(), AdjustBudgetParams{
		CampaignID: c.ID, AdjustmentID: "adj-1", ExpectedVersion: 1,
		TotalBudget: money.MustParse("2000", "CNY"), DailyCap: money.MustParse("200", "CNY"),
	}); err != nil {
		t.Fatal(err)
	}

	svc2, err := NewService(clk, st)
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc2.GetCampaign(context.Background(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 2 || got.TotalBudget.String() != "2000" || got.Curve == nil {
		t.Fatalf("campaign restored: %+v", got)
	}
	// 调整幂等索引恢复：重放不产生新版本。
	if _, err := svc2.AdjustBudget(context.Background(), AdjustBudgetParams{
		CampaignID: c.ID, AdjustmentID: "adj-1", ExpectedVersion: 1,
		TotalBudget: money.MustParse("2000", "CNY"), DailyCap: money.MustParse("200", "CNY"),
	}); err != nil {
		t.Fatal(err)
	}
	if b := balance(t, svc2, c.ID); b.Version != 2 {
		t.Fatalf("version = %d, replay must not bump version", b.Version)
	}
	// 节奏统计恢复。
	rep, err := svc2.GetPacing(context.Background(), c.ID, "2026-09-26")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Slots[10].SlotSpent.String() != "30" {
		t.Fatalf("slot spent after restart = %s", rep.Slots[10].SlotSpent)
	}
}
