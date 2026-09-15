package service

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/gigmatch/gigmatch/internal/constants"
	"github.com/gigmatch/gigmatch/internal/dto"
	"github.com/gigmatch/gigmatch/internal/model"
	"github.com/gigmatch/gigmatch/internal/repository"
)

var fixtureSeq atomic.Int64

// newChangeTestDB opens a private in-memory SQLite database for one fixture.
// The connection pool is pinned to a single connection so write transactions
// serialize the same way row locks serialize them on MySQL: a losing
// conditional UPDATE is re-evaluated after the winner commits and matches zero
// rows, instead of failing with a shared-cache lock error.
func newChangeTestDB(t *testing.T, seq int64) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:chgmem%d?mode=memory&cache=shared", seq)
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	if err := db.AutoMigrate(&model.User{}, &model.Requirement{}, &model.Bid{}, &model.Contract{}, &model.ContractChange{}, &model.OperationLog{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// changeFixture wires the services around an in-memory SQLite database and
// returns a signed, in-progress contract between partyA and partyB.
type changeFixture struct {
	db          *gorm.DB
	partyA      *model.User
	partyB      *model.User
	outlier     *model.User
	requirement *model.Requirement
	bid         *model.Bid
	contract    *model.Contract
	contractSvc *ContractService
	changeSvc   *ContractChangeService
	reqSvc      *RequirementService
	bidSvc      *BidService
}

func buildChangeFixture(t *testing.T) *changeFixture {
	t.Helper()
	seq := fixtureSeq.Add(1)
	db := newChangeTestDB(t, seq)
	logger := discardLogger()

	userRepo := repository.NewUserRepository(db)
	reqRepo := repository.NewRequirementRepository(db)
	bidRepo := repository.NewBidRepository(db)
	contractRepo := repository.NewContractRepository(db)
	changeRepo := repository.NewContractChangeRepository(db)
	logRepo := repository.NewOperationLogRepository(db)

	// Each fixture uses its own private in-memory database; identities only
	// need to be unique within that database.
	partyA := &model.User{Username: "chg-a", PasswordHash: "x", Name: "甲方", Role: constants.RoleRequester}
	partyB := &model.User{Username: "chg-b", PasswordHash: "x", Name: "乙方", Role: constants.RoleFreelancer}
	outlier := &model.User{Username: "chg-out", PasswordHash: "x", Name: "外人", Role: constants.RoleFreelancer}
	for _, u := range []*model.User{partyA, partyB, outlier} {
		if err := userRepo.Create(u); err != nil {
			t.Fatalf("create user: %v", err)
		}
	}

	logSvc := NewOperationLogService(logRepo, logger)
	contractSvc := NewContractService(db, contractRepo, changeRepo, logSvc, logger)
	changeSvc := NewContractChangeService(db, contractRepo, changeRepo, logSvc, logger)
	reqSvc := NewRequirementService(reqRepo, bidRepo, logSvc, logger)
	bidSvc := NewBidService(bidRepo, reqRepo, logSvc, logger)

	req, err := reqSvc.Create(dto.CreateRequirementRequest{
		Title: "变更测试需求", Description: "用于验证合同变更单的全流程", MinBudget: 10000, MaxBudget: 80000, Skills: []string{"Go"},
	}, partyA.ID, partyA.Name, partyA.Role)
	if err != nil {
		t.Fatalf("create requirement: %v", err)
	}
	bid, err := bidSvc.Create(dto.CreateBidRequest{
		RequirementID: req.ID, Amount: 30000, DurationDays: 20, Proposal: "承接该项目",
	}, partyB.ID, partyB.Name, partyB.Role)
	if err != nil {
		t.Fatalf("create bid: %v", err)
	}
	contract, err := reqSvc.AcceptBid(req.ID, bid.ID, partyA.ID, partyA.Name, "installments", contractSvc)
	if err != nil {
		t.Fatalf("accept bid: %v", err)
	}
	if _, err := contractSvc.Sign(contract.ID, partyB.ID, partyB.Name); err != nil {
		t.Fatalf("sign contract: %v", err)
	}
	contract, err = contractSvc.Get(contract.ID)
	if err != nil {
		t.Fatal(err)
	}
	return &changeFixture{
		db:          db,
		partyA:      partyA,
		partyB:      partyB,
		outlier:     outlier,
		requirement: req,
		bid:         bid,
		contract:    contract,
		contractSvc: contractSvc,
		changeSvc:   changeSvc,
		reqSvc:      reqSvc,
		bidSvc:      bidSvc,
	}
}

// validChangeRequest builds a conservation-valid change request under the
// frozen-done-stage rule: done stages are copied verbatim and the remaining
// amount (new total minus done sum) is spread evenly across unfinished stages.
func validChangeRequest(total, delta float64, stages []model.ContractStage, reason, scope string) dto.CreateContractChangeRequest {
	newTotal := round2(total + delta)
	proposed := make([]dto.ChangeStageItem, 0, len(stages))
	var doneSum float64
	unfinished := 0
	for _, st := range stages {
		if st.Status == "done" {
			doneSum += st.Amount
		} else {
			unfinished++
		}
	}
	remaining := round2(newTotal - round2(doneSum))
	each := round2(remaining / float64(unfinished))
	last := round2(remaining - each*float64(unfinished-1))
	idx := 0
	for _, st := range stages {
		item := dto.ChangeStageItem{Name: st.Name, Status: st.Status, DueAt: st.DueAt}
		if st.Status == "done" {
			item.Amount = st.Amount
		} else {
			idx++
			if idx == unfinished {
				item.Amount = last
			} else {
				item.Amount = each
			}
		}
		proposed = append(proposed, item)
	}
	return dto.CreateContractChangeRequest{Reason: reason, Scope: scope, AmountDelta: delta, Stages: proposed}
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}

func isConflictAppError(t *testing.T, err error, wantSubstr string) {
	t.Helper()
	var appErr *constants.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("err = %v, want AppError", err)
	}
	if appErr.Code != constants.CodeConflict {
		t.Fatalf("code = %d, want %d (%s)", appErr.Code, constants.CodeConflict, appErr.Message)
	}
	if wantSubstr != "" && !strings.Contains(appErr.Message, wantSubstr) {
		t.Fatalf("message = %q, want substring %q", appErr.Message, wantSubstr)
	}
}

func isBadRequestAppError(t *testing.T, err error, wantSubstr string) {
	t.Helper()
	var appErr *constants.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("err = %v, want AppError", err)
	}
	if appErr.Code != constants.CodeBadRequest {
		t.Fatalf("code = %d, want %d (%s)", appErr.Code, constants.CodeBadRequest, appErr.Message)
	}
	if wantSubstr != "" && !strings.Contains(appErr.Message, wantSubstr) {
		t.Fatalf("message = %q, want substring %q", appErr.Message, wantSubstr)
	}
}

