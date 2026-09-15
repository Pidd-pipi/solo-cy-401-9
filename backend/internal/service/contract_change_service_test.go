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
	contractSvc := NewContractService(contractRepo, changeRepo, logSvc, logger)
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

// validChangeRequest builds a conservation-valid change request: done stages
// are preserved and the proposed new total is spread evenly across the
// unfinished stages.
func validChangeRequest(total, delta float64, stages []model.ContractStage, reason, scope string) dto.CreateContractChangeRequest {
	newTotal := round2(total + delta)
	proposed := make([]dto.ChangeStageItem, 0, len(stages))
	unfinished := 0
	for _, st := range stages {
		if st.Status != "done" {
			unfinished++
		}
	}
	each := round2(newTotal / float64(unfinished))
	remaining := round2(newTotal - each*float64(unfinished-1))
	idx := 0
	for _, st := range stages {
		item := dto.ChangeStageItem{Name: st.Name, Status: st.Status, DueAt: st.DueAt}
		if st.Status == "done" {
			item.Amount = st.Amount
		} else {
			idx++
			if idx == unfinished {
				item.Amount = remaining
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
	var unfinishedSum float64
	for _, st := range updated.Stages {
		if st.Status != "done" {
			unfinishedSum += st.Amount
		}
	}
	if round2(unfinishedSum) != 40000 {
		t.Fatalf("unfinished stages sum = %v, want new total 40000", unfinishedSum)
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
