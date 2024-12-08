package main

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/jmoiron/sqlx"
)

// このAPIをインスタンス内から一定間隔で叩かせることで、椅子とライドをマッチングさせる
func internalGetMatching(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// 待っているリクエストを取得
	rides := []*Ride{}
	if err := db.SelectContext(ctx, &rides, "SELECT * FROM rides WHERE chair_id IS NULL ORDER BY created_at"); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	// 空きイスを取得
	chairs := []*Chair{}
	if err := db.SelectContext(ctx, &chairs, `
	SELECT * 
	FROM chairs 
	WHERE is_active = TRUE`); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	// 椅子IDを収集
	chairIDs := make([]string, len(chairs))
	for i, chair := range chairs {
		chairIDs[i] = chair.ID
	}

	// 椅子に関連するライドの状態を一括取得
	rideStatuses := []struct {
		ChairID   string `db:"chair_id"`
		RideID    string `db:"ride_id"`
		Completed bool   `db:"completed"`
	}{}

	query, args, err := sqlx.In(`
	SELECT rides.chair_id, rides.id AS ride_id, 
		   COUNT(ride_statuses.chair_sent_at) = 6 AS completed
	FROM rides
	JOIN ride_statuses ON ride_statuses.ride_id = rides.id
	WHERE rides.chair_id IN (?)
	GROUP BY rides.id`, chairIDs)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	query = db.Rebind(query)

	if err := db.SelectContext(ctx, &rideStatuses, query, args...); err != nil && !errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	chairToCompletionStatus := make(map[string]bool)
	for _, status := range rideStatuses {
		if !status.Completed {
			chairToCompletionStatus[status.ChairID] = false
		} else if _, exists := chairToCompletionStatus[status.ChairID]; !exists {
			chairToCompletionStatus[status.ChairID] = true
		}
	}

	freeChairs := []*Chair{}
	for _, chair := range chairs {
		if chairToCompletionStatus[chair.ID] {
			freeChairs = append(freeChairs, chair)
		}
	}

	// イスの性能を取得
	tmp2 := []*ChairModel{}
	if err := db.SelectContext(ctx, &tmp2, "SELECT * FROM chair_models"); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	chairModels := map[string]int{}
	for _, model := range tmp2 {
		chairModels[model.Name] = model.Speed
	}

	// マッチング
	isChairUsed := map[int]bool{}
	for _, ride := range rides {
		bestChairIdx := -1
		bestChairTime := 1000000000.0
		for i, chair := range freeChairs {
			if isChairUsed[i] || !chair.LocationLat.Valid || !chair.LocationLon.Valid {
				continue
			}
			dist := abs(int(chair.LocationLat.Int32)-ride.PickupLatitude) + abs(int(chair.LocationLon.Int32)-ride.PickupLongitude)
			speed := chairModels[chair.Model]
			time := float64(dist) / float64(speed)
			if time < bestChairTime {
				bestChairTime = time
				bestChairIdx = i
			}
		}
		if bestChairIdx == -1 {
			continue
		}
		isChairUsed[bestChairIdx] = true
		if _, err := db.ExecContext(ctx, "UPDATE rides SET chair_id = ? WHERE id = ?", freeChairs[bestChairIdx].ID, ride.ID); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		chairByAuthTokenCacheMutex.Lock()
		delete(chairByAuthTokenCache, freeChairs[bestChairIdx].AccessToken)
		chairByAuthTokenCacheMutex.Unlock()
	}

	w.WriteHeader(http.StatusNoContent)
}
