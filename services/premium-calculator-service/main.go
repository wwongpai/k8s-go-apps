package main

// NOTE: This service intentionally uses stdlib net/http (NOT Gin).
// tracer.Start() is injected by Orchestrion — do NOT add it manually.
// net/http handlers and go-redis/v9 are auto-traced by Orchestrion.

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/redis/go-redis/v9"
)

type AutoCalcRequest struct {
	Age           int    `json:"age"`
	VehicleYear   int    `json:"vehicleYear"`
	VehicleType   string `json:"vehicleType"`   // sedan, suv, truck, sports
	DrivingRecord string `json:"drivingRecord"` // clean, minor, major
}

type HomeCalcRequest struct {
	PropertyValue    float64 `json:"propertyValue"`
	YearBuilt        int     `json:"yearBuilt"`
	Location         string  `json:"location"`         // urban, suburban, rural
	SecurityFeatures bool    `json:"securityFeatures"`
}

type LifeCalcRequest struct {
	Age            int     `json:"age"`
	Gender         string  `json:"gender"`
	CoverageAmount float64 `json:"coverageAmount"`
	HealthStatus   string  `json:"healthStatus"` // excellent, good, fair, poor
}

type HealthCalcRequest struct {
	Age         int    `json:"age"`
	Plan        string `json:"plan"`    // basic, standard, premium
	Smoker      bool   `json:"smoker"`
	PreExisting bool   `json:"preExisting"`
}

type CalcResponse struct {
	Premium   float64            `json:"premium"`
	RiskScore float64            `json:"riskScore"`
	Breakdown map[string]float64 `json:"breakdown"`
	CacheHit  bool               `json:"cacheHit"`
}

type BatchRequest struct {
	Calculations []struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	} `json:"calculations"`
}

type logEntry struct {
	Message string `json:"message"`
	Level   string `json:"level"`
	Service string `json:"service"`
}

