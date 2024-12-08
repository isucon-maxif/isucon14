package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/oklog/ulid/v2"
)

type chairPostChairsRequest struct {
	Name               string `json:"name"`
	Model              string `json:"model"`
	ChairRegisterToken string `json:"chair_register_token"`
}

type chairPostChairsResponse struct {
	ID      string `json:"id"`
	OwnerID string `json:"owner_id"`
}

func chairPostChairs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	req := &chairPostChairsRequest{}
	if err := bindJSON(r, req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if req.Name == "" || req.Model == "" || req.ChairRegisterToken == "" {
		writeError(w, http.StatusBadRequest, errors.New("some of required fields(name, model, chair_register_token) are empty"))
		return
	}

	owner := &Owner{}
	if err := db.GetContext(ctx, owner, "SELECT * FROM owners WHERE chair_register_token = ?", req.ChairRegisterToken); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusUnauthorized, errors.New("invalid chair_register_token"))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	chairID := ulid.Make().String()
	accessToken := secureRandomStr(32)

	_, err := db.ExecContext(
		ctx,
		"INSERT INTO chairs (id, owner_id, name, model, is_active, access_token) VALUES (?, ?, ?, ?, ?, ?)",
		chairID, owner.ID, req.Name, req.Model, false, accessToken,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Path:  "/",
		Name:  "chair_session",
		Value: accessToken,
	})

	writeJSON(w, http.StatusCreated, &chairPostChairsResponse{
		ID:      chairID,
		OwnerID: owner.ID,
	})
}

type postChairActivityRequest struct {
	IsActive bool `json:"is_active"`
}