func TestContractChangeCreateValidation(t *testing.T) {
	f := buildChangeFixture(t)

	t.Run("non-party is forbidden", func(t *testing.T) {
		req := validChangeRequest(f.contract.TotalAmount, 5000, f.contract.Stages, "需求新增", "增加一个模块")
		_, err := f.changeSvc.Create(f.contract.ID, req, f.outlier.ID, f.outlier.Name)
		if !errors.Is(err, constants.ErrForbidden) {
			t.Fatalf("err = %v, want forbidden", err)
		}
	})

	t.Run("party can raise a change on in-progress contract", func(t *testing.T) {
		req := validChangeRequest(f.contract.TotalAmount, 5000, f.contract.Stages, "需求新增", "增加一个报表模块")
		ch, err := f.changeSvc.Create(f.contract.ID, req, f.partyB.ID, f.partyB.Name)
		if err != nil {
			t.Fatalf("create change: %v", err)
		}
		if ch.Status != constants.ChangePending || ch.NewAmount != 35000 || ch.AmountDelta != 5000 {
			t.Fatalf("unexpected change: %+v", ch)
		}
		if ch.ProposerParty != constants.ChangePartyB || ch.ProposerID != f.partyB.ID {
			t.Fatalf("unexpected proposer: %+v", ch)
		}
	})

	t.Run("duplicate pending change is rejected", func(t *testing.T) {
		req := validChangeRequest(f.contract.TotalAmount, 1000, f.contract.Stages, "再次变更", "再增加一点")
		_, err := f.changeSvc.Create(f.contract.ID, req, f.partyA.ID, f.partyA.Name)
		isConflictAppError(t, err, "待处理变更")
	})

	t.Run("money conservation mismatch is rejected", func(t *testing.T) {
		// Settle the pending change so the contract accepts a new proposal.
		pending, err := f.changeSvc.ListByContract(f.contract.ID, f.partyA.ID)
		if err != nil || len(pending) == 0 {
			t.Fatalf("load pending: %v %d", err, len(pending))
		}
		if _, err := f.changeSvc.Withdraw(f.contract.ID, pending[0].ID, f.partyB.ID, f.partyB.Name); err != nil {
			t.Fatalf("withdraw: %v", err)
		}
		bad := validChangeRequest(f.contract.TotalAmount, 5000, f.contract.Stages, "破坏守恒", "金额对不上")
		bad.Stages[1].Amount += 100 // unfinished sum no longer equals the new total
		_, err = f.changeSvc.Create(f.contract.ID, bad, f.partyA.ID, f.partyA.Name)
		isBadRequestAppError(t, err, "金额不守恒")
	})
}

