package main

import (
	crand "crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"sync"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
	"github.com/kaz/pprotein/integration/standalone"
)

var (
	db                         *sqlx.DB
	chairByAuthTokenCache      = map[string]*Chair{}
	chairByAuthTokenCacheMutex sync.RWMutex
	rideCacheByChairID         = map[string]*Ride{}
	rideCacheByChairIDMutex    sync.RWMutex
)

func main() {
	go func() {
		standalone.Integrate(":6000")
	}()
	mux := setup()
	slog.Info("Listening on :8080")
	http.ListenAndServe(":8080", mux)
}

func setup() http.Handler {
	host := os.Getenv("ISUCON_DB_HOST")
	if host == "" {
		host = "127.0.0.1"
	}
	port := os.Getenv("ISUCON_DB_PORT")
	if port == "" {
		port = "3306"
	}
	_, err := strconv.Atoi(port)
	if err != nil {
		panic(fmt.Sprintf("failed to convert DB port number from ISUCON_DB_PORT environment variable into int: %v", err))
	}
	user := os.Getenv("ISUCON_DB_USER")
	if user == "" {
		user = "isucon"
	}
	password := os.Getenv("ISUCON_DB_PASSWORD")
	if password == "" {
		password = "isucon"
	}
	dbname := os.Getenv("ISUCON_DB_NAME")
	if dbname == "" {
		dbname = "isuride"
	}

	dbConfig := mysql.NewConfig()
	dbConfig.User = user
	dbConfig.Passwd = password
	dbConfig.Addr = net.JoinHostPort(host, port)
	dbConfig.Net = "tcp"
	dbConfig.DBName = dbname
	dbConfig.ParseTime = true

	_db, err := sqlx.Connect("mysql", dbConfig.FormatDSN())
	if err != nil {
		panic(err)
	}
	_db.SetMaxOpenConns(1000)
	db = _db

	mux := chi.NewRouter()
	mux.Use(middleware.Logger)
	mux.Use(middleware.Recoverer)
	mux.HandleFunc("POST /api/initialize", postInitialize)

	// app handlers
	{
		mux.HandleFunc("POST /api/app/users", appPostUsers)

		authedMux := mux.With(appAuthMiddleware)
		authedMux.HandleFunc("POST /api/app/payment-methods", appPostPaymentMethods)
		authedMux.HandleFunc("GET /api/app/rides", appGetRides)
		authedMux.HandleFunc("POST /api/app/rides", appPostRides)
		authedMux.HandleFunc("POST /api/app/rides/estimated-fare", appPostRidesEstimatedFare)
		authedMux.HandleFunc("POST /api/app/rides/{ride_id}/evaluation", appPostRideEvaluatation)
		authedMux.HandleFunc("GET /api/app/notification", appGetNotification)
		authedMux.HandleFunc("GET /api/app/nearby-chairs", appGetNearbyChairs)
	}

	// owner handlers
	{
		mux.HandleFunc("POST /api/owner/owners", ownerPostOwners)

		authedMux := mux.With(ownerAuthMiddleware)
		authedMux.HandleFunc("GET /api/owner/sales", ownerGetSales)
		authedMux.HandleFunc("GET /api/owner/chairs", ownerGetChairs)
	}

	// chair handlers
	{
		mux.HandleFunc("POST /api/chair/chairs", chairPostChairs)

		authedMux := mux.With(chairAuthMiddleware)
		authedMux.HandleFunc("POST /api/chair/activity", chairPostActivity)
		authedMux.HandleFunc("POST /api/chair/coordinate", chairPostCoordinate)
		authedMux.HandleFunc("GET /api/chair/notification", chairGetNotification)
		authedMux.HandleFunc("POST /api/chair/rides/{ride_id}/status", chairPostRideStatus)
	}

	// internal handlers
	{
		mux.HandleFunc("GET /api/internal/matching", internalGetMatching)
	}

	return mux
}

type postInitializeRequest struct {
	PaymentServer string `json:"payment_server"`
}

type postInitializeResponse struct {
	Language string `json:"language"`
}

