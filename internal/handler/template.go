package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/quizzzone/backend/internal/db"
	"github.com/quizzzone/backend/internal/model"
	"github.com/quizzzone/backend/internal/pkg/audit"
	"github.com/quizzzone/backend/internal/pkg/license"
	"gorm.io/gorm"
)

// bankQuestion is the portable JSON shape stored in Template.Questions.
type bankQuestion struct {
	Content       string      `json:"content"`
	Type          string      `json:"type"`
	Options       interface{} `json:"options"`
	CorrectAnswer string      `json:"correct_answer"`
	Duration      int         `json:"duration"`
	Points        int         `json:"points"`
}

type CreateTemplateReq struct {
	Title       string `json:"title" binding:"required,max=255"`
	Description string `json:"description" binding:"max=2000"`
	Questions   string `json:"questions" binding:"required,max=500000"`
}

type PatchTemplateReq struct {
	Title       *string `json:"title" binding:"omitempty,max=255"`
	Description *string `json:"description" binding:"omitempty,max=2000"`
	Questions   *string `json:"questions" binding:"omitempty,max=500000"`
}

type InstantiateTemplateReq struct {
	Title string `json:"title"`
}

type ImportBankQuestionsReq struct {
	QuizID  uint  `json:"quiz_id" binding:"required"`
	Indices []int `json:"indices"` // empty = import all; 0-based into bank array
}

func validateTemplateQuestions(raw string) error {
	if len(raw) > 500_000 {
		return fmt.Errorf("questions payload too large")
	}
	var parsed interface{}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return fmt.Errorf("questions must be valid JSON")
	}
	switch v := parsed.(type) {
	case []interface{}:
		if len(v) > 200 {
			return fmt.Errorf("too many questions (max 200)")
		}
	case map[string]interface{}:
		// allow object wrappers
	default:
		return fmt.Errorf("questions must be a JSON array or object")
	}
	return nil
}

func parseBankQuestions(raw string) ([]bankQuestion, error) {
	if err := validateTemplateQuestions(raw); err != nil {
		return nil, err
	}
	var list []bankQuestion
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return nil, fmt.Errorf("questions must be a JSON array")
	}
	return list, nil
}

func countBankQuestions(raw string) int {
	list, err := parseBankQuestions(raw)
	if err != nil {
		return 0
	}
	return len(list)
}

func quizQuestionsToBankJSON(questions []model.Question) (string, error) {
	out := make([]bankQuestion, 0, len(questions))
	for _, q := range questions {
		var opts interface{}
		_ = json.Unmarshal([]byte(q.Options), &opts)
		out = append(out, bankQuestion{
			Content:       q.Content,
			Type:          q.Type,
			Options:       opts,
			CorrectAnswer: q.CorrectAnswer,
			Duration:      q.Duration,
			Points:        q.Points,
		})
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func bankToQuestionReqs(list []bankQuestion) []QuestionReq {
	reqs := make([]QuestionReq, 0, len(list))
	for i, q := range list {
		typ := q.Type
		if typ == "" {
			typ = "multiple_choice"
		}
		dur := q.Duration
		if dur < 5 {
			dur = 30
		}
		if dur > 120 {
			dur = 120
		}
		pts := q.Points
		if pts <= 0 {
			pts = 1000
		}
		ans := q.CorrectAnswer
		if ans == "" {
			ans = "A"
		}
		reqs = append(reqs, QuestionReq{
			ID:            0,
			Content:       strings.TrimSpace(q.Content),
			Type:          typ,
			Options:       q.Options,
			CorrectAnswer: ans,
			Duration:      dur,
			Points:        pts,
			Order:         i + 1,
		})
	}
	return reqs
}

func checkTemplateQuota(uid uint) error {
	ents, err := license.GetEntitlements(uid)
	if err != nil {
		return fmt.Errorf("Failed to resolve license")
	}
	var count int64
	db.DB.Model(&model.Template{}).Where("host_id = ?", uid).Count(&count)
	if license.Enforcing() && !license.IsUnlimited(ents.MaxTemplates) && int(count) >= ents.MaxTemplates {
		return fmt.Errorf("Question bank limit reached (%d). Upgrade to Pro for more packs.", ents.MaxTemplates)
	}
	return nil
}

func templateListItem(t model.Template) gin.H {
	return gin.H{
		"id":             t.ID,
		"host_id":        t.HostID,
		"title":          t.Title,
		"description":    t.Description,
		"questions":      t.Questions,
		"question_count": countBankQuestions(t.Questions),
		"created_at":     t.CreatedAt,
		"updated_at":     t.UpdatedAt,
	}
}

func CreateTemplate(c *gin.Context) {
	userID, _ := c.Get("user_id")
	uid := userID.(uint)

	if err := checkTemplateQuota(uid); err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": err.Error()})
		return
	}

	var req CreateTemplateReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := validateTemplateQuestions(req.Questions); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if n := countBankQuestions(req.Questions); n == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Question bank pack must include at least 1 question"})
		return
	}

	template := model.Template{
		HostID:      uid,
		Title:       req.Title,
		Description: req.Description,
		Questions:   req.Questions,
	}

	if err := db.DB.Create(&template).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create template"})
		return
	}

	audit.Record(uid, "create_template", fmt.Sprintf("template_%d", template.ID), c.ClientIP())
	c.JSON(http.StatusCreated, templateListItem(template))
}

