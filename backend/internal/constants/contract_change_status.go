package constants

// Contract change order statuses.
const (
	ChangePending   = "pending"   // 待另一方处理
	ChangeApproved  = "approved"  // 另一方已同意，合同已同步更新
	ChangeRejected  = "rejected"  // 另一方已拒绝，合同保持原样
	ChangeWithdrawn = "withdrawn" // 发起人已撤回，合同保持原样
)

// Which contract side raised the change.
const (
	ChangePartyA = "party_a"
	ChangePartyB = "party_b"
)

// ValidChangeStatus reports whether a change status is valid.
func ValidChangeStatus(s string) bool {
	switch s {
	case ChangePending, ChangeApproved, ChangeRejected, ChangeWithdrawn:
		return true
	}
	return false
}
