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
	if err := validateChangeStages(c.Stages, proposed, c.TotalAmount, req.AmountDelta); err != nil {
		return nil, err
	}

	newAmount := roundAmount(c.TotalAmount + req.AmountDelta)
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
	if err := s.changes.Create(change); err != nil {
		if errors.Is(err, constants.ErrConflict) {
			return nil, constants.NewAppError(constants.CodeConflict, "该合同已存在待处理变更，请勿重复发起")
		}
		return nil, fmt.Errorf("create contract change: %w", err)
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

		// Conditional transition is the concurrency guard: approve/reject/withdraw
		// races can affect at most one row.
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

		live, txErr := contractsTx.FindByID(c.ID)
		if txErr != nil {
			return txErr
		}
		if live.Status != constants.ContractInProgress && live.Status != constants.ContractPendingReview {
			return constants.NewAppError(constants.CodeConflict, "合同状态已变化，无法应用变更")
		}
		live.TotalAmount = ch.NewAmount
		live.Stages = append([]model.ContractStage(nil), ch.ProposedStages...)
		if txErr := contractsTx.Update(live); txErr != nil {
			return txErr
		}
		updated = live
		return nil
	})
	if err != nil {
		var appErr *constants.AppError
		if errors.As(err, &appErr) {
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("%s contract change: %w", action, err)
	}

	finalChange, err := s.changes.FindByID(ch.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("reload contract change: %w", err)
	}
	s.logs.Record(userID, userName, "contract_change."+toStatus, "contract_change", ch.ID, action+"合同变更")
	return finalChange, updated, nil
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

// validateChangeStages enforces the money-conservation invariant: the amounts
// of unfinished stages (status != done) must add up to the proposed new total
// (current total + amount delta).
func validateChangeStages(original, proposed []model.ContractStage, originalTotal, delta float64) error {
	_ = original
	newAmount := roundAmount(originalTotal + delta)
	if newAmount < 0 {
		return constants.NewAppError(constants.CodeBadRequest, "变更后合同总额不能为负")
	}
	if len(proposed) == 0 {
		return constants.NewAppError(constants.CodeBadRequest, "至少保留一个合同阶段")
	}

	var unfinishedSum float64
	for _, st := range proposed {
		if st.Status != "done" {
			unfinishedSum += st.Amount
		}
	}
	if !amountsEqual(roundAmount(unfinishedSum), newAmount) {
		return constants.NewAppError(constants.CodeBadRequest,
			fmt.Sprintf("金额不守恒：未完成阶段金额合计 %.2f 必须等于新总额 %.2f", roundAmount(unfinishedSum), newAmount))
	}
	return nil
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
