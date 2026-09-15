package dto

// GenerateContractRequest is the payload for creating a contract from a bid.
type GenerateContractRequest struct {
	BidID       uint   `json:"bidId" validate:"required"`
	PaymentType string `json:"paymentType" validate:"required,oneof=installments one_time"`
}

// ChangeStageItem is one stage of a proposed contract change.
type ChangeStageItem struct {
	Name   string  `json:"name" validate:"required,max=100"`
	Amount float64 `json:"amount" validate:"gte=0"`
	Status string  `json:"status" validate:"required,oneof=pending in_progress done"`
	DueAt  string  `json:"dueAt" validate:"max=100"`
}

// CreateContractChangeRequest is the payload for raising a contract change order.
type CreateContractChangeRequest struct {
	Reason      string            `json:"reason" validate:"required,min=2,max=500"`
	Scope       string            `json:"scope" validate:"required,min=2,max=1000"`
	AmountDelta float64           `json:"amountDelta" validate:"min=-99999999,max=99999999"`
	Stages      []ChangeStageItem `json:"stages" validate:"required,min=1,dive"`
}
