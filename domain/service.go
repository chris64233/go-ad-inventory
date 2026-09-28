package domain

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/chris64233/go-ad-inventory/clock"
	"github.com/chris64233/go-ad-inventory/money"
	"github.com/chris64233/go-ad-inventory/store"
)

// captureRecord 记录一笔已核销回执，用于按回执号去重。
type captureRecord struct {
	ReceiptID     string
	ReservationID string
	Amount        money.Money
	At            time.Time
}

// adjustmentRecord 是预算调整号的幂等记录内容。
type adjustmentRecord struct {
	CampaignID      string
	ExpectedVersion int64
	TotalBudget     money.Money
	DailyCap        money.Money
	SetTotal        bool
	SetDaily        bool
}

// Service 是预算领域服务。所有写操作在单互斥锁内完成
// “校验 → 事件落盘 → 更新内存投影”，因此并发下三级预算（总预算/日预算/节奏）
// 都不会被突破，且核销/取消/过期三者竞争时只会落入一个终态。
type Service struct {
	mu    sync.Mutex
	clock clock.Clock
	store store.Store

	campaigns map[string]*campaignState
	// 预占幂等索引：campaignID -> requestID -> reservationID。
	byRequest map[string]map[string]string
	// 回执去重索引：receiptID -> 核销记录。
	byReceipt map[string]captureRecord
	// 预算调整幂等索引：adjustmentID -> 调整结果。
	byAdjustment map[string]BudgetAdjustment
	reservations map[string]*Reservation
}