func logWithTrace(ctx context.Context, level, msg string) {
	entry := logEntry{Message: msg, Level: level, Service: "premium-calculator-service"}
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

func cacheKey(calcType string, data interface{}) string {
	b, _ := json.Marshal(data)
	h := md5.Sum(b)
	return fmt.Sprintf("calc:%s:%x", calcType, h)
}

func getFromCache(ctx context.Context, key string) (*CalcResponse, bool) {
	val, err := rdb.Get(ctx, key).Result()
	if err != nil {
		return nil, false
	}
	var resp CalcResponse
	if err := json.Unmarshal([]byte(val), &resp); err != nil {
		return nil, false
	}
	resp.CacheHit = true
	return &resp, true
}

func saveToCache(ctx context.Context, key string, resp CalcResponse) {
	b, _ := json.Marshal(resp)
	rdb.Set(ctx, key, string(b), 10*time.Minute)
}

// calculateAutoRisk computes auto insurance premium.
//
//dd:span calculator.type:auto
func calculateAutoRisk(ctx context.Context, req AutoCalcRequest) (CalcResponse, error) {
	breakdown := make(map[string]float64)
	base := 800.0
	breakdown["base"] = base

	ageFactor := 1.0
	if req.Age < 25 || req.Age > 65 {
		ageFactor = 1.3
	}
	breakdown["age_factor"] = ageFactor

	yearFactor := 1.0
	if time.Now().Year()-req.VehicleYear > 10 {
		yearFactor = 1.2
	}
	breakdown["year_factor"] = yearFactor

	typeFactor := map[string]float64{
		"sedan": 1.0, "suv": 1.1, "truck": 1.1, "sports": 1.4,
	}
	tf := typeFactor[req.VehicleType]
	if tf == 0 {
		tf = 1.0
	}
	breakdown["type_factor"] = tf

	recordFactor := map[string]float64{"clean": 1.0, "minor": 1.2, "major": 1.5}
	rf := recordFactor[req.DrivingRecord]
	if rf == 0 {
		rf = 1.0
	}
	breakdown["record_factor"] = rf

	premium := base * ageFactor * yearFactor * tf * rf
	riskScore := (ageFactor-1)*0.4 + (tf-1)*0.3 + (rf-1)*0.3
	if riskScore > 1.0 {
		riskScore = 1.0
	}

	logWithTrace(ctx, "info", fmt.Sprintf("auto premium calculated: %.2f riskScore=%.2f", premium, riskScore))
	return CalcResponse{Premium: premium, RiskScore: riskScore, Breakdown: breakdown}, nil
}

// calculateHomePremium computes home insurance premium.
//
//dd:span calculator.type:home
func calculateHomePremium(ctx context.Context, req HomeCalcRequest) (CalcResponse, error) {
	breakdown := make(map[string]float64)
	base := req.PropertyValue * 0.005
	breakdown["base"] = base

	ageFactor := 1.0
	if req.YearBuilt < 1980 {
		ageFactor = 1.3
	}
	breakdown["age_factor"] = ageFactor

	locationFactor := map[string]float64{"urban": 1.2, "suburban": 1.0, "rural": 0.9}
	lf := locationFactor[req.Location]
	if lf == 0 {
		lf = 1.0
	}
	breakdown["location_factor"] = lf

	securityFactor := 1.0
	if req.SecurityFeatures {
		securityFactor = 0.95
	}
	breakdown["security_factor"] = securityFactor

	premium := base * ageFactor * lf * securityFactor
	riskScore := (ageFactor - 1) * 0.5
	if lf > 1 {
		riskScore += 0.2
	}

	logWithTrace(ctx, "info", fmt.Sprintf("home premium calculated: %.2f", premium))
	return CalcResponse{Premium: premium, RiskScore: riskScore, Breakdown: breakdown}, nil
}

// calculateLifePremium computes life insurance premium.
//
//dd:span calculator.type:life
func calculateLifePremium(ctx context.Context, req LifeCalcRequest) (CalcResponse, error) {
	breakdown := make(map[string]float64)

	ratePerThousand := 1.5
	if req.Age >= 40 && req.Age < 50 {
		ratePerThousand = 2.5
	} else if req.Age >= 50 && req.Age < 60 {
		ratePerThousand = 4.0
	} else if req.Age >= 60 {
		ratePerThousand = 7.0
	}
	breakdown["rate_per_thousand"] = ratePerThousand

	healthMultiplier := map[string]float64{"excellent": 0.9, "good": 1.0, "fair": 1.3, "poor": 1.6}
	hm := healthMultiplier[req.HealthStatus]
	if hm == 0 {
		hm = 1.0
	}
	breakdown["health_multiplier"] = hm

	genderFactor := 1.0
	if req.Gender == "female" {
		genderFactor = 0.95
	}
	breakdown["gender_factor"] = genderFactor

	premium := (req.CoverageAmount / 1000) * ratePerThousand * hm * genderFactor
	riskScore := (hm - 0.9) / 0.7

	logWithTrace(ctx, "info", fmt.Sprintf("life premium calculated: %.2f", premium))
	return CalcResponse{Premium: premium, RiskScore: riskScore, Breakdown: breakdown}, nil
}

// calculateHealthPremium computes health insurance premium.
//
//dd:span calculator.type:health
func calculateHealthPremium(ctx context.Context, req HealthCalcRequest) (CalcResponse, error) {
	breakdown := make(map[string]float64)

	planBase := map[string]float64{"basic": 200.0, "standard": 350.0, "premium": 550.0}
	base := planBase[req.Plan]
	if base == 0 {
		base = 200.0
	}
	breakdown["plan_base"] = base

	smokerFactor := 1.0
	if req.Smoker {
		smokerFactor = 1.5
	}
	breakdown["smoker_factor"] = smokerFactor

	preExistingFactor := 1.0
	if req.PreExisting {
		preExistingFactor = 1.3
	}
	breakdown["pre_existing_factor"] = preExistingFactor

	ageFactor := 1.0
	if req.Age > 60 {
		ageFactor = 1.4
	} else if req.Age > 50 {
		ageFactor = 1.2
	}
	breakdown["age_factor"] = ageFactor

	premium := base * smokerFactor * preExistingFactor * ageFactor
	riskScore := (smokerFactor-1)*0.4 + (preExistingFactor-1)*0.3 + (ageFactor-1)*0.3

	logWithTrace(ctx, "info", fmt.Sprintf("health premium calculated: %.2f", premium))
	return CalcResponse{Premium: premium, RiskScore: riskScore, Breakdown: breakdown}, nil
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func calculateAutoHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var req AutoCalcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	key := cacheKey("auto", req)
	if cached, ok := getFromCache(ctx, key); ok {
		writeJSON(w, http.StatusOK, cached)
		return
	}
	resp, err := calculateAutoRisk(ctx, req)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	saveToCache(ctx, key, resp)
	writeJSON(w, http.StatusOK, resp)
}

func calculateHomeHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var req HomeCalcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	key := cacheKey("home", req)
	if cached, ok := getFromCache(ctx, key); ok {
		writeJSON(w, http.StatusOK, cached)
		return
	}
	resp, err := calculateHomePremium(ctx, req)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	saveToCache(ctx, key, resp)
	writeJSON(w, http.StatusOK, resp)
}

func calculateLifeHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var req LifeCalcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	key := cacheKey("life", req)
	if cached, ok := getFromCache(ctx, key); ok {
		writeJSON(w, http.StatusOK, cached)
		return
	}
	resp, err := calculateLifePremium(ctx, req)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	saveToCache(ctx, key, resp)
	writeJSON(w, http.StatusOK, resp)
}

func calculateHealthHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var req HealthCalcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	key := cacheKey("health", req)
	if cached, ok := getFromCache(ctx, key); ok {
		writeJSON(w, http.StatusOK, cached)
		return
	}
	resp, err := calculateHealthPremium(ctx, req)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	saveToCache(ctx, key, resp)
	writeJSON(w, http.StatusOK, resp)
}

func calculateFactorsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	factors := map[string]interface{}{
		"auto": map[string]interface{}{
			"vehicleTypes":   []string{"sedan", "suv", "truck", "sports"},
			"drivingRecords": []string{"clean", "minor", "major"},
			"baseRate":       800.0,
		},
		"home": map[string]interface{}{
			"locations":      []string{"urban", "suburban", "rural"},
			"baseRatePercent": 0.5,
		},
		"life": map[string]interface{}{
			"healthStatuses": []string{"excellent", "good", "fair", "poor"},
			"genders":        []string{"male", "female", "other"},
		},
		"health": map[string]interface{}{
			"plans": []string{"basic", "standard", "premium"},
		},
	}
	writeJSON(w, http.StatusOK, factors)
}

func calculateBatchHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var batch BatchRequest
	if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	results := make([]interface{}, 0, len(batch.Calculations))
	for _, calc := range batch.Calculations {
		switch calc.Type {
		case "auto":
			var req AutoCalcRequest
			json.Unmarshal(calc.Data, &req)
			resp, _ := calculateAutoRisk(ctx, req)
			results = append(results, map[string]interface{}{"type": "auto", "result": resp})
		case "home":
			var req HomeCalcRequest
			json.Unmarshal(calc.Data, &req)
			resp, _ := calculateHomePremium(ctx, req)
			results = append(results, map[string]interface{}{"type": "home", "result": resp})
		case "life":
			var req LifeCalcRequest
			json.Unmarshal(calc.Data, &req)
			resp, _ := calculateLifePremium(ctx, req)
			results = append(results, map[string]interface{}{"type": "life", "result": resp})
		case "health":
			var req HealthCalcRequest
			json.Unmarshal(calc.Data, &req)
			resp, _ := calculateHealthPremium(ctx, req)
			results = append(results, map[string]interface{}{"type": "health", "result": resp})
		default:
			results = append(results, map[string]interface{}{"type": calc.Type, "error": "unknown type"})
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"results": results})
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "service": "premium-calculator-service"})
}

func main() {
	initRedis()
	defer rdb.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/calculate/auto", calculateAutoHandler)
	mux.HandleFunc("/calculate/home", calculateHomeHandler)
	mux.HandleFunc("/calculate/life", calculateLifeHandler)
	mux.HandleFunc("/calculate/health", calculateHealthHandler)
	mux.HandleFunc("/calculate/factors", calculateFactorsHandler)
	mux.HandleFunc("/calculate/batch", calculateBatchHandler)

	port := getEnv("PORT", "8084")
	log.Printf(`{"level":"info","service":"premium-calculator-service","message":"starting on :%s"}`, port)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatalf(`{"level":"fatal","service":"premium-calculator-service","message":"%v"}`, err)
	}
}