func TestContractChangeAppliesAtomicallyOnApprove(t *testing.T) {
	f := buildChangeFixture(t)
	req := validChangeRequest(f.contract.TotalAmount, 10000, f.contract.Stages, "范围扩大", "增加支付模块")
	ch, err := f.changeSvc.Create(f.contract.ID, req, f.partyA.ID, f.partyA.Name)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	finalChange, updated, err := f.changeSvc.Approve(f.contract.ID, ch.ID, f.partyB.ID, f.partyB.Name)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if finalChange.Status != constants.ChangeApproved || finalChange.ResponderID != f.partyB.ID {
		t.Fatalf("change not approved: %+v", finalChange)
	}
	if updated.TotalAmount != 40000 {
		t.Fatalf("total = %v, want 40000", updated.TotalAmount)
	}
	// New invariant: total == done amounts + unfinished amounts, and the done
	// stage keeps its original name/amount/status.
	var doneSum, unfinishedSum float64
	for _, st := range updated.Stages {
		if st.Status == "done" {
			doneSum += st.Amount
		} else {
			unfinishedSum += st.Amount
		}
	}
	if round2(doneSum+unfinishedSum) != 40000 {
		t.Fatalf("stage sum = %v, want new total 40000", round2(doneSum+unfinishedSum))
	}
	if round2(unfinishedSum) != 31000 || round2(doneSum) != 9000 {
		t.Fatalf("done=%v unfinished=%v, want 9000/31000", doneSum, unfinishedSum)
	}
	if updated.Stages[0].Name != f.contract.Stages[0].Name || updated.Stages[0].Amount != f.contract.Stages[0].Amount || updated.Stages[0].Status != "done" {
		t.Fatalf("done stage must be frozen, got %+v", updated.Stages[0])
	}

	// Persistence: re-read through the contract service.
	reloaded, err := f.contractSvc.Get(f.contract.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.TotalAmount != 40000 || reloaded.ActiveChange != nil {
		t.Fatalf("contract not synced: total=%v active=%v", reloaded.TotalAmount, reloaded.ActiveChange)
	}
	history, err := f.changeSvc.ListByContract(f.contract.ID, f.partyA.ID)
	if err != nil || len(history) != 1 || history[0].Status != constants.ChangeApproved {
		t.Fatalf("history = %v, err = %v", history, err)
	}
}

func TestContractChangeRejectAndWithdrawKeepContractIntact(t *testing.T) {
	t.Run("reject leaves contract untouched", func(t *testing.T) {
		f := buildChangeFixture(t)
		req := validChangeRequest(f.contract.TotalAmount, -3000, f.contract.Stages, "缩减范围", "去掉一个模块")
		ch, err := f.changeSvc.Create(f.contract.ID, req, f.partyB.ID, f.partyB.Name)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		rejected, err := f.changeSvc.Reject(f.contract.ID, ch.ID, f.partyA.ID, f.partyA.Name)
		if err != nil {
			t.Fatalf("reject: %v", err)
		}
		if rejected.Status != constants.ChangeRejected {
			t.Fatalf("status = %s", rejected.Status)
		}
		c, _ := f.contractSvc.Get(f.contract.ID)
		if c.TotalAmount != 30000 || c.ActiveChange != nil {
			t.Fatalf("contract changed after reject: %v %v", c.TotalAmount, c.ActiveChange)
		}
	})

	t.Run("withdraw leaves contract untouched and allows a new change", func(t *testing.T) {
		f := buildChangeFixture(t)
		req := validChangeRequest(f.contract.TotalAmount, 2000, f.contract.Stages, "小幅调整", "调整文案")
		ch, err := f.changeSvc.Create(f.contract.ID, req, f.partyA.ID, f.partyA.Name)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := f.changeSvc.Withdraw(f.contract.ID, ch.ID, f.partyA.ID, f.partyA.Name); err != nil {
			t.Fatalf("withdraw: %v", err)
		}
		c, _ := f.contractSvc.Get(f.contract.ID)
		if c.TotalAmount != 30000 || c.ActiveChange != nil {
			t.Fatalf("contract changed after withdraw: %v %v", c.TotalAmount, c.ActiveChange)
		}
		// A fresh change can be raised after the pending one was withdrawn.
		again := validChangeRequest(f.contract.TotalAmount, 1500, f.contract.Stages, "再次发起", "重新调整")
		if _, err := f.changeSvc.Create(f.contract.ID, again, f.partyB.ID, f.partyB.Name); err != nil {
			t.Fatalf("create after withdraw: %v", err)
		}
	})

	t.Run("only the proposer can withdraw and only the other party can respond", func(t *testing.T) {
		f := buildChangeFixture(t)
		req := validChangeRequest(f.contract.TotalAmount, 1000, f.contract.Stages, "权限测试", "校验操作方")
		ch, err := f.changeSvc.Create(f.contract.ID, req, f.partyA.ID, f.partyA.Name)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := f.changeSvc.Withdraw(f.contract.ID, ch.ID, f.partyB.ID, f.partyB.Name); err == nil {
			t.Fatal("non-proposer withdraw should fail")
		}
		if _, _, err := f.changeSvc.Approve(f.contract.ID, ch.ID, f.partyA.ID, f.partyA.Name); err == nil {
			t.Fatal("proposer self-approval should fail")
		}
	})
}

func TestPendingChangePausesCompletionAndChangeRules(t *testing.T) {
	f := buildChangeFixture(t)
	req := validChangeRequest(f.contract.TotalAmount, 4000, f.contract.Stages, "完成前变更", "新增内容")
	if _, err := f.changeSvc.Create(f.contract.ID, req, f.partyB.ID, f.partyB.Name); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := f.contractSvc.Complete(f.contract.ID, f.partyA.ID, f.partyA.Name); err == nil {
		t.Fatal("completion must be paused while a change is pending")
	} else {
		isConflictAppError(t, err, "待处理合同变更")
	}

	// After approval the contract can be completed.
	c, _ := f.contractSvc.Get(f.contract.ID)
	if _, _, err := f.changeSvc.Approve(f.contract.ID, c.ActiveChange.ID, f.partyA.ID, f.partyA.Name); err != nil {
		t.Fatalf("approve: %v", err)
	}
	done, err := f.contractSvc.Complete(f.contract.ID, f.partyA.ID, f.partyA.Name)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if done.Status != constants.ContractCompleted {
		t.Fatalf("status = %s", done.Status)
	}

	// No more changes once the contract is completed.
	blocked := dto.CreateContractChangeRequest{
		Reason:      "完成后变更",
		Scope:       "不应允许",
		AmountDelta: 1000,
		Stages:      []dto.ChangeStageItem{{Name: "结项", Amount: done.TotalAmount + 1000, Status: "pending", DueAt: "无"}},
	}
	_, err = f.changeSvc.Create(done.ID, blocked, f.partyA.ID, f.partyA.Name)
	isConflictAppError(t, err, "")
}

func TestConcurrentSettlementOnlyOneWins(t *testing.T) {
	f := buildChangeFixture(t)
	req := validChangeRequest(f.contract.TotalAmount, 6000, f.contract.Stages, "并发变更", "双方同时处理")
	ch, err := f.changeSvc.Create(f.contract.ID, req, f.partyA.ID, f.partyA.Name)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	var ok, conflict int
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _, err := f.changeSvc.Approve(f.contract.ID, ch.ID, f.partyB.ID, f.partyB.Name)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				ok++
			} else {
				var appErr *constants.AppError
				if errors.As(err, &appErr) && appErr.Code == constants.CodeConflict {
					conflict++
				}
			}
		}()
	}
	close(start)
	wg.Wait()

	if ok != 1 || conflict != 1 {
		t.Fatalf("ok=%d conflict=%d, want exactly one winner", ok, conflict)
	}
	c, err := f.contractSvc.Get(f.contract.ID)
	if err != nil {
		t.Fatal(err)
	}
	if c.TotalAmount != 36000 {
		t.Fatalf("total = %v, want 36000", c.TotalAmount)
	}
}

