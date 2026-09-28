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

// halfDayCurve：前半天（小时桶 0..11）累计目标 50%，后半天投满。
func halfDayCurve() *PacingCurve {
	targets := make([]int64, SlotCount)
	for h := 0; h < 12; h++ {
		targets[h] = PacingPPM / 2
	}
	for h := 12; h < SlotCount; h++ {
		targets[h] = PacingPPM
	}
	return &PacingCurve{Targets: targets}
}

func mustCurveCampaign(t *testing.T, svc *Service, total, daily string, curve *PacingCurve) *Campaign {
	t.Helper()
	c, err := svc.CreateCampaign(context.Background(), CreateCampaignParams{
		Name:        "pacing",
		TotalBudget: money.MustParse(total, "CNY"),
		DailyCap:    money.MustParse(daily, "CNY"),
		Timezone:    "UTC",
		DefaultTTL:  48 * time.Hour,
		Curve:       curve,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestPacingCurveValidation(t *testing.T) {
	valid := []PacingCurve{
		EvenPacingCurve(),
		*halfDayCurve(),
	}
	for i, c := range valid {
		if err := c.Validate(); err != nil {
			t.Errorf("curve %d should be valid: %v", i, err)
		}
	}

	bad := []PacingCurve{
		{Targets: make([]int64, 23)}, // 长度不足
		{Targets: func() []int64 { // 长度 25
			t := make([]int64, 25)
			t[24] = PacingPPM
			return t
		}()},
		{Targets: func() []int64 { // 末点不是 100%
			t := make([]int64, SlotCount)
			for i := range t {
				t[i] = PacingPPM / 2
			}
			return t
		}()},
		{Targets: func() []int64 { // 递减
			t := make([]int64, SlotCount)
			for i := range t {
				t[i] = PacingPPM - int64(i)
			}
			return t
		}()},
		{Targets: func() []int64 { // 超出范围
			t := make([]int64, SlotCount)
			for i := range t {
				t[i] = PacingPPM + 1
			}
			return t
		}()},
	}
	for i, c := range bad {
		if err := c.Validate(); err == nil {
			t.Errorf("bad curve %d should fail validation", i)
		}
	}
}

func TestCreateCampaignRejectsBadCurve(t *testing.T) {
	svc, _ := newTestService(t)
	bad := &PacingCurve{Targets: make([]int64, SlotCount)} // 全 0，末点不为满
	if _, err := svc.CreateCampaign(context.Background(), CreateCampaignParams{
		Name:        "x",
		TotalBudget: money.MustParse("100", "CNY"),
		DailyCap:    money.MustParse("50", "CNY"),
		DefaultTTL:  time.Hour,
		Curve:       bad,
	}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("err = %v, want invalid_argument", err)
	}
}

// TestReservePacingLimited 预占受当前时段累计节奏额度限制（第三级预算）。
func TestReservePacingLimited(t *testing.T) {
	svc, clk := newTestService(t)
	// testStart 为 UTC 10:00，半天曲线在 10 点（桶 10）只允许日上限的 50%。
	c := mustCurveCampaign(t, svc, "10000", "100", halfDayCurve())

	if r := mustReserve(t, svc, c.ID, "req-1", "50"); r.Slot != 10 || r.ConfigVersion != 1 {
		t.Fatalf("slot=%d version=%d", r.Slot, r.ConfigVersion)
	}
	// 节奏额度已打满：总预算与日预算都充足，但节奏不允许。
	_, err := svc.Reserve(context.Background(), ReserveParams{
		CampaignID: c.ID, RequestID: "req-2", Amount: money.MustParse("1", "CNY"),
	})
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("err = %v, want budget_exceeded", err)
	}
	de, _ := AsError(err)
	if de.Level != "pacing" {
		t.Fatalf("level = %q, want pacing", de.Level)
	}
	if de.Available.String() != "0" {
		t.Fatalf("pacing available = %s, want 0", de.Available)
	}

	// 推进到下午（桶 12），累计目标放到 100%，可以继续投放。
	clk.Set(time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC))
	mustReserve(t, svc, c.ID, "req-2", "50")
	b := balance(t, svc, c.ID)
	if b.DailyReserved.String() != "100" {
		t.Fatalf("daily reserved = %s, want 100", b.DailyReserved)
	}
	if !b.PacingEnabled || b.PacingTarget.String() != "100" {
		t.Fatalf("pacing target = %s enabled=%v", b.PacingTarget, b.PacingEnabled)
	}
}

// TestReservePacingFloorRounding 节奏目标金额在最小单位（10⁻⁶ 元）上向下取整，
// 绝不被取整改写放大。
func TestReservePacingFloorRounding(t *testing.T) {
	svc, _ := newTestService(t)
	// 均匀曲线桶 10 的 ppm 点为 floor(1_000_000*11/24)=458333，
	// 目标 = floor(10 元 * 458333/1_000_000) = 4.58333 元（整数最小单位上向下取整）。
	c := func() *Campaign {
		cv := EvenPacingCurve()
		return mustCurveCampaign(t, svc, "10000", "10", &cv)
	}()
	mustReserve(t, svc, c.ID, "req-1", "4.58333")
	_, err := svc.Reserve(context.Background(), ReserveParams{
		CampaignID: c.ID, RequestID: "req-2", Amount: money.MustParse("0.000001", "CNY"),
	})
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("err = %v", err)
	}
	if de, _ := AsError(err); de.Level != "pacing" || de.Available.String() != "0" {
		t.Fatalf("level=%s available = %s, want pacing/0 (floor rounding)", de.Level, de.Available)
	}
}