// NewService 从事件存储重放全部历史，恢复内存投影。
func NewService(clk clock.Clock, st store.Store) (*Service, error) {
	if clk == nil {
		clk = clock.System{}
	}
	s := &Service{
		clock:        clk,
		store:        st,
		campaigns:    make(map[string]*campaignState),
		byRequest:    make(map[string]map[string]string),
		byReceipt:    make(map[string]captureRecord),
		byAdjustment: make(map[string]BudgetAdjustment),
		reservations: make(map[string]*Reservation),
	}
	events, err := st.Events(context.Background())
	if err != nil {
		return nil, fmt.Errorf("domain: load events: %w", err)
	}
	for _, ev := range events {
		if err := s.apply(storeEventToRaw(ev)); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// newID 生成随机十六进制 ID。
func newID(prefix string) string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return prefix + "_" + hex.EncodeToString(b[:])
}

// ---------- 活动配置 ----------

// CreateCampaignParams 是创建活动的入参。
type CreateCampaignParams struct {
	Name        string
	TotalBudget money.Money
	DailyCap    money.Money
	Timezone    string        // IANA 时区名，空则使用本地时区
	DefaultTTL  time.Duration // 凭证默认有效期，必须为正
	// Curve 为可选的每日投放曲线；nil 表示不做时段节奏限制。
	Curve *PacingCurve
}

// CreateCampaign 创建活动并持久化配置事件。初始配置版本为 1。
func (s *Service) CreateCampaign(ctx context.Context, p CreateCampaignParams) (*Campaign, error) {
	const op = "CreateCampaign"
	if p.Name == "" {
		return nil, eInvalid(op, "name is required")
	}
	if !p.TotalBudget.IsPositive() {
		return nil, eInvalid(op, "total budget must be positive")
	}
	if p.DailyCap.IsNegative() || p.DailyCap.IsZero() {
		return nil, eInvalid(op, "daily cap must be positive")
	}
	if p.DailyCap.Currency() != p.TotalBudget.Currency() {
		return nil, eInvalid(op, "daily cap currency %q differs from total budget currency %q",
			p.DailyCap.Currency(), p.TotalBudget.Currency())
	}
	if p.DefaultTTL <= 0 {
		return nil, eInvalid(op, "default TTL must be positive")
	}
	var curve *PacingCurve
	if p.Curve != nil {
		if err := p.Curve.Validate(); err != nil {
			return nil, eInvalid(op, "%s", err)
		}
		cp := *p.Curve
		curve = &cp
	}
	loc := time.Local
	if p.Timezone != "" {
		l, err := time.LoadLocation(p.Timezone)
		if err != nil {
			return nil, eInvalid(op, "invalid timezone %q", p.Timezone)
		}
		loc = l
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.clock.Now()
	id := newID("cmp")
	ev, err := marshalEvent(EvCampaignCreated, now, CampaignCreatedData{
		CampaignID:    id,
		Name:          p.Name,
		TotalBudget:   toEvt(p.TotalBudget),
		DailyCap:      toEvt(p.DailyCap),
		Timezone:      loc.String(),
		DefaultTTL:    p.DefaultTTL,
		ConfigVersion: InitialConfigVersion,
		Curve:         curve,
		CreatedAt:     now,
	})
	if err != nil {
		return nil, err
	}
	if err := s.append(ctx, ev); err != nil {
		return nil, err
	}
	return s.campaigns[id].campaign, nil
}

// GetCampaign 返回活动配置（含当前配置版本与投放曲线）。
func (s *Service) GetCampaign(_ context.Context, id string) (*Campaign, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cs, ok := s.campaigns[id]
	if !ok {
		return nil, eNotFound("GetCampaign", "campaign %q not found", id)
	}
	return cs.campaign, nil
}

// ---------- 配置切换（时区 / 投放曲线） ----------

// UpdateConfigParams 切换活动时区与/或投放曲线。
//
// 期望版本用于乐观并发：必须等于活动当前 ConfigVersion，否则整体拒绝。
// Timezone 为 nil 表示时区不变；非 nil 表示切换（空字符串表示运行环境本地时区）。
// SetCurve 为 false 表示曲线不变；为 true 时整体替换曲线，Curve 为 nil 表示移除曲线。
type UpdateConfigParams struct {
	CampaignID      string
	ExpectedVersion int64
	Timezone        *string
	SetCurve        bool
	Curve           *PacingCurve
}

// UpdateConfig 切换时区与/或投放曲线，成功后配置版本递增。
//
// 切换只影响之后新创建的预占：历史凭证在创建时已经冻结日期、时段与配置版本，
// 迟到回执仍按冻结坐标归还额度，不会被新时区或新曲线重新归类。
func (s *Service) UpdateConfig(ctx context.Context, p UpdateConfigParams) (*Campaign, error) {
	const op = "UpdateConfig"
	if p.CampaignID == "" {
		return nil, eInvalid(op, "campaign id is required")
	}
	if p.ExpectedVersion <= 0 {
		return nil, eInvalid(op, "expected config version is required")
	}
	if p.Timezone == nil && !p.SetCurve {
		return nil, eInvalid(op, "nothing to update: timezone and curve both unchanged")
	}
	var newCurve *PacingCurve
	if p.SetCurve && p.Curve != nil {
		if err := p.Curve.Validate(); err != nil {
			return nil, eInvalid(op, "%s", err)
		}
		cp := *p.Curve
		newCurve = &cp
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	cs, ok := s.campaigns[p.CampaignID]
	if !ok {
		return nil, eNotFound(op, "campaign %q not found", p.CampaignID)
	}
	c := cs.campaign
	if c.ConfigVersion != p.ExpectedVersion {
		return nil, eVersion(op, p.ExpectedVersion, c.ConfigVersion)
	}

	tzName := c.Location.String()
	if p.Timezone != nil {
		loc := time.Local
		if *p.Timezone != "" {
			l, err := time.LoadLocation(*p.Timezone)
			if err != nil {
				return nil, eInvalid(op, "invalid timezone %q", *p.Timezone)
			}
			loc = l
		}
		tzName = loc.String()
	}
	// 事件中的曲线：不切换时沿用当前曲线；切换时为新曲线（可能为 nil=移除）。
	eventCurve := c.Curve
	if p.SetCurve {
		eventCurve = newCurve
	}
	newVersion := c.ConfigVersion + 1
	now := s.clock.Now()
	ev, err := marshalEvent(EvConfigUpdated, now, ConfigUpdatedData{
		CampaignID:    c.ID,
		ConfigVersion: newVersion,
		Timezone:      tzName,
		Curve:         eventCurve,
		At:            now,
	})
	if err != nil {
		return nil, err
	}
	// 投影（时区、曲线、版本号）只在事件落盘成功后的 apply 中推进。
	if err := s.append(ctx, ev); err != nil {
		return nil, err
	}
	return c, nil
}

// ---------- 预算调整 ----------

// AdjustBudgetParams 是预算调整入参。
//
// SetTotal/SetDaily 区分“不修改”与“显式设置”；至少设置一项。
// ExpectedVersion 必须等于当前配置版本；AdjustmentID 为调用方幂等号。
type AdjustBudgetParams struct {
	CampaignID      string
	AdjustmentID    string
	ExpectedVersion int64
	TotalBudget     money.Money
	DailyCap        money.Money
	SetTotal        bool
	SetDaily        bool
}

// BudgetAdjustment 是一次成功预算调整的结果。
type BudgetAdjustment struct {
	AdjustmentID  string      `json:"adjustment_id"`
	CampaignID    string      `json:"campaign_id"`
	ConfigVersion int64       `json:"config_version"`
	TotalBudget   money.Money `json:"total_budget"`
	DailyCap      money.Money `json:"daily_cap"`
	At            time.Time   `json:"at"`
}

// AdjustBudget 调整总预算与/或日上限，成功后配置版本递增。
//
// 提高预算立即生效；降低预算：
//   - 总预算不得小于“已核销 + 有效预占”，否则以 budget_exceeded(level=total_floor) 整体拒绝；
//   - 日上限对每一个仍存在有效预占的自然日，不得小于该日“已核销 + 有效预占”，
//     否则以 budget_exceeded(level=daily_floor) 整体拒绝。
//
// 拒绝时不会取消或改动任何凭证。同一 AdjustmentID 重放且内容一致 → 幂等返回原结果；
// 内容变化 → idempotency_conflict。
func (s *Service) AdjustBudget(ctx context.Context, p AdjustBudgetParams) (*BudgetAdjustment, error) {
	const op = "AdjustBudget"
	if p.CampaignID == "" {
		return nil, eInvalid(op, "campaign id is required")
	}
	if p.AdjustmentID == "" {
		return nil, eInvalid(op, "adjustment id is required")
	}
	if p.ExpectedVersion <= 0 {
		return nil, eInvalid(op, "expected config version is required")
	}
	if !p.SetTotal && !p.SetDaily {
		return nil, eInvalid(op, "nothing to adjust: set at least one of total budget or daily cap")
	}
	if p.SetTotal && !p.TotalBudget.IsPositive() {
		return nil, eInvalid(op, "total budget must be positive")
	}
	if p.SetDaily && !p.DailyCap.IsPositive() {
		return nil, eInvalid(op, "daily cap must be positive")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// 幂等检查优先：重放不看当前版本，直接返回首次结果。
	if prev, ok := s.byAdjustment[p.AdjustmentID]; ok {
		if s.adjustmentMatches(p, prev) {
			return &prev, nil
		}
		return nil, eIdemConflict(op,
			"adjustment %q already applied with a different payload", p.AdjustmentID)
	}

	cs, ok := s.campaigns[p.CampaignID]
	if !ok {
		return nil, eNotFound(op, "campaign %q not found", p.CampaignID)
	}
	c := cs.campaign
	if p.SetTotal && p.TotalBudget.Currency() != c.TotalBudget.Currency() {
		return nil, eInvalid(op, "total budget currency %q differs from campaign currency %q",
			p.TotalBudget.Currency(), c.TotalBudget.Currency())
	}
	if p.SetDaily && p.DailyCap.Currency() != c.TotalBudget.Currency() {
		return nil, eInvalid(op, "daily cap currency %q differs from campaign currency %q",
			p.DailyCap.Currency(), c.TotalBudget.Currency())
	}
	if c.ConfigVersion != p.ExpectedVersion {
		return nil, eVersion(op, p.ExpectedVersion, c.ConfigVersion)
	}

	newTotal := c.TotalBudget
	if p.SetTotal {
		newTotal = p.TotalBudget
	}
	newDaily := c.DailyCap
	if p.SetDaily {
		newDaily = p.DailyCap
	}

	// 降低总预算的下限：已核销 + 有效预占。不足则整体拒绝，绝不静默取消凭证。
	committedTotal := cs.totalSpent.Add(cs.totalReserved)
	if newTotal.Cmp(committedTotal) < 0 {
		return nil, &Error{
			Code:      CodeBudgetExceeded,
			Op:        op,
			Level:     "total_floor",
			Requested: newTotal,
			Available: committedTotal,
			Message: fmt.Sprintf("cannot reduce total budget below committed spend: requested %s, floor is %s",
				newTotal.String(), committedTotal.String()),
		}
	}
	// 降低日上限的下限：只约束仍有有效预占的自然日（历史日期不再参与未来投放）。
	if p.SetDaily {
		for dayKey, d := range cs.days {
			if d.reserved.IsZero() {
				continue
			}
			committedDay := d.spent.Add(d.reserved)
			if newDaily.Cmp(committedDay) < 0 {
				return nil, &Error{
					Code:      CodeBudgetExceeded,
					Op:        op,
					Level:     "daily_floor",
					Requested: newDaily,
					Available: committedDay,
					Message: fmt.Sprintf("cannot reduce daily cap below committed spend on %s: requested %s, floor is %s",
						dayKey, newDaily.String(), committedDay.String()),
				}
			}
		}
	}

	now := s.clock.Now()
	adj := BudgetAdjustment{
		AdjustmentID:  p.AdjustmentID,
		CampaignID:    c.ID,
		ConfigVersion: c.ConfigVersion + 1,
		TotalBudget:   newTotal,
		DailyCap:      newDaily,
		At:            now,
	}
	ev, err := marshalEvent(EvBudgetAdjusted, now, BudgetAdjustedData{
		CampaignID:    adj.CampaignID,
		AdjustmentID:  adj.AdjustmentID,
		ConfigVersion: adj.ConfigVersion,
		TotalBudget:   toEvt(adj.TotalBudget),
		DailyCap:      toEvt(adj.DailyCap),
		At:            now,
	})
	if err != nil {
		return nil, err
	}
	if err := s.append(ctx, ev); err != nil {
		return nil, err
	}
	return &adj, nil
}

// adjustmentMatches 判断幂等重放入参与首次调整是否等价。
func (s *Service) adjustmentMatches(p AdjustBudgetParams, prev BudgetAdjustment) bool {
	if p.CampaignID != prev.CampaignID {
		return false
	}
	if p.SetTotal && p.TotalBudget.Cmp(prev.TotalBudget) != 0 {
		return false
	}
	if p.SetDaily && p.DailyCap.Cmp(prev.DailyCap) != 0 {
		return false
	}
	return true
}

// GetBudgetAdjustments 返回活动按时间顺序的全部预算调整记录（含版本号）。
func (s *Service) GetBudgetAdjustments(_ context.Context, campaignID string) ([]BudgetAdjustment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cs, ok := s.campaigns[campaignID]
	if !ok {
		return nil, eNotFound("GetBudgetAdjustments", "campaign %q not found", campaignID)
	}
	out := make([]BudgetAdjustment, len(cs.adjustments))
	copy(out, cs.adjustments)
	return out, nil
}

// ---------- 预占 ----------

// ReserveParams 是一次投放请求的预占入参。
type ReserveParams struct {
	CampaignID string
	RequestID  string        // 外部请求号，幂等依据
	Amount     money.Money   // 预占金额，必须为正
	TTL        time.Duration // 可选；<=0 时使用活动默认有效期
}

// Reserve 同时占住总预算、日预算与当前时段节奏额度三级，成功返回带失效时间的凭证。
//
// 凭证在创建时刻按活动当时的时区冻结 DayKey、小时桶 Slot 与 ConfigVersion。
// 幂等：同一 (campaign, requestID) 且金额相同 → 返回原凭证；
// 金额不同 → 幂等冲突。任一级额度不足 → 整体失败，不落任何事件。
func (s *Service) Reserve(ctx context.Context, p ReserveParams) (*Reservation, error) {
	const op = "Reserve"
	if p.CampaignID == "" {
		return nil, eInvalid(op, "campaign id is required")
	}
	if p.RequestID == "" {
		return nil, eInvalid(op, "request id is required")
	}
	if !p.Amount.IsPositive() {
		return nil, eInvalid(op, "amount must be positive")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	cs, ok := s.campaigns[p.CampaignID]
	if !ok {
		return nil, eNotFound(op, "campaign %q not found", p.CampaignID)
	}
	c := cs.campaign
	if p.Amount.Currency() != c.TotalBudget.Currency() {
		return nil, eInvalid(op, "amount currency %q differs from campaign currency %q",
			p.Amount.Currency(), c.TotalBudget.Currency())
	}

	// 幂等检查：同请求号同内容返回原凭证，内容变化报冲突。
	if rid, ok := s.byRequest[p.CampaignID][p.RequestID]; ok {
		orig := s.reservations[rid]
		if orig.Amount.Cmp(p.Amount) == 0 {
			return orig, nil
		}
		return nil, eIdemConflict(op,
			"request %q already reserved with amount %s, cannot reuse with %s",
			p.RequestID, orig.Amount.String(), p.Amount.String())
	}

	now := s.clock.Now()
	// 先惰性过期，再判断余额，保证过期额度及时归还。
	s.expireLocked(now)

	localNow := now.In(c.Location)
	dayKey := localNow.Format("2006-01-02")
	slot := localNow.Hour()
	// 三级额度必须同时满足，否则整体失败。
	if avail := cs.totalAvailable(); p.Amount.Cmp(avail) > 0 {
		return nil, eBudget(op, "total", p.Amount, avail)
	}
	if avail := cs.dailyAvailable(dayKey); p.Amount.Cmp(avail) > 0 {
		return nil, eBudget(op, "daily", p.Amount, avail)
	}
	if c.Curve != nil {
		if avail := cs.pacingAvailable(dayKey, slot); p.Amount.Cmp(avail) > 0 {
			return nil, eBudget(op, "pacing", p.Amount, avail)
		}
	}

	ttl := p.TTL
	if ttl <= 0 {
		ttl = c.DefaultTTL
	}
	expiresAt := now.Add(ttl)

	r := &Reservation{
		ID:            newID("rsv"),
		CampaignID:    p.CampaignID,
		RequestID:     p.RequestID,
		Amount:        p.Amount,
		DayKey:        dayKey,
		Slot:          slot,
		ConfigVersion: c.ConfigVersion,
		CreatedAt:     now,
		ExpiresAt:     expiresAt,
		Status:        StatusReserved,
		Captured:      money.Zero(p.Amount.Currency()),
	}
	ev, err := marshalEvent(EvReserved, now, ReservedData{
		ReservationID: r.ID,
		CampaignID:    r.CampaignID,
		RequestID:     r.RequestID,
		Amount:        toEvt(r.Amount),
		DayKey:        r.DayKey,
		Slot:          r.Slot,
		ConfigVersion: r.ConfigVersion,
		CreatedAt:     r.CreatedAt,
		ExpiresAt:     r.ExpiresAt,
	})
	if err != nil {
		return nil, err
	}
	if err := s.append(ctx, ev); err != nil {
		return nil, err
	}
	return s.reservations[r.ID], nil
}

// ---------- 核销 ----------

// CaptureParams 是曝光回执核销的入参。
type CaptureParams struct {
	ReservationID string
	ReceiptID     string      // 回执标识，去重依据
	Amount        money.Money // 实际费用，必须为正且不超过预占额
}

// Capture 按实际费用核销：核销额计入已花费，差额释放回三级预算与冻结时段桶。
//
// 费用归属永远使用凭证创建时冻结的 DayKey/Slot/ConfigVersion；
// 即使活动已经切换时区或曲线、即使回执迟到跨天，也不会被重新归类。
//
// 回执去重：同一 receiptID 且内容一致 → 返回原核销结果；
// 同一 receiptID 内容不同 → 幂等冲突。凭证已取消/已过期 → 状态冲突。
func (s *Service) Capture(ctx context.Context, p CaptureParams) (*Reservation, error) {
	const op = "Capture"
	if p.ReservationID == "" {
		return nil, eInvalid(op, "reservation id is required")
	}
	if p.ReceiptID == "" {
		return nil, eInvalid(op, "receipt id is required")
	}
	if !p.Amount.IsPositive() {
		return nil, eInvalid(op, "capture amount must be positive")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.clock.Now()
	s.expireLocked(now)

	// 回执去重优先于凭证状态检查：重复回执无论凭证现状如何都返回原结果。
	if rec, ok := s.byReceipt[p.ReceiptID]; ok {
		if rec.ReservationID == p.ReservationID && rec.Amount.Cmp(p.Amount) == 0 {
			return s.reservations[rec.ReservationID], nil
		}
		return nil, eIdemConflict(op,
			"receipt %q already captured reservation %q with amount %s",
			p.ReceiptID, rec.ReservationID, rec.Amount.String())
	}

	r, ok := s.reservations[p.ReservationID]
	if !ok {
		return nil, eNotFound(op, "reservation %q not found", p.ReservationID)
	}
	if p.Amount.Currency() != r.Amount.Currency() {
		return nil, eInvalid(op, "capture currency %q differs from reservation currency %q",
			p.Amount.Currency(), r.Amount.Currency())
	}
	switch r.Status {
	case StatusCaptured:
		// 不同回执号打到已核销凭证：状态冲突。
		return nil, eConflict(op, "reservation %q already captured (receipt %q)",
			r.ID, r.ReceiptID)
	case StatusCancelled:
		return nil, eConflict(op, "reservation %q already cancelled", r.ID)
	case StatusExpired:
		return nil, eConflict(op, "reservation %q already expired", r.ID)
	}
	if p.Amount.Cmp(r.Amount) > 0 {
		return nil, eInvalid(op, "capture amount %s exceeds reserved %s",
			p.Amount.String(), r.Amount.String())
	}

	released := r.Amount.Sub(p.Amount)
	ev, err := marshalEvent(EvCaptured, now, CapturedData{
		ReservationID: r.ID,
		CampaignID:    r.CampaignID,
		ReceiptID:     p.ReceiptID,
		Captured:      toEvt(p.Amount),
		Released:      toEvt(released),
		DayKey:        r.DayKey, // 冻结坐标，不随当前时间或新配置变化
		Slot:          r.Slot,
		ConfigVersion: r.ConfigVersion,
		At:            now,
	})
	if err != nil {
		return nil, err
	}
	if err := s.append(ctx, ev); err != nil {
		return nil, err
	}
	return s.reservations[r.ID], nil
}

// ---------- 取消 ----------

// Cancel 主动取消预占并释放全部额度（归还到创建时冻结的日期与时段桶）。
//
// 已取消 → 幂等返回；已核销/已过期 → 状态冲突（迟到取消不能冲销已核销费用）。
func (s *Service) Cancel(ctx context.Context, reservationID string) (*Reservation, error) {
	const op = "Cancel"
	if reservationID == "" {
		return nil, eInvalid(op, "reservation id is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.clock.Now()
	s.expireLocked(now)

	r, ok := s.reservations[reservationID]
	if !ok {
		return nil, eNotFound(op, "reservation %q not found", reservationID)
	}
	switch r.Status {
	case StatusCancelled:
		return r, nil // 幂等
	case StatusCaptured:
		return nil, eConflict(op,
			"reservation %q already captured; late cancel cannot reverse captured spend", r.ID)
	case StatusExpired:
		return nil, eConflict(op, "reservation %q already expired", r.ID)
	}

	ev, err := marshalEvent(EvCancelled, now, s.releaseData(r, now))
	if err != nil {
		return nil, err
	}
	if err := s.append(ctx, ev); err != nil {
		return nil, err
	}
	return s.reservations[r.ID], nil
}

// ---------- 过期 ----------

// ExpireSweep 主动扫描并过期所有到期的 reserved 凭证，返回过期数量。
// 常规操作前也会执行惰性过期；本方法供后台定时任务调用。
func (s *Service) ExpireSweep(ctx context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.expireLocked(s.clock.Now()), nil
}

// releaseData 构造取消/过期事件负载，坐标全部取自冻结的凭证。
func (s *Service) releaseData(r *Reservation, at time.Time) releaseData {
	return releaseData{
		ReservationID: r.ID,
		CampaignID:    r.CampaignID,
		Released:      toEvt(r.Amount),
		DayKey:        r.DayKey,
		Slot:          r.Slot,
		ConfigVersion: r.ConfigVersion,
		At:            at,
	}
}

// expireLocked 把 now 之前到期的 reserved 凭证批量置为 expired 并落事件。
// 调用方必须持有锁。返回处理的凭证数。
func (s *Service) expireLocked(now time.Time) int {
	var ids []string
	for id, r := range s.reservations {
		if r.Status == StatusReserved && !now.Before(r.ExpiresAt) {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return 0
	}
	sort.Strings(ids) // 确定性顺序，便于测试与审计
	events := make([]rawEvent, 0, len(ids))
	for _, id := range ids {
		r := s.reservations[id]
		ev, err := marshalEvent(EvExpired, now, s.releaseData(r, now))
		if err != nil {
			continue // 序列化不会失败；防御性跳过
		}
		events = append(events, ev)
	}
	if err := s.append(context.Background(), events...); err != nil {
		// 落盘失败则不改内存态，下次操作重试。
		return 0
	}
	return len(ids)
}

// ---------- 查询 ----------

// Balance 是活动余额视图。
type Balance struct {
	CampaignID     string
	ConfigVersion  int64
	TotalBudget    money.Money
	TotalSpent     money.Money
	TotalReserved  money.Money
	TotalAvailable money.Money
	DayKey         string
	Slot           int
	DailyCap       money.Money
	DailySpent     money.Money
	DailyReserved  money.Money
	DailyAvailable money.Money
	// PacingEnabled 为 true 时 PacingTarget/PacingAvailable 反映当前时段累计节奏。
	PacingEnabled   bool
	PacingTarget    money.Money
	PacingSpent     money.Money
	PacingReserved  money.Money
	PacingAvailable money.Money
}

// GetBalance 返回活动的总预算、当日预算与当前时段节奏使用情况。
func (s *Service) GetBalance(ctx context.Context, campaignID string) (*Balance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.clock.Now()
	s.expireLocked(now)

	cs, ok := s.campaigns[campaignID]
	if !ok {
		return nil, eNotFound("GetBalance", "campaign %q not found", campaignID)
	}
	localNow := now.In(cs.campaign.Location)
	dayKey := localNow.Format("2006-01-02")
	slot := localNow.Hour()
	d := cs.day(dayKey)
	b := &Balance{
		CampaignID:     campaignID,
		ConfigVersion:  cs.campaign.ConfigVersion,
		TotalBudget:    cs.campaign.TotalBudget,
		TotalSpent:     cs.totalSpent,
		TotalReserved:  cs.totalReserved,
		TotalAvailable: cs.totalAvailable(),
		DayKey:         dayKey,
		Slot:           slot,
		DailyCap:       cs.campaign.DailyCap,
		DailySpent:     d.spent,
		DailyReserved:  d.reserved,
		DailyAvailable: cs.dailyAvailable(dayKey),
	}
	if cs.campaign.Curve != nil {
		b.PacingEnabled = true
		b.PacingTarget = cs.pacingCap(slot)
		b.PacingSpent = d.spent
		b.PacingReserved = d.reserved
		b.PacingAvailable = cs.pacingAvailable(dayKey, slot)
	} else {
		zero := money.Zero(cs.campaign.TotalBudget.Currency())
		b.PacingTarget, b.PacingSpent, b.PacingReserved, b.PacingAvailable = zero, zero, zero, zero
	}
	return b, nil
}

// GetReservation 返回凭证当前状态（读取前会惰性过期）。
func (s *Service) GetReservation(_ context.Context, id string) (*Reservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked(s.clock.Now())
	r, ok := s.reservations[id]
	if !ok {
		return nil, eNotFound("GetReservation", "reservation %q not found", id)
	}
	return r, nil
}

// GetPacingReport 返回某自然日（dayKey 为空表示活动时区的今天）的逐时段节奏报告。
//
// 目标金额按“当前”配置版本的曲线与日上限计算；各行 Spent/Reserved 来自凭证
// 创建时冻结的小时桶，配置切换后也不会被重新归类，因此报告可直接对比偏差。
func (s *Service) GetPacingReport(_ context.Context, campaignID, dayKey string) (*PacingReport, error) {
	const op = "GetPacingReport"
	s.mu.Lock()
	defer s.mu.Unlock()

	s.expireLocked(s.clock.Now())

	cs, ok := s.campaigns[campaignID]
	if !ok {
		return nil, eNotFound(op, "campaign %q not found", campaignID)
	}
	c := cs.campaign
	if dayKey == "" {
		dayKey = s.clock.Now().In(c.Location).Format("2006-01-02")
	} else if _, err := time.ParseInLocation("2006-01-02", dayKey, c.Location); err != nil {
		return nil, eInvalid(op, "invalid day key %q: want YYYY-MM-DD", dayKey)
	}
	// 只读查找：查询一个没有任何费用的日期不应在内存中留下空桶。
	d := cs.days[dayKey]
	currency := c.TotalBudget.Currency()

	report := &PacingReport{
		CampaignID:    campaignID,
		ConfigVersion: c.ConfigVersion,
		Timezone:      c.Location.String(),
		DayKey:        dayKey,
		DailyCap:      c.DailyCap,
		CurveEnabled:  c.Curve != nil,
		Slots:         make([]SlotPacing, 0, SlotCount),
	}

	cumulative := money.Zero(currency)
	for h := 0; h < SlotCount; h++ {
		var ppm int64 = PacingPPM
		if c.Curve != nil {
			ppm = c.Curve.CumulativeTargetPPM(h)
		}
		target := scaledFloor(c.DailyCap, ppm)
		var spent, reserved money.Money
		if d != nil {
			spent = d.slots[h].spent
			reserved = d.slots[h].reserved
		} else {
			spent = money.Zero(currency)
			reserved = money.Zero(currency)
		}
		cumulative = cumulative.Add(spent)
		report.Slots = append(report.Slots, SlotPacing{
			Hour:                h,
			CumulativeTargetPPM: ppm,
			Target:              target,
			Spent:               spent,
			Reserved:            reserved,
			CumulativeSpent:     cumulative,
			Variance:            cumulative.Sub(target),
		})
	}
	if d != nil {
		report.TotalSpent = d.spent
		report.TotalReserved = d.reserved
	} else {
		report.TotalSpent = money.Zero(currency)
		report.TotalReserved = money.Zero(currency)
	}
	return report, nil
}

// ---------- 事件应用与持久化 ----------

// append 先把事件写入存储，成功后应用到内存投影；两步在同一临界区内，
// 因此内存态与磁盘事件流始终一致。
func (s *Service) append(ctx context.Context, events ...rawEvent) error {
	se := make([]store.Event, len(events))
	for i, ev := range events {
		se[i] = store.Event{Type: ev.Type, At: ev.At, Data: ev.Data}
	}
	if err := s.store.Append(ctx, se); err != nil {
		return fmt.Errorf("domain: persist events: %w", err)
	}
	for _, ev := range events {
		if err := s.apply(ev); err != nil {
			return err
		}
	}
	return nil
}

func storeEventToRaw(ev store.Event) rawEvent {
	return rawEvent{Type: ev.Type, At: ev.At, Data: ev.Data}
}

// apply 把单个事件应用到内存投影。必须幂等（重放与实时路径共用）。
func (s *Service) apply(ev rawEvent) error {
	switch ev.Type {
	case EvCampaignCreated:
		var d CampaignCreatedData
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			return errBadEvent("campaign.created: %v", err)
		}
		total, err := fromEvt(d.TotalBudget)
		if err != nil {
			return err
		}
		daily, err := fromEvt(d.DailyCap)
		if err != nil {
			return err
		}
		loc, err := time.LoadLocation(d.Timezone)
		if err != nil {
			loc = time.Local
		}
		version := d.ConfigVersion
		if version == 0 { // 兼容首轮未带版本号的历史事件流
			version = InitialConfigVersion
		}
		s.campaigns[d.CampaignID] = newCampaignState(&Campaign{
			ID:            d.CampaignID,
			Name:          d.Name,
			TotalBudget:   total,
			DailyCap:      daily,
			Location:      loc,
			DefaultTTL:    d.DefaultTTL,
			ConfigVersion: version,
			Curve:         d.Curve,
			CreatedAt:     d.CreatedAt,
		})
		if s.byRequest[d.CampaignID] == nil {
			s.byRequest[d.CampaignID] = make(map[string]string)
		}

	case EvConfigUpdated:
		var d ConfigUpdatedData
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			return errBadEvent("campaign.config_updated: %v", err)
		}
		cs, ok := s.campaigns[d.CampaignID]
		if !ok {
			return errBadEvent("config update for unknown campaign %q", d.CampaignID)
		}
		loc, err := time.LoadLocation(d.Timezone)
		if err != nil {
			loc = time.Local
		}
		cs.campaign.Location = loc
		cs.campaign.Curve = d.Curve
		cs.campaign.ConfigVersion = d.ConfigVersion

	case EvBudgetAdjusted:
		var d BudgetAdjustedData
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			return errBadEvent("campaign.budget_adjusted: %v", err)
		}
		cs, ok := s.campaigns[d.CampaignID]
		if !ok {
			return errBadEvent("budget adjustment for unknown campaign %q", d.CampaignID)
		}
		total, err := fromEvt(d.TotalBudget)
		if err != nil {
			return err
		}
		daily, err := fromEvt(d.DailyCap)
		if err != nil {
			return err
		}
		cs.campaign.TotalBudget = total
		cs.campaign.DailyCap = daily
		cs.campaign.ConfigVersion = d.ConfigVersion
		adj := BudgetAdjustment{
			AdjustmentID:  d.AdjustmentID,
			CampaignID:    d.CampaignID,
			ConfigVersion: d.ConfigVersion,
			TotalBudget:   total,
			DailyCap:      daily,
			At:            d.At,
		}
		cs.adjustments = append(cs.adjustments, adj)
		s.byAdjustment[d.AdjustmentID] = adj

	case EvReserved:
		var d ReservedData
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			return errBadEvent("reservation.reserved: %v", err)
		}
		amount, err := fromEvt(d.Amount)
		if err != nil {
			return err
		}
		cs, ok := s.campaigns[d.CampaignID]
		if !ok {
			return errBadEvent("reservation %q for unknown campaign %q", d.ReservationID, d.CampaignID)
		}
		cs.applyReserved(amount, d.DayKey, d.Slot)
		version := d.ConfigVersion
		if version == 0 {
			version = InitialConfigVersion
		}
		s.reservations[d.ReservationID] = &Reservation{
			ID:            d.ReservationID,
			CampaignID:    d.CampaignID,
			RequestID:     d.RequestID,
			Amount:        amount,
			DayKey:        d.DayKey,
			Slot:          d.Slot,
			ConfigVersion: version,
			CreatedAt:     d.CreatedAt,
			ExpiresAt:     d.ExpiresAt,
			Status:        StatusReserved,
			Captured:      money.Zero(amount.Currency()),
		}
		s.byRequest[d.CampaignID][d.RequestID] = d.ReservationID

	case EvCaptured:
		var d CapturedData
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			return errBadEvent("reservation.captured: %v", err)
		}
		captured, err := fromEvt(d.Captured)
		if err != nil {
			return err
		}
		r, ok := s.reservations[d.ReservationID]
		if !ok {
			return errBadEvent("capture for unknown reservation %q", d.ReservationID)
		}
		cs := s.campaigns[d.CampaignID]
		// 归属坐标以事件负载（冻结值）为准；缺失时退回凭证上的冻结值。
		dayKey, slot := d.DayKey, d.Slot
		if dayKey == "" {
			dayKey, slot = r.DayKey, r.Slot
		}
		cs.applyCaptured(r.Amount, captured, dayKey, slot)
		r.Status = StatusCaptured
		r.Captured = captured
		r.ReceiptID = d.ReceiptID
		r.TerminalAt = d.At
		s.byReceipt[d.ReceiptID] = captureRecord{
			ReceiptID:     d.ReceiptID,
			ReservationID: d.ReservationID,
			Amount:        captured,
			At:            d.At,
		}

	case EvCancelled, EvExpired:
		var d releaseData
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			return errBadEvent("%s: %v", ev.Type, err)
		}
		r, ok := s.reservations[d.ReservationID]
		if !ok {
			return errBadEvent("release for unknown reservation %q", d.ReservationID)
		}
		cs := s.campaigns[d.CampaignID]
		released, err := fromEvt(d.Released)
		if err != nil {
			return err
		}
		dayKey, slot := d.DayKey, d.Slot
		if dayKey == "" {
			dayKey, slot = r.DayKey, r.Slot
		}
		cs.applyReleased(released, dayKey, slot)
		if ev.Type == EvCancelled {
			r.Status = StatusCancelled
		} else {
			r.Status = StatusExpired
		}
		r.TerminalAt = d.At

	default:
		return errBadEvent("unknown event type %q", ev.Type)
	}
	return nil
}
