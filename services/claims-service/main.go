package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/redis/go-redis/v9"
)

type Claim struct {
	ID           string     `json:"id"`
	PolicyID     string     `json:"policyId"`
	Description  string     `json:"description"`
	Amount       float64    `json:"amount"`
	IncidentDate string     `json:"incidentDate"`
	Status       string     `json:"status"`
	CreatedAt    time.Time  `json:"createdAt"`
	UpdatedAt    time.Time  `json:"updatedAt"`
}

type ClaimStats struct {
	Total       int `json:"total"`
	Pending     int `json:"pending"`
	UnderReview int `json:"underReview"`
	Approved    int `json:"approved"`
	Rejected    int `json:"rejected"`
	Paid        int `json:"paid"`
}

type logEntry struct {
	Message string `json:"message"`
	Level   string `json:"level"`
	Service string `json:"service"`
}

func logWithTrace(ctx context.Context, level, msg string) {
	entry := logEntry{Message: msg, Level: level, Service: "claims-service"}
	b, _ := json.Marshal(entry)
	log.Println(string(b))
}

var (
	db  *sql.DB
	rdb *redis.Client
)

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func initDB() error {
	host := getEnv("POSTGRES_HOST", "localhost")
	port := getEnv("POSTGRES_PORT", "5432")
	dbName := getEnv("POSTGRES_DB", "insurancedb")
	user := getEnv("POSTGRES_USER", "insuranceuser")
	password := os.Getenv("POSTGRES_PASSWORD")

	dsn := fmt.Sprintf("host=%s port=%s dbname=%s user=%s password=%s sslmode=disable",
		host, port, dbName, user, password)

	var err error
	db, err = sql.Open("postgres", dsn)
	if err != nil {
		return fmt.Errorf("sql.Open: %w", err)
	}
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("db.Ping: %w", err)
	}

	_, err = db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS claims (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			policy_id UUID NOT NULL,
			description TEXT NOT NULL,
			amount DECIMAL(10,2) NOT NULL,
			incident_date DATE NOT NULL,
			status VARCHAR(20) NOT NULL DEFAULT 'pending',
			created_at TIMESTAMP DEFAULT NOW(),
			updated_at TIMESTAMP DEFAULT NOW()
		)
	`)
	return err
}

func initRedis() {
	// go-redis/v9 is auto-traced by Orchestrion — do NOT use redistrace wrappers
	rdb = redis.NewClient(&redis.Options{
		Addr:     fmt.Sprintf("%s:%s", getEnv("REDIS_HOST", "localhost"), getEnv("REDIS_PORT", "6379")),
		Password: os.Getenv("REDIS_PASSWORD"),
		DB:       0,
	})
}

// validateClaimEligibility checks if a claim is eligible for processing.
//
//dd:span claim.operation:validate_eligibility
func validateClaimEligibility(ctx context.Context, claimID string, policyID string) (bool, error) {
	if claimID == "" && policyID == "" {
		return false, fmt.Errorf("either claimID or policyID must be provided")
	}
	// In production: check policy is active, claim amount within limits, etc.
	logWithTrace(ctx, "debug", fmt.Sprintf("validating claim eligibility policyID=%s", policyID))
	return true, nil
}

// processClaimApproval handles auto-approval logic for claims under threshold.
//
//dd:span claim.operation:process_approval
func processClaimApproval(ctx context.Context, claimID string, amount float64) error {
	// Auto-approve claims under $1000
	if amount < 1000 {
		logWithTrace(ctx, "info", fmt.Sprintf("auto-approving claim %s amount=%.2f", claimID, amount))
		_, err := db.ExecContext(ctx,
			`UPDATE claims SET status='approved', updated_at=NOW() WHERE id=$1`, claimID)
		return err
	}
	logWithTrace(ctx, "info", fmt.Sprintf("claim %s requires manual review amount=%.2f", claimID, amount))
	return nil
}

func cacheClaimStatus(ctx context.Context, id, status string) {
	key := fmt.Sprintf("claim:status:%s", id)
	rdb.Set(ctx, key, status, 5*time.Minute)
}

func getCachedClaimStatus(ctx context.Context, id string) (string, bool) {
	key := fmt.Sprintf("claim:status:%s", id)
	val, err := rdb.Get(ctx, key).Result()
	if err != nil {
		return "", false
	}
	return val, true
}

func createClaimHandler(c *gin.Context) {
	ctx := c.Request.Context()
	var req Claim
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.PolicyID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "policyId is required"})
		return
	}
	if req.Amount <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "amount must be positive"})
		return
	}

	eligible, err := validateClaimEligibility(ctx, "", req.PolicyID)
	if err != nil || !eligible {
		c.JSON(http.StatusBadRequest, gin.H{"error": "claim not eligible"})
		return
	}

	var claim Claim
	err = db.QueryRowContext(ctx, `
		INSERT INTO claims (id, policy_id, description, amount, incident_date, status)
		VALUES (gen_random_uuid(), $1, $2, $3, $4, 'pending')
		RETURNING id, policy_id, description, amount, incident_date, status, created_at, updated_at
	`, req.PolicyID, req.Description, req.Amount, req.IncidentDate).
		Scan(&claim.ID, &claim.PolicyID, &claim.Description, &claim.Amount,
			&claim.IncidentDate, &claim.Status, &claim.CreatedAt, &claim.UpdatedAt)
	if err != nil {
		logWithTrace(ctx, "error", "db insert failed: "+err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create claim"})
		return
	}

	cacheClaimStatus(ctx, claim.ID, claim.Status)

	// Attempt auto-approval
	if err := processClaimApproval(ctx, claim.ID, claim.Amount); err != nil {
		logWithTrace(ctx, "warn", "auto-approval failed: "+err.Error())
	}

	logWithTrace(ctx, "info", fmt.Sprintf("claim created id=%s policyId=%s", claim.ID, claim.PolicyID))
	c.JSON(http.StatusCreated, claim)
}

func getClaimHandler(c *gin.Context) {
	ctx := c.Request.Context()
	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid claim id"})
		return
	}

	// Check Redis cache for status
	if status, hit := getCachedClaimStatus(ctx, id); hit {
		logWithTrace(ctx, "debug", fmt.Sprintf("cache hit for claim %s", id))
		// Still fetch full claim from DB but log cache hit
		_ = status
	}

	var claim Claim
	err := db.QueryRowContext(ctx,
		`SELECT id, policy_id, description, amount, incident_date, status, created_at, updated_at
		 FROM claims WHERE id=$1`, id).
		Scan(&claim.ID, &claim.PolicyID, &claim.Description, &claim.Amount,
			&claim.IncidentDate, &claim.Status, &claim.CreatedAt, &claim.UpdatedAt)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "claim not found"})
		return
	}
	if err != nil {
		logWithTrace(ctx, "error", "db query failed: "+err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get claim"})
		return
	}
	c.JSON(http.StatusOK, claim)
}

func updateClaimHandler(c *gin.Context) {
	ctx := c.Request.Context()
	id := c.Param("id")
	var req Claim
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	var claim Claim
	err := db.QueryRowContext(ctx, `
		UPDATE claims SET description=$1, amount=$2, updated_at=NOW()
		WHERE id=$3
		RETURNING id, policy_id, description, amount, incident_date, status, created_at, updated_at
	`, req.Description, req.Amount, id).
		Scan(&claim.ID, &claim.PolicyID, &claim.Description, &claim.Amount,
			&claim.IncidentDate, &claim.Status, &claim.CreatedAt, &claim.UpdatedAt)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "claim not found"})
		return
	}
	if err != nil {
		logWithTrace(ctx, "error", "db update failed: "+err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update claim"})
		return
	}
	c.JSON(http.StatusOK, claim)
}

func getClaimsByPolicyHandler(c *gin.Context) {
	ctx := c.Request.Context()
	policyID := c.Param("policyId")
	rows, err := db.QueryContext(ctx,
		`SELECT id, policy_id, description, amount, incident_date, status, created_at, updated_at
		 FROM claims WHERE policy_id=$1 ORDER BY created_at DESC`, policyID)
	if err != nil {
		logWithTrace(ctx, "error", "db query failed: "+err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list claims"})
		return
	}
	defer rows.Close()
	claims := []Claim{}
	for rows.Next() {
		var cl Claim
		if err := rows.Scan(&cl.ID, &cl.PolicyID, &cl.Description, &cl.Amount,
			&cl.IncidentDate, &cl.Status, &cl.CreatedAt, &cl.UpdatedAt); err != nil {
			continue
		}
		claims = append(claims, cl)
	}
	c.JSON(http.StatusOK, claims)
}

func updateClaimStatusHandler(c *gin.Context) {
	ctx := c.Request.Context()
	id := c.Param("id")
	var body struct {
		Status string `json:"status"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	validStatuses := map[string]bool{
		"pending": true, "under_review": true, "approved": true, "rejected": true, "paid": true,
	}
	if !validStatuses[body.Status] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid status"})
		return
	}
	var claim Claim
	err := db.QueryRowContext(ctx, `
		UPDATE claims SET status=$1, updated_at=NOW() WHERE id=$2
		RETURNING id, policy_id, description, amount, incident_date, status, created_at, updated_at
	`, body.Status, id).
		Scan(&claim.ID, &claim.PolicyID, &claim.Description, &claim.Amount,
			&claim.IncidentDate, &claim.Status, &claim.CreatedAt, &claim.UpdatedAt)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "claim not found"})
		return
	}
	if err != nil {
		logWithTrace(ctx, "error", "db update status failed: "+err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update status"})
		return
	}
	cacheClaimStatus(ctx, id, body.Status)
	logWithTrace(ctx, "info", fmt.Sprintf("claim %s status updated to %s", id, body.Status))
	c.JSON(http.StatusOK, claim)
}

