package adinventory

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"
)

// DefaultReservationTTL 是预占凭证的默认有效期。
const DefaultReservationTTL = 5 * time.Minute

// dayLayout 是日预算使用的自然日键格式（UTC 自然日）。
const dayLayout = "2006-01-02"

// ReserveInput 是创建预占的请求参数。
type ReserveInput struct {
	CampaignID string
	RequestNo  string // 外部请求号，预占操作的幂等键
	Amount     Money
	Payload    string // 请求内容；参与幂等指纹比对
}

// CaptureInput 是曝光回执核销的请求参数。
type CaptureInput struct {
	Token      string
	ReceiptNo  string // 外部回执号，回执去重键
	ActualCost Money  // 实际费用，不得高于预占额
}

// requestFingerprint 记录某个外部请求号首次出现时的内容，
// 后续同号请求逐字段比对。
type requestFingerprint struct {
	CampaignID string
	Amount     Money
	Payload    string
}

func (f requestFingerprint) equal(in ReserveInput) bool {
	return f.CampaignID == in.CampaignID &&
		f.Amount.Cmp(in.Amount) == 0 &&
		f.Payload == in.Payload
}

// receiptRecord 记录某个回执号的处理结果，用于回执去重与乱序重放。
type receiptRecord struct {
	Token      string
	Status     ReservationStatus
	ActualCost Money
}

// Service 是预算预占/核销服务。单实例内通过互斥锁串行化所有状态变迁，
// 任何“检查余额 → 落盘事件 → 更新内存”都在锁内完成，因此并发争抢
// 不会突破总预算或日预算，终态也只会落入一次。
type Service struct {
	mu    sync.Mutex
	store Store
	clock Clock
	ttl   time.Duration

	campaigns  map[string]*Campaign
	tokens     map[string]*Reservation       // token → 凭证
	reqIndex   map[string]requestFingerprint // 外部请求号 → 首次内容
	tokenByReq map[string]string             // 外部请求号 → token
	receipts   map[string]receiptRecord      // 回执号 → 处理结果

	// 额度台账（可由事件流完整重建）：
	//   reserved + captured <= budget
	totalReserved Money
	totalCaptured Money
	dayReserved   map[string]Money
	dayCaptured   map[string]Money
}

// NewService 基于已有 Store 构建服务并重放全部历史事件。
func NewService(store Store, clock Clock, ttl time.Duration) (*Service, error) {
	if clock == nil {
		clock = SystemClock{}
	}
	if ttl <= 0 {
		ttl = DefaultReservationTTL
	}
	s := &Service{
		store:       store,
		clock:       clock,
		ttl:         ttl,
		campaigns:   map[string]*Campaign{},
		tokens:      map[string]*Reservation{},
		reqIndex:    map[string]requestFingerprint{},
		tokenByReq:  map[string]string{},
		receipts:    map[string]receiptRecord{},
		dayReserved: map[string]Money{},
		dayCaptured: map[string]Money{},
	}
	if err := store.Replay(s.applyEvent); err != nil {
		return nil, err
	}
	return s, nil
}

// CreateCampaign 创建活动配置。预算必须为非负数，且日预算不得高于总预算。
func (s *Service) CreateCampaign(id string, totalBudget, dailyBudget Money) (*Campaign, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, invalidArgumentf("campaign id is required")
	}
	if totalBudget.IsNegative() || dailyBudget.IsNegative() {
		return nil, invalidArgumentf("budget must not be negative")
	}
	if dailyBudget.Cmp(totalBudget) > 0 {
		return nil, invalidArgumentf("daily budget %s exceeds total budget %s", dailyBudget, totalBudget)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.campaigns[id]; ok {
		return nil, invalidArgumentf("campaign %q already exists", id)
	}
	now := s.clock.Now().UTC()
	c := &Campaign{ID: id, TotalBudget: totalBudget, DailyBudget: dailyBudget, CreatedAt: now}
	if _, err := s.store.Append(evCampaignCreated, c, now); err != nil {
		return nil, err
	}
	s.campaigns[id] = c
	return cloneCampaign(c), nil
}