// TestNoCurveMeansNoPacingLimit 未配置曲线时节奏额度等于日上限。
func TestNoCurveMeansNoPacingLimit(t *testing.T) {
	svc, _ := newTestService(t)
	c := mustCampaign(t, svc, "1000", "100")
	mustReserve(t, svc, c.ID, "req-1", "100")
	b := balance(t, svc, c.ID)
	if b.PacingEnabled {
		t.Fatalf("pacing should be disabled")
	}
}

// ---------- 预算调整 ----------

func TestAdjustBudgetIncreaseImmediate(t *testing.T) {
	svc, _ := newTestService(t)
	c := mustCampaign(t, svc, "100", "1000")
	mustReserve(t, svc, c.ID, "req-1", "80")

	adj, err := svc.AdjustBudget(context.Background(), AdjustBudgetParams{
		CampaignID: c.ID, AdjustmentID: "adj-1", ExpectedVersion: 1,
		SetTotal: true, TotalBudget: money.MustParse("200", "CNY"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if adj.ConfigVersion != 2 || adj.TotalBudget.String() != "200" {
		t.Fatalf("version=%d total=%s", adj.ConfigVersion, adj.TotalBudget)
	}
	b := balance(t, svc, c.ID)
	if b.TotalBudget.String() != "200" || b.TotalAvailable.String() != "120" {
		t.Fatalf("budget=%s available=%s", b.TotalBudget, b.TotalAvailable)
	}
	// 提高后立即能再预占。
	mustReserve(t, svc, c.ID, "req-2", "120")
}

func TestAdjustBudgetDecreaseFloor(t *testing.T) {
	svc, _ := newTestService(t)
	c := mustCampaign(t, svc, "100", "1000")
	r := mustReserve(t, svc, c.ID, "req-1", "30")
	if _, err := svc.Capture(context.Background(), CaptureParams{
		ReservationID: r.ID, ReceiptID: "rcpt-1", Amount: money.MustParse("20", "CNY"),
	}); err != nil {
		t.Fatal(err)
	}
	// spent=20, reserved=0：降到 19 低于已核销下限 → 整体拒绝。
	_, err := svc.AdjustBudget(context.Background(), AdjustBudgetParams{
		CampaignID: c.ID, AdjustmentID: "adj-low", ExpectedVersion: 1,
		SetTotal: true, TotalBudget: money.MustParse("19", "CNY"),
	})
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("err = %v, want budget_exceeded", err)
	}
	de, _ := AsError(err)
	if de.Level != "total_floor" || de.Available.String() != "20" {
		t.Fatalf("level=%s floor=%s", de.Level, de.Available)
	}
	// 被拒后预算与版本不变。
	if got, err := svc.GetCampaign(context.Background(), c.ID); err != nil ||
		got.ConfigVersion != 1 || got.TotalBudget.String() != "100" {
		t.Fatalf("campaign mutated after rejection: %+v err=%v", got, err)
	}

	// 有效预占也计入下限：先再预占 50（committed = 20 spent + 50 reserved = 70）。
	mustReserve(t, svc, c.ID, "req-2", "50")
	// 降到 69 必须拒绝，且不能静默取消凭证（版本仍停留在 1）。
	_, err = svc.AdjustBudget(context.Background(), AdjustBudgetParams{
		CampaignID: c.ID, AdjustmentID: "adj-low2", ExpectedVersion: 1,
		SetTotal: true, TotalBudget: money.MustParse("69", "CNY"),
	})
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("err = %v", err)
	}
	if de, _ := AsError(err); de.Level != "total_floor" || de.Available.String() != "70" {
		t.Fatalf("level=%s floor=%s", de.Level, de.Available)
	}
	rid := svc.byRequest[c.ID]["req-2"]
	rr := svc.reservations[rid]
	if rr.Status != StatusReserved || rr.Amount.String() != "50" {
		t.Fatalf("reservation must survive rejected adjustment: status=%s amount=%s", rr.Status, rr.Amount)
	}

	// 降到恰好下限 70 可以，凭证保持有效；之后可用额度为 0。
	if _, err := svc.AdjustBudget(context.Background(), AdjustBudgetParams{
		CampaignID: c.ID, AdjustmentID: "adj-70", ExpectedVersion: 1,
		SetTotal: true, TotalBudget: money.MustParse("70", "CNY"),
	}); err != nil {
		t.Fatalf("decrease to exact floor 70: %v", err)
	}
	if b := balance(t, svc, c.ID); b.ConfigVersion != 2 || b.TotalAvailable.String() != "0" {
		t.Fatalf("version=%d available=%s, want 2/0", b.ConfigVersion, b.TotalAvailable)
	}
}

func TestAdjustBudgetDailyFloor(t *testing.T) {
	svc, clk := newTestService(t)
	// 长 TTL（48h）保证跨天预占仍然有效，否则过期归还后不构成日下限。
	c, err := svc.CreateCampaign(context.Background(), CreateCampaignParams{
		Name:        "daily-floor",
		TotalBudget: money.MustParse("10000", "CNY"),
		DailyCap:    money.MustParse("100", "CNY"),
		Timezone:    "UTC",
		DefaultTTL:  48 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 第一天预占 50（长 TTL 跨天），第二天预占 30。
	mustReserve(t, svc, c.ID, "req-day1", "50")
	clk.Set(time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC))
	mustReserve(t, svc, c.ID, "req-day2", "30")

	// 日上限降到 40：第一天仍有 50 有效预占 → daily_floor 拒绝。
	_, err = svc.AdjustBudget(context.Background(), AdjustBudgetParams{
		CampaignID: c.ID, AdjustmentID: "adj-d", ExpectedVersion: 1,
		SetDaily: true, DailyCap: money.MustParse("40", "CNY"),
	})
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("err = %v", err)
	}
	if de, _ := AsError(err); de.Level != "daily_floor" || de.Available.String() != "50" {
		t.Fatalf("level=%s floor=%s", de.Level, de.Available)
	}
	// 降到 50：两天的有效占用都 ≤ 50，成功。
	if _, err := svc.AdjustBudget(context.Background(), AdjustBudgetParams{
		CampaignID: c.ID, AdjustmentID: "adj-d2", ExpectedVersion: 1,
		SetDaily: true, DailyCap: money.MustParse("50", "CNY"),
	}); err != nil {
		t.Fatal(err)
	}
	// 第二天已占 30，新上限 50 下只能再占 20。
	_, err = svc.Reserve(context.Background(), ReserveParams{
		CampaignID: c.ID, RequestID: "req-day2b", Amount: money.MustParse("21", "CNY"),
	})
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("err = %v, want budget_exceeded", err)
	}
}