// TestValidateChangeStagesFreezesDoneStages verifies the repaired conservation
// rule: settled milestones are immutable and the new total must equal the done
// amount plus the unfinished stage amounts.
func TestValidateChangeStagesFreezesDoneStages(t *testing.T) {
	original := []model.ContractStage{
		{Name: "项目启动", Amount: 9000, Status: "done", DueAt: "签约后3日内"},
		{Name: "中期交付", Amount: 12000, Status: "in_progress", DueAt: "工期过半"},
		{Name: "验收结项", Amount: 9000, Status: "pending", DueAt: "验收通过后"},
	}
	const total = 30000.0

	clone := func() []model.ContractStage {
		out := make([]model.ContractStage, len(original))
		copy(out, original)
		return out
	}
	// A valid proposal: done 9000 frozen, unfinished 15000+16000 = 31000, new total 40000.
	valid := func() []model.ContractStage {
		p := clone()
		p[1].Amount = 15000
		p[2].Amount = 16000
		return p
	}

	t.Run("valid proposal passes and returns new total", func(t *testing.T) {
		got, err := validateChangeStages(original, valid(), total, 10000)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != 40000 {
			t.Fatalf("newTotal = %v, want 40000", got)
		}
	})

	t.Run("tampering a done stage amount is rejected", func(t *testing.T) {
		p := valid()
		p[0].Amount = 9001
		_, err := validateChangeStages(original, p, total, 10000)
		isBadRequestAppError(t, err, "已完成阶段")
	})

	t.Run("tampering a done stage name is rejected", func(t *testing.T) {
		p := valid()
		p[0].Name = "改名"
		_, err := validateChangeStages(original, p, total, 10000)
		isBadRequestAppError(t, err, "已完成阶段")
	})

	t.Run("reopening a done stage is rejected", func(t *testing.T) {
		p := valid()
		p[0].Status = "pending"
		_, err := validateChangeStages(original, p, total, 10000)
		isBadRequestAppError(t, err, "已完成阶段")
	})

	t.Run("deleting a stage is rejected", func(t *testing.T) {
		p := valid()
		_, err := validateChangeStages(original, p[:2], total, 10000)
		isBadRequestAppError(t, err, "阶段数量")
	})

	t.Run("flipping an unfinished stage to done is rejected", func(t *testing.T) {
		p := valid()
		p[1].Status = "done"
		_, err := validateChangeStages(original, p, total, 10000)
		isBadRequestAppError(t, err, "不能通过变更单标记为已完成")
	})

	t.Run("new total below done amount is rejected", func(t *testing.T) {
		p := clone()
		// Declared new total 30000 - 25000 = 5000, below the settled 9000.
		p[1].Amount = 0
		p[2].Amount = 0
		_, err := validateChangeStages(original, p, total, -25000)
		isBadRequestAppError(t, err, "不能低于已完成阶段金额")
	})

	t.Run("done plus unfinished not equal to new total is rejected", func(t *testing.T) {
		p := valid()
		p[2].Amount += 500 // 9000 + 15000 + 16500 = 40500 != 40000
		_, err := validateChangeStages(original, p, total, 10000)
		isBadRequestAppError(t, err, "金额不守恒")
	})
}