func getPendingClaimsHandler(c *gin.Context) {
	ctx := c.Request.Context()
	rows, err := db.QueryContext(ctx,
		`SELECT id, policy_id, description, amount, incident_date, status, created_at, updated_at
		 FROM claims WHERE status='pending' ORDER BY created_at ASC LIMIT 100`)
	if err != nil {
		logWithTrace(ctx, "error", "db query failed: "+err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list pending claims"})
		return
	}
	defer rows.Close()
	claims := []Claim{}
	for rows.Next() {
		var cl Claim
		if err := rows.Scan(&cl.ID, &cl.PolicyID, &cl.Description, &cl.Amount,
			&cl.IncidentDate, &cl.Status, &cl.CreatedAt, &cl.UpdatedAt); err != nil {
			continue
		}
		claims = append(claims, cl)
	}
	c.JSON(http.StatusOK, gin.H{"claims": claims, "count": len(claims)})
}

func getClaimsStatsHandler(c *gin.Context) {
	ctx := c.Request.Context()
	rows, err := db.QueryContext(ctx,
		`SELECT status, COUNT(*) FROM claims GROUP BY status`)
	if err != nil {
		logWithTrace(ctx, "error", "db query failed: "+err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get stats"})
		return
	}
	defer rows.Close()
	stats := ClaimStats{}
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			continue
		}
		stats.Total += count
		switch status {
		case "pending":
			stats.Pending = count
		case "under_review":
			stats.UnderReview = count
		case "approved":
			stats.Approved = count
		case "rejected":
			stats.Rejected = count
		case "paid":
			stats.Paid = count
		}
	}
	c.JSON(http.StatusOK, stats)
}

func healthHandler(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok", "service": "claims-service"})
}

func main() {
	if err := initDB(); err != nil {
		log.Fatalf(`{"level":"fatal","service":"claims-service","message":"db init failed: %v"}`, err)
	}
	defer db.Close()

	initRedis()
	defer rdb.Close()
	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery())

	r.GET("/health", healthHandler)
	r.POST("/claims", createClaimHandler)
	r.GET("/claims/pending", getPendingClaimsHandler)
	r.GET("/claims/stats", getClaimsStatsHandler)
	r.GET("/claims/policy/:policyId", getClaimsByPolicyHandler)
	r.GET("/claims/:id", getClaimHandler)
	r.PUT("/claims/:id", updateClaimHandler)
	r.PATCH("/claims/:id/status", updateClaimStatusHandler)

	port := getEnv("PORT", "8082")
	log.Printf(`{"level":"info","service":"claims-service","message":"starting on :%s"}`, port)
	if err := r.Run(":" + port); err != nil {
		log.Fatalf(`{"level":"fatal","service":"claims-service","message":"%v"}`, err)
	}
}
