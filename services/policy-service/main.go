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
)

type Policy struct {
	ID         string    `json:"id"`
	CustomerID string    `json:"customerId"`
	Type       string    `json:"type"`
	Coverage   string    `json:"coverage"`
	Premium    float64   `json:"premium"`
	StartDate  string    `json:"startDate"`
	EndDate    string    `json:"endDate"`
	Status     string    `json:"status"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

type logEntry struct {
	Message string `json:"message"`
	Level   string `json:"level"`
	Service string `json:"service"`
}

func logWithTrace(ctx context.Context, level, msg string) {
	entry := logEntry{Message: msg, Level: level, Service: "policy-service"}
	b, _ := json.Marshal(entry)
	log.Println(string(b))
}

var db *sql.DB

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
		CREATE TABLE IF NOT EXISTS policies (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			customer_id UUID NOT NULL,
			type VARCHAR(50) NOT NULL,
			coverage VARCHAR(50) NOT NULL,
			premium DECIMAL(10,2) NOT NULL,
			start_date DATE NOT NULL,
			end_date DATE NOT NULL,
			status VARCHAR(20) NOT NULL DEFAULT 'pending',
			created_at TIMESTAMP DEFAULT NOW(),
			updated_at TIMESTAMP DEFAULT NOW()
		)
	`)
	return err
}

// validatePolicy validates policy fields.
//
//dd:span policy.operation:validate
func validatePolicy(ctx context.Context, p Policy) error {
	validTypes := map[string]bool{"auto": true, "home": true, "life": true, "health": true}
	if !validTypes[p.Type] {
		return fmt.Errorf("invalid policy type: %s", p.Type)
	}
	validCoverage := map[string]bool{"basic": true, "standard": true, "comprehensive": true}
	if !validCoverage[p.Coverage] {
		return fmt.Errorf("invalid coverage: %s", p.Coverage)
	}
	if p.Premium <= 0 {
		return fmt.Errorf("premium must be positive")
	}
	if p.CustomerID == "" {
		return fmt.Errorf("customerId is required")
	}
	logWithTrace(ctx, "debug", "policy validation passed")
	return nil
}

// calculatePolicyRisk returns a risk score 0.0–1.0 based on type and coverage.
//
//dd:span policy.operation:calculate_risk
func calculatePolicyRisk(ctx context.Context, policyType string, coverage string) float64 {
	baseRisk := map[string]float64{
		"auto":   0.3,
		"home":   0.2,
		"life":   0.1,
		"health": 0.25,
	}
	coverageMultiplier := map[string]float64{
		"basic":         0.8,
		"standard":      1.0,
		"comprehensive": 1.3,
	}
	risk := baseRisk[policyType] * coverageMultiplier[coverage]
	logWithTrace(ctx, "debug", fmt.Sprintf("calculated risk score: %f for type=%s coverage=%s", risk, policyType, coverage))
	return risk
}

func createPolicyHandler(c *gin.Context) {
	ctx := c.Request.Context()
	var req Policy
	if err := c.ShouldBindJSON(&req); err != nil {
		logWithTrace(ctx, "error", "invalid request body: "+err.Error())
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := validatePolicy(ctx, req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	risk := calculatePolicyRisk(ctx, req.Type, req.Coverage)
	logWithTrace(ctx, "info", fmt.Sprintf("creating policy for customer %s risk=%.2f", req.CustomerID, risk))

	var p Policy
	err := db.QueryRowContext(ctx, `
		INSERT INTO policies (id, customer_id, type, coverage, premium, start_date, end_date, status)
		VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, 'pending')
		RETURNING id, customer_id, type, coverage, premium, start_date, end_date, status, created_at, updated_at
	`, req.CustomerID, req.Type, req.Coverage, req.Premium, req.StartDate, req.EndDate).
		Scan(&p.ID, &p.CustomerID, &p.Type, &p.Coverage, &p.Premium,
			&p.StartDate, &p.EndDate, &p.Status, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		logWithTrace(ctx, "error", "db insert failed: "+err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create policy"})
		return
	}
	c.JSON(http.StatusCreated, p)
}

func getPolicyHandler(c *gin.Context) {
	ctx := c.Request.Context()
	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid policy id"})
		return
	}
	var p Policy
	err := db.QueryRowContext(ctx,
		`SELECT id, customer_id, type, coverage, premium, start_date, end_date, status, created_at, updated_at
		 FROM policies WHERE id = $1`, id).
		Scan(&p.ID, &p.CustomerID, &p.Type, &p.Coverage, &p.Premium,
			&p.StartDate, &p.EndDate, &p.Status, &p.CreatedAt, &p.UpdatedAt)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "policy not found"})
		return
	}
	if err != nil {
		logWithTrace(ctx, "error", "db query failed: "+err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get policy"})
		return
	}
	c.JSON(http.StatusOK, p)
}

