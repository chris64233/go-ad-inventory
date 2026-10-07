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

// adjustmentRecord 记录一次已落库的预算/配置调整，用于按调整号幂等重放。
type adjustmentRecord struct {
	AdjustmentID    string
	CampaignID      string
	Kind            string // "budget" 或 "config"
	ExpectedVersion int64
	NewVersion      int64
	// budget 类调整的完整内容。
	TotalBudget money.Money
	DailyCap    money.Money
	// config 类调整的完整内容。
	Timezone     string
	HasTimezone  bool
	CurveWeights []int64
	HasCurve     bool
	RemoveCurve  bool
	// 调整原因与依据（两类调整都可能携带）。
	Reason string
}

// Service 是预算领域服务。所有写操作在单互斥锁内完成
// “校验 → 事件落盘 → 更新内存投影”，因此并发下总预算、日预算与时段节奏额度
// 都不会被突破，且核销/取消/过期三者竞争时只会落入一个终态。
type Service struct {
	mu    sync.Mutex
	clock clock.Clock
	store store.Store

	campaigns map[string]*campaignState
	// 预占幂等索引（全局）：requestID -> reservationID。
	// 同一外部请求号跨活动、跨时段或金额不同时必须报幂等冲突，
	// 因此索引不按活动划分。
	byRequest map[string]string
	// 回执去重索引：receiptID -> 核销记录。
	byReceipt map[string]captureRecord
	// 调整幂等索引：adjustmentID -> 调整记录。
	byAdjustment map[string]adjustmentRecord
	// 配置版本历史：campaignID -> 按版本号升序的变更记录（首条为创建）。
	history      map[string][]ConfigVersion
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
		byRequest:    make(map[string]string),
		byReceipt:    make(map[string]captureRecord),
		byAdjustment: make(map[string]adjustmentRecord),
		history:      make(map[string][]ConfigVersion),
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
	Name         string
	TotalBudget  money.Money
	DailyCap     money.Money
	Timezone     string        // IANA 时区名，空则使用本地时区
	DefaultTTL   time.Duration // 凭证默认有效期，必须为正
	CurveWeights []int64       // 可选：24 个非负时段权重；nil 表示不做时段节奏限制
}

// CreateCampaign 创建活动并持久化配置事件（配置版本从 1 开始）。
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
	loc := time.Local
	if p.Timezone != "" {
		l, err := time.LoadLocation(p.Timezone)
		if err != nil {
			return nil, eInvalid(op, "invalid timezone %q", p.Timezone)
		}
		loc = l
	}
	var curve *Curve
	if p.CurveWeights != nil {
		c, err := NewCurve(p.CurveWeights)
		if err != nil {
			return nil, eInvalid(op, "%s", err)
		}
		curve = c
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.clock.Now()
	id := newID("cmp")
	ev, err := marshalEvent(EvCampaignCreated, now, CampaignCreatedData{
		CampaignID:  id,
		Name:        p.Name,
		TotalBudget: toEvt(p.TotalBudget),
		DailyCap:    toEvt(p.DailyCap),
		Timezone:    loc.String(),
		DefaultTTL:  p.DefaultTTL,
		Curve:       curveWeights(curve),
		CreatedAt:   now,
	})
	if err != nil {
		return nil, err
	}
	if err := s.append(ctx, ev); err != nil {
		return nil, err
	}
	return s.campaigns[id].campaign, nil
}

// GetCampaign 返回活动配置。
func (s *Service) GetCampaign(_ context.Context, id string) (*Campaign, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cs, ok := s.campaigns[id]
	if !ok {
		return nil, eNotFound("GetCampaign", "campaign %q not found", id)
	}
	return cs.campaign, nil
}

// ---------- 预算调整 ----------

// AdjustBudgetParams 是带配置版本的预算调整入参。
type AdjustBudgetParams struct {
	CampaignID      string
	AdjustmentID    string      // 调整号，幂等依据，必填
	ExpectedVersion int64       // 调用方看到的当前配置版本，乐观锁
	TotalBudget     money.Money // 调整后的完整新总预算（必填，必须为正）
	DailyCap        money.Money // 调整后的完整新日预算（必填，必须为正）
	Reason          string      // 可选：本次调整的原因与依据，随版本历史永久保存
}