// CreateTemplateFromQuiz snapshots a quiz's questions into a reusable question-bank pack.
func CreateTemplateFromQuiz(c *gin.Context) {
	userID, _ := c.Get("user_id")
	uid := userID.(uint)

	quizID, err := strconv.ParseUint(c.Param("quizId"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid quiz ID"})
		return
	}

	if err := checkTemplateQuota(uid); err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": err.Error()})
		return
	}

	var quiz model.Quiz
	if err := db.DB.Preload("Questions", func(db *gorm.DB) *gorm.DB {
		return db.Order("questions.order ASC, questions.id ASC")
	}).Where("id = ? AND host_id = ?", uint(quizID), uid).First(&quiz).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Quiz not found"})
		return
	}
	if len(quiz.Questions) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Quiz has no questions to save"})
		return
	}

	payload, err := quizQuestionsToBankJSON(quiz.Questions)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to serialize questions"})
		return
	}

	var body struct {
		Title       string `json:"title"`
		Description string `json:"description"`
	}
	_ = c.ShouldBindJSON(&body)

	title := strings.TrimSpace(body.Title)
	if title == "" {
		title = quiz.Title + " (Bank)"
	}
	desc := strings.TrimSpace(body.Description)
	if desc == "" {
		desc = quiz.Description
	}

	template := model.Template{
		HostID:      uid,
		Title:       title,
		Description: desc,
		Questions:   payload,
	}
	if err := db.DB.Create(&template).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create question bank pack"})
		return
	}

	audit.Record(uid, "create_template_from_quiz", fmt.Sprintf("template_%d_from_quiz_%d", template.ID, quiz.ID), c.ClientIP())
	c.JSON(http.StatusCreated, templateListItem(template))
}

func ListTemplates(c *gin.Context) {
	userID, _ := c.Get("user_id")

	var templates []model.Template
	if err := db.DB.Where("host_id = ?", userID.(uint)).Order("updated_at DESC").Find(&templates).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch templates"})
		return
	}

	out := make([]gin.H, 0, len(templates))
	for _, t := range templates {
		item := templateListItem(t)
		// List: omit heavy questions blob for speed; keep count
		delete(item, "questions")
		out = append(out, item)
	}
	c.JSON(http.StatusOK, out)
}

func GetTemplate(c *gin.Context) {
	userID, _ := c.Get("user_id")
	idStr := c.Param("id")
	id, err := strconv.ParseUint(idStr, 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid template ID"})
		return
	}

	var template model.Template
	if err := db.DB.Where("id = ? AND host_id = ?", uint(id), userID.(uint)).First(&template).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Template not found"})
		return
	}

	c.JSON(http.StatusOK, templateListItem(template))
}

