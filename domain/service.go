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

// Service 是预算领域服务。所有写操作在单互斥锁内完成
// “校验 → 事件落盘 → 更新内存投影”，因此并发下两级预算都不会被突破，
// 且核销/取消/过期三者竞争时只会落入一个终态。
type Service struct {
	mu    sync.Mutex
	clock clock.Clock
	store store.Store

	campaigns map[string]*campaignState
	// 预占幂等索引：campaignID -> requestID -> reservationID。
	byRequest map[string]map[string]string
	// 回执去重索引：receiptID -> 核销记录。
	byReceipt    map[string]captureRecord
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
}

// CreateCampaign 创建活动并持久化配置事件。
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

// ---------- 预占 ----------

// ReserveParams 是一次投放请求的预占入参。
type ReserveParams struct {
	CampaignID string
	RequestID  string        // 外部请求号，幂等依据
	Amount     money.Money   // 预占金额，必须为正
	TTL        time.Duration // 可选；<=0 时使用活动默认有效期
}

// Reserve 同时占住总预算与日预算两级额度，成功返回带失效时间的凭证。
//
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

	dayKey := clock.DateKey(now, c.Location)
	// 两级额度必须同时满足，否则整体失败。
	if avail := cs.totalAvailable(); p.Amount.Cmp(avail) > 0 {
		return nil, eBudget(op, "total", p.Amount, avail)
	}
	if avail := cs.dailyAvailable(dayKey); p.Amount.Cmp(avail) > 0 {
		return nil, eBudget(op, "daily", p.Amount, avail)
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

// Capture 按实际费用核销：核销额计入已花费，差额释放回两级预算。
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

// Cancel 主动取消预占并释放全部额度。
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
	TotalBudget    money.Money
	TotalSpent     money.Money
	TotalReserved  money.Money
	TotalAvailable money.Money
	DayKey         string
	DailyCap       money.Money
	DailySpent     money.Money
	DailyReserved  money.Money
	DailyAvailable money.Money
}

// GetBalance 返回活动的总预算与当日预算使用情况。
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
	d := cs.day(dayKey)
	return &Balance{
		CampaignID:     campaignID,
		TotalBudget:    cs.campaign.TotalBudget,
		TotalSpent:     cs.totalSpent,
		TotalReserved:  cs.totalReserved,
		TotalAvailable: cs.totalAvailable(),
		DayKey:         dayKey,
		DailyCap:       cs.campaign.DailyCap,
		DailySpent:     d.spent,
		DailyReserved:  d.reserved,
		DailyAvailable: cs.dailyAvailable(dayKey),
	}, nil
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
		s.campaigns[d.CampaignID] = newCampaignState(&Campaign{
			ID:          d.CampaignID,
			Name:        d.Name,
			TotalBudget: total,
			DailyCap:    daily,
			Location:    loc,
			DefaultTTL:  d.DefaultTTL,
			CreatedAt:   d.CreatedAt,
		})
		if s.byRequest[d.CampaignID] == nil {
			s.byRequest[d.CampaignID] = make(map[string]string)
		}

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
		cs.applyReserved(amount, d.DayKey)
		s.reservations[d.ReservationID] = &Reservation{
			ID:         d.ReservationID,
			CampaignID: d.CampaignID,
			RequestID:  d.RequestID,
			Amount:     amount,
			DayKey:     d.DayKey,
			CreatedAt:  d.CreatedAt,
			ExpiresAt:  d.ExpiresAt,
			Status:     StatusReserved,
			Captured:   money.Zero(amount.Currency()),
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
		cs.applyCaptured(r.Amount, captured, d.DayKey)
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
		cs := s.campaigns[campaignID]
		cs.applyReleased(released, r.DayKey)
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
func decodeRelease(ev rawEvent) (reservationID, campaignID string, released money.Money, at time.Time, err error) {
	var d struct {
		ReservationID string    `json:"reservation_id"`
		CampaignID    string    `json:"campaign_id"`
		Released      evtMoney  `json:"released"`
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
