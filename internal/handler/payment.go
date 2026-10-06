package handler

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/quizzzone/backend/internal/config"
	"github.com/quizzzone/backend/internal/db"
	"github.com/quizzzone/backend/internal/model"
	"github.com/quizzzone/backend/internal/pkg/audit"
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
	// A failed read falls back to closed: offering a checkout that may not be
	// wired up is worse than showing only the Zalo contact.
	enabled, _ := settings.GetBool(settings.KeyPaymentEnabled, false)
	c.JSON(http.StatusOK, gin.H{
		"payment_enabled": enabled,
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
