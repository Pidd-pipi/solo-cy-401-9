package service

import (
	"fmt"
	"log/slog"

	"gorm.io/gorm"

	"github.com/gigmatch/gigmatch/internal/constants"
	"github.com/gigmatch/gigmatch/internal/model"
	"github.com/gigmatch/gigmatch/internal/repository"
)

// ContractService manages contracts.
type ContractService struct {
	db        *gorm.DB
	contracts *repository.ContractRepository
	changes   *repository.ContractChangeRepository
	logs      *OperationLogService
	logger    *slog.Logger
}

// NewContractService builds a ContractService.
func NewContractService(db *gorm.DB, contracts *repository.ContractRepository, changes *repository.ContractChangeRepository, logs *OperationLogService, logger *slog.Logger) *ContractService {
	return &ContractService{db: db, contracts: contracts, changes: changes, logs: logs, logger: logger}
}

// ListByParty returns contracts involving the caller.
func (s *ContractService) ListByParty(userID uint) ([]model.Contract, error) {
	list, err := s.contracts.ListByParty(userID)
	if err != nil {
		return nil, fmt.Errorf("list contracts: %w", err)
	}
	if err := s.changes.FillActive(list); err != nil {
		return nil, fmt.Errorf("attach active changes: %w", err)
	}
	return list, nil
}

// Get loads a contract with its pending change (if any).
func (s *ContractService) Get(id uint) (*model.Contract, error) {
	c, err := s.contracts.FindByID(id)
	if err != nil {
		return nil, err
	}
	active, err := s.changes.FindPendingByContractID(c.ID)
	if err != nil {
		return nil, fmt.Errorf("load active change: %w", err)
	}
	c.ActiveChange = active
	return c, nil
}

// CreateFromBid builds a contract from an accepted bid.
func (s *ContractService) CreateFromBid(r *model.Requirement, bid *model.Bid, requesterID uint, requesterName string, paymentType string) (*model.Contract, error) {
	if paymentType == "" {
		paymentType = "one_time"
	}
	stages := []model.ContractStage{
		{Name: "项目启动", Amount: bid.Amount * 0.3, Status: "done", DueAt: "签约后3日内"},
		{Name: "中期交付", Amount: bid.Amount * 0.4, Status: "in_progress", DueAt: "工期过半"},
		{Name: "验收结项", Amount: bid.Amount * 0.3, Status: "pending", DueAt: "验收通过后"},
	}
	contract := &model.Contract{
		ContractNo:    fmt.Sprintf("CY-%d-%d", r.ID, bid.ID),
		TotalAmount:   bid.Amount,
		PaymentType:   paymentType,
		Stages:        stages,
		Status:        constants.ContractPendingSignature,
		RequirementID: r.ID,
		PartyAID:      requesterID,
		PartyBID:      bid.BidderID,
	}
	if err := s.contracts.Create(contract); err != nil {
		return nil, fmt.Errorf("create contract: %w", err)
	}
	s.logs.Record(requesterID, requesterName, "contract.create", "contract", contract.ID, fmt.Sprintf("生成合同 %s", contract.ContractNo))
	return contract, nil
}

// Sign confirms a contract by either party.
func (s *ContractService) Sign(id uint, userID uint, userName string) (*model.Contract, error) {
	c, err := s.contracts.FindByID(id)
	if err != nil {
		return nil, err
	}
	if c.PartyAID != userID && c.PartyBID != userID {
		return nil, constants.ErrForbidden
	}
	if c.Status != constants.ContractPendingSignature {
		return nil, constants.NewAppError(constants.CodeConflict, "合同当前不可签署")
	}
	c.Status = constants.ContractInProgress
	if err := s.contracts.Update(c); err != nil {
		return nil, fmt.Errorf("sign contract: %w", err)
	}
	s.logs.Record(userID, userName, "contract.sign", "contract", c.ID, "签署确认合同")
	return c, nil
}

// Complete confirms completion (requester side). A pending change order pauses
// completion until the parties settle it. The status flip, stage update and
// pending-change check run in one transaction with an optimistic-lock guard,
// so a completion racing with an approved change has a single stable outcome:
// exactly one side wins and the loser gets a conflict without any partial
// write.
func (s *ContractService) Complete(id uint, userID uint, userName string) (*model.Contract, error) {
	c, err := s.contracts.FindByID(id)
	if err != nil {
		return nil, err
	}
	if c.PartyAID != userID {
		return nil, constants.ErrForbidden
	}
	if c.Status != constants.ContractInProgress && c.Status != constants.ContractPendingReview {
		return nil, constants.NewAppError(constants.CodeConflict, "合同当前不可完成确认")
	}

	var updated *model.Contract
	err = s.db.Transaction(func(tx *gorm.DB) error {
		contractsTx := s.contracts.WithTx(tx)
		changesTx := s.changes.WithTx(tx)

		// Acquire the contract row lock first — the change-approval flow takes
		// the same lock before it settles an order, so completion and approval
		// are serialized and this completion can never commit against a change
		// being approved at the same time.
		live, txErr := contractsTx.FindByIDForUpdate(c.ID)
		if txErr != nil {
			return txErr
		}
		if live.Status != constants.ContractInProgress && live.Status != constants.ContractPendingReview {
			return constants.NewAppError(constants.CodeConflict, "合同当前不可完成确认")
		}
		pending, txErr := changesTx.FindPendingByContractID(c.ID)
		if txErr != nil {
			return txErr
		}
		if pending != nil {
			return constants.NewAppError(constants.CodeConflict, "存在待处理合同变更，请先处理后再完成合同")
		}

		finalStages := make([]model.ContractStage, len(live.Stages))
		copy(finalStages, live.Stages)
		for i := range finalStages {
			finalStages[i].Status = "done"
		}
		stagesJS, txErr := repository.MarshalStages(finalStages)
		if txErr != nil {
			return txErr
		}
		ok, txErr := contractsTx.UpdateIfCurrent(live.ID, live.LockVersion, map[string]any{
			"status": constants.ContractCompleted,
			"stages": stagesJS,
		}, constants.ContractInProgress, constants.ContractPendingReview)
		if txErr != nil {
			return txErr
		}
		if !ok {
			return constants.NewAppError(constants.CodeConflict, "合同刚被变更或更新，请刷新后重试")
		}
		live.Status = constants.ContractCompleted
		live.Stages = finalStages
		live.LockVersion++
		updated = live
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.logs.Record(userID, userName, "contract.complete", "contract", c.ID, "确认合同完成")
	return updated, nil
}
