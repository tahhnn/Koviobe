package payment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/quizzzone/backend/internal/config"
	"github.com/quizzzone/backend/internal/db"
	"github.com/quizzzone/backend/internal/model"
)

// Reconciliation pulls SePay's transaction list and feeds every incoming
// transfer through the same ApplySePay path as the webhook.
//
// It exists for the webhook SePay gave up on: seven retries over five hours,
// then nothing. A transfer that only SePay knows about is a customer who paid
// and was never activated. Replaying is safe because ApplySePay is idempotent
// on the SePay id, and a bank reference already on record is skipped too (see
// knownReference), so an id mismatch between the webhook and the list API
// cannot record one transfer twice.

// reconcileSettle leaves very recent transfers to the webhook, which normally
// lands within seconds; reconciling them too would only race it.
const reconcileSettle = 15 * time.Minute

var sepayHTTP = &http.Client{Timeout: 20 * time.Second}

// flexInt decodes a number SePay may send as 2000, "2000" or "2000.00".
type flexInt int64

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	if i := strings.IndexByte(s, '.'); i >= 0 {
		s = s[:i]
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("not a number: %q", string(b))
	}
	*f = flexInt(n)
	return nil
}

// sepayListTxn is one row of GET /userapi/transactions/list.
type sepayListTxn struct {
	ID                 flexInt `json:"id"`
	BankBrandName      string  `json:"bank_brand_name"`
	AccountNumber      string  `json:"account_number"`
	TransactionDate    string  `json:"transaction_date"`
	AmountOut          flexInt `json:"amount_out"`
	AmountIn           flexInt `json:"amount_in"`
	TransactionContent string  `json:"transaction_content"`
	ReferenceNumber    string  `json:"reference_number"`
	Code               string  `json:"code"`
	SubAccount         string  `json:"sub_account"`
}

type sepayListResponse struct {
	Status       int            `json:"status"`
	Error        any            `json:"error"`
	Transactions []sepayListTxn `json:"transactions"`
}

// webhook converts a list row to the webhook shape so both paths share
// SePayWebhook.Transaction and ApplySePay.
func (r sepayListTxn) webhook() SePayWebhook {
	w := SePayWebhook{
		ID:              int64(r.ID),
		Gateway:         r.BankBrandName,
		TransactionDate: r.TransactionDate,
		AccountNumber:   r.AccountNumber,
		SubAccount:      r.SubAccount,
		Code:            r.Code,
		Content:         r.TransactionContent,
		ReferenceCode:   r.ReferenceNumber,
		TransferType:    "in",
		TransferAmount:  int64(r.AmountIn),
	}
	if r.AmountIn <= 0 {
		w.TransferType = "out"
		w.TransferAmount = int64(r.AmountOut)
	}
	return w
}

// ReconcileResult summarises one run.
type ReconcileResult struct {
	Fetched   int       `json:"fetched"`
	New       int       `json:"new"`        // recorded now — the webhook never delivered these
	Paid      int       `json:"paid"`       // of the new ones, orders granted
	NeedHuman int       `json:"need_human"` // of the new ones, sent to the review queue
	Skipped   int       `json:"skipped"`    // too recent, outgoing, or already on record
	Since     time.Time `json:"since"`
}

// ErrReconcileNotConfigured: no SEPAY_API_TOKEN, nothing to pull from.
var ErrReconcileNotConfigured = errors.New("SePay API token is not configured")

// ReconcileConfigured reports whether a run can reach SePay.
func ReconcileConfigured() bool {
	c := config.AppConfig
	return c != nil && c.SePayAPIToken != "" && c.PaymentBankAccount != ""
}

// Reconcile fetches incoming transfers since `since` and applies the ones the
// ledger does not hold. onApplied runs after each newly recorded transfer
// (receipt mail, alert) — outside any transaction.
func Reconcile(ctx context.Context, since time.Time, onApplied func(*model.BankTransaction, Outcome)) (ReconcileResult, error) {
	res := ReconcileResult{Since: since}
	if !ReconcileConfigured() {
		return res, ErrReconcileNotConfigured
	}
	rows, err := fetchSePay(ctx, since)
	if err != nil {
		return res, err
	}
	res.Fetched = len(rows)
	cutoff := time.Now().Add(-reconcileSettle)

	for _, r := range rows {
		w := r.webhook()
		t := w.Transaction(mustJSON(r))
		if t.TransferType != "in" || t.TransactionDate.After(cutoff) {
			res.Skipped++
			continue
		}
		t.Note = "reconciled"
		out, err := ApplySePay(t)
		if err != nil {
			return res, fmt.Errorf("apply sepay #%d: %w", w.ID, err)
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

// knownReference: the same bank reference for the same amount into the same
// account is already in the ledger, under whatever SePay id it arrived with.
// Bank references (FT…) are unique per transfer, so this only ever matches a
// transfer we already hold.
func knownReference(t *model.BankTransaction) bool {
	if t.ReferenceCode == "" {
		return false
	}
	var n int64
	db.DB.Model(&model.BankTransaction{}).
		Where("provider = ? AND reference_code = ? AND amount_vnd = ? AND account_number = ?",
			t.Provider, t.ReferenceCode, t.AmountVND, t.AccountNumber).
		Count(&n)
	return n > 0
}

func fetchSePay(ctx context.Context, since time.Time) ([]sepayListTxn, error) {
	c := config.AppConfig
	q := url.Values{}
	q.Set("account_number", c.PaymentBankAccount)
	q.Set("transaction_date_min", since.In(vnLocation).Format("2006-01-02"))
	q.Set("limit", "5000")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(c.SePayAPIBase, "/")+"/transactions/list?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.SePayAPIToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := sepayHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("sepay list: HTTP %d: %s", resp.StatusCode, clip(string(body), 200))
	}
	var parsed sepayListResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("sepay list: %w", err)
	}
	return parsed.Transactions, nil
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{}`)
	}
	return b
}
