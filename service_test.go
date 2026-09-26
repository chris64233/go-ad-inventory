package adinventory

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCreateCampaignValidation(t *testing.T) {
	svc, _, _ := newTestService(t, time.Minute)

	if _, err := svc.CreateCampaign("", MoneyFromInt(1), MoneyFromInt(1)); err == nil {
		t.Fatal("empty id should fail")
	} else {
		assertErrKind(t, err, KindInvalidArgument)
	}
	if _, err := svc.CreateCampaign("c1", mustMoney(t, "-1"), MoneyFromInt(1)); err == nil {
		t.Fatal("negative total budget should fail")
	} else {
		assertErrKind(t, err, KindInvalidArgument)
	}
	if _, err := svc.CreateCampaign("c1", MoneyFromInt(10), MoneyFromInt(11)); err == nil {
		t.Fatal("daily > total should fail")
	} else {
		assertErrKind(t, err, KindInvalidArgument)
	}

	seedCampaign(t, svc, "c1", "100", "20")
	if _, err := svc.CreateCampaign("c1", MoneyFromInt(100), MoneyFromInt(20)); err == nil {
		t.Fatal("duplicate campaign should fail")
	} else {
		assertErrKind(t, err, KindInvalidArgument)
	}

	if _, err := svc.GetCampaign("missing"); err == nil {
		t.Fatal("missing campaign should fail")
	} else {
		assertErrKind(t, err, KindNotFound)
	}
}

func TestReserveHoldsBothLevelsAndReturnsToken(t *testing.T) {
	svc, store, _ := newTestService(t, 2*time.Minute)
	seedCampaign(t, svc, "c1", "100", "30")

	r, err := svc.Reserve(ReserveInput{CampaignID: "c1", RequestNo: "req-1", Amount: mustMoney(t, "20.50")})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if r.Token == "" || r.Status != StatusReserved {
		t.Fatalf("bad reservation: %+v", r)
	}
	if !r.ExpiresAt.Equal(r.CreatedAt.Add(2 * time.Minute)) {
		t.Fatalf("expires_at = %v, want +2m", r.ExpiresAt)
	}
	if r.Day != "2026-01-15" {
		t.Fatalf("day = %q", r.Day)
	}

	b, _ := svc.GetBalance("c1", "")
	if b.TotalReserved.String() != "20.5" || b.TotalAvailable.String() != "79.5" {
		t.Fatalf("total balance wrong: %+v", b)
	}
	if b.DayReserved.String() != "20.5" || b.DayAvailable.String() != "9.5" {
		t.Fatalf("daily balance wrong: %+v", b)
	}

	types := store.eventTypes()
	if len(types) != 2 || types[0] != evCampaignCreated || types[1] != evReservationCreated {
		t.Fatalf("unexpected events: %v", types)
	}
}

func TestReserveBudgetExceededLeavesNoHalfReservation(t *testing.T) {
	svc, store, _ := newTestService(t, time.Minute)
	seedCampaign(t, svc, "c1", "100", "30")

	_, err := svc.Reserve(ReserveInput{CampaignID: "c1", RequestNo: "req-1", Amount: mustMoney(t, "25")})
	if err != nil {
		t.Fatal(err)
	}

	// 总预算足够（已用25，再要80 => 105 > 100），日预算也不足（25+80 > 30）。
	_, err = svc.Reserve(ReserveInput{CampaignID: "c1", RequestNo: "req-2", Amount: mustMoney(t, "80")})
	assertErrKind(t, err, KindBudgetExceeded)
	if be, _ := AsError(err); be.Scope != "total" && be.Scope != "daily" {
		t.Fatalf("budget scope = %q", be.Scope)
	}

	b, _ := svc.GetBalance("c1", "")
	if b.TotalReserved.String() != "25" || b.DayReserved.String() != "25" {
		t.Fatalf("failed reserve must not hold any budget: %+v", b)
	}
	// 失败的预占不落任何事件。
	if len(store.eventTypes()) != 2 {
		t.Fatalf("failed reserve appended events: %v", store.eventTypes())
	}

	// 仅日预算触顶：总预算够但当日不够，必须整体失败。
	_, err = svc.Reserve(ReserveInput{CampaignID: "c1", RequestNo: "req-3", Amount: mustMoney(t, "10")})
	assertErrKind(t, err, KindBudgetExceeded)
	if be, _ := AsError(err); be.Scope != "daily" {
		t.Fatalf("scope = %q, want daily", be.Scope)
	}
}

