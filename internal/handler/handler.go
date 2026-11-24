package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"wallet-service/internal/domain"
	"wallet-service/internal/service"
)

// Service is what the handler needs (an interface keeps the handler thin and testable).
type Service interface {
	CreateTransfer(ctx context.Context, in service.CreateTransferInput) (*service.Result, error)
	GetBalance(ctx context.Context, walletID string) (int64, error)
	CreateWallet(ctx context.Context, id string, opening int64) error
}

type Handler struct{ svc Service }

func New(svc Service) *Handler { return &Handler{svc: svc} }

func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /transfers", h.createTransfer)
	mux.HandleFunc("GET /wallets/{id}/balance", h.getBalance)
	mux.HandleFunc("POST /wallets", h.createWallet)
	return mux
}

type createTransferRequest struct {
	IdempotencyKey string `json:"idempotencyKey"`
	FromWalletID   string `json:"fromWalletId"`
	ToWalletID     string `json:"toWalletId"`
	Amount         int64  `json:"amount"`
}

type transferResponse struct {
	TransferID       string `json:"transferId"`
	Status           string `json:"status"`
	FailureReason    string `json:"failureReason,omitempty"`
	FromWalletID     string `json:"fromWalletId"`
	ToWalletID       string `json:"toWalletId"`
	Amount           int64  `json:"amount"`
	IdempotentReplay bool   `json:"idempotentReplay"`
}

func (h *Handler) createTransfer(w http.ResponseWriter, r *http.Request) {
	var req createTransferRequest
	if !decode(w, r, &req) {
		return
	}
	res, err := h.svc.CreateTransfer(r.Context(), service.CreateTransferInput{
		IdempotencyKey: req.IdempotencyKey,
		FromWalletID:   req.FromWalletID,
		ToWalletID:     req.ToWalletID,
		Amount:         req.Amount,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	code := http.StatusCreated // 201 = new transfer
	if res.Replayed {
		code = http.StatusOK // 200 = duplicate, original result
	}
	t := res.Transfer
	writeJSON(w, code, transferResponse{
		TransferID: t.ID, Status: string(t.Status), FailureReason: t.FailureReason,
		FromWalletID: t.FromWalletID, ToWalletID: t.ToWalletID, Amount: t.Amount,
		IdempotentReplay: res.Replayed,
	})
}

func (h *Handler) getBalance(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	bal, err := h.svc.GetBalance(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"walletId": id, "balance": bal})
}

func (h *Handler) createWallet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID             string `json:"id"`
		OpeningBalance int64  `json:"openingBalance"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := h.svc.CreateWallet(r.Context(), req.ID, req.OpeningBalance); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": req.ID, "balance": req.OpeningBalance})
}

// ---------------------------------------------------------------- helpers

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: " + err.Error()})
		return false
	}
	return true
}

func writeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrInvalidRequest):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	case errors.Is(err, domain.ErrWalletNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
	case errors.Is(err, domain.ErrWalletExists):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	case errors.Is(err, domain.ErrIdempotencyKeyReuse):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
	default:
		slog.Error("internal error", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