func UpdateTemplate(c *gin.Context) {
	userID, _ := c.Get("user_id")
	idStr := c.Param("id")
	id, err := strconv.ParseUint(idStr, 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid template ID"})
		return
	}

	var req PatchTemplateReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.Title == nil && req.Description == nil && req.Questions == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No fields to update"})
		return
	}
	if req.Questions != nil {
		if err := validateTemplateQuestions(*req.Questions); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if countBankQuestions(*req.Questions) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Question bank pack must include at least 1 question"})
			return
		}
	}

	var template model.Template
	if err := db.DB.Where("id = ? AND host_id = ?", uint(id), userID.(uint)).First(&template).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Template not found"})
		return
	}

	if req.Title != nil {
		title := strings.TrimSpace(*req.Title)
		if title == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "title cannot be empty"})
			return
		}
		template.Title = title
	}
	if req.Description != nil {
		template.Description = *req.Description
	}
	if req.Questions != nil {
		template.Questions = *req.Questions
	}

	if err := db.DB.Save(&template).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update template"})
		return
	}

	audit.Record(userID.(uint), "update_template", fmt.Sprintf("template_%d", template.ID), c.ClientIP())
	c.JSON(http.StatusOK, templateListItem(template))
}

func DeleteTemplate(c *gin.Context) {
	userID, _ := c.Get("user_id")
	idStr := c.Param("id")
	id, err := strconv.ParseUint(idStr, 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid template ID"})
		return
	}

	result := db.DB.Where("id = ? AND host_id = ?", uint(id), userID.(uint)).Delete(&model.Template{})
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete template"})
		return
	}
	if result.RowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Template not found"})
		return
	}

	audit.Record(userID.(uint), "delete_template", fmt.Sprintf("template_%d", id), c.ClientIP())
	c.JSON(http.StatusOK, gin.H{"message": "Template deleted"})
}

// InstantiateTemplate creates a new playable Quiz by deep-copying bank questions.
func InstantiateTemplate(c *gin.Context) {
	userID, _ := c.Get("user_id")
	uid := userID.(uint)
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid template ID"})
		return
	}

	var template model.Template
	if err := db.DB.Where("id = ? AND host_id = ?", uint(id), uid).First(&template).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Template not found"})
		return
	}

	bank, err := parseBankQuestions(template.Questions)
	if err != nil || len(bank) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Question bank pack is empty or invalid"})
		return
	}

	ents, err := license.GetEntitlements(uid)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to resolve license"})
		return
	}
	var quizCount int64
	db.DB.Model(&model.Quiz{}).Where("host_id = ?", uid).Count(&quizCount)
	if license.Enforcing() && !license.IsUnlimited(ents.MaxQuizzes) && int(quizCount) >= ents.MaxQuizzes {
		c.JSON(http.StatusForbidden, gin.H{"error": fmt.Sprintf("Quiz limit reached (%d)", ents.MaxQuizzes)})
		return
	}
	if license.Enforcing() && !license.IsUnlimited(ents.MaxQuestionsPerQuiz) && len(bank) > ents.MaxQuestionsPerQuiz {
		c.JSON(http.StatusForbidden, gin.H{"error": fmt.Sprintf("Too many questions for plan (max %d)", ents.MaxQuestionsPerQuiz)})
		return
	}

	var body InstantiateTemplateReq
	_ = c.ShouldBindJSON(&body)
	title := strings.TrimSpace(body.Title)
	if title == "" {
		title = template.Title
	}

	tx := db.DB.Begin()
	quiz := model.Quiz{
		HostID:      uid,
		Title:       title,
		Description: template.Description,
	}
	if err := tx.Create(&quiz).Error; err != nil {
		tx.Rollback()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create quiz"})
		return
	}

	for i, reqQ := range bankToQuestionReqs(bank) {
		if strings.TrimSpace(reqQ.Content) == "" {
			continue
		}
		optsJSON, err := json.Marshal(reqQ.Options)
		if err != nil {
			tx.Rollback()
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Invalid options for question %d", i+1)})
			return
		}
		q := model.Question{
			QuizID:        quiz.ID,
			Content:       reqQ.Content,
			Type:          reqQ.Type,
			Options:       string(optsJSON),
			CorrectAnswer: reqQ.CorrectAnswer,
			Duration:      reqQ.Duration,
			Points:        reqQ.Points,
			Order:         i + 1,
		}
		if err := tx.Create(&q).Error; err != nil {
			tx.Rollback()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create questions"})
			return
		}
	}
	if err := tx.Commit().Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to commit quiz"})
		return
	}

	audit.Record(uid, "instantiate_template", fmt.Sprintf("quiz_%d_from_template_%d", quiz.ID, template.ID), c.ClientIP())
	c.JSON(http.StatusCreated, gin.H{
		"quiz_id":        quiz.ID,
		"title":          quiz.Title,
		"question_count": len(bank),
		"template_id":    template.ID,
	})
}

