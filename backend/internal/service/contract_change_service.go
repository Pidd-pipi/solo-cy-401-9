package service

import (
	"errors"
	"fmt"
	"log/slog"
	"math"

	"gorm.io/gorm"

	"github.com/gigmatch/gigmatch/internal/constants"
	"github.com/gigmatch/gigmatch/internal/dto"
	"github.com/gigmatch/gigmatch/internal/model"
	"github.com/gigmatch/gigmatch/internal/repository"
)

// amountTolerance is the accepted rounding error for money conservation checks.
const amountTolerance = 0.01

// changeContractStatuses are the contract statuses on which a change order may
// be raised or applied.
var changeContractStatuses = []string{constants.ContractInProgress, constants.ContractPendingReview}

// ContractChangeService manages the contract change-order lifecycle.
type ContractChangeService struct {
	db        *gorm.DB
	contracts *repository.ContractRepository
	changes   *repository.ContractChangeRepository
	logs      *OperationLogService
	logger    *slog.Logger
}

// NewContractChangeService builds a ContractChangeService.
func NewContractChangeService(db *gorm.DB, contracts *repository.ContractRepository, changes *repository.ContractChangeRepository, logs *OperationLogService, logger *slog.Logger) *ContractChangeService {
	return &ContractChangeService{db: db, contracts: contracts, changes: changes, logs: logs, logger: logger}
}

// ListByContract returns the change history of a contract (newest first).
func (s *ContractChangeService) ListByContract(contractID uint, userID uint) ([]model.ContractChange, error) {
	c, err := s.loadPartyContract(contractID, userID)
	if err != nil {
		return nil, err
	}
	list, err := s.changes.ListByContractID(c.ID)
	if err != nil {
		return nil, fmt.Errorf("list contract changes: %w", err)
	}
	return list, nil
}

// Get loads a single change order, accessible only to the contract parties.
func (s *ContractChangeService) Get(contractID, changeID, userID uint) (*model.ContractChange, error) {
	if _, err := s.loadPartyContract(contractID, userID); err != nil {
		return nil, err
	}
	return s.loadOwnedChange(contractID, changeID)
}

