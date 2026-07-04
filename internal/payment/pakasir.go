package payment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type PakasirClient struct {
	BaseURL      string
	APIKey       string
	MerchantSlug string
	HTTPClient   *http.Client
}

type CreateTransactionResult struct {
	OrderID        string
	PaymentURL     string
	PaymentMethod  string
	PaymentNumber  string
	Fee            int
	TotalPayment   int
	ExpiredAt      string
	ProviderStatus string
}

type TransactionStatus struct {
	OrderID       string
	Project       string
	Amount        int
	Status        string
	PaymentMethod string
	CompletedAt   string
}

func (c PakasirClient) Enabled() bool {
	return strings.TrimSpace(c.APIKey) != "" && strings.TrimSpace(c.MerchantSlug) != ""
}

func (c PakasirClient) CreateQRISTransaction(ctx context.Context, orderID string, amount int, redirectURL string) (CreateTransactionResult, error) {
	if !c.Enabled() {
		return CreateTransactionResult{}, errors.New("pakasir is not configured")
	}

	body, err := json.Marshal(map[string]any{
		"project":  c.MerchantSlug,
		"order_id": orderID,
		"amount":   amount,
		"api_key":  c.APIKey,
	})
	if err != nil {
		return CreateTransactionResult{}, err
	}

	endpoint := strings.TrimRight(c.baseURL(), "/") + "/api/transactioncreate/qris"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return CreateTransactionResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client().Do(req)
	if err != nil {
		return CreateTransactionResult{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return CreateTransactionResult{}, fmt.Errorf("pakasir create transaction failed: %s", resp.Status)
	}

	var payload struct {
		Payment struct {
			OrderID       string `json:"order_id"`
			Amount        int    `json:"amount"`
			Fee           int    `json:"fee"`
			TotalPayment  int    `json:"total_payment"`
			PaymentMethod string `json:"payment_method"`
			PaymentNumber string `json:"payment_number"`
			ExpiredAt     string `json:"expired_at"`
		} `json:"payment"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return CreateTransactionResult{}, err
	}

	return CreateTransactionResult{
		OrderID:        payload.Payment.OrderID,
		PaymentURL:     c.paymentURL(orderID, amount, redirectURL),
		PaymentMethod:  payload.Payment.PaymentMethod,
		PaymentNumber:  payload.Payment.PaymentNumber,
		Fee:            payload.Payment.Fee,
		TotalPayment:   payload.Payment.TotalPayment,
		ExpiredAt:      payload.Payment.ExpiredAt,
		ProviderStatus: "pending",
	}, nil
}

func (c PakasirClient) TransactionDetail(ctx context.Context, orderID string, amount int) (TransactionStatus, error) {
	if !c.Enabled() {
		return TransactionStatus{}, errors.New("pakasir is not configured")
	}

	query := url.Values{}
	query.Set("project", c.MerchantSlug)
	query.Set("amount", fmt.Sprintf("%d", amount))
	query.Set("order_id", orderID)
	query.Set("api_key", c.APIKey)

	endpoint := strings.TrimRight(c.baseURL(), "/") + "/api/transactiondetail?" + query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return TransactionStatus{}, err
	}

	resp, err := c.client().Do(req)
	if err != nil {
		return TransactionStatus{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return TransactionStatus{}, fmt.Errorf("pakasir transaction detail failed: %s", resp.Status)
	}

	var payload struct {
		Transaction struct {
			OrderID       string `json:"order_id"`
			Project       string `json:"project"`
			Amount        int    `json:"amount"`
			Status        string `json:"status"`
			PaymentMethod string `json:"payment_method"`
			CompletedAt   string `json:"completed_at"`
		} `json:"transaction"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return TransactionStatus{}, err
	}

	return TransactionStatus{
		OrderID:       payload.Transaction.OrderID,
		Project:       payload.Transaction.Project,
		Amount:        payload.Transaction.Amount,
		Status:        payload.Transaction.Status,
		PaymentMethod: payload.Transaction.PaymentMethod,
		CompletedAt:   payload.Transaction.CompletedAt,
	}, nil
}

func (c PakasirClient) paymentURL(orderID string, amount int, redirectURL string) string {
	base := strings.TrimRight(c.baseURL(), "/")
	query := url.Values{}
	query.Set("order_id", orderID)
	query.Set("qris_only", "1")
	redirect := strings.TrimSpace(redirectURL)
	if redirect != "" {
		query.Set("redirect", redirect)
	}
	return fmt.Sprintf("%s/pay/%s/%d?%s", base, c.MerchantSlug, amount, query.Encode())
}

func (c PakasirClient) baseURL() string {
	base := strings.TrimSpace(c.BaseURL)
	if base == "" {
		return "https://app.pakasir.com"
	}
	return base
}

func (c PakasirClient) client() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 15 * time.Second}
}
