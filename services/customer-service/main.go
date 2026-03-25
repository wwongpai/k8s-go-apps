package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
)

type Customer struct {
	ID          string    `json:"id"`
	FirstName   string    `json:"firstName"`
	LastName    string    `json:"lastName"`
	Email       string    `json:"email"`
	Phone       string    `json:"phone,omitempty"`
	DateOfBirth string    `json:"dateOfBirth,omitempty"`
	Verified    bool      `json:"verified"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

type logEntry struct {
	Message string `json:"message"`
	Level   string `json:"level"`
	Service string `json:"service"`
}

func logWithTrace(ctx context.Context, level, msg string) {
	entry := logEntry{Message: msg, Level: level, Service: "customer-service"}
	b, _ := json.Marshal(entry)
	log.Println(string(b))
}

var (
	db            *sql.DB
	httpClient    = &http.Client{Timeout: 5 * time.Second}
	emailRegexp   = regexp.MustCompile(`^[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}$`)
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
		CREATE TABLE IF NOT EXISTS customers (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			first_name VARCHAR(100) NOT NULL,
			last_name VARCHAR(100) NOT NULL,
			email VARCHAR(255) UNIQUE NOT NULL,
			phone VARCHAR(20),
			date_of_birth DATE,
			verified BOOLEAN DEFAULT FALSE,
			created_at TIMESTAMP DEFAULT NOW(),
			updated_at TIMESTAMP DEFAULT NOW()
		)
	`)
	return err
}

// validateCustomer validates customer fields.
//
//dd:span customer.operation:validate
func validateCustomer(ctx context.Context, c Customer) error {
	if c.FirstName == "" || c.LastName == "" {
		return fmt.Errorf("firstName and lastName are required")
	}
	if !emailRegexp.MatchString(c.Email) {
		return fmt.Errorf("invalid email format")
	}
	logWithTrace(ctx, "debug", "customer validation passed")
	return nil
}

// performKYCCheck simulates KYC verification for a customer.
//
//dd:span customer.operation:kyc_check
func performKYCCheck(ctx context.Context, customerID string) error {
	logWithTrace(ctx, "info", fmt.Sprintf("running KYC check for customer %s", customerID))
	// Simulate KYC processing time
	time.Sleep(10 * time.Millisecond)
	// In production: integrate with KYC provider API
	logWithTrace(ctx, "info", fmt.Sprintf("KYC check passed for customer %s", customerID))
	return nil
}

func scanCustomer(row interface {
	Scan(...interface{}) error
}) (Customer, error) {
	var c Customer
	var phone, dob sql.NullString
	err := row.Scan(&c.ID, &c.FirstName, &c.LastName, &c.Email,
		&phone, &dob, &c.Verified, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return c, err
	}
	c.Phone = phone.String
	c.DateOfBirth = dob.String
	return c, nil
}

func createCustomerHandler(c *gin.Context) {
	ctx := c.Request.Context()
	var req Customer
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := validateCustomer(ctx, req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	logWithTrace(ctx, "info", fmt.Sprintf("creating customer email=%s", req.Email))

	row := db.QueryRowContext(ctx, `
		INSERT INTO customers (id, first_name, last_name, email, phone, date_of_birth)
		VALUES (gen_random_uuid(), $1, $2, $3, $4, $5)
		RETURNING id, first_name, last_name, email, phone, date_of_birth, verified, created_at, updated_at
	`, req.FirstName, req.LastName, req.Email,
		sql.NullString{String: req.Phone, Valid: req.Phone != ""},
		sql.NullString{String: req.DateOfBirth, Valid: req.DateOfBirth != ""})

	customer, err := scanCustomer(row)
	if err != nil {
		logWithTrace(ctx, "error", "db insert failed: "+err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create customer"})
		return
	}
	c.JSON(http.StatusCreated, customer)
}

func getCustomerHandler(c *gin.Context) {
	ctx := c.Request.Context()
	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid customer id"})
		return
	}
	row := db.QueryRowContext(ctx,
		`SELECT id, first_name, last_name, email, phone, date_of_birth, verified, created_at, updated_at
		 FROM customers WHERE id=$1`, id)
	customer, err := scanCustomer(row)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "customer not found"})
		return
	}
	if err != nil {
		logWithTrace(ctx, "error", "db query failed: "+err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get customer"})
		return
	}
	c.JSON(http.StatusOK, customer)
}

