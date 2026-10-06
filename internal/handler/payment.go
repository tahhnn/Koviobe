package handler

import (
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/quizzzone/backend/internal/config"
	"github.com/quizzzone/backend/internal/db"
	"github.com/quizzzone/backend/internal/model"
	"github.com/quizzzone/backend/internal/pkg/audit"
	"github.com/quizzzone/backend/internal/pkg/payment"
	"github.com/quizzzone/backend/internal/pkg/settings"
)

// paymentContact is how a buyer reaches a person to purchase. Served by the API
// so the frontend never hardcodes it; empty fields are omitted and the matching
// button is hidden.
type paymentContact struct {
	ZaloURL string `json:"zalo_url,omitempty"`
}

func currentPaymentContact() paymentContact {
	if config.AppConfig == nil {
		return paymentContact{}
	}
	return paymentContact{ZaloURL: config.AppConfig.PaymentZaloURL}
}

// ListPaymentProducts is the public price list for the pricing page.
//
// payment_enabled says whether self-service QR checkout is open. The contact
// block is returned regardless: buying through Zalo works with checkout off,
// and is the only way to buy until it is on.
func ListPaymentProducts(c *gin.Context) {
	var products []model.PaymentProduct
	if err := db.DB.Where("is_active = ?", true).Order("sort_order ASC").Find(&products).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load products"})
		return
	}
	// CheckoutEnabled falls back to closed on a failed read: offering a
	// checkout that may not be wired up is worse than showing only Zalo.
	c.JSON(http.StatusOK, gin.H{
		"payment_enabled": payment.CheckoutEnabled(),
		"products":        products,
		"contact":         currentPaymentContact(),
	})
}

// AdminListPaymentProducts returns every product, active or not.
func AdminListPaymentProducts(c *gin.Context) {
	var products []model.PaymentProduct
	if err := db.DB.Order("sort_order ASC").Find(&products).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load products"})
		return
	}
	c.JSON(http.StatusOK, products)
}

// AdminUpdatePaymentProduct edits one product's name, term, price, order or
// availability. Fields are pointers so an omitted field is left alone while an
// explicit false/0 is still written — a map or struct update would have GORM
// skip the zero values, and is_active=false is exactly the one that matters.
func AdminUpdatePaymentProduct(c *gin.Context) {
	var req struct {
		Name         *string `json:"name"`
		DurationDays *int    `json:"duration_days"`
		AmountVND    *int    `json:"amount_vnd"`
		IsActive     *bool   `json:"is_active"`
		SortOrder    *int    `json:"sort_order"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var p model.PaymentProduct
	if err := db.DB.Where("id = ?", c.Param("id")).First(&p).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Product not found"})
		return
	}

	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" || len(name) > 100 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Product name must be 1-100 characters"})
			return
		}
		p.Name = name
	}
	// Lifetime terms are not sold through checkout; they stay an admin grant.
	if req.DurationDays != nil {
		if *req.DurationDays <= 0 || *req.DurationDays > 3660 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "duration_days must be between 1 and 3660"})
			return
		}
		p.DurationDays = *req.DurationDays
	}
	if req.AmountVND != nil {
		if *req.AmountVND < 1000 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "amount_vnd must be at least 1000"})
			return
		}
		p.AmountVND = *req.AmountVND
	}
	if req.IsActive != nil {
		p.IsActive = *req.IsActive
	}
	if req.SortOrder != nil {
		p.SortOrder = *req.SortOrder
	}

	if err := db.DB.Model(&p).Select("name", "duration_days", "amount_vnd", "is_active", "sort_order", "updated_at").
		Updates(&p).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update product"})
		return
	}

	adminID, _ := c.Get("user_id")
	audit.Log(c, adminID.(uint), "admin_payment_product_update", "product_"+p.ID, map[string]any{
		"product_id":    p.ID,
		"plan_id":       p.PlanID,
		"amount_vnd":    p.AmountVND,
		"duration_days": p.DurationDays,
		"is_active":     p.IsActive,
	})
	c.JSON(http.StatusOK, p)
}

// paymentError maps a payment-package error to a status. Business refusals
// carry their own message (translated by the Localize middleware); anything
// else is an internal failure and says so without leaking the cause.
func paymentError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, payment.ErrOrderNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
	case errors.Is(err, payment.ErrProductUnavailable), errors.Is(err, payment.ErrConfirmFields):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case errors.Is(err, payment.ErrCheckoutClosed), errors.Is(err, payment.ErrLifetimePlan),
		errors.Is(err, payment.ErrOrderNotPending), errors.Is(err, payment.ErrOrderAlreadyPaid),
		errors.Is(err, payment.ErrOrderNotCancelable), errors.Is(err, payment.ErrCheckoutNotReady):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
	default:
		log.Printf("[payment] %s %s: %v", c.Request.Method, c.FullPath(), err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Payment request failed"})
	}
}

func orderResponse(o *model.PaymentOrder) gin.H {
	h := gin.H{"order": o}
	if o.Status == model.OrderPending {
		h["checkout"] = payment.CheckoutFor(o)
	}
	return h
}

// CreatePaymentOrder opens a QR checkout for the signed-in host.
func CreatePaymentOrder(c *gin.Context) {
	var req struct {
		ProductID string `json:"product_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	uid := c.GetUint("user_id")
	o, err := payment.CreateOrder(uid, strings.TrimSpace(req.ProductID))
	if err != nil {
		paymentError(c, err)
		return
	}
	audit.Log(c, uid, "payment_order_create", o.OrderCode, map[string]any{
		"product_id": o.ProductID, "amount_vnd": o.AmountVND,
	})
	c.JSON(http.StatusCreated, orderResponse(o))
}

// GetPaymentOrder is what the checkout page polls. Owner only.
func GetPaymentOrder(c *gin.Context) {
	o, err := payment.GetUserOrder(c.GetUint("user_id"), c.Param("code"))
	if err != nil {
		paymentError(c, err)
		return
	}
	c.JSON(http.StatusOK, orderResponse(o))
}

// ListMyPaymentOrders returns the signed-in host's recent orders.
func ListMyPaymentOrders(c *gin.Context) {
	orders, err := payment.ListUserOrders(c.GetUint("user_id"), 50)
	if err != nil {
		paymentError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"orders": orders})
}

