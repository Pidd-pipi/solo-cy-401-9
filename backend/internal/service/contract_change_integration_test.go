//go:build integration
// +build integration

// Package service — real-database concurrency tests.
//
// These tests run against a real MySQL/InnoDB server reached over TCP with a
// multi-connection pool. They deliberately do NOT use SQLite, in-memory
// substitutes, mocks, single-connection serialization, or custom doubles: the
// FOR UPDATE row locks, unique index, transaction isolation and connection
// scheduling under real concurrency are exactly what is under test.
//
// Run with:
//
//	TEST_MYSQL_DSN="it:it_pwd@tcp(127.0.0.1:38109)/gigmatch_it?charset=utf8mb4&parseTime=true&loc=Local" \
//	go test -tags integration -count=1 -v ./internal/service/
package service

import (
	"errors"
	"log/slog"
	"os"
	"sync"
	"testing"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/gigmatch/gigmatch/internal/constants"
	"github.com/gigmatch/gigmatch/internal/dto"
	"github.com/gigmatch/gigmatch/internal/model"
	"github.com/gigmatch/gigmatch/internal/repository"
)

const defaultIntegrationDSN = "it:it_pwd@tcp(127.0.0.1:38109)/gigmatch_it?charset=utf8mb4&parseTime=true&loc=Local"

// itEnv holds real-DB wired services for one integration run.
type itEnv struct {
	db        *gorm.DB
	logger    *slog.Logger
	users     *repository.UserRepository
	reqs      *repository.RequirementRepository
	bids      *repository.BidRepository
	contracts *repository.ContractRepository
	changes   *repository.ContractChangeRepository
	logs      *repository.OperationLogRepository
}

func integrationDSN() string {
	if dsn := os.Getenv("TEST_MYSQL_DSN"); dsn != "" {
		return dsn
	}
	return defaultIntegrationDSN
}