func TestAdjustBudgetIdempotency(t *testing.T) {
	svc, _ := newTestService(t)
	c := mustCampaign(t, svc, "100", "1000")

	params := AdjustBudgetParams{
		CampaignID: c.ID, AdjustmentID: "adj-1", ExpectedVersion: 1,
		SetTotal: true, TotalBudget: money.MustParse("150", "CNY"),
	}
	first, err := svc.AdjustBudget(context.Background(), params)
	if err != nil {
		t.Fatal(err)
	}
	// 重放：即使带的是过期期望版本，也返回首次结果，版本不再递增。
	again, err := svc.AdjustBudget(context.Background(), params)
	if err != nil {
		t.Fatal(err)
	}
	if again.ConfigVersion != first.ConfigVersion || again.TotalBudget.String() != "150" {
		t.Fatalf("replay diverged: %+v vs %+v", again, first)
	}
	if b := balance(t, svc, c.ID); b.ConfigVersion != 2 {
		t.Fatalf("version = %d, want 2", b.ConfigVersion)
	}

	// 同调整号不同内容 → 幂等冲突。
	params.TotalBudget = money.MustParse("160", "CNY")
	if _, err := svc.AdjustBudget(context.Background(), params); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("err = %v, want idempotency_conflict", err)
	}
}