func chairPostActivity(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	chair := ctx.Value("chair").(*Chair)

	req := &postChairActivityRequest{}
	if err := bindJSON(r, req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	_, err := db.ExecContext(ctx, "UPDATE chairs SET is_active = ? WHERE id = ?", req.IsActive, chair.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

type chairPostCoordinateResponse struct {
	RecordedAt int64 `json:"recorded_at"`
}

func chairPostCoordinate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	req := &Coordinate{}
	if err := bindJSON(r, req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	chair := ctx.Value("chair").(*Chair)

	response := make(chan struct {
		RecordedAt int64 `json:"recorded_at"`
	})
	chairQueue <- QueueItem{chair, req, response}

	res := <-response

	writeJSON(w, http.StatusOK, &chairPostCoordinateResponse{
		RecordedAt: res.RecordedAt,
	})
}

type QueueItem struct {
	chair *Chair
	req   *Coordinate
	res   chan struct {
		RecordedAt int64 `json:"recorded_at"`
	}
}

var (
	chairQueue = make(chan QueueItem, 10000) // キューのチャネル
)

func batchInsertWorker() {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for range ticker.C {
		batchInsertChairs()
	}
}

func batchInsertChairs() {
	if len(chairQueue) == 0 {
		return
	}

	tx, err := db.Beginx()
	if err != nil {
		return
	}
	defer tx.Rollback()

	ctx := context.Background()

	queueItems := []QueueItem{}
	for len(chairQueue) > 0 {
		queueItem := <-chairQueue
		queueItems = append(queueItems, queueItem)
	}

	var locations []ChairLocation
	for _, queueItem := range queueItems {
		locations = append(locations, ChairLocation{
			ID:        ulid.Make().String(),
			ChairID:   queueItem.chair.ID,
			Latitude:  queueItem.req.Latitude,
			Longitude: queueItem.req.Longitude,
		})
	}

	var rows []interface{}
	for _, location := range locations {
		rows = append(rows, location.ID, location.ChairID, location.Latitude, location.Longitude)
	}

	query, args, err := sqlx.In(`INSERT INTO chair_locations (id, chair_id, latitude, longitude) VALUES (?, ?, ?, ?)`, rows...)
	if err != nil {
		return
	}

	query = tx.Rebind(query)
	if _, err = tx.ExecContext(ctx, query, args...); err != nil {
		return
	}

	// `UPDATE chairs SET location_lat = ?, location_lon = ? WHERE id = ?`,
	query = ""
	args = []interface{}{}
	for _, location := range locations {
		query += "UPDATE chairs SET location_lat = ?, location_lon = ? WHERE id = ?;"
		args = append(args, location.Latitude, location.Longitude, location.ChairID)
	}

	if _, err = tx.ExecContext(ctx, query, args...); err != nil {
		return
	}

	chairIDs := []string{}
	for _, queueItem := range queueItems {
		chairIDs = append(chairIDs, queueItem.chair.ID)
	}

	var rides []Ride
	query, args, err = sqlx.In(`
	SELECT id, user_id, chair_id, pickup_latitude, pickup_longitude, destination_latitude, destination_longitude, evaluation, created_at, updated_at FROM (
		SELECT id, user_id, chair_id, pickup_latitude, pickup_longitude, destination_latitude, destination_longitude, evaluation, created_at, updated_at,
			   ROW_NUMBER() OVER (PARTITION BY chair_id ORDER BY updated_at DESC) as rn
		FROM rides
	) tmp
	WHERE rn = 1 AND chair_id IN (?)
	`, chairIDs)
	if err != nil {
		return
	}

	query = tx.Rebind(query)
	if err := tx.SelectContext(ctx, &rides, query, args...); err != nil {
		return
	}

	var ridesIDs []string
	for _, ride := range rides {
		ridesIDs = append(ridesIDs, ride.ID)
	}

	statusMap, err := getLatestRideStatusBulk(ctx, tx, ridesIDs)
	if err != nil {
		return
	}

	var rows2 []interface{}

	for _, ride := range rides {
		status, ok := statusMap[ride.ID]
		if !ok {
			continue
		}

		if status != "COMPLETED" && status != "CANCELED" {
			if locations[0].Latitude == ride.PickupLatitude && locations[0].Longitude == ride.PickupLongitude && status == "ENROUTE" {
				rows2 = append(rows2, ulid.Make().String(), ride.ID, "PICKUP")
			}

			if locations[0].Latitude == ride.DestinationLatitude && locations[0].Longitude == ride.DestinationLongitude && status == "CARRYING" {
				rows2 = append(rows2, ulid.Make().String(), ride.ID, "ARRIVED")
			}
		}
	}

	query, args, err = sqlx.In(`INSERT INTO ride_statuses (id, ride_id, status) VALUES (?, ?, ?)`, rows2...)
	if err != nil {
		return
	}

	query = tx.Rebind(query)
	if _, err = tx.ExecContext(ctx, query, args...); err != nil {
		return
	}

	if err := tx.Commit(); err != nil {
		return
	}

	for _, queueItem := range queueItems {
		queueItem.res <- struct {
			RecordedAt int64 `json:"recorded_at"`
		}{RecordedAt: time.Now().UnixMilli()}
	}

}

type simpleUser struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type chairGetNotificationResponse struct {
	Data         *chairGetNotificationResponseData `json:"data"`
	RetryAfterMs int                               `json:"retry_after_ms"`
}

type chairGetNotificationResponseData struct {
	RideID                string     `json:"ride_id"`
	User                  simpleUser `json:"user"`
	PickupCoordinate      Coordinate `json:"pickup_coordinate"`
	DestinationCoordinate Coordinate `json:"destination_coordinate"`
	Status                string     `json:"status"`
}

func chairGetNotification(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	chair := ctx.Value("chair").(*Chair)

	tx, err := db.Beginx()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()
	ride := &Ride{}
	yetSentRideStatus := RideStatus{}
	status := ""

	if err := tx.GetContext(ctx, ride, `SELECT * FROM rides WHERE chair_id = ? ORDER BY updated_at DESC LIMIT 1`, chair.ID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusOK, &chairGetNotificationResponse{
				RetryAfterMs: 500,
			})
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	if err := tx.GetContext(ctx, &yetSentRideStatus, `SELECT * FROM ride_statuses WHERE ride_id = ? AND chair_sent_at IS NULL ORDER BY created_at ASC LIMIT 1`, ride.ID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			status, err = getLatestRideStatus(ctx, tx, ride.ID)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
		} else {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	} else {
		status = yetSentRideStatus.Status
	}

	user := &User{}
	err = tx.GetContext(ctx, user, "SELECT * FROM users WHERE id = ? FOR SHARE", ride.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	if yetSentRideStatus.ID != "" {
		_, err := tx.ExecContext(ctx, `UPDATE ride_statuses SET chair_sent_at = CURRENT_TIMESTAMP(6) WHERE id = ?`, yetSentRideStatus.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}

	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	writeJSON(w, http.StatusOK, &chairGetNotificationResponse{
		Data: &chairGetNotificationResponseData{
			RideID: ride.ID,
			User: simpleUser{
				ID:   user.ID,
				Name: fmt.Sprintf("%s %s", user.Firstname, user.Lastname),
			},
			PickupCoordinate: Coordinate{
				Latitude:  ride.PickupLatitude,
				Longitude: ride.PickupLongitude,
			},
			DestinationCoordinate: Coordinate{
				Latitude:  ride.DestinationLatitude,
				Longitude: ride.DestinationLongitude,
			},
			Status: status,
		},
		RetryAfterMs: 500,
	})
}

type postChairRidesRideIDStatusRequest struct {
	Status string `json:"status"`
}

func chairPostRideStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rideID := r.PathValue("ride_id")

	chair := ctx.Value("chair").(*Chair)

	req := &postChairRidesRideIDStatusRequest{}
	if err := bindJSON(r, req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	tx, err := db.Beginx()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()

	ride := &Ride{}
	if err := tx.GetContext(ctx, ride, "SELECT * FROM rides WHERE id = ? FOR UPDATE", rideID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, errors.New("ride not found"))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	if ride.ChairID.String != chair.ID {
		writeError(w, http.StatusBadRequest, errors.New("not assigned to this ride"))
		return
	}

	switch req.Status {
	// Acknowledge the ride
	case "ENROUTE":
		if _, err := tx.ExecContext(ctx, "INSERT INTO ride_statuses (id, ride_id, status) VALUES (?, ?, ?)", ulid.Make().String(), ride.ID, "ENROUTE"); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	// After Picking up user
	case "CARRYING":
		status, err := getLatestRideStatus(ctx, tx, ride.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if status != "PICKUP" {
			writeError(w, http.StatusBadRequest, errors.New("chair has not arrived yet"))
			return
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO ride_statuses (id, ride_id, status) VALUES (?, ?, ?)", ulid.Make().String(), ride.ID, "CARRYING"); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	default:
		writeError(w, http.StatusBadRequest, errors.New("invalid status"))
	}

	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