func TestReserveValidation(t *testing.T) {
	svc, _, _ := newTestService(t, time.Minute)
	seedCampaign(t, svc, "c1", "100", "30")

	_, err := svc.Reserve(ReserveInput{CampaignID: "c1", RequestNo: "r", Amount: MoneyFromInt(0)})
	assertErrKind(t, err, KindInvalidArgument)
	_, err = svc.Reserve(ReserveInput{CampaignID: "c1", RequestNo: "r", Amount: mustMoney(t, "-1")})
	assertErrKind(t, err, KindInvalidArgument)
	_, err = svc.Reserve(ReserveInput{CampaignID: "c1", RequestNo: "", Amount: MoneyFromInt(1)})
	assertErrKind(t, err, KindInvalidArgument)
	_, err = svc.Reserve(ReserveInput{CampaignID: "missing", RequestNo: "r", Amount: MoneyFromInt(1)})
	assertErrKind(t, err, KindNotFound)
}

func TestReserveIdempotency(t *testing.T) {
	svc, store, _ := newTestService(t, time.Minute)
	seedCampaign(t, svc, "c1", "100", "30")
	in := ReserveInput{CampaignID: "c1", RequestNo: "req-1", Amount: mustMoney(t, "10"), Payload: "placement-A"}

	r1, err := svc.Reserve(in)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := svc.Reserve(in)
	if err != nil {
		t.Fatalf("idempotent replay failed: %v", err)
	}
	if r1.Token != r2.Token {
		t.Fatalf("idempotent replay returned different token: %s vs %s", r1.Token, r2.Token)
	}
	b, _ := svc.GetBalance("c1", "")
	if b.TotalReserved.String() != "10" {
		t.Fatalf("replayed reserve held budget twice: %s", b.TotalReserved)
	}
	if n := len(store.eventTypes()); n != 2 {
		t.Fatalf("replay appended events: %d", n)
	}

	// 同号不同内容 -> 幂等冲突（逐字段比对：活动、金额、载荷）。
	for _, mutated := range []ReserveInput{
		{CampaignID: "c1", RequestNo: "req-1", Amount: mustMoney(t, "11"), Payload: "placement-A"},
		{CampaignID: "c1", RequestNo: "req-1", Amount: mustMoney(t, "10"), Payload: "placement-B"},
	} {
		_, err := svc.Reserve(mutated)
		assertErrKind(t, err, KindIdempotencyConflict)
	}
}

