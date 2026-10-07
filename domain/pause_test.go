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

// TestPauseBlocksReserve 暂停后新的曝光预占被整体拒绝，且不产生任何消耗记录。
func TestPauseBlocksReserve(t *testing.T) {
	svc, _ := newTestService(t)
	c := mustCampaign(t, svc, "100", "50")

	if _, err := svc.PauseCampaign(context.Background(), c.ID, "预算超投排查"); err != nil {
		t.Fatal(err)
	}
	_, err := svc.Reserve(context.Background(), ReserveParams{
		CampaignID: c.ID, RequestID: "req-p1", Amount: money.MustParse("10", "CNY"),
	})
	if !errors.Is(err, ErrCampaignPaused) {
		t.Fatalf("err = %v, want campaign_paused", err)
	}
	// 拒绝不留半条消耗记录。
	b := balance(t, svc, c.ID)
	if b.TotalReserved.String() != "0" || b.TotalSpent.String() != "0" {
		t.Fatalf("reserved=%s spent=%s, want both 0", b.TotalReserved, b.TotalSpent)
	}
	if b.Status != CampaignStatusPaused {
		t.Fatalf("balance status = %s, want paused", b.Status)
	}
	// 暂停期间失败的请求号未被占用：恢复后同号可正常使用。
	if _, err := svc.ResumeCampaign(context.Background(), c.ID, "排查完成"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Reserve(context.Background(), ReserveParams{
		CampaignID: c.ID, RequestID: "req-p1", Amount: money.MustParse("10", "CNY"),
	}); err != nil {
		t.Fatalf("reserve after resume: %v", err)
	}
}

// TestPauseResumeIdempotent 重复暂停/恢复是幂等状态操作，不追加新事件。
func TestPauseResumeIdempotent(t *testing.T) {
	svc, _ := newTestService(t)
	c := mustCampaign(t, svc, "100", "50")

	for i := 0; i < 2; i++ {
		got, err := svc.PauseCampaign(context.Background(), c.ID, "重复暂停")
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != CampaignStatusPaused {
			t.Fatalf("status = %s, want paused", got.Status)
		}
	}
	for i := 0; i < 2; i++ {
		got, err := svc.ResumeCampaign(context.Background(), c.ID, "重复恢复")
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != CampaignStatusActive {
			t.Fatalf("status = %s, want active", got.Status)
		}
	}
	if _, err := svc.PauseCampaign(context.Background(), "", "x"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("empty id err = %v, want invalid_argument", err)
	}
	if _, err := svc.PauseCampaign(context.Background(), "cmp_missing", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing campaign err = %v, want not_found", err)
	}
}

// TestCaptureWhilePaused 暂停前已确认的曝光（凭证）在暂停期间仍可核销，
// 已确认的消耗不因暂停而丢失。
func TestCaptureWhilePaused(t *testing.T) {
	svc, _ := newTestService(t)
	c := mustCampaign(t, svc, "100", "50")

	r := mustReserve(t, svc, c.ID, "req-1", "30")
	if _, err := svc.PauseCampaign(context.Background(), c.ID, "暂停"); err != nil {
		t.Fatal(err)
	}
	got, err := svc.Capture(context.Background(), CaptureParams{
		ReservationID: r.ID, ReceiptID: "rcpt-1", Amount: money.MustParse("20", "CNY"),
	})
	if err != nil {
		t.Fatalf("capture while paused: %v", err)
	}
	if got.Status != StatusCaptured || got.Captured.String() != "20" {
		t.Fatalf("status=%s captured=%s", got.Status, got.Captured)
	}
	if b := balance(t, svc, c.ID); b.TotalSpent.String() != "20" {
		t.Fatalf("spent = %s, want 20", b.TotalSpent)
	}
}

// TestResumeContinuesFromConfirmedSpend 恢复后从当前已确认消耗继续：
// 暂停期间的失败请求不被补记，余额口径与暂停前完全一致。
func TestResumeContinuesFromConfirmedSpend(t *testing.T) {
	svc, _ := newTestService(t)
	c := mustCampaign(t, svc, "100", "50")

	r := mustReserve(t, svc, c.ID, "req-1", "30")
	if _, err := svc.Capture(context.Background(), CaptureParams{
		ReservationID: r.ID, ReceiptID: "rcpt-1", Amount: money.MustParse("20", "CNY"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PauseCampaign(context.Background(), c.ID, "暂停"); err != nil {
		t.Fatal(err)
	}
	// 暂停期间的失败请求（不应留下任何痕迹）。
	for i := 0; i < 3; i++ {
		_, err := svc.Reserve(context.Background(), ReserveParams{
			CampaignID: c.ID, RequestID: fmt.Sprintf("req-gap-%d", i),
			Amount: money.MustParse("5", "CNY"),
		})
		if !errors.Is(err, ErrCampaignPaused) {
			t.Fatalf("err = %v, want campaign_paused", err)
		}
	}
	if _, err := svc.ResumeCampaign(context.Background(), c.ID, "恢复"); err != nil {
		t.Fatal(err)
	}
	b := balance(t, svc, c.ID)
	if b.TotalSpent.String() != "20" || b.TotalReserved.String() != "0" ||
		b.TotalAvailable.String() != "80" {
		t.Fatalf("spent=%s reserved=%s available=%s, want 20/0/80",
			b.TotalSpent, b.TotalReserved, b.TotalAvailable)
	}
	// 恢复后立即可用剩余额度继续投放（日预算 50 已花 20，当日还可占 30）。
	mustReserve(t, svc, c.ID, "req-2", "30")
	if b := balance(t, svc, c.ID); b.DailyAvailable.String() != "0" {
		t.Fatalf("daily available = %s, want 0", b.DailyAvailable)
	}
}

// TestConcurrentPauseAndReserve 暂停与预占并发竞争：每笔预占要么完整成功、
// 要么整体失败，预算不变量恒成立。
func TestConcurrentPauseAndReserve(t *testing.T) {
	svc, _ := newTestService(t)
	c := mustCampaign(t, svc, "1000", "1000")

	if _, err := svc.PauseCampaign(context.Background(), c.ID, "先暂停"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var succeeded, paused int
	for w := 0; w < 32; w++ {
		wg.Add(2)
		go func(w int) {
			defer wg.Done()
			_, err := svc.Reserve(context.Background(), ReserveParams{
				CampaignID: c.ID, RequestID: fmt.Sprintf("req-%d", w),
				Amount: money.MustParse("10", "CNY"),
			})
			mu.Lock()
			switch {
			case err == nil:
				succeeded++
			case errors.Is(err, ErrCampaignPaused):
				paused++
			default:
				t.Errorf("unexpected err: %v", err)
			}
			mu.Unlock()
		}(w)
		go func(w int) {
			defer wg.Done()
			if w%2 == 0 {
				_, _ = svc.ResumeCampaign(context.Background(), c.ID, "并发恢复")
			} else {
				_, _ = svc.PauseCampaign(context.Background(), c.ID, "并发暂停")
			}
		}(w)
	}
	wg.Wait()
	if succeeded+paused != 32 {
		t.Fatalf("succeeded=%d paused=%d, want total 32", succeeded, paused)
	}
	// 无论竞争结果如何，占用额恒等于成功笔数 * 10，绝不超支或漏记。
	b := balance(t, svc, c.ID)
	want := money.MustParse("10", "CNY")
	total := money.Zero("CNY")
	for i := 0; i < succeeded; i++ {
		total = total.Add(want)
	}
	if b.TotalReserved.Cmp(total) != 0 {
		t.Fatalf("reserved = %s, want %s (%d succeeded)", b.TotalReserved, total, succeeded)
	}
}

// TestRequestIDConflictAcrossCampaign 同一外部请求号用于不同活动 → 明确幂等冲突。
func TestRequestIDConflictAcrossCampaign(t *testing.T) {
	svc, _ := newTestService(t)
	c1 := mustCampaign(t, svc, "100", "50")
	c2 := mustCampaign(t, svc, "100", "50")

	mustReserve(t, svc, c1.ID, "req-x", "10")
	_, err := svc.Reserve(context.Background(), ReserveParams{
		CampaignID: c2.ID, RequestID: "req-x", Amount: money.MustParse("10", "CNY"),
	})
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("err = %v, want idempotency_conflict", err)
	}
	// 冲突不消耗 c2 的任何额度。
	if b := balance(t, svc, c2.ID); b.TotalReserved.String() != "0" {
		t.Fatalf("c2 reserved = %s, want 0", b.TotalReserved)
	}
}

// TestRequestIDConflictAcrossSlot 同一请求号跨时段重放 → 明确幂等冲突，
// 旧计划的迟到请求不能把活动重新推进到已关闭的时间段。
func TestRequestIDConflictAcrossSlot(t *testing.T) {
	svc, clk := newTestService(t)
	c := mustCampaign(t, svc, "100", "50")

	r1 := mustReserve(t, svc, c.ID, "req-1", "10") // slot 10
	clk.Advance(time.Hour)                         // slot 11
	_, err := svc.Reserve(context.Background(), ReserveParams{
		CampaignID: c.ID, RequestID: "req-1", Amount: money.MustParse("10", "CNY"),
	})
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("err = %v, want idempotency_conflict", err)
	}
	// 同时段同内容重放仍返回原凭证。
	clk.Set(testStart)
	r2, err := svc.Reserve(context.Background(), ReserveParams{
		CampaignID: c.ID, RequestID: "req-1", Amount: money.MustParse("10", "CNY"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if r2.ID != r1.ID {
		t.Fatalf("replayed reservation %s, want %s", r2.ID, r1.ID)
	}
}

// TestAdjustmentReasonRecorded 每次预算/节奏调整的原因随版本历史保存。
func TestAdjustmentReasonRecorded(t *testing.T) {
	svc, _ := newTestService(t)
	c := mustCampaign(t, svc, "100", "50")

	if _, err := svc.AdjustBudget(context.Background(), AdjustBudgetParams{
		CampaignID: c.ID, AdjustmentID: "adj-1", ExpectedVersion: 1,
		TotalBudget: money.MustParse("200", "CNY"),
		DailyCap:    money.MustParse("80", "CNY"),
		Reason:      "双十一加量：依据上周同期消耗翻倍",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AdjustConfig(context.Background(), AdjustConfigParams{
		CampaignID: c.ID, AdjustmentID: "adj-2", ExpectedVersion: 2,
		CurveWeights: uniformWeights(),
		Reason:       "引入均匀节奏：避免早间集中消耗",
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
	if versions[1].Reason != "双十一加量：依据上周同期消耗翻倍" {
		t.Fatalf("v2 reason = %q", versions[1].Reason)
	}
	if versions[2].Reason != "引入均匀节奏：避免早间集中消耗" {
		t.Fatalf("v3 reason = %q", versions[2].Reason)
	}
	// 同调整号但原因不同 → 幂等冲突。
	_, err = svc.AdjustBudget(context.Background(), AdjustBudgetParams{
		CampaignID: c.ID, AdjustmentID: "adj-1", ExpectedVersion: 1,
		TotalBudget: money.MustParse("200", "CNY"),
		DailyCap:    money.MustParse("80", "CNY"),
		Reason:      "另一个原因",
	})
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("err = %v, want idempotency_conflict", err)
	}
}

// TestPauseResumePersistAcrossRestart 暂停状态经事件重放完整恢复。
func TestPauseResumePersistAcrossRestart(t *testing.T) {
	clk := clock.NewFake(testStart)
	st := store.NewMemStore()

	svc1, err := NewService(clk, st)
	if err != nil {
		t.Fatal(err)
	}
	c := mustCampaign(t, svc1, "100", "50")
	if _, err := svc1.PauseCampaign(context.Background(), c.ID, "重启前暂停"); err != nil {
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
	if got.Status != CampaignStatusPaused {
		t.Fatalf("status after replay = %s, want paused", got.Status)
	}
	// 重放后暂停仍然生效。
	_, err = svc2.Reserve(context.Background(), ReserveParams{
		CampaignID: c.ID, RequestID: "req-1", Amount: money.MustParse("10", "CNY"),
	})
	if !errors.Is(err, ErrCampaignPaused) {
		t.Fatalf("err = %v, want campaign_paused", err)
	}
	// 恢复后事件继续追加，状态正确翻转。
	if _, err := svc2.ResumeCampaign(context.Background(), c.ID, "重启后恢复"); err != nil {
		t.Fatal(err)
	}
	svc3, err := NewService(clk, st)
	if err != nil {
		t.Fatal(err)
	}
	got, _ = svc3.GetCampaign(context.Background(), c.ID)
	if got.Status != CampaignStatusActive {
		t.Fatalf("status after second replay = %s, want active", got.Status)
	}
}