// AdjustBudget 按配置版本调整总预算与日预算。
//
// 提高预算立即生效；降低预算时：
//   - 新总预算不得小于 已核销额 + 全部有效预占；
//   - 新日预算不得小于任何一天的 已核销额 + 当日有效预占（含历史日）。
//
// 任一条件不满足则**整体拒绝**，绝不静默取消既有凭证。
// 同一调整号重放：内容一致返回原结果；内容变化报幂等冲突。
// 期望版本与当前版本不一致报 version_conflict（body 带当前版本）。
func (s *Service) AdjustBudget(ctx context.Context, p AdjustBudgetParams) (*Campaign, error) {
	const op = "AdjustBudget"
	if p.CampaignID == "" {
		return nil, eInvalid(op, "campaign id is required")
	}
	if p.AdjustmentID == "" {
		return nil, eInvalid(op, "adjustment id is required")
	}
	if !p.TotalBudget.IsPositive() {
		return nil, eInvalid(op, "total budget must be positive")
	}
	if !p.DailyCap.IsPositive() {
		return nil, eInvalid(op, "daily cap must be positive")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	cs, ok := s.campaigns[p.CampaignID]
	if !ok {
		return nil, eNotFound(op, "campaign %q not found", p.CampaignID)
	}
	c := cs.campaign
	if p.TotalBudget.Currency() != c.TotalBudget.Currency() ||
		p.DailyCap.Currency() != c.TotalBudget.Currency() {
		return nil, eInvalid(op, "budget currency %q/%q differs from campaign currency %q",
			p.TotalBudget.Currency(), p.DailyCap.Currency(), c.TotalBudget.Currency())
	}

	// 幂等重放优先：同调整号无论版本如何都先查。
	if rec, ok := s.byAdjustment[p.AdjustmentID]; ok {
		if rec.CampaignID == p.CampaignID && rec.Kind == KindBudgetAdjust &&
			rec.ExpectedVersion == p.ExpectedVersion &&
			rec.TotalBudget.Cmp(p.TotalBudget) == 0 &&
			rec.DailyCap.Cmp(p.DailyCap) == 0 &&
			rec.Reason == p.Reason {
			// 返回该调整落库时的版本快照，而不是当前版本（之后可能又有新调整）。
			return s.campaignAtVersion(cs, rec.NewVersion)
		}
		return nil, eIdemConflict(op,
			"adjustment %q already applied with different content", p.AdjustmentID)
	}

	if p.ExpectedVersion != c.Version {
		return nil, eVersionConflict(op, p.ExpectedVersion, c.Version)
	}

	// 降低总预算：不得小于已核销 + 有效预占（该值同时也 >= 已核销额）。
	if p.TotalBudget.Cmp(c.TotalBudget) < 0 {
		if committed := cs.totalCommitted(); p.TotalBudget.Cmp(committed) < 0 {
			return nil, eAdjustRejected(op, p.TotalBudget, committed)
		}
	}
	// 降低日预算：每一天（含历史日）的已核销 + 有效预占都不得超过新上限。
	if p.DailyCap.Cmp(c.DailyCap) < 0 {
		for dayKey := range cs.days {
			if committed := cs.dailyCommitted(dayKey); p.DailyCap.Cmp(committed) < 0 {
				return nil, eDailyAdjustRejected(op, dayKey, p.DailyCap, committed)
			}
		}
	}

	newVersion := c.Version + 1
	now := s.clock.Now()
	ev, err := marshalEvent(EvBudgetAdjusted, now, BudgetAdjustedData{
		CampaignID:      c.ID,
		AdjustmentID:    p.AdjustmentID,
		ExpectedVersion: p.ExpectedVersion,
		NewVersion:      newVersion,
		TotalBudget:     toEvt(p.TotalBudget),
		DailyCap:        toEvt(p.DailyCap),
		Reason:          p.Reason,
		At:              now,
	})
	if err != nil {
		return nil, err
	}
	if err := s.append(ctx, ev); err != nil {
		return nil, err
	}
	return c, nil
}

// ---------- 时区 / 曲线配置切换 ----------

// 配置调整类型标记。
const (
	KindCreate       = "create"
	KindBudgetAdjust = "budget"
	KindConfigAdjust = "config"
)

// AdjustConfigParams 切换活动时区和/或投放曲线。
//
// Timezone 为 nil 表示不改时区；CurveWeights 非 nil 表示替换曲线；
// RemoveCurve 为 true 表示移除节奏限制。后两者互斥。
// 配置切换同样推进版本并使用调整号幂等。既有预占已冻结创建时的日期/时段，
// 不受新时区、新曲线影响。
type AdjustConfigParams struct {
	CampaignID      string
	AdjustmentID    string
	ExpectedVersion int64
	Timezone        *string
	CurveWeights    []int64 // 非 nil：替换为 24 权重曲线
	RemoveCurve     bool    // true：移除节奏曲线
	Reason          string  // 可选：本次调整的原因与依据，随版本历史永久保存
}

// AdjustConfig 切换时区/投放曲线并推进配置版本。
func (s *Service) AdjustConfig(ctx context.Context, p AdjustConfigParams) (*Campaign, error) {
	const op = "AdjustConfig"
	if p.CampaignID == "" {
		return nil, eInvalid(op, "campaign id is required")
	}
	if p.AdjustmentID == "" {
		return nil, eInvalid(op, "adjustment id is required")
	}
	if p.Timezone == nil && p.CurveWeights == nil && !p.RemoveCurve {
		return nil, eInvalid(op, "nothing to adjust: timezone and curve are both unchanged")
	}
	if p.RemoveCurve && p.CurveWeights != nil {
		return nil, eInvalid(op, "curve_weights and remove_curve are mutually exclusive")
	}
	var newLoc *time.Location
	if p.Timezone != nil {
		l, err := time.LoadLocation(*p.Timezone)
		if err != nil {
			return nil, eInvalid(op, "invalid timezone %q", *p.Timezone)
		}
		newLoc = l
	}
	if p.CurveWeights != nil {
		if _, err := NewCurve(p.CurveWeights); err != nil {
			return nil, eInvalid(op, "%s", err)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	cs, ok := s.campaigns[p.CampaignID]
	if !ok {
		return nil, eNotFound(op, "campaign %q not found", p.CampaignID)
	}
	c := cs.campaign

	tzName := ""
	hasTZ := p.Timezone != nil
	if hasTZ {
		tzName = newLoc.String()
	}
	hasCurve := p.CurveWeights != nil || p.RemoveCurve
	if rec, ok := s.byAdjustment[p.AdjustmentID]; ok {
		if rec.CampaignID == p.CampaignID && rec.Kind == KindConfigAdjust &&
			rec.ExpectedVersion == p.ExpectedVersion &&
			rec.HasTimezone == hasTZ && rec.Timezone == tzName &&
			rec.HasCurve == hasCurve && rec.RemoveCurve == p.RemoveCurve &&
			equalInt64s(rec.CurveWeights, p.CurveWeights) &&
			rec.Reason == p.Reason {
			return s.campaignAtVersion(cs, rec.NewVersion)
		}
		return nil, eIdemConflict(op,
			"adjustment %q already applied with different content", p.AdjustmentID)
	}

	if p.ExpectedVersion != c.Version {
		return nil, eVersionConflict(op, p.ExpectedVersion, c.Version)
	}

	newVersion := c.Version + 1
	now := s.clock.Now()
	ev, err := marshalEvent(EvConfigAdjusted, now, ConfigAdjustedData{
		CampaignID:      c.ID,
		AdjustmentID:    p.AdjustmentID,
		ExpectedVersion: p.ExpectedVersion,
		NewVersion:      newVersion,
		Timezone:        tzName,
		HasTimezone:     hasTZ,
		Curve:           p.CurveWeights,
		HasCurve:        hasCurve,
		RemoveCurve:     p.RemoveCurve,
		Reason:          p.Reason,
		At:              now,
	})
	if err != nil {
		return nil, err
	}
	if err := s.append(ctx, ev); err != nil {
		return nil, err
	}
	return c, nil
}

func equalInt64s(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---------- 暂停 / 恢复 ----------

// PauseCampaign 暂停活动：此后新的预占（曝光确认）一律以 campaign_paused 拒绝，
// 不产生任何事件与消耗记录；暂停前已确认的凭证不受影响，
// 其迟到回执仍可正常核销（已确认的曝光不因暂停而丢失）。
//
// 暂停是幂等状态：重复暂停返回当前活动，不追加新事件。
// reason 记录暂停原因与依据，随事件永久保存。
func (s *Service) PauseCampaign(ctx context.Context, campaignID, reason string) (*Campaign, error) {
	const op = "PauseCampaign"
	if campaignID == "" {
		return nil, eInvalid(op, "campaign id is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	cs, ok := s.campaigns[campaignID]
	if !ok {
		return nil, eNotFound(op, "campaign %q not found", campaignID)
	}
	if cs.campaign.Status == CampaignStatusPaused {
		return cs.campaign, nil // 幂等：已暂停
	}
	now := s.clock.Now()
	ev, err := marshalEvent(EvCampaignPaused, now, StatusChangedData{
		CampaignID: campaignID,
		Reason:     reason,
		At:         now,
	})
	if err != nil {
		return nil, err
	}
	if err := s.append(ctx, ev); err != nil {
		return nil, err
	}
	return cs.campaign, nil
}

// ResumeCampaign 恢复活动：从当前已确认的消耗（已核销 + 有效预占）继续投放。
// 暂停期间被拒绝的请求不曾落库，恢复后不会也不应被补记；
// 调用方需以新的请求号重新发起预占。
//
// 恢复是幂等状态：重复恢复返回当前活动，不追加新事件。
func (s *Service) ResumeCampaign(ctx context.Context, campaignID, reason string) (*Campaign, error) {
	const op = "ResumeCampaign"
	if campaignID == "" {
		return nil, eInvalid(op, "campaign id is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	cs, ok := s.campaigns[campaignID]
	if !ok {
		return nil, eNotFound(op, "campaign %q not found", campaignID)
	}
	if cs.campaign.Status == CampaignStatusActive {
		return cs.campaign, nil // 幂等：本就活跃
	}
	now := s.clock.Now()
	ev, err := marshalEvent(EvCampaignResumed, now, StatusChangedData{
		CampaignID: campaignID,
		Reason:     reason,
		At:         now,
	})
	if err != nil {
		return nil, err
	}
	if err := s.append(ctx, ev); err != nil {
		return nil, err
	}
	return cs.campaign, nil
}

// campaignAtVersion 返回活动在指定配置版本时的配置快照（用于调整幂等重放，
// 让调用方拿到该调整真正生效的版本，而不是后来的版本）。
// 调用方必须持有锁。version 必须存在于版本流中。
func (s *Service) campaignAtVersion(cs *campaignState, version int64) (*Campaign, error) {
	hist := s.history[cs.campaign.ID]
	idx := int(version) - 1
	if idx < 0 || idx >= len(hist) {
		// 版本流损坏属不应发生的编程/持久化错误。
		return nil, errBadEvent("version %d missing in history of campaign %q",
			version, cs.campaign.ID)
	}
	v := hist[idx]
	loc, err := time.LoadLocation(v.Timezone)
	if err != nil {
		loc = cs.campaign.Location
	}
	var curve *Curve
	if v.CurveWeights != nil {
		curve, err = NewCurve(v.CurveWeights)
		if err != nil {
			return nil, errBadEvent("version %d has bad curve: %v", version, err)
		}
	}
	return &Campaign{
		ID:          cs.campaign.ID,
		Name:        cs.campaign.Name,
		Status:      cs.campaign.Status,
		TotalBudget: v.TotalBudget,
		DailyCap:    v.DailyCap,
		Location:    loc,
		DefaultTTL:  cs.campaign.DefaultTTL,
		Curve:       curve,
		Version:     v.Version,
		CreatedAt:   cs.campaign.CreatedAt,
	}, nil
}

// curveWeights 返回曲线权重快照；无曲线时为 nil。
func curveWeights(c *Curve) []int64 {
	if c == nil {
		return nil
	}
	return c.Weights()
}

// ---------- 预占 ----------

// ReserveParams 是一次投放请求的预占入参。
type ReserveParams struct {
	CampaignID string
	RequestID  string        // 外部请求号，幂等依据
	Amount     money.Money   // 预占金额，必须为正
	TTL        time.Duration // 可选；<=0 时使用活动默认有效期
}

// Reserve 同时占住总预算、日预算与当前时段节奏额度，成功返回带失效时间的凭证。
//
// 凭证冻结创建时刻的日期、小时时段与配置版本；此后的时区/曲线/预算调整
// 都不改变该笔费用的归属。
//
// 幂等：同一 (campaign, requestID) 且金额相同 → 返回原凭证；
// 金额不同 → 幂等冲突。任一额度不足 → 整体失败，不落任何事件。
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

	// 幂等检查（全局请求号）：同编号且活动、金额、归属日期与时段完全一致
	// 才返回原凭证；任一维度不同都报幂等冲突，绝不当作重复成功。
	if rid, ok := s.byRequest[p.RequestID]; ok {
		orig := s.reservations[rid]
		now := s.clock.Now()
		dayKey := clock.DateKey(now, c.Location)
		slot := slotOf(now, c.Location)
		if orig.CampaignID == p.CampaignID && orig.Amount.Cmp(p.Amount) == 0 &&
			orig.DayKey == dayKey && orig.Slot == slot {
			return orig, nil
		}
		return nil, eIdemConflict(op,
			"request %q already reserved on campaign %q with amount %s (day %s slot %d), "+
				"cannot reuse for campaign %q amount %s (day %s slot %d)",
			p.RequestID, orig.CampaignID, orig.Amount.String(), orig.DayKey, orig.Slot,
			p.CampaignID, p.Amount.String(), dayKey, slot)
	}

	now := s.clock.Now()
	// 先惰性过期，再判断余额，保证过期额度及时归还。
	s.expireLocked(now)

	// 活动状态、三级额度必须同时满足，否则整体失败，不落任何事件。
	if c.Status == CampaignStatusPaused {
		return nil, eCampaignPaused(op, c.ID)
	}
	dayKey := clock.DateKey(now, c.Location)
	slot := slotOf(now, c.Location)
	// 顺序：总预算 → 日预算 → 时段节奏。
	if avail := cs.totalAvailable(); p.Amount.Cmp(avail) > 0 {
		return nil, eBudget(op, "total", p.Amount, avail)
	}
	if avail := cs.dailyAvailable(dayKey); p.Amount.Cmp(avail) > 0 {
		return nil, eBudget(op, "daily", p.Amount, avail)
	}
	if c.Curve != nil {
		if avail := cs.slotAvailable(dayKey, slot); p.Amount.Cmp(avail) > 0 {
			return nil, eBudget(op, "slot", p.Amount, avail)
		}
	}

	ttl := p.TTL
	if ttl <= 0 {
		ttl = c.DefaultTTL
	}
	expiresAt := now.Add(ttl)

	r := &Reservation{
		ID:         newID("rsv"),
		CampaignID: p.CampaignID,
		RequestID:  p.RequestID,
		Amount:     p.Amount,
		DayKey:     dayKey,
		Slot:       slot,
		Version:    c.Version,
		CreatedAt:  now,
		ExpiresAt:  expiresAt,
		Status:     StatusReserved,
		Captured:   money.Zero(p.Amount.Currency()),
	}
	ev, err := marshalEvent(EvReserved, now, ReservedData{
		ReservationID: r.ID,
		CampaignID:    r.CampaignID,
		RequestID:     r.RequestID,
		Amount:        toEvt(r.Amount),
		DayKey:        r.DayKey,
		Slot:          r.Slot,
		Version:       r.Version,
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

// Capture 按实际费用核销：核销额计入已花费，差额释放回三级额度。
//
// 费用始终归入凭证创建时冻结的日期与时段——即使回执迟到、期间活动切换过时区
// 或投放曲线，也不会被重新归类。
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
		DayKey:        r.DayKey,
		Slot:          r.Slot,
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

// Cancel 主动取消预占并释放全部额度（归还到冻结的日期与时段）。
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

	ev, err := marshalEvent(EvCancelled, now, CancelledData{
		ReservationID: r.ID,
		CampaignID:    r.CampaignID,
		Released:      toEvt(r.Amount),
		DayKey:        r.DayKey,
		Slot:          r.Slot,
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

// ---------- 过期 ----------

// ExpireSweep 主动扫描并过期所有到期的 reserved 凭证，返回过期数量。
// 常规操作前也会惰性执行同样的逻辑；本方法供后台定时任务调用。
func (s *Service) ExpireSweep(ctx context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.expireLocked(s.clock.Now()), nil
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
		ev, err := marshalEvent(EvExpired, now, ExpiredData{
			ReservationID: r.ID,
			CampaignID:    r.CampaignID,
			Released:      toEvt(r.Amount),
			DayKey:        r.DayKey,
			Slot:          r.Slot,
			At:            now,
		})
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
	Status         CampaignStatus
	Version        int64
	TotalBudget    money.Money
	TotalSpent     money.Money
	TotalReserved  money.Money
	TotalAvailable money.Money
	DayKey         string
	Slot           int // 当前小时时段（活动时区）；未配置曲线时仍返回
	PacingLimited  bool
	DailyCap       money.Money
	DailySpent     money.Money
	DailyReserved  money.Money
	DailyAvailable money.Money
	SlotTarget     money.Money // 当前时段的累计节奏目标；未配置曲线时等于日预算
	SlotAvailable  money.Money // 当前节奏额度下尚可预占金额
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
	dayKey := clock.DateKey(now, cs.campaign.Location)
	slot := slotOf(now, cs.campaign.Location)
	d := cs.day(dayKey)
	b := &Balance{
		CampaignID:     campaignID,
		Status:         cs.campaign.Status,
		Version:        cs.campaign.Version,
		TotalBudget:    cs.campaign.TotalBudget,
		TotalSpent:     cs.totalSpent,
		TotalReserved:  cs.totalReserved,
		TotalAvailable: cs.totalAvailable(),
		DayKey:         dayKey,
		Slot:           slot,
		PacingLimited:  cs.campaign.Curve != nil,
		DailyCap:       cs.campaign.DailyCap,
		DailySpent:     d.spent,
		DailyReserved:  d.reserved,
		DailyAvailable: cs.dailyAvailable(dayKey),
	}
	if cs.campaign.Curve != nil {
		b.SlotTarget = cs.slotTarget(slot)
	} else {
		b.SlotTarget = cs.campaign.DailyCap
	}
	b.SlotAvailable = b.SlotTarget.Sub(d.spent).Sub(d.reserved)
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

// ConfigVersion 是一条配置版本记录（预算版本 / 配置切换统一流）。
type ConfigVersion struct {
	Version      int64
	Kind         string // "create" | "budget" | "config"
	AdjustmentID string // create 时为空
	Reason       string // 本次调整的原因与依据；create 时为空
	TotalBudget  money.Money
	DailyCap     money.Money
	Timezone     string
	CurveWeights []int64 // 该版本生效的曲线权重；无曲线为 nil
	At           time.Time
}

// GetBudgetVersions 返回活动配置版本流（含创建版本 1），按版本升序。
func (s *Service) GetBudgetVersions(_ context.Context, campaignID string) ([]ConfigVersion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.campaigns[campaignID]; !ok {
		return nil, eNotFound("GetBudgetVersions", "campaign %q not found", campaignID)
	}
	hist := s.history[campaignID]
	out := make([]ConfigVersion, len(hist))
	for i, v := range hist {
		if v.CurveWeights != nil {
			v.CurveWeights = append([]int64(nil), v.CurveWeights...)
		}
		out[i] = v
	}
	return out, nil
}

// SlotPacing 是单个时段的节奏视图。
type SlotPacing struct {
	Slot                int
	Weight              int64
	CumulativeTarget    money.Money // 到本时段结束的累计目标
	SlotSpent           money.Money // 本时段已核销
	SlotReserved        money.Money // 本时段有效预占
	CumulativeSpent     money.Money // 截至本时段的累计已核销
	CumulativeReserved  money.Money // 截至本时段的累计有效预占
	CumulativeCommitted money.Money // 累计已核销 + 累计有效预占
	Variance            money.Money // 累计已核销 - 累计目标，正为超投、负为欠投
}

// PacingReport 是某一天 24 个时段的节奏报告。
type PacingReport struct {
	CampaignID    string
	Version       int64
	Timezone      string
	DayKey        string
	DailyCap      money.Money
	PacingLimited bool
	Slots         []SlotPacing
}

// GetPacing 返回指定日（dayKey 为空时取活动时区“今天”）的逐时段节奏报告：
// 每个时段的曲线权重、累计目标、实际核销、有效预占与偏差。
func (s *Service) GetPacing(ctx context.Context, campaignID, dayKey string) (*PacingReport, error) {
	const op = "GetPacing"
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.clock.Now()
	s.expireLocked(now)

	cs, ok := s.campaigns[campaignID]
	if !ok {
		return nil, eNotFound(op, "campaign %q not found", campaignID)
	}
	if dayKey == "" {
		dayKey = clock.DateKey(now, cs.campaign.Location)
	} else {
		// 严格校验：拒绝 "2026-02-30" 这类被 time.Parse 静默归一化的日期。
		t, err := time.ParseInLocation("2006-01-02", dayKey, cs.campaign.Location)
		if err != nil || t.Format("2006-01-02") != dayKey {
			return nil, eInvalid(op, "invalid day key %q, want YYYY-MM-DD", dayKey)
		}
	}
	d := cs.days[dayKey]
	cur := cs.campaign.TotalBudget.Currency()

	report := &PacingReport{
		CampaignID:    campaignID,
		Version:       cs.campaign.Version,
		Timezone:      cs.campaign.Location.String(),
		DayKey:        dayKey,
		DailyCap:      cs.campaign.DailyCap,
		PacingLimited: cs.campaign.Curve != nil,
		Slots:         make([]SlotPacing, 0, SlotsPerDay),
	}
	cumSpent := money.Zero(cur)
	cumReserved := money.Zero(cur)
	for h := 0; h < SlotsPerDay; h++ {
		var weight int64
		var target money.Money
		if cs.campaign.Curve != nil {
			weight = cs.campaign.Curve.weights[h]
			target = cs.slotTarget(h)
		} else {
			target = cs.campaign.DailyCap
		}
		slotSpent := money.Zero(cur)
		slotReserved := money.Zero(cur)
		if d != nil {
			if sb, ok := d.slots[h]; ok {
				slotSpent = sb.spent
				slotReserved = sb.reserved
			}
		}
		cumSpent = cumSpent.Add(slotSpent)
		cumReserved = cumReserved.Add(slotReserved)
		report.Slots = append(report.Slots, SlotPacing{
			Slot:                h,
			Weight:              weight,
			CumulativeTarget:    target,
			SlotSpent:           slotSpent,
			SlotReserved:        slotReserved,
			CumulativeSpent:     cumSpent,
			CumulativeReserved:  cumReserved,
			CumulativeCommitted: cumSpent.Add(cumReserved),
			Variance:            cumSpent.Sub(target),
		})
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
		var curve *Curve
		if d.Curve != nil {
			curve, err = NewCurve(d.Curve)
			if err != nil {
				return errBadEvent("campaign.created: %v", err)
			}
		}
		c := &Campaign{
			ID:          d.CampaignID,
			Name:        d.Name,
			Status:      CampaignStatusActive,
			TotalBudget: total,
			DailyCap:    daily,
			Location:    loc,
			DefaultTTL:  d.DefaultTTL,
			Curve:       curve,
			Version:     1,
			CreatedAt:   d.CreatedAt,
		}
		s.campaigns[d.CampaignID] = newCampaignState(c)
		s.history[d.CampaignID] = []ConfigVersion{{
			Version:      1,
			Kind:         KindCreate,
			TotalBudget:  total,
			DailyCap:     daily,
			Timezone:     loc.String(),
			CurveWeights: curveWeights(curve),
			At:           d.CreatedAt,
		}}

	case EvBudgetAdjusted:
		var d BudgetAdjustedData
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			return errBadEvent("campaign.budget_adjusted: %v", err)
		}
		total, err := fromEvt(d.TotalBudget)
		if err != nil {
			return err
		}
		daily, err := fromEvt(d.DailyCap)
		if err != nil {
			return err
		}
		cs, ok := s.campaigns[d.CampaignID]
		if !ok {
			return errBadEvent("budget adjustment for unknown campaign %q", d.CampaignID)
		}
		cs.applyBudget(total, daily)
		cs.campaign.Version = d.NewVersion
		s.byAdjustment[d.AdjustmentID] = adjustmentRecord{
			AdjustmentID:    d.AdjustmentID,
			CampaignID:      d.CampaignID,
			Kind:            KindBudgetAdjust,
			ExpectedVersion: d.ExpectedVersion,
			NewVersion:      d.NewVersion,
			TotalBudget:     total,
			DailyCap:        daily,
			Reason:          d.Reason,
		}
		s.history[d.CampaignID] = append(s.history[d.CampaignID], ConfigVersion{
			Version:      d.NewVersion,
			Kind:         KindBudgetAdjust,
			AdjustmentID: d.AdjustmentID,
			Reason:       d.Reason,
			TotalBudget:  total,
			DailyCap:     daily,
			Timezone:     cs.campaign.Location.String(),
			CurveWeights: curveWeights(cs.campaign.Curve),
			At:           d.At,
		})

	case EvConfigAdjusted:
		var d ConfigAdjustedData
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			return errBadEvent("campaign.config_adjusted: %v", err)
		}
		cs, ok := s.campaigns[d.CampaignID]
		if !ok {
			return errBadEvent("config adjustment for unknown campaign %q", d.CampaignID)
		}
		if d.HasTimezone {
			loc, err := time.LoadLocation(d.Timezone)
			if err != nil {
				return errBadEvent("config adjusted to invalid timezone %q: %v", d.Timezone, err)
			}
			cs.campaign.Location = loc
		}
		var weights []int64
		switch {
		case d.RemoveCurve:
			cs.campaign.Curve = nil
		case d.HasCurve:
			curve, err := NewCurve(d.Curve)
			if err != nil {
				return errBadEvent("config adjusted with bad curve: %v", err)
			}
			cs.campaign.Curve = curve
			weights = curve.Weights()
		default:
			weights = curveWeights(cs.campaign.Curve)
		}
		cs.campaign.Version = d.NewVersion
		tzForRecord := ""
		if d.HasTimezone {
			tzForRecord = d.Timezone
		} else {
			tzForRecord = cs.campaign.Location.String()
		}
		s.byAdjustment[d.AdjustmentID] = adjustmentRecord{
			AdjustmentID:    d.AdjustmentID,
			CampaignID:      d.CampaignID,
			Kind:            KindConfigAdjust,
			ExpectedVersion: d.ExpectedVersion,
			NewVersion:      d.NewVersion,
			Timezone:        d.Timezone,
			HasTimezone:     d.HasTimezone,
			CurveWeights:    d.Curve,
			HasCurve:        d.HasCurve,
			RemoveCurve:     d.RemoveCurve,
			Reason:          d.Reason,
		}
		s.history[d.CampaignID] = append(s.history[d.CampaignID], ConfigVersion{
			Version:      d.NewVersion,
			Kind:         KindConfigAdjust,
			AdjustmentID: d.AdjustmentID,
			Reason:       d.Reason,
			TotalBudget:  cs.campaign.TotalBudget,
			DailyCap:     cs.campaign.DailyCap,
			Timezone:     tzForRecord,
			CurveWeights: weights,
			At:           d.At,
		})

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
		slot := d.Slot
		cs.applyReserved(amount, d.DayKey, slot)
		s.reservations[d.ReservationID] = &Reservation{
			ID:         d.ReservationID,
			CampaignID: d.CampaignID,
			RequestID:  d.RequestID,
			Amount:     amount,
			DayKey:     d.DayKey,
			Slot:       slot,
			Version:    d.Version,
			CreatedAt:  d.CreatedAt,
			ExpiresAt:  d.ExpiresAt,
			Status:     StatusReserved,
			Captured:   money.Zero(amount.Currency()),
		}
		s.byRequest[d.RequestID] = d.ReservationID

	case EvCampaignPaused, EvCampaignResumed:
		var d StatusChangedData
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			return errBadEvent("%s: %v", ev.Type, err)
		}
		cs, ok := s.campaigns[d.CampaignID]
		if !ok {
			return errBadEvent("status change for unknown campaign %q", d.CampaignID)
		}
		if ev.Type == EvCampaignPaused {
			cs.campaign.Status = CampaignStatusPaused
		} else {
			cs.campaign.Status = CampaignStatusActive
		}

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
		cs, ok := s.campaigns[d.CampaignID]
		if !ok {
			return errBadEvent("capture for reservation %q of unknown campaign %q",
				d.ReservationID, d.CampaignID)
		}
		// 以凭证冻结的日期/时段入账，防止迟到回执被重新归类。
		cs.applyCaptured(r.Amount, captured, r.DayKey, r.Slot)
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
		reservationID, campaignID, released, at, err := decodeRelease(ev)
		if err != nil {
			return err
		}
		r, ok := s.reservations[reservationID]
		if !ok {
			return errBadEvent("release for unknown reservation %q", reservationID)
		}
		cs, ok := s.campaigns[campaignID]
		if !ok {
			return errBadEvent("release for reservation %q of unknown campaign %q",
				reservationID, campaignID)
		}
		// 归还到凭证冻结的日期/时段，保证配置切换后迟到事件也不会串桶。
		cs.applyReleased(released, r.DayKey, r.Slot)
		if ev.Type == EvCancelled {
			r.Status = StatusCancelled
		} else {
			r.Status = StatusExpired
		}
		r.TerminalAt = at

	default:
		return errBadEvent("unknown event type %q", ev.Type)
	}
	return nil
}

// decodeRelease 解码取消/过期事件的公共字段。
func decodeRelease(ev rawEvent) (reservationID, campaignID string, released money.Money,
	at time.Time, err error) {
	var d struct {
		ReservationID string    `json:"reservation_id"`
		CampaignID    string    `json:"campaign_id"`
		Released      evtMoney  `json:"released"`
		DayKey        string    `json:"day_key"`
		Slot          int       `json:"slot"`
		At            time.Time `json:"at"`
	}
	if err = json.Unmarshal(ev.Data, &d); err != nil {
		err = errBadEvent("%s: %v", ev.Type, err)
		return
	}
	released, err = fromEvt(d.Released)
	if err != nil {
		return
	}
	return d.ReservationID, d.CampaignID, released, d.At, nil
}