// CancelPaymentOrder abandons the host's own pending checkout.
func CancelPaymentOrder(c *gin.Context) {
	uid := c.GetUint("user_id")
	o, err := payment.CancelUserOrder(uid, c.Param("code"))
	if err != nil {
		paymentError(c, err)
		return
	}
	audit.Log(c, uid, "payment_order_cancel", o.OrderCode, nil)
	c.JSON(http.StatusOK, orderResponse(o))
}

// AdminListPaymentOrders: ?status=&q=&from=YYYY-MM-DD&to=YYYY-MM-DD&limit=
func AdminListPaymentOrders(c *gin.Context) {
	f := payment.AdminOrderFilter{Status: c.Query("status"), Q: c.Query("q")}
	if v := c.Query("limit"); v != "" {
		f.Limit, _ = strconv.Atoi(v)
	}
	// Same day-boundary reading as the history filter: dates are local days,
	// and "to" includes the whole day.
	if v := c.Query("from"); v != "" {
		t, err := time.ParseInLocation("2006-01-02", v, time.Local)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid from date"})
			return
		}
		f.From = &t
	}
	if v := c.Query("to"); v != "" {
		t, err := time.ParseInLocation("2006-01-02", v, time.Local)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid to date"})
			return
		}
		t = t.AddDate(0, 0, 1)
		f.To = &t
	}
	rows, err := payment.ListAdminOrders(f)
	if err != nil {
		paymentError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"orders": rows})
}

// AdminConfirmPaymentOrder settles an order by hand and grants its plan.
func AdminConfirmPaymentOrder(c *gin.Context) {
	var req struct {
		AmountVND   int    `json:"amount_vnd"`
		ExternalRef string `json:"external_ref"`
		Note        string `json:"note"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	adminID := c.GetUint("user_id")
	o, err := payment.AdminConfirm(c.Param("code"), payment.ConfirmInput{
		AmountVND:   req.AmountVND,
		ExternalRef: req.ExternalRef,
		Note:        req.Note,
		ActorUserID: adminID,
		IPAddress:   c.ClientIP(),
	})
	if err != nil {
		paymentError(c, err)
		return
	}
	audit.Log(c, adminID, "admin_payment_confirm", o.OrderCode, map[string]any{
		"target_user_id": o.UserID, "plan_id": o.PlanID, "amount_vnd": o.PaidAmountVND,
		"reason": o.Note,
	})
	c.JSON(http.StatusOK, gin.H{"order": o})
}

// AdminCancelPaymentOrder closes an order without granting anything.
func AdminCancelPaymentOrder(c *gin.Context) {
	var req struct {
		Note string `json:"note"`
	}
	_ = c.ShouldBindJSON(&req)
	adminID := c.GetUint("user_id")
	o, err := payment.AdminCancel(c.Param("code"), req.Note, adminID)
	if err != nil {
		paymentError(c, err)
		return
	}
	audit.Log(c, adminID, "admin_payment_cancel", o.OrderCode, map[string]any{
		"target_user_id": o.UserID, "reason": o.Note,
	})
	c.JSON(http.StatusOK, gin.H{"order": o})
}

func checkoutState() gin.H {
	on, _ := settings.GetBool(settings.KeyPaymentEnabled, false)
	return gin.H{
		"enabled":    on,
		"configured": payment.CheckoutConfigured(),
		// What buyers actually see: the switch and the configuration together.
		"effective": on && payment.CheckoutConfigured(),
	}
}

// AdminGetCheckout reports the QR checkout switch.
func AdminGetCheckout(c *gin.Context) {
	c.JSON(http.StatusOK, checkoutState())
}

// AdminSetCheckout flips QR checkout. Turning it on is refused until the bank
// account and SePay key are configured, so the switch can never show buyers a
// QR that pays nobody or that nothing will ever match.
func AdminSetCheckout(c *gin.Context) {
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.Enabled && !payment.CheckoutConfigured() {
		paymentError(c, payment.ErrCheckoutNotReady)
		return
	}
	adminID := c.GetUint("user_id")
	if err := settings.SetBool(settings.KeyPaymentEnabled, req.Enabled, adminID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save checkout setting"})
		return
	}
	audit.Log(c, adminID, "admin_payment_checkout_toggle", "payment.enabled", map[string]any{
		"is_active": req.Enabled,
	})
	c.JSON(http.StatusOK, checkoutState())
}
