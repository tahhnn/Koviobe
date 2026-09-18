package handler

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/quizzzone/backend/internal/pkg/audit"
	"github.com/quizzzone/backend/internal/pkg/license"
)

// parseDayParam reads a YYYY-MM-DD query param in the server's local zone.
// An unparseable value is treated as absent rather than as an error: a bad date
// in a reporting filter should widen the report, not break the page.
func parseDayParam(raw string) *time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	t, err := time.ParseInLocation("2006-01-02", raw, time.Local)
	if err != nil {
		return nil
	}
	return &t
}

// historyFilterFromQuery builds the shared filter for the JSON and CSV views, so
// an export always covers exactly what the operator was looking at on screen.
func historyFilterFromQuery(c *gin.Context) license.HistoryFilter {
	f := license.HistoryFilter{
		Email:       strings.TrimSpace(c.Query("email")),
		PlanID:      strings.TrimSpace(c.Query("plan_id")),
		Action:      strings.TrimSpace(c.Query("action")),
		Source:      strings.TrimSpace(c.Query("source")),
		ExternalRef: strings.TrimSpace(c.Query("external_ref")),
		From:        parseDayParam(c.Query("from")),
	}
	// "to" is inclusive for a human picking a date; the query is a half-open
	// range, so push it to the start of the next day or the last day is missing.
	if to := parseDayParam(c.Query("to")); to != nil {
		end := to.AddDate(0, 0, 1)
		f.To = &end
	}
	if v := c.Query("user_id"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 32); err == nil {
			f.UserID = uint(n)
		}
	}
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			f.Limit = n
		}
	}
	return f
}

// AdminListSubscriptionHistory returns the subscription audit trail plus the
// revenue total for the same period.
func AdminListSubscriptionHistory(c *gin.Context) {
	f := historyFilterFromQuery(c)

	events, err := license.ListHistory(f)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load history"})
		return
	}

	summary, err := license.SummarizeRevenue(f.From, f.To)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to summarize revenue"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"events":  events,
		"total":   len(events),
		"summary": summary,
	})
}

// AdminExportSubscriptionHistory streams the same rows as CSV for reconciliation
// against the payment intermediary's statement.
func AdminExportSubscriptionHistory(c *gin.Context) {
	f := historyFilterFromQuery(c)
	if f.Limit == 0 {
		// An export is for reconciliation, not for browsing: defaulting to the
		// screen's 200 rows would hand the operator a silently truncated ledger.
		f.Limit = 5000
	}

	events, err := license.ListHistory(f)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load history"})
		return
	}

	filename := fmt.Sprintf("license-history-%s.csv", time.Now().Format("20060102-150405"))
	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", `attachment; filename="`+filename+`"`)

	// UTF-8 BOM: without it Excel on Windows reads the file as the system
	// codepage and mangles every Vietnamese name in it.
	_, _ = c.Writer.WriteString("\ufeff")

	w := csv.NewWriter(c.Writer)
	defer w.Flush()

	_ = w.Write([]string{
		"created_at", "user_id", "email", "action", "source", "source_ref",
		"previous_plan_id", "plan_id", "starts_at", "ends_at",
		"amount_vnd", "external_ref", "actor_user_id", "note",
	})

	for _, e := range events {
		endsAt := ""
		if e.EndsAt != nil {
			endsAt = e.EndsAt.Format(time.RFC3339)
		}
		_ = w.Write([]string{
			e.CreatedAt.Format(time.RFC3339),
			strconv.FormatUint(uint64(e.UserID), 10),
			e.Email,
			e.Action,
			e.Source,
			e.SourceRef,
			e.PreviousPlanID,
			e.PlanID,
			e.StartsAt.Format(time.RFC3339),
			endsAt,
			strconv.Itoa(e.AmountVND),
			e.ExternalRef,
			strconv.FormatUint(uint64(e.ActorUserID), 10),
			e.Note,
		})
	}

	adminID, _ := c.Get("user_id")
	audit.Record(adminID.(uint), "admin_export_license_history",
		fmt.Sprintf("rows_%d", len(events)), c.ClientIP())
}

// AdminSendLicenseCodes emails a batch of codes to a buyer.
//
// Body: { "codes": ["KOVIO-..."], "email": "buyer@x.com", "name": "Tên người mua" }
func AdminSendLicenseCodes(c *gin.Context) {
	adminID, _ := c.Get("user_id")

	var req struct {
		Codes []string `json:"codes" binding:"required,min=1,max=200"`
		Email string   `json:"email" binding:"required,email"`
		Name  string   `json:"name"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := license.DeliverCodes(req.Codes, req.Email, req.Name); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	audit.Record(adminID.(uint), "admin_send_license_codes",
		fmt.Sprintf("%s_x%d", req.Email, len(req.Codes)), c.ClientIP())
	c.JSON(http.StatusOK, gin.H{
		"message": fmt.Sprintf("Đã gửi %d mã tới %s", len(req.Codes), req.Email),
		"sent":    len(req.Codes),
	})
}