func updateCustomerHandler(c *gin.Context) {
	ctx := c.Request.Context()
	id := c.Param("id")
	var req Customer
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	row := db.QueryRowContext(ctx, `
		UPDATE customers SET first_name=$1, last_name=$2, phone=$3, date_of_birth=$4, updated_at=NOW()
		WHERE id=$5
		RETURNING id, first_name, last_name, email, phone, date_of_birth, verified, created_at, updated_at
	`, req.FirstName, req.LastName,
		sql.NullString{String: req.Phone, Valid: req.Phone != ""},
		sql.NullString{String: req.DateOfBirth, Valid: req.DateOfBirth != ""},
		id)
	customer, err := scanCustomer(row)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "customer not found"})
		return
	}
	if err != nil {
		logWithTrace(ctx, "error", "db update failed: "+err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update customer"})
		return
	}
	c.JSON(http.StatusOK, customer)
}

func deleteCustomerHandler(c *gin.Context) {
	ctx := c.Request.Context()
	id := c.Param("id")
	res, err := db.ExecContext(ctx, `DELETE FROM customers WHERE id=$1`, id)
	if err != nil {
		logWithTrace(ctx, "error", "db delete failed: "+err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete customer"})
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "customer not found"})
		return
	}
	c.Status(http.StatusNoContent)
}

func searchCustomersHandler(c *gin.Context) {
	ctx := c.Request.Context()
	q := c.Query("q")
	if q == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "query parameter 'q' is required"})
		return
	}
	pattern := "%" + q + "%"
	rows, err := db.QueryContext(ctx, `
		SELECT id, first_name, last_name, email, phone, date_of_birth, verified, created_at, updated_at
		FROM customers
		WHERE first_name ILIKE $1 OR last_name ILIKE $1 OR email ILIKE $1
		ORDER BY last_name, first_name LIMIT 50
	`, pattern)
	if err != nil {
		logWithTrace(ctx, "error", "db search failed: "+err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "search failed"})
		return
	}
	defer rows.Close()
	customers := []Customer{}
	for rows.Next() {
		customer, err := scanCustomer(rows)
		if err != nil {
			continue
		}
		customers = append(customers, customer)
	}
	c.JSON(http.StatusOK, customers)
}

func verifyCustomerHandler(c *gin.Context) {
	ctx := c.Request.Context()
	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid customer id"})
		return
	}

	if err := performKYCCheck(ctx, id); err != nil {
		logWithTrace(ctx, "error", "KYC check failed: "+err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "KYC check failed"})
		return
	}

	row := db.QueryRowContext(ctx, `
		UPDATE customers SET verified=TRUE, updated_at=NOW() WHERE id=$1
		RETURNING id, first_name, last_name, email, phone, date_of_birth, verified, created_at, updated_at
	`, id)
	customer, err := scanCustomer(row)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "customer not found"})
		return
	}
	if err != nil {
		logWithTrace(ctx, "error", "db update failed: "+err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to verify customer"})
		return
	}
	logWithTrace(ctx, "info", fmt.Sprintf("customer %s verified", id))
	c.JSON(http.StatusOK, customer)
}

func getCustomerPoliciesHandler(c *gin.Context) {
	ctx := c.Request.Context()
	id := c.Param("id")
	policyServiceURL := getEnv("POLICY_SERVICE_URL", "http://policy-service:8081")

	// http.Client calls are auto-traced by Orchestrion (outbound request spans)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/policies/customer/%s", policyServiceURL, id), nil)
	if err != nil {
		c.JSON(http.StatusOK, []interface{}{})
		return
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		logWithTrace(ctx, "warn", "policy service unreachable: "+err.Error())
		c.JSON(http.StatusOK, []interface{}{})
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	c.Data(resp.StatusCode, "application/json", body)
}

func healthHandler(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok", "service": "customer-service"})
}

func main() {
	if err := initDB(); err != nil {
		log.Fatalf(`{"level":"fatal","service":"customer-service","message":"db init failed: %v"}`, err)
	}
	defer db.Close()

	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery())

	r.GET("/health", healthHandler)
	r.GET("/customers/search", searchCustomersHandler)
	r.POST("/customers", createCustomerHandler)
	r.GET("/customers/:id", getCustomerHandler)
	r.PUT("/customers/:id", updateCustomerHandler)
	r.DELETE("/customers/:id", deleteCustomerHandler)
	r.POST("/customers/:id/verify", verifyCustomerHandler)
	r.GET("/customers/:id/policies", getCustomerPoliciesHandler)

	port := getEnv("PORT", "8083")
	log.Printf(`{"level":"info","service":"customer-service","message":"starting on :%s"}`, port)
	if err := r.Run(":" + port); err != nil {
		log.Fatalf(`{"level":"fatal","service":"customer-service","message":"%v"}`, err)
	}
}