func updatePolicyHandler(c *gin.Context) {
	ctx := c.Request.Context()
	id := c.Param("id")
	var req Policy
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	var p Policy
	err := db.QueryRowContext(ctx, `
		UPDATE policies SET coverage=$1, premium=$2, start_date=$3, end_date=$4, updated_at=NOW()
		WHERE id=$5
		RETURNING id, customer_id, type, coverage, premium, start_date, end_date, status, created_at, updated_at
	`, req.Coverage, req.Premium, req.StartDate, req.EndDate, id).
		Scan(&p.ID, &p.CustomerID, &p.Type, &p.Coverage, &p.Premium,
			&p.StartDate, &p.EndDate, &p.Status, &p.CreatedAt, &p.UpdatedAt)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "policy not found"})
		return
	}
	if err != nil {
		logWithTrace(ctx, "error", "db update failed: "+err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update policy"})
		return
	}
	c.JSON(http.StatusOK, p)
}

func deletePolicyHandler(c *gin.Context) {
	ctx := c.Request.Context()
	id := c.Param("id")
	res, err := db.ExecContext(ctx, `DELETE FROM policies WHERE id=$1`, id)
	if err != nil {
		logWithTrace(ctx, "error", "db delete failed: "+err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete policy"})
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "policy not found"})
		return
	}
	c.Status(http.StatusNoContent)
}

func getPoliciesByCustomerHandler(c *gin.Context) {
	ctx := c.Request.Context()
	customerID := c.Param("customerId")
	rows, err := db.QueryContext(ctx,
		`SELECT id, customer_id, type, coverage, premium, start_date, end_date, status, created_at, updated_at
		 FROM policies WHERE customer_id=$1 ORDER BY created_at DESC`, customerID)
	if err != nil {
		logWithTrace(ctx, "error", "db query failed: "+err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list policies"})
		return
	}
	defer rows.Close()
	policies := []Policy{}
	for rows.Next() {
		var p Policy
		if err := rows.Scan(&p.ID, &p.CustomerID, &p.Type, &p.Coverage, &p.Premium,
			&p.StartDate, &p.EndDate, &p.Status, &p.CreatedAt, &p.UpdatedAt); err != nil {
			continue
		}
		policies = append(policies, p)
	}
	c.JSON(http.StatusOK, policies)
}

func getPoliciesByTypeHandler(c *gin.Context) {
	ctx := c.Request.Context()
	pType := c.Param("type")
	rows, err := db.QueryContext(ctx,
		`SELECT id, customer_id, type, coverage, premium, start_date, end_date, status, created_at, updated_at
		 FROM policies WHERE type=$1 ORDER BY created_at DESC`, pType)
	if err != nil {
		logWithTrace(ctx, "error", "db query failed: "+err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list policies"})
		return
	}
	defer rows.Close()
	policies := []Policy{}
	for rows.Next() {
		var p Policy
		if err := rows.Scan(&p.ID, &p.CustomerID, &p.Type, &p.Coverage, &p.Premium,
			&p.StartDate, &p.EndDate, &p.Status, &p.CreatedAt, &p.UpdatedAt); err != nil {
			continue
		}
		policies = append(policies, p)
	}
	c.JSON(http.StatusOK, policies)
}

func updatePolicyStatusHandler(c *gin.Context) {
	ctx := c.Request.Context()
	id := c.Param("id")
	var body struct {
		Status string `json:"status"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	validStatuses := map[string]bool{"active": true, "inactive": true, "cancelled": true, "pending": true}
	if !validStatuses[body.Status] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid status"})
		return
	}
	var p Policy
	err := db.QueryRowContext(ctx, `
		UPDATE policies SET status=$1, updated_at=NOW() WHERE id=$2
		RETURNING id, customer_id, type, coverage, premium, start_date, end_date, status, created_at, updated_at
	`, body.Status, id).
		Scan(&p.ID, &p.CustomerID, &p.Type, &p.Coverage, &p.Premium,
			&p.StartDate, &p.EndDate, &p.Status, &p.CreatedAt, &p.UpdatedAt)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "policy not found"})
		return
	}
	if err != nil {
		logWithTrace(ctx, "error", "db update status failed: "+err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update status"})
		return
	}
	logWithTrace(ctx, "info", fmt.Sprintf("policy %s status updated to %s", id, body.Status))
	c.JSON(http.StatusOK, p)
}

func healthHandler(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok", "service": "policy-service"})
}

func main() {
	if err := initDB(); err != nil {
		log.Fatalf(`{"level":"fatal","service":"policy-service","message":"db init failed: %v"}`, err)
	}
	defer db.Close()

	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery())

	r.GET("/health", healthHandler)
	r.POST("/policies", createPolicyHandler)
	r.GET("/policies/customer/:customerId", getPoliciesByCustomerHandler)
	r.GET("/policies/type/:type", getPoliciesByTypeHandler)
	r.GET("/policies/:id", getPolicyHandler)
	r.PUT("/policies/:id", updatePolicyHandler)
	r.DELETE("/policies/:id", deletePolicyHandler)
	r.PATCH("/policies/:id/status", updatePolicyStatusHandler)

	port := getEnv("PORT", "8081")
	log.Printf(`{"level":"info","service":"policy-service","message":"starting on :%s"}`, port)
	if err := r.Run(":" + port); err != nil {
		log.Fatalf(`{"level":"fatal","service":"policy-service","message":"%v"}`, err)
	}
}
