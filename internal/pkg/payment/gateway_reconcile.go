package payment

// Reconciliation for the SePay Payment Gateway: the IPN's safety net.
//
// An IPN that never arrives (SePay gave up, or our endpoint was briefly down)
// leaves a buyer who paid on SePay's page with no code. This polls SePay's
// order API for orders still unpaid on our side and settles the ones SePay
// reports CAPTURED — through the same ApplyGatewayIPN path, so a code is minted
// exactly once whether the IPN or this job gets there first.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/quizzzone/backend/internal/config"
	"github.com/quizzzone/backend/internal/db"
	"github.com/quizzzone/backend/internal/model"
)

// gatewayReconcileSettle leaves very fresh orders to the IPN, which normally
// lands within seconds; polling them too would only race it.
const gatewayReconcileSettle = 15 * time.Minute

// GatewayReconcileConfigured reports whether a gateway reconcile can run.
func GatewayReconcileConfigured() bool {
	return UsesGateway() && gatewayConfigured()
}

// gatewayAPIBaseOverride points the order API at a test server; empty in prod.
var gatewayAPIBaseOverride string

func gatewayAPIBase() string {
	if gatewayAPIBaseOverride != "" {
		return gatewayAPIBaseOverride
	}
	if config.AppConfig != nil && config.AppConfig.SePayPGEnv == "production" {
		return "https://pgapi.sepay.vn"
	}
	return "https://pgapi-sandbox.sepay.vn"
}

// gatewayOrderDetail is the subset of GET /v1/order/detail/{invoice} we read.
type gatewayOrderDetail struct {
	Data struct {
		OrderID      flexString `json:"order_id"`
		OrderStatus  string     `json:"order_status"`
		OrderAmount  flexString `json:"order_amount"`
		CustomerID   flexString `json:"customer_id"`
		Transactions []struct {
			ID                flexString `json:"id"`
			PaymentMethod     string     `json:"payment_method"`
			TransactionType   string     `json:"transaction_type"`
			TransactionStatus string     `json:"transaction_status"`
			TransactionAmount flexString `json:"transaction_amount"`
			TransactionDate   string     `json:"transaction_date"`
		} `json:"transactions"`
	} `json:"data"`
}

// fetchGatewayOrder queries SePay for one order by its invoice number (our
// order code). A 404 means SePay never saw a payment for it: return nil, nil.
func fetchGatewayOrder(ctx context.Context, invoice string) (*gatewayOrderDetail, error) {
	c := config.AppConfig
	u := gatewayAPIBase() + "/v1/order/detail/" + invoice
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(c.SePayPGMerchantID, c.SePayPGSecretKey)
	req.Header.Set("Accept", "application/json")
	resp, err := sepayHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("sepay order detail %s: http %d", invoice, resp.StatusCode)
	}
	var d gatewayOrderDetail
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return nil, err
	}
	return &d, nil
}

// ipnFromDetail synthesises an ORDER_PAID notification from an order the detail
// API reports CAPTURED, so reconcile and the real IPN share one settle path.
// The transaction id is deterministic ("reconcile:<code>") so repeated runs
// dedup; it cannot collide with SePay's own IPN id, and settle refuses to grant
// twice on an order anyway.
func ipnFromDetail(code string, d *gatewayOrderDetail) GatewayIPN {
	var n GatewayIPN
	n.NotificationType = ipnOrderPaid
	n.Order.OrderStatus = d.Data.OrderStatus
	n.Order.OrderID = d.Data.OrderID
	n.Order.OrderAmount = d.Data.OrderAmount
	n.Order.OrderCurrency = "VND"
	n.Order.OrderInvoiceNumber = flexString(code)
	n.Order.OrderDescription = "reconciled from SePay order API"
	n.Customer.CustomerID = d.Data.CustomerID
	n.Transaction.ID = flexString("reconcile:" + code)
	n.Transaction.TransactionType = "PAYMENT"
	n.Transaction.TransactionStatus = "APPROVED"
	n.Transaction.TransactionCurrency = "VND"
	n.Transaction.TransactionAmount = d.Data.OrderAmount
	// Prefer a real approved payment transaction's fields when the API gives one.
	for _, t := range d.Data.Transactions {
		if t.TransactionStatus == "APPROVED" && t.TransactionType == "PAYMENT" {
			n.Transaction.PaymentMethod = t.PaymentMethod
			n.Transaction.TransactionDate = t.TransactionDate
			if strings.TrimSpace(string(t.TransactionAmount)) != "" {
				n.Transaction.TransactionAmount = t.TransactionAmount
			}
			break
		}
	}
	return n
}

// ReconcileGateway settles orders SePay reports paid but we still hold as
// unpaid. onApplied runs after each newly settled order (receipt, alert),
// outside the transaction.
func ReconcileGateway(ctx context.Context, window time.Duration, onApplied func(*model.BankTransaction, Outcome)) (ReconcileResult, error) {
	res := ReconcileResult{Since: time.Now().Add(-window)}
	if !GatewayReconcileConfigured() {
		return res, ErrReconcileNotConfigured
	}

	var orders []model.PaymentOrder
	cutoff := time.Now().Add(-gatewayReconcileSettle)
	if err := db.DB.
		Where("status IN ? AND created_at > ? AND created_at < ?",
			[]string{model.OrderPending, model.OrderExpired}, res.Since, cutoff).
		Order("id").Find(&orders).Error; err != nil {
		return res, err
	}
	res.Fetched = len(orders)

	for i := range orders {
		o := orders[i]
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		detail, err := fetchGatewayOrder(ctx, o.OrderCode)
		if err != nil {
			return res, fmt.Errorf("fetch %s: %w", o.OrderCode, err)
		}
		if detail == nil || detail.Data.OrderStatus != "CAPTURED" {
			res.Skipped++
			continue
		}
		raw, _ := json.Marshal(detail)
		t, out, err := ApplyGatewayIPN(ipnFromDetail(o.OrderCode, detail), raw)
		if err != nil {
			return res, fmt.Errorf("apply %s: %w", o.OrderCode, err)
		}
		if out.Duplicate {
			res.Skipped++
			continue
		}
		res.New++
		if out.Paid {
			res.Paid++
		}
		if out.NeedsHuman() {
			res.NeedHuman++
		}
		if onApplied != nil {
			onApplied(t, out)
		}
	}
	return res, nil
}