// TestApproveReadBackIsConsistent verifies the approved contract, re-read from
// the database, satisfies total == sum(all stages) and matches the proposal.
func TestApproveReadBackIsConsistent(t *testing.T) {
	f := buildChangeFixture(t)
	req := validChangeRequest(f.contract.TotalAmount, 10000, f.contract.Stages, "范围扩大", "增加支付模块")
	ch, err := f.changeSvc.Create(f.contract.ID, req, f.partyA.ID, f.partyA.Name)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_, updated, err := f.changeSvc.Approve(f.contract.ID, ch.ID, f.partyB.ID, f.partyB.Name)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	var sum float64
	for _, st := range updated.Stages {
		sum += st.Amount
	}
	if round2(sum) != updated.TotalAmount {
		t.Fatalf("stage sum %v != total %v after read-back", round2(sum), updated.TotalAmount)
	}
	if updated.TotalAmount != ch.NewAmount {
		t.Fatalf("total %v != approved new amount %v", updated.TotalAmount, ch.NewAmount)
	}
}

// TestCompleteVsApproveRaceStable runs completion and approval of the same
// contract concurrently and asserts that every possible serialization leaves a
// consistent final state: no partial write, no stale snapshot overwrite, and
// change history that agrees with the contract terminal state.
func TestCompleteVsApproveRaceStable(t *testing.T) {
	const iterations = 20
	for i := 0; i < iterations; i++ {
		f := buildChangeFixture(t)
		// Party B proposes so party A (the only one who can complete) approves.
		req := validChangeRequest(f.contract.TotalAmount, 8000, f.contract.Stages, "并发", "完成与审批同时发生")
		ch, err := f.changeSvc.Create(f.contract.ID, req, f.partyB.ID, f.partyB.Name)
		if err != nil {
			t.Fatalf("create: %v", err)
		}

		var wg sync.WaitGroup
		var completeErr, approveErr error
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, completeErr = f.contractSvc.Complete(f.contract.ID, f.partyA.ID, f.partyA.Name)
		}()
		go func() {
			defer wg.Done()
			<-start
			_, _, approveErr = f.changeSvc.Approve(f.contract.ID, ch.ID, f.partyA.ID, f.partyA.Name)
		}()
		close(start)
		wg.Wait()

		finalContract, err := f.contractSvc.Get(f.contract.ID)
		if err != nil {
			t.Fatalf("reload contract: %v", err)
		}
		finalChange, err := f.changeSvc.Get(f.contract.ID, ch.ID, f.partyA.ID)
		if err != nil {
			t.Fatalf("reload change: %v", err)
		}

		// Universal invariant 1: stage amounts always sum to the total.
		var sum float64
		for _, st := range finalContract.Stages {
			sum += st.Amount
		}
		if round2(sum) != finalContract.TotalAmount {
			t.Fatalf("iter %d: stage sum %v != total %v", i, round2(sum), finalContract.TotalAmount)
		}

		// Universal invariant 2: completed contracts carry no pending change.
		if finalContract.Status == constants.ContractCompleted && finalChange.Status == constants.ChangePending {
			t.Fatalf("iter %d: completed contract still has a pending change", i)
		}

		// Universal invariant 3: if the order was approved, the contract must
		// reflect exactly its amount and stage amounts/names, never the old
		// snapshot. If completion also ran afterwards, every stage is legally
		// marked done; otherwise statuses match the proposal.
		if finalChange.Status == constants.ChangeApproved {
			if finalContract.TotalAmount != ch.NewAmount {
				t.Fatalf("iter %d: approved total %v not applied, contract %v", i, ch.NewAmount, finalContract.TotalAmount)
			}
			if len(finalContract.Stages) != len(finalChange.ProposedStages) {
				t.Fatalf("iter %d: stage count mismatch", i)
			}
			completed := finalContract.Status == constants.ContractCompleted
			for j := range finalContract.Stages {
				got, want := finalContract.Stages[j], finalChange.ProposedStages[j]
				if got.Name != want.Name || !amountsEqual(got.Amount, want.Amount) {
					t.Fatalf("iter %d: stage %d name/amount = %+v, want %+v", i, j, got, want)
				}
				if completed {
					if got.Status != "done" {
						t.Fatalf("iter %d: completed contract stage %d status = %s, want done", i, j, got.Status)
					}
				} else if got.Status != want.Status {
					t.Fatalf("iter %d: stage %d status = %s, want %s", i, j, got.Status, want.Status)
				}
			}
		} else {
			// Not approved: the contract keeps the original total and stages.
			if finalContract.TotalAmount != f.contract.TotalAmount {
				t.Fatalf("iter %d: unapproved change altered total to %v", i, finalContract.TotalAmount)
			}
		}

		// A failure must be an explicit conflict/business error, never nil+mutated.
		if completeErr != nil && !isConflictOrForbidden(completeErr) {
			t.Fatalf("iter %d: unexpected complete error %v", i, completeErr)
		}
		if approveErr != nil && !isConflictOrForbidden(approveErr) {
			t.Fatalf("iter %d: unexpected approve error %v", i, approveErr)
		}
	}
}

func isConflictOrForbidden(err error) bool {
	var appErr *constants.AppError
	if errors.As(err, &appErr) {
		return appErr.Code == constants.CodeConflict || appErr.Code == constants.CodeForbidden
	}
	return errors.Is(err, constants.ErrForbidden) || errors.Is(err, constants.ErrConflict)
}