// ImportBankQuestions appends selected (or all) bank questions onto an existing quiz.
func ImportBankQuestions(c *gin.Context) {
	userID, _ := c.Get("user_id")
	uid := userID.(uint)
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid template ID"})
		return
	}

	var req ImportBankQuestionsReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var template model.Template
	if err := db.DB.Where("id = ? AND host_id = ?", uint(id), uid).First(&template).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Template not found"})
		return
	}
	bank, err := parseBankQuestions(template.Questions)
	if err != nil || len(bank) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Question bank pack is empty or invalid"})
		return
	}

	var quiz model.Quiz
	if err := db.DB.Where("id = ? AND host_id = ?", req.QuizID, uid).First(&quiz).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Quiz not found"})
		return
	}

	selected := make([]bankQuestion, 0)
	if len(req.Indices) == 0 {
		selected = bank
	} else {
		seen := map[int]bool{}
		for _, idx := range req.Indices {
			if idx < 0 || idx >= len(bank) || seen[idx] {
				continue
			}
			seen[idx] = true
			selected = append(selected, bank[idx])
		}
	}
	if len(selected) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No valid questions selected"})
		return
	}

	ents, err := license.GetEntitlements(uid)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to resolve license"})
		return
	}
	var existingCount int64
	db.DB.Model(&model.Question{}).Where("quiz_id = ?", quiz.ID).Count(&existingCount)
	newTotal := int(existingCount) + len(selected)
	if license.Enforcing() && !license.IsUnlimited(ents.MaxQuestionsPerQuiz) && newTotal > ents.MaxQuestionsPerQuiz {
		c.JSON(http.StatusForbidden, gin.H{
			"error": fmt.Sprintf("Would exceed question limit (%d on %s plan)", ents.MaxQuestionsPerQuiz, ents.PlanName),
		})
		return
	}

	var maxOrder int
	_ = db.DB.Model(&model.Question{}).Where("quiz_id = ?", quiz.ID).Select("COALESCE(MAX(\"order\"), 0)").Scan(&maxOrder).Error

	tx := db.DB.Begin()
	added := 0
	for i, reqQ := range bankToQuestionReqs(selected) {
		if strings.TrimSpace(reqQ.Content) == "" {
			continue
		}
		optsJSON, err := json.Marshal(reqQ.Options)
		if err != nil {
			tx.Rollback()
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Invalid options for question %d", i+1)})
			return
		}
		q := model.Question{
			QuizID:        quiz.ID,
			Content:       reqQ.Content,
			Type:          reqQ.Type,
			Options:       string(optsJSON),
			CorrectAnswer: reqQ.CorrectAnswer,
			Duration:      reqQ.Duration,
			Points:        reqQ.Points,
			Order:         maxOrder + i + 1,
		}
		if err := tx.Create(&q).Error; err != nil {
			tx.Rollback()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to import questions"})
			return
		}
		added++
	}
	if err := tx.Commit().Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to commit import"})
		return
	}

	audit.Record(uid, "import_bank_questions", fmt.Sprintf("quiz_%d_from_template_%d_n%d", quiz.ID, template.ID, added), c.ClientIP())
	c.JSON(http.StatusOK, gin.H{
		"quiz_id":     quiz.ID,
		"imported":    added,
		"total_after": int(existingCount) + added,
		"template_id": template.ID,
	})
}