func TestAdjustBudgetVersionConflict(t *testing.T) {
	svc, _ := newTestService(t)
	c := mustCampaign(t, svc, "100", "1000")
	_, err := svc.AdjustBudget(context.Background(), AdjustBudgetParams{
		CampaignID: c.ID, AdjustmentID: "adj-1", ExpectedVersion: 99,
		SetTotal: true, TotalBudget: money.MustParse("200", "CNY"),
	})
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("err = %v, want version_conflict", err)
	}
	de, _ := AsError(err)
	if de.ExpectedVersion != 99 || de.ActualVersion != 1 {
		t.Fatalf("expected=%d actual=%d", de.ExpectedVersion, de.ActualVersion)
	}
}

func TestAdjustBudgetValidation(t *testing.T) {
	svc, _ := newTestService(t)
	c := mustCampaign(t, svc, "100", "1000")

	cases := []AdjustBudgetParams{
		{CampaignID: c.ID, AdjustmentID: "", ExpectedVersion: 1, SetTotal: true, TotalBudget: money.MustParse("200", "CNY")},
		{CampaignID: c.ID, AdjustmentID: "a", ExpectedVersion: 0, SetTotal: true, TotalBudget: money.MustParse("200", "CNY")},
		{CampaignID: c.ID, AdjustmentID: "a", ExpectedVersion: 1}, // 什么都不改
		{CampaignID: c.ID, AdjustmentID: "a", ExpectedVersion: 1, SetTotal: true, TotalBudget: money.MustParse("0", "CNY")},
	}
	for i, p := range cases {
		if _, err := svc.AdjustBudget(context.Background(), p); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("case %d: err = %v, want invalid_argument", i, err)
		}
	}
	if _, err := svc.AdjustBudget(context.Background(), AdjustBudgetParams{
		CampaignID: "cmp_missing", AdjustmentID: "a", ExpectedVersion: 1,
		SetTotal: true, TotalBudget: money.MustParse("200", "CNY"),
	}); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing campaign: err = %v", err)
	}
}

