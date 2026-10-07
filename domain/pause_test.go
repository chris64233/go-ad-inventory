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

func mustPause(t *testing.T, svc *Service, id, reason string) {
	t.Helper()
	c, err := svc.PauseCampaign(context.Background(), id, reason)
	if err != nil {
		t.Fatal(err)
	}
	if c.Status != CampaignPaused {
		t.Fatalf("status = %s, want paused", c.Status)
	}
}

func mustResume(t *testing.T, svc *Service, id, reason string) {
	t.Helper()
	c, err := svc.ResumeCampaign(context.Background(), id, reason)
	if err != nil {
		t.Fatal(err)
	}
	if c.Status != CampaignActive {
		t.Fatalf("status = %s, want active", c.Status)
	}
}

// 暂停期间新预占被拒绝（conflict），且不产生任何消耗记录。
func TestPauseRejectsReserveWithoutSideEffects(t *testing.T) {
	svc, _ := newTestService(t)
	c := mustCampaign(t, svc, "100", "50")

	mustPause(t, svc, c.ID, "预算超投，临时止损")

	_, err := svc.Reserve(context.Background(), ReserveParams{
		CampaignID: c.ID,
		RequestID:  "req-paused",
		Amount:     money.MustParse("10", "CNY"),
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("reserve while paused: err = %v, want conflict", err)
	}

	// 失败的请求不留下任何记录：余额不变，同请求号也不被占用。
	b := balance(t, svc, c.ID)
	if !b.TotalReserved.IsZero() || !b.TotalSpent.IsZero() {
		t.Fatalf("paused reserve left residue: reserved=%s spent=%s",
			b.TotalReserved, b.TotalSpent)
	}
	mustResume(t, svc, c.ID, "止损完成")
	r := mustReserve(t, svc, c.ID, "req-paused", "10")
	if r.Status != StatusReserved {
		t.Fatalf("status = %s, want reserved", r.Status)
	}
}

// 暂停前已确认的预占仍可核销（真实消耗必须入账）；
// 恢复后从已确认消耗继续，而不是从零开始。
func TestResumeContinuesFromConfirmedSpend(t *testing.T) {
	svc, _ := newTestService(t)
	c := mustCampaign(t, svc, "100", "50")
	r := mustReserve(t, svc, c.ID, "req-1", "30")

	mustPause(t, svc, c.ID, "计划调整")
	if _, err := svc.Capture(context.Background(), CaptureParams{
		ReservationID: r.ID,
		ReceiptID:     "rcpt-1",
		Amount:        money.MustParse("25", "CNY"),
	}); err != nil {
		t.Fatalf("capture while paused should succeed: %v", err)
	}

	mustResume(t, svc, c.ID, "恢复投放")
	b := balance(t, svc, c.ID)
	if b.TotalSpent.String() != "25" {
		t.Fatalf("spent = %s, want 25", b.TotalSpent)
	}
	// 可用额度 = 100 - 25（已确认消耗），暂停期间的失败请求没有补记。
	if b.TotalAvailable.String() != "75" {
		t.Fatalf("available = %s, want 75", b.TotalAvailable)
	}
}

// 暂停与恢复都是幂等的状态操作：重复调用不重复落事件、不报错。
func TestPauseResumeIdempotent(t *testing.T) {
	svc, _ := newTestService(t)
	c := mustCampaign(t, svc, "100", "50")

	mustPause(t, svc, c.ID, "r1")
	mustPause(t, svc, c.ID, "r2") // 重复暂停：幂等
	mustResume(t, svc, c.ID, "r3")
	mustResume(t, svc, c.ID, "r4") // 重复恢复：幂等

	if _, err := svc.PauseCampaign(context.Background(), "cmp_missing", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pause unknown campaign: err = %v, want not_found", err)
	}
	if _, err := svc.ResumeCampaign(context.Background(), "", "x"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("resume empty id: err = %v, want invalid_argument", err)
	}
}

// 暂停跨时段：暂停期间时段切换后，恢复的新预占落入当前时段，
// 旧时段不会因迟到请求被重新推进。
func TestPauseAcrossSlotBoundary(t *testing.T) {
	svc, clk := newTestService(t)
	c := mustCampaignWithCurve(t, svc, "100", "24", uniformWeights())
	day := clock.DateKey(clk.Now(), c.Location)
	startSlot := slotOf(clk.Now(), c.Location)

	mustPause(t, svc, c.ID, "夜间限流")
	clk.Advance(2 * time.Hour) // 暂停期间跨过两个时段
	mustResume(t, svc, c.ID, "恢复")

	r := mustReserve(t, svc, c.ID, "req-late", "1")
	wantSlot := (startSlot + 2) % 24
	if r.DayKey != day || r.Slot != wantSlot {
		t.Fatalf("reservation froze day=%s slot=%d, want day=%s slot=%d",
			r.DayKey, r.Slot, day, wantSlot)
	}
}

// 暂停/恢复状态随事件持久化，重启重放后保持一致。
func TestPauseStateSurvivesReplay(t *testing.T) {
	clk := clock.NewFake(testStart)
	st := store.NewMemStore()
	svc, err := NewService(clk, st)
	if err != nil {
		t.Fatal(err)
	}
	c := mustCampaign(t, svc, "100", "50")
	mustPause(t, svc, c.ID, "止损")

	reloaded, err := NewService(clk, st)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reloaded.GetCampaign(context.Background(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != CampaignPaused {
		t.Fatalf("status after replay = %s, want paused", got.Status)
	}
	if _, err := reloaded.Reserve(context.Background(), ReserveParams{
		CampaignID: c.ID,
		RequestID:  "req-x",
		Amount:     money.MustParse("1", "CNY"),
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("reserve after replay while paused: err = %v, want conflict", err)
	}
}

// 并发：暂停与预占竞争时，要么预占先于暂停成功（占住额度），
// 要么被拒绝且不留记录；不变量：已确认+有效预占 <= 预算。
func TestConcurrentPauseAndReserve(t *testing.T) {
	svc, _ := newTestService(t)
	c := mustCampaign(t, svc, "100", "100")

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = svc.Reserve(context.Background(), ReserveParams{
				CampaignID: c.ID,
				RequestID:  fmt.Sprintf("req-%d", i),
				Amount:     money.MustParse("10", "CNY"),
			})
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = svc.PauseCampaign(context.Background(), c.ID, "race")
	}()
	wg.Wait()

	b := balance(t, svc, c.ID)
	committed := b.TotalSpent.Add(b.TotalReserved)
	if committed.Cmp(b.TotalBudget) > 0 {
		t.Fatalf("committed %s exceeds budget %s", committed, b.TotalBudget)
	}
	// 暂停已生效：后续预占必须全部被拒。
	if _, err := svc.Reserve(context.Background(), ReserveParams{
		CampaignID: c.ID,
		RequestID:  "req-after",
		Amount:     money.MustParse("1", "CNY"),
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("reserve after pause: err = %v, want conflict", err)
	}
}

// 预算/配置调整记录原因与依据，并出现在版本流中；
// 同调整号重放原因不一致报幂等冲突。
func TestAdjustmentReasonRecordedAndIdempotent(t *testing.T) {
	svc, _ := newTestService(t)
	c := mustCampaignWithCurve(t, svc, "100", "24", uniformWeights())

	if _, err := svc.AdjustBudget(context.Background(), AdjustBudgetParams{
		CampaignID:      c.ID,
		AdjustmentID:    "adj-1",
		ExpectedVersion: 1,
		TotalBudget:     money.MustParse("200", "CNY"),
		DailyCap:        money.MustParse("48", "CNY"),
		Reason:          "双十一加投：依据昨日 ROI 2.3",
	}); err != nil {
		t.Fatal(err)
	}
	weights := uniformWeights()
	weights[10] = 5
	if _, err := svc.AdjustConfig(context.Background(), AdjustConfigParams{
		CampaignID:      c.ID,
		AdjustmentID:    "cfg-1",
		ExpectedVersion: 2,
		CurveWeights:    weights,
		Reason:          "上午 10 点加量：配合整点秒杀",
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
	if versions[1].Reason != "双十一加投：依据昨日 ROI 2.3" {
		t.Fatalf("budget version reason = %q", versions[1].Reason)
	}
	if versions[2].Reason != "上午 10 点加量：配合整点秒杀" {
		t.Fatalf("config version reason = %q", versions[2].Reason)
	}

	// 同调整号同内容（含原因）重放：返回原版本快照。
	snap, err := svc.AdjustBudget(context.Background(), AdjustBudgetParams{
		CampaignID:      c.ID,
		AdjustmentID:    "adj-1",
		ExpectedVersion: 1,
		TotalBudget:     money.MustParse("200", "CNY"),
		DailyCap:        money.MustParse("48", "CNY"),
		Reason:          "双十一加投：依据昨日 ROI 2.3",
	})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Version != 2 {
		t.Fatalf("replay snapshot version = %d, want 2", snap.Version)
	}
	// 同调整号但原因不同：幂等冲突。
	if _, err := svc.AdjustBudget(context.Background(), AdjustBudgetParams{
		CampaignID:      c.ID,
		AdjustmentID:    "adj-1",
		ExpectedVersion: 1,
		TotalBudget:     money.MustParse("200", "CNY"),
		DailyCap:        money.MustParse("48", "CNY"),
		Reason:          "另一个理由",
	}); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("reason mismatch: err = %v, want idempotency_conflict", err)
	}
}