func TestConcurrentReserveNeverExceedsBudgets(t *testing.T) {
	svc, _, _ := newTestService(t, time.Hour)
	seedCampaign(t, svc, "c1", "100.00", "100.00")

	const n = 200
	var wg sync.WaitGroup
	var ok, exceeded int64
	var mu sync.Mutex
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			_, err := svc.Reserve(ReserveInput{
				CampaignID: "c1",
				RequestNo:  "req-" + itoa(i),
				Amount:     mustMoney(t, "1.00"),
			})
			mu.Lock()
			if err == nil {
				ok++
			} else {
				if be, isBE := AsError(err); !isBE || be.Kind != KindBudgetExceeded {
					t.Errorf("unexpected error: %v", err)
				}
				exceeded++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()

	if ok != 100 || exceeded != 100 {
		t.Fatalf("ok=%d exceeded=%d, want 100/100", ok, exceeded)
	}
	b, _ := svc.GetBalance("c1", "")
	if b.TotalReserved.Cmp(mustMoney(t, "100.00")) != 0 {
		t.Fatalf("total reserved = %s, must never exceed 100", b.TotalReserved)
	}
	if b.DayReserved.Cmp(mustMoney(t, "100.00")) != 0 {
		t.Fatalf("daily reserved = %s, must never exceed 100", b.DayReserved)
	}
}

func TestDailyBudgetResetsAcrossNaturalDay(t *testing.T) {
	// TTL 长于测试中的时间跳跃，避免第一笔预占被误判过期。
	svc, _, clock := newTestService(t, 48*time.Hour)
	seedCampaign(t, svc, "c1", "100", "30")

	_, err := svc.Reserve(ReserveInput{CampaignID: "c1", RequestNo: "d1", Amount: mustMoney(t, "30")})
	if err != nil {
		t.Fatal(err)
	}
	// 当日再要 1 块就失败（日预算）。
	_, err = svc.Reserve(ReserveInput{CampaignID: "c1", RequestNo: "d2", Amount: mustMoney(t, "1")})
	assertErrKind(t, err, KindBudgetExceeded)

	// 跨自然日：日预算恢复，但总预算仍被昨日预占占用。
	clock.t = time.Date(2026, 1, 16, 0, 0, 1, 0, time.UTC)
	r2, err := svc.Reserve(ReserveInput{CampaignID: "c1", RequestNo: "d3", Amount: mustMoney(t, "30")})
	if err != nil {
		t.Fatalf("next-day reserve should pass daily check: %v", err)
	}
	if r2.Day != "2026-01-16" {
		t.Fatalf("day = %s", r2.Day)
	}
	b, _ := svc.GetBalance("c1", "")
	if b.DayReserved.String() != "30" || b.TotalReserved.String() != "60" {
		t.Fatalf("cross-day balances wrong: %+v", b)
	}
}

func TestCaptureReleasesDifference(t *testing.T) {
	svc, _, _ := newTestService(t, time.Hour)
	seedCampaign(t, svc, "c1", "100", "100")
	r, _ := svc.Reserve(ReserveInput{CampaignID: "c1", RequestNo: "r1", Amount: mustMoney(t, "40")})

	fin, err := svc.Capture(CaptureInput{Token: r.Token, ReceiptNo: "rcpt-1", ActualCost: mustMoney(t, "32.10")})
	if err != nil {
		t.Fatal(err)
	}
	if fin.Status != StatusCaptured || fin.ActualCost.String() != "32.1" {
		t.Fatalf("bad capture result: %+v", fin)
	}
	b, _ := svc.GetBalance("c1", "")
	// 预占额度全部释放，实际费用计入已花费，差额回到可用额度。
	if b.TotalReserved.String() != "0" || b.TotalCaptured.String() != "32.1" || b.TotalAvailable.String() != "67.9" {
		t.Fatalf("after capture: %+v", b)
	}
	if b.DayReserved.String() != "0" || b.DayCaptured.String() != "32.1" || b.DayAvailable.String() != "67.9" {
		t.Fatalf("after capture daily: %+v", b)
	}
}

func TestCaptureZeroCostReleasesAll(t *testing.T) {
	svc, _, _ := newTestService(t, time.Hour)
	seedCampaign(t, svc, "c1", "10", "10")
	r, _ := svc.Reserve(ReserveInput{CampaignID: "c1", RequestNo: "r1", Amount: mustMoney(t, "10")})

	if _, err := svc.Capture(CaptureInput{Token: r.Token, ReceiptNo: "rcpt-0", ActualCost: MoneyZero()}); err != nil {
		t.Fatal(err)
	}
	b, _ := svc.GetBalance("c1", "")
	if !b.TotalAvailable.Equal(mustMoney(t, "10")) {
		t.Fatalf("zero-cost capture should release all, available=%s", b.TotalAvailable)
	}
}

func TestCaptureOverReservedAmountRejected(t *testing.T) {
	svc, _, _ := newTestService(t, time.Hour)
	seedCampaign(t, svc, "c1", "100", "100")
	r, _ := svc.Reserve(ReserveInput{CampaignID: "c1", RequestNo: "r1", Amount: mustMoney(t, "10")})

	_, err := svc.Capture(CaptureInput{Token: r.Token, ReceiptNo: "rcpt-1", ActualCost: mustMoney(t, "10.01")})
	assertErrKind(t, err, KindInvalidArgument)
	_, err = svc.Capture(CaptureInput{Token: r.Token, ReceiptNo: "rcpt-1", ActualCost: mustMoney(t, "-0.01")})
	assertErrKind(t, err, KindInvalidArgument)

	// 被拒绝的核销不得改变额度与状态。
	got, _ := svc.GetReservation(r.Token)
	if got.Status != StatusReserved {
		t.Fatalf("status = %s", got.Status)
	}
}

func TestReceiptDedupAndOutOfOrder(t *testing.T) {
	svc, _, _ := newTestService(t, time.Hour)
	seedCampaign(t, svc, "c1", "100", "100")
	r, _ := svc.Reserve(ReserveInput{CampaignID: "c1", RequestNo: "r1", Amount: mustMoney(t, "10")})

	in := CaptureInput{Token: r.Token, ReceiptNo: "rcpt-1", ActualCost: mustMoney(t, "7")}
	fin1, err := svc.Capture(in)
	if err != nil {
		t.Fatal(err)
	}
	// 完全相同的重复回执（可能乱序重投）：返回原结果，不重复入账。
	fin2, err := svc.Capture(in)
	if err != nil {
		t.Fatalf("duplicate receipt: %v", err)
	}
	if fin1.FinalizedAt != fin2.FinalizedAt || fin2.ActualCost.String() != "7" {
		t.Fatalf("duplicate receipt mismatch: %+v vs %+v", fin1, fin2)
	}
	b, _ := svc.GetBalance("c1", "")
	if b.TotalCaptured.String() != "7" {
		t.Fatalf("duplicate receipt double-captured: %s", b.TotalCaptured)
	}

	// 同回执号但内容变了 -> 幂等冲突。
	_, err = svc.Capture(CaptureInput{Token: r.Token, ReceiptNo: "rcpt-1", ActualCost: mustMoney(t, "8")})
	assertErrKind(t, err, KindIdempotencyConflict)
	// 同一回执号拿去核销另一张凭证 -> 幂等冲突。
	r2, _ := svc.Reserve(ReserveInput{CampaignID: "c1", RequestNo: "r2", Amount: mustMoney(t, "5")})
	_, err = svc.Capture(CaptureInput{Token: r2.Token, ReceiptNo: "rcpt-1", ActualCost: mustMoney(t, "7")})
	assertErrKind(t, err, KindIdempotencyConflict)
	// r2 未被错误核销。
	got, _ := svc.GetReservation(r2.Token)
	if got.Status != StatusReserved {
		t.Fatalf("r2 status = %s, want reserved", got.Status)
	}
}

func TestCaptureAfterCaptureIsStateConflict(t *testing.T) {
	svc, _, _ := newTestService(t, time.Hour)
	seedCampaign(t, svc, "c1", "100", "100")
	r, _ := svc.Reserve(ReserveInput{CampaignID: "c1", RequestNo: "r1", Amount: mustMoney(t, "10")})
	if _, err := svc.Capture(CaptureInput{Token: r.Token, ReceiptNo: "a", ActualCost: mustMoney(t, "9")}); err != nil {
		t.Fatal(err)
	}
	// 已核销凭证收到另一个回执 -> 明确的状态冲突。
	_, err := svc.Capture(CaptureInput{Token: r.Token, ReceiptNo: "b", ActualCost: mustMoney(t, "1")})
	assertErrKind(t, err, KindStateConflict)
}

func TestCancelReleasesAllAndIsIdempotent(t *testing.T) {
	svc, _, _ := newTestService(t, time.Hour)
	seedCampaign(t, svc, "c1", "100", "100")
	r, _ := svc.Reserve(ReserveInput{CampaignID: "c1", RequestNo: "r1", Amount: mustMoney(t, "40")})

	c1, err := svc.Cancel(r.Token)
	if err != nil || c1.Status != StatusCancelled {
		t.Fatalf("cancel: %v %+v", err, c1)
	}
	c2, err := svc.Cancel(r.Token) // 重复取消，幂等
	if err != nil || c2.Status != StatusCancelled {
		t.Fatalf("duplicate cancel: %v %+v", err, c2)
	}
	b, _ := svc.GetBalance("c1", "")
	if !b.TotalReserved.IsZero() || !b.TotalAvailable.Equal(mustMoney(t, "100")) {
		t.Fatalf("cancel did not release all: %+v", b)
	}
}

func TestLateCancelAfterCaptureConflict(t *testing.T) {
	svc, _, _ := newTestService(t, time.Hour)
	seedCampaign(t, svc, "c1", "100", "100")
	r, _ := svc.Reserve(ReserveInput{CampaignID: "c1", RequestNo: "r1", Amount: mustMoney(t, "10")})
	if _, err := svc.Capture(CaptureInput{Token: r.Token, ReceiptNo: "a", ActualCost: mustMoney(t, "6")}); err != nil {
		t.Fatal(err)
	}
	// 迟到取消不能冲销已核销费用。
	_, err := svc.Cancel(r.Token)
	assertErrKind(t, err, KindStateConflict)
	b, _ := svc.GetBalance("c1", "")
	if b.TotalCaptured.String() != "6" {
		t.Fatalf("captured cost was reversed: %s", b.TotalCaptured)
	}
}

func TestExpiryReleasesAll(t *testing.T) {
	svc, _, clock := newTestService(t, time.Minute)
	seedCampaign(t, svc, "c1", "100", "30")
	r, _ := svc.Reserve(ReserveInput{CampaignID: "c1", RequestNo: "r1", Amount: mustMoney(t, "30")})

	clock.advance(61 * time.Second)
	if n := svc.SweepExpired(); n != 1 {
		t.Fatalf("swept %d, want 1", n)
	}
	got, _ := svc.GetReservation(r.Token)
	if got.Status != StatusExpired {
		t.Fatalf("status = %s", got.Status)
	}
	b, _ := svc.GetBalance("c1", "")
	if !b.TotalReserved.IsZero() || !b.DayReserved.IsZero() {
		t.Fatalf("expiry did not release: %+v", b)
	}
}

func TestExpiredReservationReceiptIsStateConflict(t *testing.T) {
	svc, _, clock := newTestService(t, time.Minute)
	seedCampaign(t, svc, "c1", "100", "100")
	r, _ := svc.Reserve(ReserveInput{CampaignID: "c1", RequestNo: "r1", Amount: mustMoney(t, "10")})

	clock.advance(61 * time.Second)
	// 不显式 Sweep，回执到达时惰性过期并明确返回状态冲突。
	_, err := svc.Capture(CaptureInput{Token: r.Token, ReceiptNo: "late", ActualCost: mustMoney(t, "10")})
	assertErrKind(t, err, KindStateConflict)
	got, _ := svc.GetReservation(r.Token)
	if got.Status != StatusExpired {
		t.Fatalf("status = %s, want expired", got.Status)
	}

	// 过期后取消同样冲突。
	_, err = svc.Cancel(r.Token)
	assertErrKind(t, err, KindStateConflict)
}

func TestCaptureCancelRaceSingleTerminalState(t *testing.T) {
	// 凭证仍在有效期：核销与取消高并发竞争，每张凭证只能落入一个终态，
	// 额度只能释放/转化一次。
	svc, _, _ := newTestService(t, time.Hour)
	seedCampaign(t, svc, "c1", "100000", "100000")

	const m = 100
	tokens := make([]string, m)
	for i := 0; i < m; i++ {
		r, err := svc.Reserve(ReserveInput{
			CampaignID: "c1", RequestNo: "r" + itoa(i), Amount: mustMoney(t, "1"),
		})
		if err != nil {
			t.Fatal(err)
		}
		tokens[i] = r.Token
	}

	var wg sync.WaitGroup
	for _, tk := range tokens {
		tk := tk
		wg.Add(2)
		go func() {
			defer wg.Done()
			svc.Capture(CaptureInput{Token: tk, ReceiptNo: "rcpt-" + tk, ActualCost: mustMoney(t, "1")})
		}()
		go func() { defer wg.Done(); svc.Cancel(tk) }()
	}
	wg.Wait()

	captured, other := 0, 0
	for _, tk := range tokens {
		got, _ := svc.GetReservation(tk)
		switch got.Status {
		case StatusCaptured:
			captured++
		case StatusCancelled:
			other++
		default:
			t.Fatalf("token %s non-terminal: %s", tk, got.Status)
		}
	}
	if captured+other != m {
		t.Fatalf("terminal counts %d+%d != %d", captured, other, m)
	}
	b, _ := svc.GetBalance("c1", "")
	if !b.TotalReserved.IsZero() {
		t.Fatalf("race left held budget: %s", b.TotalReserved)
	}
	if b.TotalCaptured.Cmp(mustMoney(t, itoa(captured))) != 0 {
		t.Fatalf("captured total %s != winning captures %d", b.TotalCaptured, captured)
	}
	wantAvail := mustMoney(t, "100000").Sub(mustMoney(t, itoa(captured)))
	if !b.TotalAvailable.Equal(wantAvail) || !b.DayAvailable.Equal(wantAvail) {
		t.Fatalf("available %s/%s, want %s", b.TotalAvailable, b.DayAvailable, wantAvail)
	}
}

func TestExpiryRacesWithCaptureAndCancel(t *testing.T) {
	// 已过失效时间：过期扫描、迟到回执、迟到取消竞争。
	// 语义上过期处理胜出，全部凭证必须是 expired，且没有任何费用被核销。
	svc, _, clock := newTestService(t, 50*time.Millisecond)
	seedCampaign(t, svc, "c1", "100000", "100000")

	const m = 100
	tokens := make([]string, m)
	for i := 0; i < m; i++ {
		r, err := svc.Reserve(ReserveInput{
			CampaignID: "c1", RequestNo: "r" + itoa(i), Amount: mustMoney(t, "1"),
		})
		if err != nil {
			t.Fatal(err)
		}
		tokens[i] = r.Token
	}
	clock.advance(60 * time.Millisecond)

	var wg sync.WaitGroup
	for _, tk := range tokens {
		tk := tk
		wg.Add(3)
		go func() { defer wg.Done(); svc.SweepExpired() }()
		go func() {
			defer wg.Done()
			_, err := svc.Capture(CaptureInput{Token: tk, ReceiptNo: "rcpt-" + tk, ActualCost: mustMoney(t, "1")})
			be, ok := AsError(err)
			if !ok || be.Kind != KindStateConflict {
				t.Errorf("expired capture on %s: want state_conflict, got %v", tk, err)
			}
		}()
		go func() {
			defer wg.Done()
			_, err := svc.Cancel(tk)
			be, ok := AsError(err)
			if !ok || be.Kind != KindStateConflict {
				t.Errorf("expired cancel on %s: want state_conflict, got %v", tk, err)
			}
		}()
	}
	wg.Wait()

	for _, tk := range tokens {
		got, _ := svc.GetReservation(tk)
		if got.Status != StatusExpired {
			t.Fatalf("token %s = %s, want expired", tk, got.Status)
		}
	}
	b, _ := svc.GetBalance("c1", "")
	if !b.TotalReserved.IsZero() || !b.TotalCaptured.IsZero() {
		t.Fatalf("expiry race ledger wrong: reserved=%s captured=%s", b.TotalReserved, b.TotalCaptured)
	}
	if !b.TotalAvailable.Equal(mustMoney(t, "100000")) {
		t.Fatalf("available = %s, want 100000", b.TotalAvailable)
	}
}

func TestStoreAppendFailureLeavesNoReservation(t *testing.T) {
	store := &memStore{}
	clock := &mockClock{t: time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC)}
	svc, err := NewService(store, clock, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	seedCampaign(t, svc, "c1", "100", "100")

	store.fail = true
	_, err = svc.Reserve(ReserveInput{CampaignID: "c1", RequestNo: "r1", Amount: mustMoney(t, "10")})
	if err == nil || !strings.Contains(err.Error(), "injected") {
		t.Fatalf("want injected error, got %v", err)
	}
	store.fail = false

	// 落盘失败后服务仍一致：额度未被占用，请求号未被登记。
	r2, err := svc.Reserve(ReserveInput{CampaignID: "c1", RequestNo: "r1", Amount: mustMoney(t, "10")})
	if err != nil {
		t.Fatalf("reserve after failed append: %v", err)
	}
	b, _ := svc.GetBalance("c1", "")
	if b.TotalReserved.String() != "10" {
		t.Fatalf("reserved = %s, want 10", b.TotalReserved)
	}
	_ = r2
}

func TestStateReconstructsFromEventLog(t *testing.T) {
	store := &memStore{}
	clock := &mockClock{t: time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC)}
	svc, err := NewService(store, clock, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	seedCampaign(t, svc, "c1", "100", "50")
	r1, _ := svc.Reserve(ReserveInput{CampaignID: "c1", RequestNo: "r1", Amount: mustMoney(t, "40")})
	svc.Capture(CaptureInput{Token: r1.Token, ReceiptNo: "rc1", ActualCost: mustMoney(t, "30")})
	r2, _ := svc.Reserve(ReserveInput{CampaignID: "c1", RequestNo: "r2", Amount: mustMoney(t, "20")})
	svc.Cancel(r2.Token)

	// 全新服务实例重放同一事件流，状态（含台账）必须一致。
	svc2, err := NewService(store, clock, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	b1, _ := svc.GetBalance("c1", "")
	b2, _ := svc2.GetBalance("c1", "")
	if b1.TotalReserved.Cmp(b2.TotalReserved) != 0 ||
		b1.TotalCaptured.Cmp(b2.TotalCaptured) != 0 ||
		b1.TotalAvailable.Cmp(b2.TotalAvailable) != 0 {
		t.Fatalf("replayed state differs:\n%+v\n%+v", b1, b2)
	}
	// 幂等索引也要重建：同号同内容返回原 token。
	replay, err := svc2.Reserve(ReserveInput{CampaignID: "c1", RequestNo: "r1", Amount: mustMoney(t, "40")})
	if err != nil || replay.Token != r1.Token {
		t.Fatalf("idempotency index not rebuilt: %+v %v", replay, err)
	}
	// 回执去重索引……（回执记录由终态事件重建）。
	again, err := svc2.Capture(CaptureInput{Token: r1.Token, ReceiptNo: "rc1", ActualCost: mustMoney(t, "30")})
	if err != nil || again.Status != StatusCaptured {
		t.Fatalf("receipt dedup not rebuilt: %+v %v", again, err)
	}
}

func TestMissingTokenAndCancelUnknown(t *testing.T) {
	svc, _, _ := newTestService(t, time.Minute)
	if _, err := svc.GetReservation("nope"); err == nil {
		t.Fatal("want not found")
	} else {
		assertErrKind(t, err, KindNotFound)
	}
	if _, err := svc.Cancel("nope"); err == nil {
		t.Fatal("want not found")
	} else {
		assertErrKind(t, err, KindNotFound)
	}
	if _, err := svc.Capture(CaptureInput{Token: "nope", ReceiptNo: "x"}); err == nil {
		t.Fatal("want not found")
	} else {
		assertErrKind(t, err, KindNotFound)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}