func TestGetBudgetAdjustmentsHistory(t *testing.T) {
	svc, _ := newTestService(t)
	c := mustCampaign(t, svc, "100", "1000")
	for i, v := range []string{"120", "150"} {
		if _, err := svc.AdjustBudget(context.Background(), AdjustBudgetParams{
			CampaignID:      c.ID,
			AdjustmentID:    fmt.Sprintf("adj-%d", i+1),
			ExpectedVersion: int64(i + 1),
			SetTotal:        true,
			TotalBudget:     money.MustParse(v, "CNY"),
		}); err != nil {
			t.Fatal(err)
		}
	}
	adjs, err := svc.GetBudgetAdjustments(context.Background(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(adjs) != 2 {
		t.Fatalf("got %d adjustments", len(adjs))
	}
	if adjs[0].ConfigVersion != 2 || adjs[0].TotalBudget.String() != "120" {
		t.Fatalf("first = %+v", adjs[0])
	}
	if adjs[1].ConfigVersion != 3 || adjs[1].AdjustmentID != "adj-2" {
		t.Fatalf("second = %+v", adjs[1])
	}
}

// ---------- 配置切换与费用冻结归属 ----------

func TestUpdateConfigBumpsVersion(t *testing.T) {
	svc, _ := newTestService(t)
	c := mustCurveCampaign(t, svc, "1000", "100", halfDayCurve())

	tz := "Asia/Shanghai"
	got, err := svc.UpdateConfig(context.Background(), UpdateConfigParams{
		CampaignID: c.ID, ExpectedVersion: 1, Timezone: &tz,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ConfigVersion != 2 || got.Location.String() != "Asia/Shanghai" {
		t.Fatalf("version=%d tz=%s", got.ConfigVersion, got.Location)
	}
	// 旧版本号再改 → 版本冲突。
	tz2 := "UTC"
	if _, err := svc.UpdateConfig(context.Background(), UpdateConfigParams{
		CampaignID: c.ID, ExpectedVersion: 1, Timezone: &tz2,
	}); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("err = %v, want version_conflict", err)
	}

	// 移除曲线，版本继续递增。
	got2, err := svc.UpdateConfig(context.Background(), UpdateConfigParams{
		CampaignID: c.ID, ExpectedVersion: 2, SetCurve: true, Curve: nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got2.ConfigVersion != 3 || got2.Curve != nil {
		t.Fatalf("version=%d curve=%v", got2.ConfigVersion, got2.Curve)
	}
}

// TestLateReceiptStaysInFrozenBucket 时区切换后，迟到回执仍归入创建时冻结的日期与时段，
// 当前日余额不被重复扣减，节奏统计不被重新归类。
func TestLateReceiptStaysInFrozenBucket(t *testing.T) {
	svc, clk := newTestService(t)
	c := mustCurveCampaign(t, svc, "10000", "1000", halfDayCurve())

	// UTC 2026-09-26 10:00 创建预占，冻结 day=2026-09-26 slot=10 version=1。
	r := mustReserve(t, svc, c.ID, "req-1", "30")

	// 切换时区到上海（UTC+8）。
	tz := "Asia/Shanghai"
	if _, err := svc.UpdateConfig(context.Background(), UpdateConfigParams{
		CampaignID: c.ID, ExpectedVersion: 1, Timezone: &tz,
	}); err != nil {
		t.Fatal(err)
	}

	// 时钟推到 UTC 2026-09-27 17:00 = 上海 2026-09-28 01:00，凭证仍在 48h TTL 内。
	clk.Set(time.Date(2026, 9, 27, 17, 0, 0, 0, time.UTC))
	if _, err := svc.Capture(context.Background(), CaptureParams{
		ReservationID: r.ID, ReceiptID: "rcpt-late", Amount: money.MustParse("25", "CNY"),
	}); err != nil {
		t.Fatal(err)
	}

	// 当前（上海 09-28）余额：当日没有任何费用。
	b := balance(t, svc, c.ID)
	if b.DayKey != "2026-09-28" {
		t.Fatalf("current day key = %s, want 2026-09-28", b.DayKey)
	}
	if b.DailySpent.String() != "0" || b.TotalSpent.String() != "25" {
		t.Fatalf("daily spent=%s (want 0), total spent=%s (want 25)", b.DailySpent, b.TotalSpent)
	}

	// 冻结日 09-26 的节奏报告：slot10 应记录 spent=25, reserved=0，其余桶为空。
	report, err := svc.GetPacingReport(context.Background(), c.ID, "2026-09-26")
	if err != nil {
		t.Fatal(err)
	}
	s10 := report.Slots[10]
	if s10.Spent.String() != "25" || s10.Reserved.String() != "0" {
		t.Fatalf("frozen slot: spent=%s reserved=%s", s10.Spent, s10.Reserved)
	}
	if s10.CumulativeSpent.String() != "25" {
		t.Fatalf("cumulative spent = %s, want 25", s10.CumulativeSpent)
	}
	if report.TotalSpent.String() != "25" || report.TotalReserved.String() != "0" {
		t.Fatalf("day report spent=%s reserved=%s, want 25/0",
			report.TotalSpent, report.TotalReserved)
	}
	// 释放的 5 回到总预算池：10000 - 25 = 9975 可用。
	if b.TotalAvailable.String() != "9975" {
		t.Fatalf("total available = %s, want 9975", b.TotalAvailable)
	}
}

// TestExpiryAfterConfigSwitchReleasesFrozenBucket 配置切换后凭证过期，
// 额度释放回冻结桶，不按新配置归类。
func TestExpiryAfterConfigSwitchReleasesFrozenBucket(t *testing.T) {
	svc, clk := newTestService(t)
	c := mustCurveCampaign(t, svc, "10000", "1000", halfDayCurve())
	mustReserve(t, svc, c.ID, "req-1", "30")

	tz := "Asia/Shanghai"
	if _, err := svc.UpdateConfig(context.Background(), UpdateConfigParams{
		CampaignID: c.ID, ExpectedVersion: 1, Timezone: &tz,
	}); err != nil {
		t.Fatal(err)
	}
	// 推进到凭证过期（48h TTL）。
	clk.Set(time.Date(2026, 9, 28, 11, 0, 0, 0, time.UTC))
	if n, err := svc.ExpireSweep(context.Background()); err != nil || n != 1 {
		t.Fatalf("sweep n=%d err=%v", n, err)
	}

	// 冻结日桶的预占必须清零。
	report, err := svc.GetPacingReport(context.Background(), c.ID, "2026-09-26")
	if err != nil {
		t.Fatal(err)
	}
	if report.TotalReserved.String() != "0" {
		t.Fatalf("frozen day reserved = %s, want 0", report.TotalReserved)
	}
	if report.Slots[10].Reserved.String() != "0" {
		t.Fatalf("slot 10 reserved = %s, want 0", report.Slots[10].Reserved)
	}
	b := balance(t, svc, c.ID)
	if b.TotalReserved.String() != "0" || b.TotalAvailable.String() != "10000" {
		t.Fatalf("total reserved=%s available=%s", b.TotalReserved, b.TotalAvailable)
	}
}

// TestCurveSwitchKeepsOldBuckets 曲线收紧后：旧预占仍占旧桶，新预占受新曲线约束，
// 旧凭证核销仍落在冻结桶。
func TestCurveSwitchKeepsOldBuckets(t *testing.T) {
	svc, _ := newTestService(t)
	c := mustCurveCampaign(t, svc, "10000", "100", halfDayCurve())
	old := mustReserve(t, svc, c.ID, "req-old", "40") // slot10，曲线允许 50

	// 新曲线：桶 10 只允许 30%。
	targets := make([]int64, SlotCount)
	for h := 0; h <= 10; h++ {
		targets[h] = 300000
	}
	for h := 11; h < 23; h++ {
		targets[h] = 600000
	}
	targets[23] = PacingPPM
	newCurve := &PacingCurve{Targets: targets}
	if _, err := svc.UpdateConfig(context.Background(), UpdateConfigParams{
		CampaignID: c.ID, ExpectedVersion: 1, SetCurve: true, Curve: newCurve,
	}); err != nil {
		t.Fatal(err)
	}

	// 新预占按新曲线：桶 10 目标 30，旧预占不计入新曲线判断？
	// ——计入：桶内占用按冻结桶聚合，与当前曲线无关。已用 40 > 新目标 30，节奏额度为负。
	_, err := svc.Reserve(context.Background(), ReserveParams{
		CampaignID: c.ID, RequestID: "req-new", Amount: money.MustParse("1", "CNY"),
	})
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("err = %v, want budget_exceeded under tightened curve", err)
	}
	if de, _ := AsError(err); de.Level != "pacing" {
		t.Fatalf("level = %s", de.Level)
	}

	// 旧凭证核销仍归冻结桶 10。
	if _, err := svc.Capture(context.Background(), CaptureParams{
		ReservationID: old.ID, ReceiptID: "rcpt-1", Amount: money.MustParse("40", "CNY"),
	}); err != nil {
		t.Fatal(err)
	}
	report, err := svc.GetPacingReport(context.Background(), c.ID, "2026-09-26")
	if err != nil {
		t.Fatal(err)
	}
	if report.Slots[10].Spent.String() != "40" || report.Slots[10].Reserved.String() != "0" {
		t.Fatalf("slot10 spent=%s reserved=%s", report.Slots[10].Spent, report.Slots[10].Reserved)
	}
}

// TestPacingReportVariance 逐时段报告的目标、累计核销与偏差计算正确。
func TestPacingReportVariance(t *testing.T) {
	svc, _ := newTestService(t)
	c := mustCurveCampaign(t, svc, "10000", "100", halfDayCurve())
	r := mustReserve(t, svc, c.ID, "req-1", "50") // 打满桶10的50%目标
	if _, err := svc.Capture(context.Background(), CaptureParams{
		ReservationID: r.ID, ReceiptID: "rcpt-1", Amount: money.MustParse("30", "CNY"),
	}); err != nil {
		t.Fatal(err)
	}
	report, err := svc.GetPacingReport(context.Background(), c.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if report.DayKey != "2026-09-26" || !report.CurveEnabled {
		t.Fatalf("day=%s enabled=%v", report.DayKey, report.CurveEnabled)
	}
	s10 := report.Slots[10]
	if s10.Target.String() != "50" || s10.CumulativeSpent.String() != "30" {
		t.Fatalf("target=%s cumSpent=%s", s10.Target, s10.CumulativeSpent)
	}
	// 偏差 = 累计核销 - 目标 = -20（欠投）。
	if s10.Variance.String() != "-20" {
		t.Fatalf("variance = %s, want -20", s10.Variance)
	}
	// 上午更早的桶没有费用，偏差 -50。
	if report.Slots[5].Variance.String() != "-50" {
		t.Fatalf("slot5 variance = %s", report.Slots[5].Variance)
	}
}

// TestPacingReportInvalidDay 非法日期参数返回参数错误。
func TestPacingReportInvalidDay(t *testing.T) {
	svc, _ := newTestService(t)
	c := mustCurveCampaign(t, svc, "1000", "100", halfDayCurve())
	if _, err := svc.GetPacingReport(context.Background(), c.ID, "2026-9-26"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("err = %v, want invalid_argument", err)
	}
}

// TestPacingReportWithoutCurve 未配置曲线时每个桶的累计目标都等于日上限
// （即不做节奏限制），报告仍提供逐时段实际值与偏差。
func TestPacingReportWithoutCurve(t *testing.T) {
	svc, _ := newTestService(t)
	c := mustCampaign(t, svc, "1000", "100")
	r := mustReserve(t, svc, c.ID, "req-1", "30")
	if _, err := svc.Capture(context.Background(), CaptureParams{
		ReservationID: r.ID, ReceiptID: "rcpt-1", Amount: money.MustParse("30", "CNY"),
	}); err != nil {
		t.Fatal(err)
	}
	report, err := svc.GetPacingReport(context.Background(), c.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if report.CurveEnabled {
		t.Fatalf("curve should be disabled")
	}
	for _, s := range report.Slots {
		if s.CumulativeTargetPPM != PacingPPM || s.Target.String() != "100" {
			t.Fatalf("slot %d target ppm=%d amount=%s, want full daily cap",
				s.Hour, s.CumulativeTargetPPM, s.Target)
		}
	}
	if report.Slots[10].Spent.String() != "30" || report.Slots[10].Variance.String() != "-70" {
		t.Fatalf("slot10 = %+v", report.Slots[10])
	}
}

// TestPacingAndBudgetReplayAcrossRestart 重启重放后曲线、版本、调整历史、
// 冻结桶归属全部恢复。
func TestPacingAndBudgetReplayAcrossRestart(t *testing.T) {
	clk := clock.NewFake(testStart)
	st := store.NewMemStore()

	svc1, err := NewService(clk, st)
	if err != nil {
		t.Fatal(err)
	}
	c := mustCurveCampaign(t, svc1, "1000", "100", halfDayCurve())
	r := mustReserve(t, svc1, c.ID, "req-1", "40")
	if _, err := svc1.AdjustBudget(context.Background(), AdjustBudgetParams{
		CampaignID: c.ID, AdjustmentID: "adj-1", ExpectedVersion: 1,
		SetTotal: true, TotalBudget: money.MustParse("2000", "CNY"),
	}); err != nil {
		t.Fatal(err)
	}
	tz := "Asia/Shanghai"
	if _, err := svc1.UpdateConfig(context.Background(), UpdateConfigParams{
		CampaignID: c.ID, ExpectedVersion: 2, Timezone: &tz,
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
	if got.ConfigVersion != 3 || got.TotalBudget.String() != "2000" {
		t.Fatalf("version=%d budget=%s", got.ConfigVersion, got.TotalBudget)
	}
	if got.Location.String() != "Asia/Shanghai" || got.Curve == nil {
		t.Fatalf("tz=%s curve=%v", got.Location, got.Curve)
	}
	// 调整历史与幂等索引恢复。
	adjs, _ := svc2.GetBudgetAdjustments(context.Background(), c.ID)
	if len(adjs) != 1 || adjs[0].AdjustmentID != "adj-1" {
		t.Fatalf("adjustments = %+v", adjs)
	}
	replay, err := svc2.AdjustBudget(context.Background(), AdjustBudgetParams{
		CampaignID: c.ID, AdjustmentID: "adj-1", ExpectedVersion: 1,
		SetTotal: true, TotalBudget: money.MustParse("2000", "CNY"),
	})
	if err != nil || replay.ConfigVersion != 2 {
		t.Fatalf("idempotent replay after restart: %+v err=%v", replay, err)
	}
	// 迟到核销仍归冻结桶（UTC 09-26 slot10），尽管当前时区是上海。
	clk.Set(time.Date(2026, 9, 26, 17, 0, 0, 0, time.UTC))
	if _, err := svc2.Capture(context.Background(), CaptureParams{
		ReservationID: r.ID, ReceiptID: "rcpt-1", Amount: money.MustParse("40", "CNY"),
	}); err != nil {
		t.Fatal(err)
	}
	report, err := svc2.GetPacingReport(context.Background(), c.ID, "2026-09-26")
	if err != nil {
		t.Fatal(err)
	}
	if report.Slots[10].Spent.String() != "40" {
		t.Fatalf("frozen bucket spent = %s, want 40", report.Slots[10].Spent)
	}
}

// TestConcurrentReserveVsBudgetIncrease 预算提高与并发预占竞争时，
// 总预算不被突破（所有成功预占之和 ≤ 当前预算）。
func TestConcurrentReserveVsBudgetIncrease(t *testing.T) {
	for trial := 0; trial < 20; trial++ {
		svc, _ := newTestService(t)
		c := mustCampaign(t, svc, "1000", "1000")

		stop := make(chan struct{})
		var budgetWG sync.WaitGroup

		// 一个 worker 不断提高预算（带版本重试）。
		budgetWG.Add(1)
		go func() {
			defer budgetWG.Done()
			version := int64(1)
			budget := int64(1000)
			for {
				select {
				case <-stop:
					return
				default:
				}
				budget += 100
				_, err := svc.AdjustBudget(context.Background(), AdjustBudgetParams{
					CampaignID:      c.ID,
					AdjustmentID:    fmt.Sprintf("adj-t%d-%d", trial, budget),
					ExpectedVersion: version,
					SetTotal:        true,
					TotalBudget:     money.FromMinor(budget*1e6, "CNY"),
				})
				if err == nil {
					version++
				}
			}
		}()

		// 多个 worker 尝试小额定额预占。
		var reserveWG sync.WaitGroup
		const workers = 16
		reserveWG.Add(workers)
		for w := 0; w < workers; w++ {
			go func(w int) {
				defer reserveWG.Done()
				for i := 0; i < 20; i++ {
					_, _ = svc.Reserve(context.Background(), ReserveParams{
						CampaignID: c.ID,
						RequestID:  fmt.Sprintf("req-%d-%d", w, i),
						Amount:     money.MustParse("10", "CNY"),
					})
				}
			}(w)
		}
		reserveWG.Wait()
		close(stop)
		budgetWG.Wait()

		b := balance(t, svc, c.ID)
		committed := b.TotalSpent.Add(b.TotalReserved)
		if committed.Cmp(b.TotalBudget) > 0 {
			t.Fatalf("trial %d: committed %s exceeds budget %s", trial, committed, b.TotalBudget)
		}
	}
}
