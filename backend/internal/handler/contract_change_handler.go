package handler

import (
	"log/slog"

	"github.com/gin-gonic/gin"

	"github.com/gigmatch/gigmatch/internal/dto"
	"github.com/gigmatch/gigmatch/internal/middleware"
	"github.com/gigmatch/gigmatch/internal/model"
	"github.com/gigmatch/gigmatch/internal/service"
	"github.com/gigmatch/gigmatch/internal/util"
)

// ContractChangeHandler exposes contract change-order endpoints.
type ContractChangeHandler struct {
	svc    *service.ContractChangeService
	logger *slog.Logger
}

// NewContractChangeHandler builds a ContractChangeHandler.
func NewContractChangeHandler(svc *service.ContractChangeService, logger *slog.Logger) *ContractChangeHandler {
	return &ContractChangeHandler{svc: svc, logger: logger}
}

// List handles GET /contracts/:id/changes.
func (h *ContractChangeHandler) List(c *gin.Context) {
	contractID, ok := parseUintParam(c, "id")
	if !ok {
		return
	}
	u := middleware.GetCurrentUser(c)
	changes, err := h.svc.ListByContract(contractID, u.ID)
	if err != nil {
		util.Fail(c, err)
		return
	}
	util.OK(c, changes)
}

// Get handles GET /contracts/:id/changes/:changeId.
func (h *ContractChangeHandler) Get(c *gin.Context) {
	contractID, changeID, u, ok := h.parseChangeContext(c)
	if !ok {
		return
	}
	change, err := h.svc.Get(contractID, changeID, u.ID)
	if err != nil {
		util.Fail(c, err)
		return
	}
	util.OK(c, change)
}

// Create handles POST /contracts/:id/changes.
func (h *ContractChangeHandler) Create(c *gin.Context) {
	contractID, ok := parseUintParam(c, "id")
	if !ok {
		return
	}
	var req dto.CreateContractChangeRequest
	if !util.BindAndValidate(c, &req) {
		return
	}
	u := middleware.GetCurrentUser(c)
	change, err := h.svc.Create(contractID, req, u.ID, u.Name)
	if err != nil {
		util.Fail(c, err)
		return
	}
	util.OK(c, change)
}

// Approve handles POST /contracts/:id/changes/:changeId/approve.
func (h *ContractChangeHandler) Approve(c *gin.Context) {
	contractID, changeID, u, ok := h.parseChangeContext(c)
	if !ok {
		return
	}
	change, contract, err := h.svc.Approve(contractID, changeID, u.ID, u.Name)
	if err != nil {
		util.Fail(c, err)
		return
	}
	util.OK(c, gin.H{"change": change, "contract": contract})
}

// Reject handles POST /contracts/:id/changes/:changeId/reject.
func (h *ContractChangeHandler) Reject(c *gin.Context) {
	contractID, changeID, u, ok := h.parseChangeContext(c)
	if !ok {
		return
	}
	change, err := h.svc.Reject(contractID, changeID, u.ID, u.Name)
	if err != nil {
		util.Fail(c, err)
		return
	}
	util.OK(c, change)
}

// Withdraw handles POST /contracts/:id/changes/:changeId/withdraw.
func (h *ContractChangeHandler) Withdraw(c *gin.Context) {
	contractID, changeID, u, ok := h.parseChangeContext(c)
	if !ok {
		return
	}
	change, err := h.svc.Withdraw(contractID, changeID, u.ID, u.Name)
	if err != nil {
		util.Fail(c, err)
		return
	}
	util.OK(c, change)
}

func (h *ContractChangeHandler) parseChangeContext(c *gin.Context) (uint, uint, *model.User, bool) {
	contractID, ok := parseUintParam(c, "id")
	if !ok {
		return 0, 0, nil, false
	}
	changeID, ok := parseUintParam(c, "changeId")
	if !ok {
		return 0, 0, nil, false
	}
	return contractID, changeID, middleware.GetCurrentUser(c), true
}
