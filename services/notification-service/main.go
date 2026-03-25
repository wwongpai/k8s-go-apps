package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

type Notification struct {
	ID         string     `json:"id"`
	CustomerID string     `json:"customerId"`
	Type       string     `json:"type"`    // email, sms, push
	Subject    string     `json:"subject"`
	Message    string     `json:"message"`
	Status     string     `json:"status"`  // pending, sent, failed
	CreatedAt  time.Time  `json:"createdAt"`
	SentAt     *time.Time `json:"sentAt,omitempty"`
}

type BulkRequest struct {
	CustomerIDs []string `json:"customerIds"`
	Type        string   `json:"type"`
	Subject     string   `json:"subject"`
	Message     string   `json:"message"`
}

type logEntry struct {
	Message string `json:"message"`
	Level   string `json:"level"`
	Service string `json:"service"`
}

func logWithTrace(ctx context.Context, level, msg string) {
	entry := logEntry{Message: msg, Level: level, Service: "notification-service"}
	b, _ := json.Marshal(entry)
	log.Println(string(b))
}

var rdb *redis.Client

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func initRedis() {
	rdb = redis.NewClient(&redis.Options{
		Addr:     fmt.Sprintf("%s:%s", getEnv("REDIS_HOST", "localhost"), getEnv("REDIS_PORT", "6379")),
		Password: os.Getenv("REDIS_PASSWORD"),
		DB:       0,
	})
}

func storeNotification(ctx context.Context, n Notification) error {
	b, err := json.Marshal(n)
	if err != nil {
		return err
	}
	pipe := rdb.Pipeline()
	pipe.Set(ctx, fmt.Sprintf("notification:%s", n.ID), string(b), 0)
	pipe.SAdd(ctx, fmt.Sprintf("customer:%s:notifications", n.CustomerID), n.ID)
	_, err = pipe.Exec(ctx)
	return err
}

// queueNotification adds a notification to the pending queue.
//
//dd:span notification.operation:queue
func queueNotification(ctx context.Context, n Notification) error {
	if err := storeNotification(ctx, n); err != nil {
		return fmt.Errorf("store notification: %w", err)
	}
	b, _ := json.Marshal(n)
	if err := rdb.LPush(ctx, "notifications:pending", string(b)).Err(); err != nil {
		return fmt.Errorf("queue notification: %w", err)
	}
	logWithTrace(ctx, "debug", fmt.Sprintf("notification %s queued", n.ID))
	return nil
}

// sendNotification simulates sending a notification via the specified channel.
//
//dd:span notification.operation:send
func sendNotification(ctx context.Context, n *Notification) error {
	logWithTrace(ctx, "info", fmt.Sprintf("sending %s notification to customer %s", n.Type, n.CustomerID))

	// Simulate delivery based on type
	switch n.Type {
	case "email":
		time.Sleep(5 * time.Millisecond) // simulate SMTP latency
	case "sms":
		time.Sleep(3 * time.Millisecond)
	case "push":
		time.Sleep(1 * time.Millisecond)
	}

	now := time.Now()
	n.Status = "sent"
	n.SentAt = &now

	// Update in Redis
	b, _ := json.Marshal(n)
	rdb.Set(ctx, fmt.Sprintf("notification:%s", n.ID), string(b), 0)
	logWithTrace(ctx, "info", fmt.Sprintf("notification %s sent via %s", n.ID, n.Type))
	return nil
}

// processBulkNotifications sends notifications to multiple customers.
//
//dd:span notification.operation:batch_process
func processBulkNotifications(ctx context.Context, notifications []Notification) (int, error) {
	sent := 0
	for i := range notifications {
		if err := sendNotification(ctx, &notifications[i]); err != nil {
			logWithTrace(ctx, "warn", fmt.Sprintf("failed to send notification %s: %v", notifications[i].ID, err))
			continue
		}
		if err := storeNotification(ctx, notifications[i]); err != nil {
			logWithTrace(ctx, "warn", "failed to store notification: "+err.Error())
		}
		sent++
	}
	return sent, nil
}

