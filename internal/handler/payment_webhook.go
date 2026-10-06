package handler

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/quizzzone/backend/internal/config"
	"github.com/quizzzone/backend/internal/pkg/audit"
	"github.com/quizzzone/backend/internal/pkg/notify"
	"github.com/quizzzone/backend/internal/pkg/payment"
)

// sepayAuthorized checks "Authorization: Apikey <SEPAY_API_KEY>" in constant
// time. No key configured means no webhook: an empty secret must never match.
func sepayAuthorized(c *gin.Context) bool {
	key := ""
	if config.AppConfig != nil {
		key = config.AppConfig.SePayAPIKey
	}
	if key == "" {
		return false
	}
	want := []byte("Apikey " + key)
	return subtle.ConstantTimeCompare([]byte(c.GetHeader("Authorization")), want) == 1
}

// SePayWebhook receives one bank transaction from SePay.
//
// Response contract (SePay): 200/201 with {"success": true} within 30 s, else
// it retries up to 7 times over 5 hours. So every business outcome — no order
// code, wrong amount, outgoing transfer, already recorded — answers 200: a
// retry would change nothing. Only an infrastructure failure answers 5xx, so
// that SePay retries a transfer we could not record.
func SePayWebhook(c *gin.Context) {
	if !sepayAuthorized(c) {
		notify.P1("sepay-webhook-auth", "SePay webhook rejected: bad API key from %s", c.ClientIP())
		c.JSON(http.StatusUnauthorized, gin.H{"success": false})
		return
	}
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, 64<<10))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false})
		return
	}
	var w payment.SePayWebhook
	if err := json.Unmarshal(raw, &w); err != nil || w.ID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"success": false})
		return
	}

	t := w.Transaction(raw)
	out, err := payment.ApplySePay(t)
	if err != nil {
		log.Printf("[sepay] apply id=%d failed: %v", w.ID, err)
		notify.P0("sepay-webhook-apply", "SePay webhook #%d could not be recorded (SePay will retry): %v", w.ID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"success": false})
		return
	}
	if out.Duplicate {
		c.JSON(http.StatusOK, gin.H{"success": true})
		return
	}

	log.Printf("[sepay] %s", payment.Describe(t, out))
	payment.AfterApplied(t, out)
	c.JSON(http.StatusOK, gin.H{"success": true})
}

// AdminListBankTransactions: ?status=open|<match_status>&q=
func AdminListBankTransactions(c *gin.Context) {
	rows, err := payment.ListTransactions(payment.TxnFilter{Status: c.Query("status"), Q: c.Query("q")})
	if err != nil {
		paymentError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"transactions": rows})
}

func txnID(c *gin.Context) (uint, bool) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid transaction ID"})
		return 0, false
	}
	return uint(id), true
}

// AdminAttachBankTransaction places an unmatched transfer on an order.
func AdminAttachBankTransaction(c *gin.Context) {
	id, ok := txnID(c)
	if !ok {
		return
	}
	var req struct {
		OrderCode string `json:"order_code" binding:"required"`
		Note      string `json:"note"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	adminID := c.GetUint("user_id")
	out, err := payment.AttachTransaction(id, req.OrderCode, adminID, req.Note)
	if err != nil {
		paymentError(c, err)
		return
	}
	if out.Paid && out.Order != nil {
		go payment.SendReceipt(out.Order.ID)
	}
	meta := map[string]any{"reason": req.Note}
	if out.Order != nil {
		meta["target_user_id"] = out.Order.UserID
		meta["amount_vnd"] = out.Order.PaidAmountVND
	}
	audit.Log(c, adminID, "admin_payment_attach", "bank_txn_"+c.Param("id")+"_"+req.OrderCode, meta)
	c.JSON(http.StatusOK, gin.H{"status": out.Status, "paid": out.Paid, "order": out.Order})
}

// AdminDismissBankTransaction takes a transfer that is not a checkout payment
// out of the review queue.
func AdminDismissBankTransaction(c *gin.Context) {
	id, ok := txnID(c)
	if !ok {
		return
	}
	var req struct {
		Note string `json:"note"`
	}
	_ = c.ShouldBindJSON(&req)
	adminID := c.GetUint("user_id")
	if err := payment.DismissTransaction(id, adminID, req.Note); err != nil {
		paymentError(c, err)
		return
	}
	audit.Log(c, adminID, "admin_payment_dismiss", "bank_txn_"+c.Param("id"), map[string]any{"reason": req.Note})
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