// openIntegrationDB connects to real MySQL and ensures the schema exists.
func openIntegrationDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := integrationDSN()
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{
		// Keep the output readable; the suite reports failures explicitly.
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		t.Fatalf("open real mysql (set TEST_MYSQL_DSN): %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	// A genuine multi-connection pool: each concurrent transaction checks out
	// its own TCP connection, mirroring production.
	sqlDB.SetMaxOpenConns(20)
	sqlDB.SetMaxIdleConns(20)

	if err := db.AutoMigrate(
		&model.User{}, &model.Requirement{}, &model.Bid{},
		&model.Contract{}, &model.ContractChange{}, &model.OperationLog{},
	); err != nil {
		t.Fatalf("auto migrate: %v", err)
	}
	return db
}

func newIntegrationEnv(t *testing.T) *itEnv {
	t.Helper()
	db := openIntegrationDB(t)
	return &itEnv{
		db:        db,
		logger:    discardLogger(),
		users:     repository.NewUserRepository(db),
		reqs:      repository.NewRequirementRepository(db),
		bids:      repository.NewBidRepository(db),
		contracts: repository.NewContractRepository(db),
		changes:   repository.NewContractChangeRepository(db),
		logs:      repository.NewOperationLogRepository(db),
	}
}

// wipe removes all business rows so identities stay unique per round.
func (e *itEnv) wipe(t *testing.T) {
	t.Helper()
	_ = e.db.Exec("SET FOREIGN_KEY_CHECKS = 0").Error
	for _, table := range []string{
		"operation_logs", "contract_changes", "contracts", "bids", "requirements", "users",
	} {
		if err := e.db.Exec("TRUNCATE TABLE " + table).Error; err != nil {
			t.Fatalf("truncate %s: %v", table, err)
		}
	}
	_ = e.db.Exec("SET FOREIGN_KEY_CHECKS = 1").Error
}

func (e *itEnv) services() (*ContractService, *ContractChangeService) {
	logSvc := NewOperationLogService(e.logs, e.logger)
	return NewContractService(e.db, e.contracts, e.changes, logSvc, e.logger),
		NewContractChangeService(e.db, e.contracts, e.changes, logSvc, e.logger)
}

// itParties are the two contract counterparties plus an unrelated third user.
type itParties struct {
	a, b, outsider *model.User
}

func (e *itEnv) seedParties(t *testing.T) itParties {
	t.Helper()
	a := &model.User{Username: "it_a", PasswordHash: "x", Name: "甲方", Role: constants.RoleRequester}
	b := &model.User{Username: "it_b", PasswordHash: "x", Name: "乙方", Role: constants.RoleFreelancer}
	out := &model.User{Username: "it_out", PasswordHash: "x", Name: "外人", Role: constants.RoleFreelancer}
	for _, u := range []*model.User{a, b, out} {
		if err := e.users.Create(u); err != nil {
			t.Fatalf("create user: %v", err)
		}
	}
	return itParties{a: a, b: b, outsider: out}
}

// itFixture builds a signed, in-progress contract (done/in-progress/pending
// stages at 30%/40%/30%) and returns the active change proposal builder.
type itFixture struct {
	env      *itEnv
	parties  itParties
	contract *model.Contract
	svc      *ContractService
	change   *ContractChangeService
}

func (e *itEnv) fixture(t *testing.T) *itFixture {
	t.Helper()
	p := e.seedParties(t)
	logSvc := NewOperationLogService(e.logs, e.logger)
	reqSvc := NewRequirementService(e.reqs, e.bids, logSvc, e.logger)
	bidSvc := NewBidService(e.bids, e.reqs, logSvc, e.logger)
	contractSvc, changeSvc := e.services()

	req, err := reqSvc.Create(dto.CreateRequirementRequest{
		Title: "集成测试需求", Description: "真实 MySQL 多连接并发测试", MinBudget: 10000, MaxBudget: 200000, Skills: []string{"Go"},
	}, p.a.ID, p.a.Name, p.a.Role)
	if err != nil {
		t.Fatalf("create requirement: %v", err)
	}
	bid, err := bidSvc.Create(dto.CreateBidRequest{
		RequirementID: req.ID, Amount: 100000, DurationDays: 30, Proposal: "承接",
	}, p.b.ID, p.b.Name, p.b.Role)
	if err != nil {
		t.Fatalf("create bid: %v", err)
	}
	c, err := reqSvc.AcceptBid(req.ID, bid.ID, p.a.ID, p.a.Name, "installments", contractSvc)
	if err != nil {
		t.Fatalf("accept bid: %v", err)
	}
	if _, err := contractSvc.Sign(c.ID, p.b.ID, p.b.Name); err != nil {
		t.Fatalf("sign: %v", err)
	}
	c, err = contractSvc.Get(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	return &itFixture{env: e, parties: p, contract: c, svc: contractSvc, change: changeSvc}
}

// validChange builds a conservation-valid proposal for this fixture's stages.
// Done stages stay frozen; (newTotal - doneSum) is spread across unfinished
// stages. delta is the signed amount adjustment.
func (f *itFixture) validChange(t *testing.T, delta float64, reason string) dto.CreateContractChangeRequest {
	t.Helper()
	c := f.contract
	newTotal := round2(c.TotalAmount + delta)
	var doneSum float64
	var unfinishedIdx []int
	for i, st := range c.Stages {
		if st.Status == "done" {
			doneSum += st.Amount
		} else {
			unfinishedIdx = append(unfinishedIdx, i)
		}
	}
	remaining := round2(newTotal - round2(doneSum))
	n := len(unfinishedIdx)
	each := round2(remaining / float64(n))
	amounts := make([]float64, n)
	for i := 0; i < n-1; i++ {
		amounts[i] = each
	}
	amounts[n-1] = round2(remaining - each*float64(n-1))

	items := make([]dto.ChangeStageItem, len(c.Stages))
	ui := 0
	for i, st := range c.Stages {
		items[i] = dto.ChangeStageItem{Name: st.Name, Status: st.Status, DueAt: st.DueAt, Amount: st.Amount}
		if st.Status != "done" {
			items[i].Amount = amounts[ui]
			ui++
		}
	}
	return dto.CreateContractChangeRequest{Reason: reason, Scope: "集成测试范围说明", AmountDelta: delta, Stages: items}
}

func (f *itFixture) raise(t *testing.T, proposerID uint, proposerName string, req dto.CreateContractChangeRequest) *model.ContractChange {
	t.Helper()
	ch, err := f.change.Create(f.contract.ID, req, proposerID, proposerName)
	if err != nil {
		t.Fatalf("raise change: %v", err)
	}
	return ch
}

// ---- assertions -----------------------------------------------------------

func isConflictErr(err error) bool {
	var appErr *constants.AppError
	if errors.As(err, &appErr) {
		return appErr.Code == constants.CodeConflict
	}
	return errors.Is(err, constants.ErrConflict)
}

// assertContractConsistent verifies the money invariant total == sum(all
// stages) on a fresh read-back from the database.
func (f *itFixture) assertContractConsistent(t *testing.T, round int, label string) *model.Contract {
	t.Helper()
	reloaded, err := f.svc.Get(f.contract.ID)
	if err != nil {
		t.Fatalf("[round %d %s] reload contract: %v", round, label, err)
	}
	var sum float64
	for _, st := range reloaded.Stages {
		sum += st.Amount
	}
	if !amountsEqual(round2(sum), reloaded.TotalAmount) {
		t.Fatalf("[round %d %s] money invariant broken: stage sum %.2f != total %.2f",
			round, label, round2(sum), reloaded.TotalAmount)
	}
	return reloaded
}

// assertHistoryAgrees verifies the change history is consistent with the
// contract terminal state: no pending change on a completed contract, and an
// approved change matches the contract total/stages.
func (f *itFixture) assertHistoryAgrees(t *testing.T, round int, label string, c *model.Contract) {
	t.Helper()
	list, err := f.change.ListByContract(c.ID, f.parties.a.ID)
	if err != nil {
		t.Fatalf("[round %d %s] list history: %v", round, label, err)
	}
	for _, ch := range list {
		switch ch.Status {
		case constants.ChangeApproved:
			if c.TotalAmount != ch.NewAmount {
				t.Fatalf("[round %d %s] approved change newAmount %.2f != contract total %.2f",
					round, label, ch.NewAmount, c.TotalAmount)
			}
		case constants.ChangePending:
			if c.Status == constants.ContractCompleted {
				t.Fatalf("[round %d %s] completed contract still carries a pending change", round, label)
			}
		}
	}
}

func countOutcomes(oks, conflicts int, t *testing.T, round int, label string) {
	t.Helper()
	if oks != 1 || conflicts != 1 {
		t.Fatalf("[round %d %s] expected exactly 1 success and 1 conflict, got ok=%d conflict=%d",
			round, label, oks, conflicts)
	}
}

// runPair executes two operations concurrently behind a shared start gate on
// independently checked-out connections, and counts success/conflict.
type opOutcome struct {
	ok       bool
	conflict bool
	other    error
}

func runPair(opA, opB func() error) (opOutcome, opOutcome) {
	var oa, ob opOutcome
	var wg sync.WaitGroup
	wg.Add(2)
	start := make(chan struct{})
	go func() {
		defer wg.Done()
		<-start
		switch err := opA(); {
		case err == nil:
			oa.ok = true
		case isConflictErr(err):
			oa.conflict = true
		default:
			oa.other = err
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		switch err := opB(); {
		case err == nil:
			ob.ok = true
		case isConflictErr(err):
			ob.conflict = true
		default:
			ob.other = err
		}
	}()
	close(start)
	wg.Wait()
	return oa, ob
}

func (o opOutcome) assertClean(t *testing.T, round int, label, side string) {
	t.Helper()
	if o.other != nil {
		t.Fatalf("[round %d %s] %s unexpected error (must be success or explicit conflict): %v",
			round, label, side, o.other)
	}
}

// ---- tests ----------------------------------------------------------------

const raceRounds = 15

// TestIT_ApproveVsComplete: with a pending change present, approval and
// completion fire simultaneously over independent connections. Every round
// approval must win and completion must be rejected with an explicit conflict
// (completion can never win while a change is pending); the contract is either
// updated+still open, or updated then separately completed later — never left
// half-written or completed against a pending change.
func TestIT_ApproveVsComplete(t *testing.T) {
	env := newIntegrationEnv(t)
	for round := 1; round <= raceRounds; round++ {
		env.wipe(t)
		f := env.fixture(t)
		req := f.validChange(t, 20000, "并发：审批与完成")
		ch := f.raise(t, f.parties.b.ID, f.parties.b.Name, req)

		oa, oc := runPair(
			func() error { // party A approves (as counterparty)
				_, _, err := f.change.Approve(f.contract.ID, ch.ID, f.parties.a.ID, f.parties.a.Name)
				return err
			},
			func() error { // party A completes at the same time
				_, err := f.svc.Complete(f.contract.ID, f.parties.a.ID, f.parties.a.Name)
				return err
			},
		)
		oa.assertClean(t, round, "approve-vs-complete", "approve")
		oc.assertClean(t, round, "approve-vs-complete", "complete")
		if !oa.ok || !oc.conflict {
			t.Fatalf("[round %d approve-vs-complete] approve must win and complete must conflict: approve.ok=%v complete=%+v",
				round, oa.ok, oc)
		}

		c := f.assertContractConsistent(t, round, "approve-vs-complete")
		if c.TotalAmount != ch.NewAmount {
			t.Fatalf("[round %d] total %.2f != approved %.2f", round, c.TotalAmount, ch.NewAmount)
		}
		// A change order is pending-free after approval.
		active, err := env.changes.FindPendingByContractID(c.ID)
		if err != nil {
			t.Fatal(err)
		}
		if active != nil {
			t.Fatalf("[round %d] pending change remains after approval", round)
		}
		f.assertHistoryAgrees(t, round, "approve-vs-complete", c)

		// The winner's contract remains completable afterwards (fresh action on
		// the settled state), proving the loser rolled back without partial write.
		done, err := f.svc.Complete(c.ID, f.parties.a.ID, f.parties.a.Name)
		if err != nil {
			t.Fatalf("[round %d] complete after settle failed: %v", round, err)
		}
		if done.Status != constants.ContractCompleted {
			t.Fatalf("[round %d] status %s, want completed", round, done.Status)
		}
		f.assertContractConsistent(t, round, "post-complete")
	}
}

// TestIT_ApproveVsReject: counterparty approve and reject race; exactly one
// wins each round, the loser gets a conflict, the contract total/stages reflect
// the approved content if approval won, and are untouched if reject won.
func TestIT_ApproveVsReject(t *testing.T) {
	env := newIntegrationEnv(t)
	for round := 1; round <= raceRounds; round++ {
		env.wipe(t)
		f := env.fixture(t)
		origTotal := f.contract.TotalAmount
		origStages := append([]model.ContractStage(nil), f.contract.Stages...)
		req := f.validChange(t, 15000, "并发：审批与拒绝")
		ch := f.raise(t, f.parties.a.ID, f.parties.a.Name, req)

		oa, orr := runPair(
			func() error {
				_, _, err := f.change.Approve(f.contract.ID, ch.ID, f.parties.b.ID, f.parties.b.Name)
				return err
			},
			func() error {
				_, err := f.change.Reject(f.contract.ID, ch.ID, f.parties.b.ID, f.parties.b.Name)
				return err
			},
		)
		oa.assertClean(t, round, "approve-vs-reject", "approve")
		orr.assertClean(t, round, "approve-vs-reject", "reject")
		countOutcomes(boolToInt(oa.ok)+boolToInt(orr.ok), boolToInt(oa.conflict)+boolToInt(orr.conflict), t, round, "approve-vs-reject")

		c := f.assertContractConsistent(t, round, "approve-vs-reject")
		finalChange, err := f.change.Get(f.contract.ID, ch.ID, f.parties.a.ID)
		if err != nil {
			t.Fatal(err)
		}
		switch finalChange.Status {
		case constants.ChangeApproved:
			if c.TotalAmount != ch.NewAmount {
				t.Fatalf("[round %d] approved but total %.2f != %.2f", round, c.TotalAmount, ch.NewAmount)
			}
			for i := range c.Stages {
				if c.Stages[i].Name != ch.ProposedStages[i].Name ||
					c.Stages[i].Status != ch.ProposedStages[i].Status ||
					!amountsEqual(c.Stages[i].Amount, ch.ProposedStages[i].Amount) {
					t.Fatalf("[round %d] approved stage %d mismatch", round, i)
				}
			}
		case constants.ChangeRejected:
			if c.TotalAmount != origTotal {
				t.Fatalf("[round %d] rejected but total changed to %.2f (want %.2f)", round, c.TotalAmount, origTotal)
			}
			for i := range c.Stages {
				if c.Stages[i].Name != origStages[i].Name || c.Stages[i].Status != origStages[i].Status ||
					!amountsEqual(c.Stages[i].Amount, origStages[i].Amount) {
					t.Fatalf("[round %d] rejected but stage %d was altered", round, i)
				}
			}
		default:
			t.Fatalf("[round %d] unexpected change status %s", round, finalChange.Status)
		}
		f.assertHistoryAgrees(t, round, "approve-vs-reject", c)
	}
}

// TestIT_WithdrawVsApprove: proposer withdraw and counterparty approve race;
// exactly one wins, the other gets a conflict, and the contract matches the
// outcome.
func TestIT_WithdrawVsApprove(t *testing.T) {
	env := newIntegrationEnv(t)
	for round := 1; round <= raceRounds; round++ {
		env.wipe(t)
		f := env.fixture(t)
		origTotal := f.contract.TotalAmount
		origStages := append([]model.ContractStage(nil), f.contract.Stages...)
		req := f.validChange(t, -10000, "并发：撤回与审批")
		ch := f.raise(t, f.parties.b.ID, f.parties.b.Name, req)

		ow, oa := runPair(
			func() error { // proposer withdraws
				_, err := f.change.Withdraw(f.contract.ID, ch.ID, f.parties.b.ID, f.parties.b.Name)
				return err
			},
			func() error { // counterparty approves
				_, _, err := f.change.Approve(f.contract.ID, ch.ID, f.parties.a.ID, f.parties.a.Name)
				return err
			},
		)
		ow.assertClean(t, round, "withdraw-vs-approve", "withdraw")
		oa.assertClean(t, round, "withdraw-vs-approve", "approve")
		countOutcomes(boolToInt(ow.ok)+boolToInt(oa.ok), boolToInt(ow.conflict)+boolToInt(oa.conflict), t, round, "withdraw-vs-approve")

		c := f.assertContractConsistent(t, round, "withdraw-vs-approve")
		finalChange, err := f.change.Get(f.contract.ID, ch.ID, f.parties.a.ID)
		if err != nil {
			t.Fatal(err)
		}
		switch finalChange.Status {
		case constants.ChangeApproved:
			if c.TotalAmount != ch.NewAmount {
				t.Fatalf("[round %d] approved but total mismatch", round)
			}
		case constants.ChangeWithdrawn:
			if c.TotalAmount != origTotal {
				t.Fatalf("[round %d] withdrawn but total changed", round)
			}
			for i := range c.Stages {
				if c.Stages[i].Status != origStages[i].Status ||
					!amountsEqual(c.Stages[i].Amount, origStages[i].Amount) {
					t.Fatalf("[round %d] withdrawn but stage %d altered", round, i)
				}
			}
		default:
			t.Fatalf("[round %d] unexpected change status %s", round, finalChange.Status)
		}
		f.assertHistoryAgrees(t, round, "withdraw-vs-approve", c)
	}
}

// TestIT_DoubleApproveOneWins: two concurrent approvals from the counterparty
// over separate connections; exactly one applies and the other conflicts. The
// loser's retry then returns a clear "already processed" conflict instead of
// double-applying.
func TestIT_DoubleApproveOneWins(t *testing.T) {
	env := newIntegrationEnv(t)
	for round := 1; round <= raceRounds; round++ {
		env.wipe(t)
		f := env.fixture(t)
		req := f.validChange(t, 12345, "并发：双重审批")
		ch := f.raise(t, f.parties.a.ID, f.parties.a.Name, req)

		o1, o2 := runPair(
			func() error {
				_, _, err := f.change.Approve(f.contract.ID, ch.ID, f.parties.b.ID, f.parties.b.Name)
				return err
			},
			func() error {
				_, _, err := f.change.Approve(f.contract.ID, ch.ID, f.parties.b.ID, f.parties.b.Name)
				return err
			},
		)
		o1.assertClean(t, round, "double-approve", "approve-1")
		o2.assertClean(t, round, "double-approve", "approve-2")
		countOutcomes(boolToInt(o1.ok)+boolToInt(o2.ok), boolToInt(o1.conflict)+boolToInt(o2.conflict), t, round, "double-approve")

		// A fresh retry by the loser must not apply anything a second time.
		_, _, retryErr := f.change.Approve(f.contract.ID, ch.ID, f.parties.b.ID, f.parties.b.Name)
		if retryErr == nil || !isConflictErr(retryErr) {
			t.Fatalf("[round %d] retry after approval must conflict, got %v", round, retryErr)
		}

		c := f.assertContractConsistent(t, round, "double-approve")
		if c.TotalAmount != ch.NewAmount {
			t.Fatalf("[round %d] total %.2f != %.2f", round, c.TotalAmount, ch.NewAmount)
		}
		finalChange, err := f.change.Get(f.contract.ID, ch.ID, f.parties.a.ID)
		if err != nil {
			t.Fatal(err)
		}
		if finalChange.Status != constants.ChangeApproved {
			t.Fatalf("[round %d] change status %s, want approved", round, finalChange.Status)
		}
		f.assertHistoryAgrees(t, round, "double-approve", c)
	}
}

// TestIT_ConservationRejectionsOnRealDB exercises the validation rules against
// the real database: done-stage freeze, stage-count changes, negative total and
// non-conservation must all be rejected without persisting anything.
func TestIT_ConservationRejectionsOnRealDB(t *testing.T) {
	env := newIntegrationEnv(t)
	env.wipe(t)
	f := env.fixture(t)
	c := f.contract
	baseReq := func() dto.CreateContractChangeRequest {
		r := f.validChange(t, 10000, "守恒校验")
		return r
	}

	reject := func(name string, mutate func(*dto.CreateContractChangeRequest), wantSubstr string) {
		t.Helper()
		r := baseReq()
		mutate(&r)
		_, err := f.change.Create(c.ID, r, f.parties.b.ID, f.parties.b.Name)
		var appErr *constants.AppError
		if !errors.As(err, &appErr) || appErr.Code != constants.CodeBadRequest {
			t.Fatalf("%s: want 400 AppError, got %v", name, err)
		}
		if wantSubstr != "" && !containsErr(appErr.Error(), wantSubstr) {
			t.Fatalf("%s: message %q does not contain %q", name, appErr.Message, wantSubstr)
		}
	}

	reject("tamper done stage amount", func(r *dto.CreateContractChangeRequest) {
		r.Stages[0].Amount += 1
	}, "已完成阶段")
	reject("tamper done stage name", func(r *dto.CreateContractChangeRequest) {
		r.Stages[0].Name = "改名"
	}, "已完成阶段")
	reject("reopen done stage", func(r *dto.CreateContractChangeRequest) {
		r.Stages[0].Status = "pending"
	}, "已完成阶段")
	reject("flip unfinished to done", func(r *dto.CreateContractChangeRequest) {
		r.Stages[1].Status = "done"
	}, "不能通过变更单")
	reject("remove a stage", func(r *dto.CreateContractChangeRequest) {
		r.Stages = r.Stages[:len(r.Stages)-1]
	}, "阶段数量")
	reject("add a stage", func(r *dto.CreateContractChangeRequest) {
		r.Stages = append(r.Stages, dto.ChangeStageItem{Name: "新增", Amount: 1, Status: "pending", DueAt: "无"})
	}, "阶段数量")
	reject("negative total", func(r *dto.CreateContractChangeRequest) {
		// new total = total + delta < 0.
		r.AmountDelta = -(c.TotalAmount + 5000)
		for i := range r.Stages {
			if r.Stages[i].Status != "done" {
				r.Stages[i].Amount = 0
			}
		}
	}, "不能为负")
	reject("new total below settled done amount", func(r *dto.CreateContractChangeRequest) {
		// new total 20000 >= 0 but below the frozen 30000 already settled.
		r.AmountDelta = -80000
		for i := range r.Stages {
			if r.Stages[i].Status != "done" {
				r.Stages[i].Amount = 0
			}
		}
	}, "不能低于已完成阶段金额")
	reject("non conservation", func(r *dto.CreateContractChangeRequest) {
		r.Stages[2].Amount += 500
	}, "金额不守恒")

	// None of the rejected proposals may have persisted a change order or
	// altered the contract.
	pending, err := env.changes.FindPendingByContractID(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pending != nil {
		t.Fatalf("rejected proposal persisted a pending change: %d", pending.ID)
	}
	reloaded, err := f.svc.Get(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.TotalAmount != c.TotalAmount {
		t.Fatalf("rejected validation changed total: %.2f -> %.2f", c.TotalAmount, reloaded.TotalAmount)
	}
}

// TestIT_NonPartyCannotRaise verifies authorization against the real DB.
func TestIT_NonPartyCannotRaise(t *testing.T) {
	env := newIntegrationEnv(t)
	env.wipe(t)
	f := env.fixture(t)
	req := f.validChange(t, 1000, "非当事方")
	_, err := f.change.Create(f.contract.ID, req, f.parties.outsider.ID, f.parties.outsider.Name)
	if !errors.Is(err, constants.ErrForbidden) {
		t.Fatalf("non-party raise err = %v, want forbidden", err)
	}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func containsErr(s, sub string) bool {
	return len(s) >= len(sub) && (indexStr(s, sub) >= 0)
}

func indexStr(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