func sendNotificationHandler(c *gin.Context) {
	ctx := c.Request.Context()
	var req Notification
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.CustomerID == "" || req.Message == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "customerId and message are required"})
		return
	}
	validTypes := map[string]bool{"email": true, "sms": true, "push": true}
	if !validTypes[req.Type] {
		req.Type = "email"
	}

	req.ID = uuid.New().String()
	req.CreatedAt = time.Now()
	req.Status = "pending"

	if err := sendNotification(ctx, &req); err != nil {
		logWithTrace(ctx, "error", "send failed: "+err.Error())
		req.Status = "failed"
	}

	if err := storeNotification(ctx, req); err != nil {
		logWithTrace(ctx, "error", "store failed: "+err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to store notification"})
		return
	}

	logWithTrace(ctx, "info", fmt.Sprintf("notification %s processed status=%s", req.ID, req.Status))
	c.JSON(http.StatusCreated, req)
}

func getNotificationHandler(c *gin.Context) {
	ctx := c.Request.Context()
	id := c.Param("id")
	val, err := rdb.Get(ctx, fmt.Sprintf("notification:%s", id)).Result()
	if err == redis.Nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "notification not found"})
		return
	}
	if err != nil {
		logWithTrace(ctx, "error", "redis get failed: "+err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get notification"})
		return
	}
	var n Notification
	json.Unmarshal([]byte(val), &n)
	c.JSON(http.StatusOK, n)
}

func getCustomerNotificationsHandler(c *gin.Context) {
	ctx := c.Request.Context()
	customerID := c.Param("customerId")
	ids, err := rdb.SMembers(ctx, fmt.Sprintf("customer:%s:notifications", customerID)).Result()
	if err != nil {
		logWithTrace(ctx, "error", "redis smembers failed: "+err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list notifications"})
		return
	}
	notifications := []Notification{}
	for _, id := range ids {
		val, err := rdb.Get(ctx, fmt.Sprintf("notification:%s", id)).Result()
		if err != nil {
			continue
		}
		var n Notification
		if err := json.Unmarshal([]byte(val), &n); err == nil {
			notifications = append(notifications, n)
		}
	}
	c.JSON(http.StatusOK, notifications)
}

func bulkSendHandler(c *gin.Context) {
	ctx := c.Request.Context()
	var req BulkRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if len(req.CustomerIDs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "customerIds is required"})
		return
	}

	notifications := make([]Notification, 0, len(req.CustomerIDs))
	for _, cid := range req.CustomerIDs {
		notifications = append(notifications, Notification{
			ID:         uuid.New().String(),
			CustomerID: cid,
			Type:       req.Type,
			Subject:    req.Subject,
			Message:    req.Message,
			Status:     "pending",
			CreatedAt:  time.Now(),
		})
	}

	sent, err := processBulkNotifications(ctx, notifications)
	if err != nil {
		logWithTrace(ctx, "error", "bulk send error: "+err.Error())
	}

	logWithTrace(ctx, "info", fmt.Sprintf("bulk send complete: %d/%d sent", sent, len(notifications)))
	c.JSON(http.StatusOK, gin.H{
		"total": len(notifications),
		"sent":  sent,
		"failed": len(notifications) - sent,
	})
}

func getPendingNotificationsHandler(c *gin.Context) {
	ctx := c.Request.Context()
	length, err := rdb.LLen(ctx, "notifications:pending").Result()
	if err != nil {
		logWithTrace(ctx, "error", "redis llen failed: "+err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get pending count"})
		return
	}

	// Return up to 10 pending notifications
	vals, err := rdb.LRange(ctx, "notifications:pending", 0, 9).Result()
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"count": length, "notifications": []interface{}{}})
		return
	}

	notifications := []Notification{}
	for _, val := range vals {
		var n Notification
		if err := json.Unmarshal([]byte(val), &n); err == nil {
			notifications = append(notifications, n)
		}
	}
	c.JSON(http.StatusOK, gin.H{"count": length, "notifications": notifications})
}

func healthHandler(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok", "service": "notification-service"})
}

func main() {
	initRedis()
	defer rdb.Close()

	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery())

	r.GET("/health", healthHandler)
	r.GET("/notifications/pending", getPendingNotificationsHandler)
	r.POST("/notifications/send", sendNotificationHandler)
	r.POST("/notifications/bulk", bulkSendHandler)
	r.GET("/notifications/customer/:customerId", getCustomerNotificationsHandler)
	r.GET("/notifications/:id", getNotificationHandler)

	port := getEnv("PORT", "8085")
	log.Printf(`{"level":"info","service":"notification-service","message":"starting on :%s"}`, port)
	if err := r.Run(":" + port); err != nil {
		log.Fatalf(`{"level":"fatal","service":"notification-service","message":"%v"}`, err)
	}
}