// Create raises a new pending change order.
func (s *ContractChangeService) Create(contractID uint, req dto.CreateContractChangeRequest, userID uint, userName string) (*model.ContractChange, error) {
	c, err := s.contracts.FindByID(contractID)
	if err != nil {
		return nil, err
	}
	if c.PartyAID != userID && c.PartyBID != userID {
		return nil, constants.ErrForbidden
	}
	if c.Status != constants.ContractInProgress && c.Status != constants.ContractPendingReview {
		return nil, constants.NewAppError(constants.CodeConflict, "仅进行中或待确认完成的合同可发起变更")
	}
	pending, err := s.changes.FindPendingByContractID(c.ID)
	if err != nil {
		return nil, err
	}
	if pending != nil {
		return nil, constants.NewAppError(constants.CodeConflict, "该合同已存在待处理变更，请勿重复发起")
	}

	proposed := make([]model.ContractStage, 0, len(req.Stages))
	for _, st := range req.Stages {
		proposed = append(proposed, model.ContractStage{Name: st.Name, Amount: st.Amount, Status: st.Status, DueAt: st.DueAt})
	}
	newAmount, err := validateChangeStages(c.Stages, proposed, c.TotalAmount, req.AmountDelta)
	if err != nil {
		return nil, err
	}

	original := make([]model.ContractStage, len(c.Stages))
	copy(original, c.Stages)
	party := constants.ChangePartyA
	if userID == c.PartyBID {
		party = constants.ChangePartyB
	}
	change := &model.ContractChange{
		ContractID:        c.ID,
		Reason:            req.Reason,
		Scope:             req.Scope,
		AmountDelta:       roundAmount(req.AmountDelta),
		OriginalAmount:    c.TotalAmount,
		NewAmount:         newAmount,
		OriginalStages:    original,
		ProposedStages:    proposed,
		Status:            constants.ChangePending,
		ProposerID:        userID,
		ProposerName:      userName,
		ProposerParty:     party,
		PendingContractID: &c.ID,
	}

	// Inserting the order and taking an optimistic-lock step on the contract are
	// committed together. The version bump serializes the proposal against a
	// concurrent completion/approval; a lost race rolls the whole insert back.
	err = s.db.Transaction(func(tx *gorm.DB) error {
		changesTx := s.changes.WithTx(tx)
		contractsTx := s.contracts.WithTx(tx)

		pendingTx, txErr := changesTx.FindPendingByContractID(c.ID)
		if txErr != nil {
			return txErr
		}
		if pendingTx != nil {
			return constants.NewAppError(constants.CodeConflict, "该合同已存在待处理变更，请勿重复发起")
		}
		ok, txErr := contractsTx.UpdateIfCurrent(c.ID, c.LockVersion, nil, changeContractStatuses...)
		if txErr != nil {
			return txErr
		}
		if !ok {
			return constants.NewAppError(constants.CodeConflict, "合同状态已变化，请刷新后重试")
		}
		if err := changesTx.Create(change); err != nil {
			if errors.Is(err, constants.ErrConflict) {
				return constants.NewAppError(constants.CodeConflict, "该合同已存在待处理变更，请勿重复发起")
			}
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.logs.Record(userID, userName, "contract_change.create", "contract_change", change.ID, fmt.Sprintf("发起合同变更 %s，金额 %.2f", c.ContractNo, req.AmountDelta))
	return change, nil
}

// Approve applies the change atomically: the order is settled and the contract
// total/stages are updated in one transaction. A lost race leaves everything
// untouched and reports a conflict.
func (s *ContractChangeService) Approve(contractID, changeID, userID uint, userName string) (*model.ContractChange, *model.Contract, error) {
	return s.settle(contractID, changeID, userID, userName, constants.ChangeApproved, "同意")
}

// Reject settles the order as rejected without touching the contract.
func (s *ContractChangeService) Reject(contractID, changeID, userID uint, userName string) (*model.ContractChange, error) {
	ch, _, err := s.settle(contractID, changeID, userID, userName, constants.ChangeRejected, "拒绝")
	return ch, err
}

// Withdraw cancels a pending order raised by the caller.
func (s *ContractChangeService) Withdraw(contractID, changeID, userID uint, userName string) (*model.ContractChange, error) {
	c, err := s.contracts.FindByID(contractID)
	if err != nil {
		return nil, err
	}
	if c.PartyAID != userID && c.PartyBID != userID {
		return nil, constants.ErrForbidden
	}
	ch, err := s.loadOwnedChange(c.ID, changeID)
	if err != nil {
		return nil, err
	}
	if ch.Status != constants.ChangePending {
		return nil, constants.NewAppError(constants.CodeConflict, "变更单当前状态不可撤回")
	}
	if ch.ProposerID != userID {
		return nil, constants.NewAppError(constants.CodeForbidden, "仅变更发起人可撤回")
	}
	ok, err := s.changes.Transition(ch.ID, constants.ChangePending, constants.ChangeWithdrawn, 0, "")
	if err != nil {
		return nil, fmt.Errorf("withdraw contract change: %w", err)
	}
	if !ok {
		return nil, constants.NewAppError(constants.CodeConflict, "变更单已被处理，请刷新后重试")
	}
	s.logs.Record(userID, userName, "contract_change.withdraw", "contract_change", ch.ID, "撤回合同变更")
	return s.changes.FindByID(ch.ID)
}

func (s *ContractChangeService) settle(contractID, changeID, userID uint, userName, toStatus, action string) (*model.ContractChange, *model.Contract, error) {
	c, err := s.contracts.FindByID(contractID)
	if err != nil {
		return nil, nil, err
	}
	if c.PartyAID != userID && c.PartyBID != userID {
		return nil, nil, constants.ErrForbidden
	}
	ch, err := s.loadOwnedChange(c.ID, changeID)
	if err != nil {
		return nil, nil, err
	}
	if ch.Status != constants.ChangePending {
		return nil, nil, constants.NewAppError(constants.CodeConflict, "变更单当前状态不可处理")
	}
	if ch.ProposerID == userID {
		return nil, nil, constants.NewAppError(constants.CodeForbidden, "仅合同另一方可处理该变更单")
	}

	var updated *model.Contract
	err = s.db.Transaction(func(tx *gorm.DB) error {
		changesTx := s.changes.WithTx(tx)
		contractsTx := s.contracts.WithTx(tx)

		// Lock the contract row before touching the change order. Completion
		// acquires the same lock first, so the two flows are serialized: the
		// loser re-reads the winner's committed state and either proceeds
		// consistently or fails cleanly — never a partial write.
		locked, txErr := contractsTx.FindByIDForUpdate(c.ID)
		if txErr != nil {
			return txErr
		}
		if toStatus == constants.ChangeApproved &&
			locked.Status != constants.ContractInProgress && locked.Status != constants.ContractPendingReview {
			return constants.NewAppError(constants.CodeConflict, "合同已完成，无法应用变更")
		}

		// Conditional transition is the second concurrency guard: approve/reject/
		// withdraw races can affect at most one row.
		ok, txErr := changesTx.Transition(ch.ID, constants.ChangePending, toStatus, userID, userName)
		if txErr != nil {
			return txErr
		}
		if !ok {
			return constants.NewAppError(constants.CodeConflict, "变更单已被处理，请刷新后重试")
		}

		if toStatus != constants.ChangeApproved {
			return nil
		}

		stagesJS, txErr := repository.MarshalStages(ch.ProposedStages)
		if txErr != nil {
			return txErr
		}
		ok, txErr = contractsTx.UpdateIfCurrent(locked.ID, locked.LockVersion, map[string]any{
			"total_amount": ch.NewAmount,
			"stages":       stagesJS,
		}, changeContractStatuses...)
		if txErr != nil {
			return txErr
		}
		if !ok {
			return constants.NewAppError(constants.CodeConflict, "合同已完成或已被另一方处理，变更未应用")
		}
		// Read back inside the transaction (the row is locked for the rest of
		// this unit of work) and verify the persisted contract fully matches the
		// approved order. Any inconsistency rolls back both rows, so no partial
		// update can survive a concurrent completion or stale snapshot.
		written, txErr := contractsTx.FindByID(locked.ID)
		if txErr != nil {
			return txErr
		}
		if txErr := verifyAppliedChange(written, ch); txErr != nil {
			return fmt.Errorf("verify applied change: %w", txErr)
		}
		updated = written
		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	finalChange, err := s.changes.FindByID(ch.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("reload contract change: %w", err)
	}
	if toStatus == constants.ChangeApproved {
		// Post-commit read-back: a concurrent completion may legitimately flip
		// stage statuses to done afterwards, but the immutable money invariant
		// must still hold — total == sum of every stage amount.
		reloaded, rerr := s.contracts.FindByID(c.ID)
		if rerr != nil {
			return nil, nil, fmt.Errorf("reload approved contract: %w", rerr)
		}
		if rerr := verifyContractMoneyInvariant(reloaded); rerr != nil {
			return nil, nil, rerr
		}
		updated = reloaded
	}
	s.logs.Record(userID, userName, "contract_change."+toStatus, "contract_change", ch.ID, action+"合同变更")
	return finalChange, updated, nil
}

// verifyAppliedChange is run inside the approval transaction against the
// freshly written row. It enforces total == sum(all stages) == approved new
// amount and a stage-for-stage match with the approved proposal.
func verifyAppliedChange(c *model.Contract, ch *model.ContractChange) error {
	if err := verifyContractMoneyInvariant(c); err != nil {
		return err
	}
	if !amountsEqual(c.TotalAmount, ch.NewAmount) {
		return fmt.Errorf("合同金额一致性校验失败：合同总额 %.2f 不等于已批准新总额 %.2f", c.TotalAmount, ch.NewAmount)
	}
	if len(c.Stages) != len(ch.ProposedStages) {
		return fmt.Errorf("合同金额一致性校验失败：阶段数量不匹配")
	}
	for i := range c.Stages {
		got, want := c.Stages[i], ch.ProposedStages[i]
		if got.Name != want.Name || got.Status != want.Status || !amountsEqual(got.Amount, want.Amount) {
			return fmt.Errorf("合同金额一致性校验失败：第 %d 个阶段与已批准内容不一致", i+1)
		}
	}
	return nil
}

// verifyContractMoneyInvariant enforces total == sum of every stage amount.
func verifyContractMoneyInvariant(c *model.Contract) error {
	var stageSum float64
	for _, st := range c.Stages {
		stageSum += st.Amount
	}
	stageSum = roundAmount(stageSum)
	if !amountsEqual(stageSum, c.TotalAmount) {
		return fmt.Errorf("合同金额一致性校验失败：阶段金额合计 %.2f 不等于合同总额 %.2f", stageSum, c.TotalAmount)
	}
	return nil
}

func (s *ContractChangeService) loadPartyContract(contractID, userID uint) (*model.Contract, error) {
	c, err := s.contracts.FindByID(contractID)
	if err != nil {
		return nil, err
	}
	if c.PartyAID != userID && c.PartyBID != userID {
		return nil, constants.ErrForbidden
	}
	return c, nil
}

func (s *ContractChangeService) loadOwnedChange(contractID, changeID uint) (*model.ContractChange, error) {
	ch, err := s.changes.FindByID(changeID)
	if err != nil {
		return nil, err
	}
	if ch.ContractID != contractID {
		return nil, repository.ErrNotFound
	}
	return ch, nil
}

// validateChangeStages enforces money conservation while preserving settled
// milestones:
//   - the stage list keeps the same length, and every originally done stage is
//     frozen — identical name, amount and status;
//   - only unfinished stages may change, and a previously unfinished stage may
//     not be flipped to done through a change order;
//   - proposed total = current total + amount delta must equal
//     sum(frozen done amounts) + sum(proposed unfinished amounts), i.e. the
//     finished payments plus the remaining work.
//
// It returns the validated proposed new total.
func validateChangeStages(original, proposed []model.ContractStage, originalTotal, delta float64) (float64, error) {
	newAmount := roundAmount(originalTotal + delta)
	if newAmount < 0 {
		return 0, constants.NewAppError(constants.CodeBadRequest, "变更后合同总额不能为负")
	}
	if len(proposed) == 0 {
		return 0, constants.NewAppError(constants.CodeBadRequest, "至少保留一个合同阶段")
	}
	if len(proposed) != len(original) {
		return 0, constants.NewAppError(constants.CodeBadRequest, "已完成阶段不可删除，阶段数量必须保持不变")
	}

	var doneSum, unfinishedSum float64
	for i := range original {
		got, want := proposed[i], original[i]
		switch {
		case want.Status == "done":
			if got.Status != "done" || got.Name != want.Name || !amountsEqual(got.Amount, want.Amount) {
				return 0, constants.NewAppError(constants.CodeBadRequest,
					fmt.Sprintf("已完成阶段「%s」的名称、金额和状态不可调整", want.Name))
			}
			doneSum += got.Amount
		case got.Status == "done":
			return 0, constants.NewAppError(constants.CodeBadRequest,
				fmt.Sprintf("阶段「%s」尚未完成，不能通过变更单标记为已完成", want.Name))
		default:
			if got.Amount < 0 {
				return 0, constants.NewAppError(constants.CodeBadRequest, "阶段金额不能为负")
			}
			unfinishedSum += got.Amount
		}
	}

	if roundAmount(doneSum) > newAmount {
		return 0, constants.NewAppError(constants.CodeBadRequest,
			fmt.Sprintf("新总额 %.2f 不能低于已完成阶段金额合计 %.2f", newAmount, roundAmount(doneSum)))
	}
	if !amountsEqual(roundAmount(doneSum+unfinishedSum), newAmount) {
		return 0, constants.NewAppError(constants.CodeBadRequest,
			fmt.Sprintf("金额不守恒：已完成金额 %.2f 与未完成阶段金额合计 %.2f 之和必须等于新总额 %.2f",
				roundAmount(doneSum), roundAmount(unfinishedSum), newAmount))
	}
	return newAmount, nil
}

func roundAmount(v float64) float64 {
	return math.Round(v*100) / 100
}

func amountsEqual(a, b float64) bool {
	diff := a - b
	if diff < 0 {
		diff = -diff
	}
	return diff <= amountTolerance
}
