package handler_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wallet-service/internal/handler"
	"wallet-service/internal/service"
	"wallet-service/internal/testutil"
)

func newHandler(t *testing.T) http.Handler {
	t.Helper()
	pool, store := testutil.NewDB(t)
	testutil.SeedWallet(t, pool, "wallet_1", 500)
	testutil.SeedWallet(t, pool, "wallet_2", 0)
	return handler.New(service.New(store)).Routes()
}

func do(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func body(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &m); err != nil {
		t.Fatalf("bad json %q: %v", rr.Body.String(), err)
	}
	return m
}

const transferBody = `{"idempotencyKey":"abc123","fromWalletId":"wallet_1","toWalletId":"wallet_2","amount":100}`

func TestCreateTransferThenReplay(t *testing.T) {
	h := newHandler(t)

	rr := do(h, "POST", "/transfers", transferBody)
	if rr.Code != http.StatusCreated {
		t.Fatalf("first call: want 201, got %d: %s", rr.Code, rr.Body)
	}
	first := body(t, rr)
	if first["status"] != "PROCESSED" || first["idempotentReplay"] != false {
		t.Fatalf("unexpected: %v", first)
	}

	rr = do(h, "POST", "/transfers", transferBody)
	if rr.Code != http.StatusOK {
		t.Fatalf("replay: want 200, got %d: %s", rr.Code, rr.Body)
	}
	second := body(t, rr)
	if second["transferId"] != first["transferId"] || second["idempotentReplay"] != true {
		t.Fatalf("replay must return original: %v vs %v", second, first)
	}

	rr = do(h, "GET", "/wallets/wallet_1/balance", "")
	if rr.Code != 200 || body(t, rr)["balance"] != float64(400) {
		t.Fatalf("balance wrong: %d %s", rr.Code, rr.Body)
	}
}

func TestErrorMapping(t *testing.T) {
	h := newHandler(t)
	do(h, "POST", "/transfers", transferBody) // key abc123 used with amount 100

	cases := []struct {
		name string
		body string
		want int
	}{
		{"malformed json", `{bad`, 400},
		{"unknown field", `{"idempotencyKey":"x","foo":1}`, 400},
		{"zero amount", `{"idempotencyKey":"k","fromWalletId":"wallet_1","toWalletId":"wallet_2","amount":0}`, 400},
		{"same wallet", `{"idempotencyKey":"k","fromWalletId":"wallet_1","toWalletId":"wallet_1","amount":5}`, 400},
		{"missing key", `{"fromWalletId":"wallet_1","toWalletId":"wallet_2","amount":5}`, 400},
		{"unknown wallet", `{"idempotencyKey":"k","fromWalletId":"wallet_1","toWalletId":"nope","amount":5}`, 404},
		{"key reuse with different amount", `{"idempotencyKey":"abc123","fromWalletId":"wallet_1","toWalletId":"wallet_2","amount":999}`, 422},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rr := do(h, "POST", "/transfers", c.body)
			if rr.Code != c.want {
				t.Fatalf("want %d, got %d: %s", c.want, rr.Code, rr.Body)
			}
		})
	}
}

func TestBalanceOfUnknownWallet(t *testing.T) {
	h := newHandler(t)
	if rr := do(h, "GET", "/wallets/ghost/balance", ""); rr.Code != 404 {
		t.Fatalf("want 404, got %d", rr.Code)
	}
}
