package repository

import (
	"errors"
	"fmt"
	"strings"

	"gorm.io/gorm"

	"github.com/gigmatch/gigmatch/internal/constants"
	"github.com/gigmatch/gigmatch/internal/model"
)

// ContractChangeRepository persists contract change orders.
type ContractChangeRepository struct {
	db *gorm.DB
}

// NewContractChangeRepository builds a ContractChangeRepository.
func NewContractChangeRepository(db *gorm.DB) *ContractChangeRepository {
	return &ContractChangeRepository{db: db}
}

// WithTx returns a repository bound to the given transaction.
func (r *ContractChangeRepository) WithTx(tx *gorm.DB) *ContractChangeRepository {
	return &ContractChangeRepository{db: tx}
}

// Create inserts a change order. A duplicate pending change for the same
// contract (unique index) is translated to constants.ErrConflict.
func (r *ContractChangeRepository) Create(ch *model.ContractChange) error {
	err := r.db.Create(ch).Error
	if err != nil {
		if isDuplicateKeyErr(err) {
			return constants.ErrConflict
		}
		return fmt.Errorf("create contract change: %w", err)
	}
	return nil
}

// FindByID loads a change order by id.
func (r *ContractChangeRepository) FindByID(id uint) (*model.ContractChange, error) {
	var ch model.ContractChange
	if err := r.db.First(&ch, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("find contract change by id: %w", err)
	}
	return &ch, nil
}

// FindPendingByContractID returns the pending change of a contract, or nil.
func (r *ContractChangeRepository) FindPendingByContractID(contractID uint) (*model.ContractChange, error) {
	var ch model.ContractChange
	err := r.db.Where("contract_id = ? AND status = ?", contractID, constants.ChangePending).
		First(&ch).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("find pending contract change: %w", err)
	}
	return &ch, nil
}

// FillActive attaches the pending change of each contract (if any).
func (r *ContractChangeRepository) FillActive(contracts []model.Contract) error {
	if len(contracts) == 0 {
		return nil
	}
	ids := make([]uint, 0, len(contracts))
	for i := range contracts {
		ids = append(ids, contracts[i].ID)
	}
	var changes []model.ContractChange
	if err := r.db.Where("pending_contract_id IN ?", ids).Find(&changes).Error; err != nil {
		return fmt.Errorf("fill active contract changes: %w", err)
	}
	byContract := make(map[uint]model.ContractChange, len(changes))
	for _, ch := range changes {
		byContract[ch.ContractID] = ch
	}
	for i := range contracts {
		if ch, ok := byContract[contracts[i].ID]; ok {
			change := ch
			contracts[i].ActiveChange = &change
		}
	}
	return nil
}

// ListByContractID returns the change history of a contract, newest first.
func (r *ContractChangeRepository) ListByContractID(contractID uint) ([]model.ContractChange, error) {
	var changes []model.ContractChange
	if err := r.db.Where("contract_id = ?", contractID).
		Order("created_at DESC").
		Find(&changes).Error; err != nil {
		return nil, fmt.Errorf("list contract changes: %w", err)
	}
	return changes, nil
}

// Transition moves a change order from one status to another only when it is
// still in the expected status. It returns false on a lost race / stale state.
func (r *ContractChangeRepository) Transition(id uint, fromStatus, toStatus string, responderID uint, responderName string) (bool, error) {
	updates := map[string]any{
		"status":              toStatus,
		"pending_contract_id": nil,
	}
	if responderID != 0 {
		updates["responder_id"] = responderID
		updates["responder_name"] = responderName
		updates["responded_at"] = gorm.Expr("CURRENT_TIMESTAMP")
	}
	res := r.db.Model(&model.ContractChange{}).
		Where("id = ? AND status = ?", id, fromStatus).
		Updates(updates)
	if res.Error != nil {
		return false, fmt.Errorf("transition contract change: %w", res.Error)
	}
	return res.RowsAffected == 1, nil
}

func isDuplicateKeyErr(err error) bool {
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "duplicate entry") || strings.Contains(msg, "unique constraint")
}