// GetCampaign 查询活动配置。
func (s *Service) GetCampaign(id string) (*Campaign, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.campaigns[id]
	if !ok {
		return nil, notFoundf("campaign %q not found", id)
	}
	return cloneCampaign(c), nil
}

// Reserve 执行两级额度预占。
//   - 同一 RequestNo 且内容一致：返回原凭证（幂等重放）；
//   - 同一 RequestNo 但内容变化：返回 KindIdempotencyConflict；
//   - 总预算或当日日预算不足：返回 KindBudgetExceeded，且不会产生任何预占。
func (s *Service) Reserve(in ReserveInput) (*Reservation, error) {
	in.CampaignID = strings.TrimSpace(in.CampaignID)
	in.RequestNo = strings.TrimSpace(in.RequestNo)
	if in.CampaignID == "" {
		return nil, invalidArgumentf("campaign_id is required")
	}
	if in.RequestNo == "" {
		return nil, invalidArgumentf("request_no is required")
	}
	if in.Amount.Sign() <= 0 {
		return nil, invalidArgumentf("reserve amount must be positive, got %s", in.Amount)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	c, ok := s.campaigns[in.CampaignID]
	if !ok {
		return nil, notFoundf("campaign %q not found", in.CampaignID)
	}

	// 幂等：同号请求先比对内容，完全一致直接返回原凭证。
	if fp, ok := s.reqIndex[in.RequestNo]; ok {
		if !fp.equal(in) {
			return nil, idempotencyConflictf(
				"request_no %q reused with different content", in.RequestNo)
		}
		return cloneReservation(s.tokens[s.tokenByReq[in.RequestNo]]), nil
	}

	// 先让过期凭证释放额度，再做余额判断。
	s.sweepExpiredLocked()

	if totalUsed := s.totalReserved.Add(s.totalCaptured); totalUsed.Add(in.Amount).Cmp(c.TotalBudget) > 0 {
		return nil, budgetExceededf("total",
			"reserve %s would exceed total budget %s (used %s)",
			in.Amount, c.TotalBudget, totalUsed)
	}
	day := s.clock.Now().UTC().Format(dayLayout)
	dayUsed := s.dayReserved[day].Add(s.dayCaptured[day])
	if dayUsed.Add(in.Amount).Cmp(c.DailyBudget) > 0 {
		return nil, budgetExceededf("daily",
			"reserve %s would exceed daily budget %s for %s (used %s)",
			in.Amount, c.DailyBudget, day, dayUsed)
	}

	now := s.clock.Now().UTC()
	r := &Reservation{
		Token:      newToken(),
		CampaignID: in.CampaignID,
		RequestNo:  in.RequestNo,
		Day:        day,
		Amount:     in.Amount,
		Payload:    in.Payload,
		Status:     StatusReserved,
		CreatedAt:  now,
		ExpiresAt:  now.Add(s.ttl),
	}
	// 先落盘再改内存：落盘失败则内存无变化，不存在半成品预占。
	if _, err := s.store.Append(evReservationCreated, r, now); err != nil {
		return nil, err
	}
	s.applyReservationCreated(r)
	return cloneReservation(r), nil
}

// Capture 凭曝光回执核销：实际费用不得高于预占额，差额立即释放。
//   - 重复回执（同 ReceiptNo 同内容）返回首次核销结果；
//   - 同 ReceiptNo 内容变化返回 KindIdempotencyConflict；
//   - 凭证已取消/已过期返回 KindStateConflict；
//   - 已核销凭证再收到其他回执返回 KindStateConflict。
func (s *Service) Capture(in CaptureInput) (*Reservation, error) {
	in.Token = strings.TrimSpace(in.Token)
	in.ReceiptNo = strings.TrimSpace(in.ReceiptNo)
	if in.Token == "" {
		return nil, invalidArgumentf("token is required")
	}
	if in.ReceiptNo == "" {
		return nil, invalidArgumentf("receipt_no is required")
	}
	if in.ActualCost.IsNegative() {
		return nil, invalidArgumentf("actual cost must not be negative, got %s", in.ActualCost)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// 回执号去重优先：同一回执号的重复/乱序投递得到确定结果。
	if rec, seen := s.receipts[in.ReceiptNo]; seen {
		if rec.Token == in.Token && rec.ActualCost.Cmp(in.ActualCost) == 0 {
			return cloneReservation(s.tokens[rec.Token]), nil
		}
		return nil, idempotencyConflictf(
			"receipt_no %q reused with different content", in.ReceiptNo)
	}

	r, ok := s.tokens[in.Token]
	if !ok {
		return nil, notFoundf("reservation token %q not found", in.Token)
	}
	if in.ActualCost.Cmp(r.Amount) > 0 {
		return nil, invalidArgumentf(
			"actual cost %s exceeds reserved amount %s", in.ActualCost, r.Amount)
	}

	switch r.Status {
	case StatusCaptured:
		// 凭证已被另一个回执号核销，迟到回执不能再次入账。
		return nil, stateConflictf("reservation %s already captured", in.Token)
	case StatusCancelled:
		return nil, stateConflictf("reservation %s already cancelled", in.Token)
	case StatusExpired:
		return nil, stateConflictf("reservation %s already expired", in.Token)
	case StatusReserved:
		if !s.clock.Now().UTC().Before(r.ExpiresAt) {
			// 与过期处理竞争：先把过期落终态，再明确拒绝回执。
			s.finalizeExpiredLocked(r)
			return nil, stateConflictf("reservation %s already expired", in.Token)
		}
	}

	now := s.clock.Now().UTC()
	payload := evFinalPayload{
		Token:      r.Token,
		Status:     StatusCaptured,
		ReceiptNo:  in.ReceiptNo,
		ActualCost: in.ActualCost,
	}
	if _, err := s.store.Append(evReservationFinal, payload, now); err != nil {
		return nil, err
	}
	s.applyFinalized(payload, now)
	return cloneReservation(s.tokens[r.Token]), nil
}

// Cancel 主动取消凭证并释放全部预占额度。
// 已核销的凭证不可取消（迟到取消不能冲销已核销费用），返回 KindStateConflict；
// 重复取消幂等返回；已过期返回 KindStateConflict。
func (s *Service) Cancel(token string) (*Reservation, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, invalidArgumentf("token is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	r, ok := s.tokens[token]
	if !ok {
		return nil, notFoundf("reservation token %q not found", token)
	}
	switch r.Status {
	case StatusCancelled:
		return cloneReservation(r), nil // 重复取消，幂等返回
	case StatusCaptured:
		return nil, stateConflictf("reservation %s already captured", token)
	case StatusExpired:
		return nil, stateConflictf("reservation %s already expired", token)
	case StatusReserved:
		if !s.clock.Now().UTC().Before(r.ExpiresAt) {
			s.finalizeExpiredLocked(r)
			return nil, stateConflictf("reservation %s already expired", token)
		}
	}

	now := s.clock.Now().UTC()
	payload := evFinalPayload{Token: token, Status: StatusCancelled}
	if _, err := s.store.Append(evReservationFinal, payload, now); err != nil {
		return nil, err
	}
	s.applyFinalized(payload, now)
	return cloneReservation(r), nil
}

// GetReservation 查询凭证当前状态。
func (s *Service) GetReservation(token string) (*Reservation, error) {
	token = strings.TrimSpace(token)
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.tokens[token]
	if !ok {
		return nil, notFoundf("reservation token %q not found", token)
	}
	// 惰性过期，保证查询到的状态与余额口径一致。
	if r.Status == StatusReserved && !s.clock.Now().UTC().Before(r.ExpiresAt) {
		s.finalizeExpiredLocked(r)
	}
	return cloneReservation(r), nil
}

// SweepExpired 扫描全部凭证，把已到失效时间的预占转为过期并释放全部额度。
// 可由后台定时任务调用；所有读取接口内部也会惰性触发，因此过期一定会被处理。
// 返回本次被置为过期的凭证数量。
func (s *Service) SweepExpired() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sweepExpiredLocked()
}

func (s *Service) sweepExpiredLocked() int {
	n := 0
	now := s.clock.Now().UTC()
	for _, r := range s.tokens {
		if r.Status == StatusReserved && !now.Before(r.ExpiresAt) {
			s.finalizeExpiredLocked(r)
			n++
		}
	}
	return n
}

func (s *Service) finalizeExpiredLocked(r *Reservation) {
	now := s.clock.Now().UTC()
	payload := evFinalPayload{Token: r.Token, Status: StatusExpired}
	// 过期事件落盘失败则保留 reserved，下一次操作会重试，绝不做无记录的释放。
	if _, err := s.store.Append(evReservationFinal, payload, now); err != nil {
		return
	}
	s.applyFinalized(payload, now)
}

// GetBalance 查询活动在指定自然日（空串表示时钟当前日）的两级额度视图。
func (s *Service) GetBalance(campaignID, day string) (*Balance, error) {
	campaignID = strings.TrimSpace(campaignID)
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.campaigns[campaignID]
	if !ok {
		return nil, notFoundf("campaign %q not found", campaignID)
	}
	if day == "" {
		day = s.clock.Now().UTC().Format(dayLayout)
	}

	s.sweepExpiredLocked()

	dr := s.dayReserved[day]
	dc := s.dayCaptured[day]
	return &Balance{
		CampaignID: campaignID,
		Day:        day,

		TotalBudget:    c.TotalBudget,
		TotalReserved:  s.totalReserved,
		TotalCaptured:  s.totalCaptured,
		TotalAvailable: c.TotalBudget.Sub(s.totalReserved).Sub(s.totalCaptured),

		DailyBudget:  c.DailyBudget,
		DayReserved:  dr,
		DayCaptured:  dc,
		DayAvailable: c.DailyBudget.Sub(dr).Sub(dc),
	}, nil
}

// ---- 事件应用：运行时与崩溃重放走同一份代码 ----

func (s *Service) applyEvent(ev Event) error {
	switch ev.Type {
	case evCampaignCreated:
		var c Campaign
		if err := json.Unmarshal(ev.Payload, &c); err != nil {
			return err
		}
		s.campaigns[c.ID] = &c
	case evReservationCreated:
		var r Reservation
		if err := json.Unmarshal(ev.Payload, &r); err != nil {
			return err
		}
		s.applyReservationCreated(&r)
	case evReservationFinal:
		var p evFinalPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return err
		}
		// 事件流中每个 token 只会有一条终态事件；防御性跳过重复终态。
		if s.tokens[p.Token] != nil && s.tokens[p.Token].Status.IsFinal() {
			return nil
		}
		s.applyFinalized(p, ev.At)
	default:
		// 未知事件类型跳过，保持前向兼容。
	}
	return nil
}

func (s *Service) applyReservationCreated(r *Reservation) {
	cp := *r
	s.tokens[r.Token] = &cp
	s.reqIndex[r.RequestNo] = requestFingerprint{
		CampaignID: r.CampaignID, Amount: r.Amount, Payload: r.Payload,
	}
	s.tokenByReq[r.RequestNo] = r.Token
	s.totalReserved = s.totalReserved.Add(r.Amount)
	s.dayReserved[r.Day] = s.dayReserved[r.Day].Add(r.Amount)
}

func (s *Service) applyFinalized(p evFinalPayload, at time.Time) {
	r, ok := s.tokens[p.Token]
	if !ok || r.Status.IsFinal() {
		return
	}
	r.Status = p.Status
	r.ActualCost = p.ActualCost
	r.ReceiptNo = p.ReceiptNo
	r.FinalizedAt = &at

	// 释放该凭证持有的全部预占额度……
	s.totalReserved = s.totalReserved.Sub(r.Amount)
	s.dayReserved[r.Day] = s.dayReserved[r.Day].Sub(r.Amount)
	// ……核销时再把实际费用计入已花费；取消/过期则全额释放。
	if p.Status == StatusCaptured {
		s.totalCaptured = s.totalCaptured.Add(p.ActualCost)
		s.dayCaptured[r.Day] = s.dayCaptured[r.Day].Add(p.ActualCost)
		s.receipts[p.ReceiptNo] = receiptRecord{
			Token: r.Token, Status: p.Status, ActualCost: p.ActualCost,
		}
	}
}

func newToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 失败在实际环境中不可恢复，直接 panic 暴露问题。
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

func cloneCampaign(c *Campaign) *Campaign {
	cp := *c
	return &cp
}

func cloneReservation(r *Reservation) *Reservation {
	cp := *r
	return &cp
}
