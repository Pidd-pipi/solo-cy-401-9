package model

import (
	"encoding/json"
	"time"

	"gorm.io/gorm"
)

// ContractChange is a change-order proposal raised by either contract party.
// While PendingContractID is non-null the per-contract unique index guarantees
// at most one pending change exists, even under concurrent submissions.
type ContractChange struct {
	ID                uint       `gorm:"primaryKey" json:"id"`
	ContractID        uint       `gorm:"index;not null" json:"contractId"`
	Reason            string     `gorm:"size:500;not null" json:"reason"`
	Scope             string     `gorm:"size:1000;not null" json:"scope"`
	AmountDelta       float64    `gorm:"type:decimal(14,2);not null;default:0" json:"amountDelta"`
	OriginalAmount    float64    `gorm:"type:decimal(14,2);not null" json:"originalAmount"`
	NewAmount         float64    `gorm:"type:decimal(14,2);not null" json:"newAmount"`
	OriginalStagesJS  string     `gorm:"column:original_stages;type:text" json:"-"`
	ProposedStagesJS  string     `gorm:"column:proposed_stages;type:text" json:"-"`
	Status            string     `gorm:"size:24;not null;default:pending;index" json:"status"`
	ProposerID        uint       `gorm:"not null" json:"proposerId"`
	ProposerName      string     `gorm:"size:64" json:"proposerName"`
	ProposerParty     string     `gorm:"size:16;not null" json:"proposerParty"`
	ResponderID       uint       `json:"responderId"`
	ResponderName     string     `gorm:"size:64" json:"responderName"`
	RespondedAt       *time.Time `json:"respondedAt"`
	PendingContractID *uint      `gorm:"uniqueIndex" json:"-"`
	CreatedAt         time.Time  `json:"createdAt"`
	UpdatedAt         time.Time  `json:"-"`

	// Computed fields.
	OriginalStages []ContractStage `gorm:"-" json:"originalStages"`
	ProposedStages []ContractStage `gorm:"-" json:"proposedStages"`
	Contract       *Contract       `gorm:"foreignKey:ContractID" json:"contract,omitempty"`
}

// BeforeSave serializes stage snapshots.
func (ch *ContractChange) BeforeSave(_ *gorm.DB) error {
	if ch.OriginalStages != nil {
		raw, err := json.Marshal(ch.OriginalStages)
		if err != nil {
			return err
		}
		ch.OriginalStagesJS = string(raw)
	}
	if ch.ProposedStages != nil {
		raw, err := json.Marshal(ch.ProposedStages)
		if err != nil {
			return err
		}
		ch.ProposedStagesJS = string(raw)
	}
	return nil
}

// AfterFind restores stage snapshots.
func (ch *ContractChange) AfterFind(_ *gorm.DB) error {
	ch.OriginalStages = []ContractStage{}
	if ch.OriginalStagesJS != "" {
		_ = json.Unmarshal([]byte(ch.OriginalStagesJS), &ch.OriginalStages)
	}
	ch.ProposedStages = []ContractStage{}
	if ch.ProposedStagesJS != "" {
		_ = json.Unmarshal([]byte(ch.ProposedStagesJS), &ch.ProposedStages)
	}
	return nil
}
