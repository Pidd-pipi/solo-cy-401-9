package repository

import (
	"encoding/json"
	"errors"
	"fmt"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/gigmatch/gigmatch/internal/model"
)

// ContractRepository persists contracts.
type ContractRepository struct {
	db *gorm.DB
}

// NewContractRepository builds a ContractRepository.
func NewContractRepository(db *gorm.DB) *ContractRepository {
	return &ContractRepository{db: db}
}

// WithTx returns a repository bound to the given transaction.
func (r *ContractRepository) WithTx(tx *gorm.DB) *ContractRepository {
	return &ContractRepository{db: tx}
}

// Create inserts a contract.
func (r *ContractRepository) Create(c *model.Contract) error {
	if err := r.db.Create(c).Error; err != nil {
		return fmt.Errorf("create contract: %w", err)
	}
	return nil
}

// ListByParty returns contracts where the user is either party.
func (r *ContractRepository) ListByParty(userID uint) ([]model.Contract, error) {
	var contracts []model.Contract
	if err := r.db.Where("party_a_id = ? OR party_b_id = ?", userID, userID).
		Preload("PartyA").
		Preload("PartyB").
		Preload("Requirement").
		Order("created_at DESC").
		Find(&contracts).Error; err != nil {
		return nil, fmt.Errorf("list contracts: %w", err)
	}
	return contracts, nil
}

// FindByID loads a contract by primary key.
func (r *ContractRepository) FindByID(id uint) (*model.Contract, error) {
	var c model.Contract
	err := r.db.Preload("PartyA").
		Preload("PartyB").
		Preload("Requirement").
		First(&c, id).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("find contract by id: %w", err)
	}
	return &c, nil
}

// FindByIDForUpdate loads a contract with a pessimistic row lock. Callers must
// run it inside a transaction. On InnoDB it serializes concurrent change
// approval against completion (both acquire this same lock first), so a change
// order can never be settled against a contract row that another transaction is
// simultaneously flipping to completed. The SQLite driver ignores the locking
// clause, and its single-connection test pool provides equivalent serialization.
func (r *ContractRepository) FindByIDForUpdate(id uint) (*model.Contract, error) {
	var c model.Contract
	err := r.db.Clauses(clause.Locking{Strength: "UPDATE"}).First(&c, id).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("find contract by id for update: %w", err)
	}
	return &c, nil
}

// Update persists contract changes.
func (r *ContractRepository) Update(c *model.Contract) error {
	if err := r.db.Save(c).Error; err != nil {
		return fmt.Errorf("update contract: %w", err)
	}
	return nil
}

// UpdateIfCurrent applies a conditional optimistic-lock update. It only touches
// a row whose lock_version is expectedVersion and whose status is one of
// allowedStatuses (when provided), and it bumps lock_version in the same
// statement. RowsAffected == 0 means a concurrent mutation won the race; the
// caller must roll back and report a conflict.
func (r *ContractRepository) UpdateIfCurrent(id uint, expectedVersion int, fields map[string]any, allowedStatuses ...string) (bool, error) {
	updates := map[string]any{
		"lock_version": gorm.Expr("lock_version + 1"),
	}
	for k, v := range fields {
		updates[k] = v
	}
	query := r.db.Model(&model.Contract{}).
		Where("id = ? AND lock_version = ?", id, expectedVersion)
	if len(allowedStatuses) > 0 {
		query = query.Where("status IN ?", allowedStatuses)
	}
	res := query.Updates(updates)
	if res.Error != nil {
		return false, fmt.Errorf("conditional update contract: %w", res.Error)
	}
	return res.RowsAffected == 1, nil
}

// MarshalStages serializes a stage list the same way the Contract BeforeSave
// hook does, so conditional map updates can write the stages column.
func MarshalStages(stages []model.ContractStage) (string, error) {
	raw, err := json.Marshal(stages)
	if err != nil {
		return "", fmt.Errorf("marshal stages: %w", err)
	}
	return string(raw), nil
}

// CountByParty returns the number of contracts involving a user.
func (r *ContractRepository) CountByParty(userID uint) (int64, error) {
	var count int64
	if err := r.db.Model(&model.Contract{}).
		Where("party_a_id = ? OR party_b_id = ?", userID, userID).
		Count(&count).Error; err != nil {
		return 0, fmt.Errorf("count contracts: %w", err)
	}
	return count, nil
}