func initCache() {
	chairByAuthTokenCacheMutex.Lock()
	defer chairByAuthTokenCacheMutex.Unlock()
	chairByAuthTokenCache = map[string]*Chair{}
	rideCacheByChairIDMutex.Lock()
	defer rideCacheByChairIDMutex.Unlock()
	rideCacheByChairID = map[string]*Ride{}
}

func postInitialize(w http.ResponseWriter, r *http.Request) {
	initCache()
	ctx := r.Context()
	req := &postInitializeRequest{}
	if err := bindJSON(r, req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	if out, err := exec.Command("../sql/init.sh").CombinedOutput(); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("failed to initialize: %s: %w", string(out), err))
		return
	}

	if _, err := db.ExecContext(ctx, "UPDATE settings SET value = ? WHERE name = 'payment_gateway_url'", req.PaymentServer); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	// 最新イス座標取得
	// init なので n+1 は許容
	chairs := []*Chair{}
	err := db.SelectContext(ctx, &chairs, "SELECT * FROM chairs")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	for _, chair := range chairs {
		chairLocations := []*ChairLocation{}
		err = db.SelectContext(
			ctx,
			&chairLocations,
			`SELECT * FROM chair_locations WHERE chair_id = ? ORDER BY created_at ASC`,
			chair.ID,
		)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if len(chairLocations) == 0 {
			continue
		}

		totalDistance := 0
		for i := 1; i < len(chairLocations); i++ {
			totalDistance += calculateDistance(
				chairLocations[i-1].Latitude,
				chairLocations[i-1].Longitude,
				chairLocations[i].Latitude,
				chairLocations[i].Longitude,
			)
		}
		lastChairLoc := chairLocations[len(chairLocations)-1]

		_, err = db.ExecContext(
			ctx,
			`UPDATE chairs SET location_lat = ?, location_lon = ?, total_distance = ?, total_distance_updated_at = ? WHERE id = ?`,
			lastChairLoc.Latitude, lastChairLoc.Longitude, totalDistance, lastChairLoc.CreatedAt, chair.ID,
		)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}

	// ライドの状態を更新
	rows, err := db.QueryContext(ctx, "SELECT * FROM rides")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer rows.Close()
	rideCacheByChairIDMutex.Lock()
	for rows.Next() {
		ride := &Ride{}
		if err := rows.Scan(
			&ride.ID,
			&ride.UserID,
			&ride.ChairID,
			&ride.PickupLatitude,
			&ride.PickupLongitude,
			&ride.DestinationLatitude,
			&ride.DestinationLongitude,
			&ride.Evaluation,
			&ride.CreatedAt,
			&ride.UpdatedAt,
		); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if ride.ChairID.Valid {
			rideCacheByChairID[ride.ChairID.String] = ride
		}
	}
	rideCacheByChairIDMutex.Unlock()

	go func() {
		if _, err := http.Get("http://localhost:9000/api/group/collect"); err != nil {
			log.Printf("failed to request to pprotein: %v", err)
		}
	}()

	writeJSON(w, http.StatusOK, postInitializeResponse{Language: "go"})
}

type Coordinate struct {
	Latitude  int `json:"latitude"`
	Longitude int `json:"longitude"`
}

func bindJSON(r *http.Request, v interface{}) error {
	return json.NewDecoder(r.Body).Decode(v)
}

func writeJSON(w http.ResponseWriter, statusCode int, v interface{}) {
	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	buf, err := json.Marshal(v)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(statusCode)
	w.Write(buf)
}

func writeError(w http.ResponseWriter, statusCode int, err error) {
	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	w.WriteHeader(statusCode)
	buf, marshalError := json.Marshal(map[string]string{"message": err.Error()})
	if marshalError != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"marshaling error failed"}`))
		return
	}
	w.Write(buf)

	slog.Error("error response wrote", err)
}

func secureRandomStr(b int) string {
	k := make([]byte, b)
	if _, err := crand.Read(k); err != nil {
		panic(err)
	}
	return fmt.Sprintf("%x", k)
}
