package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

type logEntry struct {
	Message string `json:"message"`
	Level   string `json:"level"`
	Service string `json:"service"`
	Method  string `json:"method,omitempty"`
	Path    string `json:"path,omitempty"`
	Status  int    `json:"status,omitempty"`
	Latency string `json:"latency,omitempty"`
}

func logWithTrace(ctx context.Context, level, msg string) {
	entry := logEntry{Message: msg, Level: level, Service: "api-gateway"}
	b, _ := json.Marshal(entry)
	log.Println(string(b))
}

var httpClient = &http.Client{Timeout: 30 * time.Second}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// jwtAuthMiddleware validates the JWT token (stub implementation for demo).
//
//dd:span middleware.operation:jwt_validate
func jwtAuthMiddleware(ctx context.Context, token string) (bool, error) {
	if token == "" {
		logWithTrace(ctx, "warn", "missing Authorization header")
		return false, nil
	}
	// Demo: accept any non-empty Bearer token
	return true, nil
}

// proxyRequest forwards an HTTP request to the target URL.
//
//dd:span gateway.operation:proxy_request
func proxyRequest(ctx context.Context, targetURL string, r *http.Request) (*http.Response, error) {
	logWithTrace(ctx, "debug", fmt.Sprintf("proxying %s %s", r.Method, targetURL))

	// http.Client calls are auto-traced by Orchestrion (outbound request spans)
	req, err := http.NewRequestWithContext(ctx, r.Method, targetURL, r.Body)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	// Copy headers (skip hop-by-hop headers)
	hopByHop := map[string]bool{
		"Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true,
		"Proxy-Authorization": true, "Te": true, "Trailers": true,
		"Transfer-Encoding": true, "Upgrade": true,
	}
	for key, values := range r.Header {
		if !hopByHop[key] {
			for _, v := range values {
				req.Header.Add(key, v)
			}
		}
	}
	req.Header.Set("X-Forwarded-For", r.RemoteAddr)

	return httpClient.Do(req)
}

func makeProxyHandler(baseURL string, stripPrefix string) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()

		// Extract the path after the strip prefix
		path := c.Request.URL.Path
		if stripped := strings.TrimPrefix(path, stripPrefix); stripped != path {
			path = stripped
		}
		if path == "" {
			path = "/"
		}

		// Build query string
		query := c.Request.URL.RawQuery
		targetURL := baseURL + path
		if query != "" {
			targetURL += "?" + query
		}

		resp, err := proxyRequest(ctx, targetURL, c.Request)
		if err != nil {
			logWithTrace(ctx, "error", fmt.Sprintf("upstream error for %s: %v", targetURL, err))
			c.JSON(http.StatusBadGateway, gin.H{"error": "upstream service unavailable"})
			return
		}
		defer resp.Body.Close()

		// Copy response headers
		for key, values := range resp.Header {
			for _, v := range values {
				c.Header(key, v)
			}
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			logWithTrace(ctx, "error", "failed to read upstream response: "+err.Error())
			c.JSON(http.StatusBadGateway, gin.H{"error": "failed to read upstream response"})
			return
		}

		c.Data(resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
}

func jwtMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		authHeader := c.GetHeader("Authorization")
		token := strings.TrimPrefix(authHeader, "Bearer ")

		valid, err := jwtAuthMiddleware(ctx, token)
		if err != nil {
			logWithTrace(ctx, "error", "jwt validation error: "+err.Error())
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authentication error"})
			return
		}
		if !valid {
			logWithTrace(ctx, "warn", "missing or invalid token — proceeding in demo mode")
			// In demo mode, allow through with a warning rather than blocking
		}
		c.Next()
	}
}

func requestLogger() gin.HandlerFunc {
	return gin.LoggerWithFormatter(func(param gin.LogFormatterParams) string {
		entry := logEntry{
			Message: "request",
			Level:   "info",
			Service: "api-gateway",
			Method:  param.Method,
			Path:    param.Path,
			Status:  param.StatusCode,
			Latency: param.Latency.String(),
		}
		b, _ := json.Marshal(entry)
		return string(b) + "\n"
	})
}

func healthCheckHandler(c *gin.Context) {
	ctx := c.Request.Context()
	upstreams := map[string]string{
		"policy-service":             getEnv("POLICY_SERVICE_URL", "http://policy-service:8081"),
		"claims-service":             getEnv("CLAIMS_SERVICE_URL", "http://claims-service:8082"),
		"customer-service":           getEnv("CUSTOMER_SERVICE_URL", "http://customer-service:8083"),
		"premium-calculator-service": getEnv("CALCULATOR_SERVICE_URL", "http://premium-calculator-service:8084"),
		"notification-service":       getEnv("NOTIFICATION_SERVICE_URL", "http://notification-service:8085"),
	}

	statuses := make(map[string]string)
	overall := "ok"

	for name, url := range upstreams {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/health", nil)
		if err != nil {
			statuses[name] = "error"
			overall = "degraded"
			continue
		}
		resp, err := httpClient.Do(req)
		if err != nil || resp.StatusCode != http.StatusOK {
			statuses[name] = "unreachable"
			overall = "degraded"
		} else {
			statuses[name] = "ok"
			resp.Body.Close()
		}
	}

	statusCode := http.StatusOK
	if overall != "ok" {
		statusCode = http.StatusServiceUnavailable
	}
	c.JSON(statusCode, gin.H{"status": overall, "service": "api-gateway", "upstreams": statuses})
}

func main() {
	policyURL := getEnv("POLICY_SERVICE_URL", "http://policy-service:8081")
	claimsURL := getEnv("CLAIMS_SERVICE_URL", "http://claims-service:8082")
	customerURL := getEnv("CUSTOMER_SERVICE_URL", "http://customer-service:8083")
	calculatorURL := getEnv("CALCULATOR_SERVICE_URL", "http://premium-calculator-service:8084")
	notificationURL := getEnv("NOTIFICATION_SERVICE_URL", "http://notification-service:8085")

	r := gin.New()
	r.Use(requestLogger())
	r.Use(gin.Recovery())

	r.GET("/health", healthCheckHandler)

	api := r.Group("/api/v1")
	api.Use(jwtMiddleware())

	// Route each resource to the appropriate upstream, stripping /api/v1 prefix
	api.Any("/policies", makeProxyHandler(policyURL, "/api/v1"))
	api.Any("/policies/*path", makeProxyHandler(policyURL, "/api/v1"))

	api.Any("/claims", makeProxyHandler(claimsURL, "/api/v1"))
	api.Any("/claims/*path", makeProxyHandler(claimsURL, "/api/v1"))

	api.Any("/customers", makeProxyHandler(customerURL, "/api/v1"))
	api.Any("/customers/*path", makeProxyHandler(customerURL, "/api/v1"))

	api.Any("/calculate/*path", makeProxyHandler(calculatorURL, "/api/v1"))

	api.Any("/notifications", makeProxyHandler(notificationURL, "/api/v1"))
	api.Any("/notifications/*path", makeProxyHandler(notificationURL, "/api/v1"))

	port := getEnv("PORT", "8080")
	log.Printf(`{"level":"info","service":"api-gateway","message":"starting on :%s","upstreams":{"policy":"%s","claims":"%s","customer":"%s","calculator":"%s","notification":"%s"}}`,
		port, policyURL, claimsURL, customerURL, calculatorURL, notificationURL)

	if err := r.Run(":" + port); err != nil {
		log.Fatalf(`{"level":"fatal","service":"api-gateway","message":"%v"}`, err)
	}
}
