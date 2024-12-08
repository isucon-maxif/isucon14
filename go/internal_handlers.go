package main

import (
	"database/sql"
	"errors"
	"net/http"
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

	// 空きイスとその座標を取得
	tmp_freeChairs := []*Chair{}
	if err := db.SelectContext(ctx, &tmp_freeChairs, "SELECT * FROM chairs WHERE is_active = TRUE AND NOT EXISTS (SELECT rides.id FROM ride_statuses JOIN rides ON ride_statuses.ride_id = rides.id WHERE rides.chair_id = chairs.id GROUP BY rides.id HAVING COUNT(*) < 6)"); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// n + 1 しちゃうけどしゃあなし
	freeChairs := []*Chair{}
	for _, chair := range tmp_freeChairs {
		isFree := true
		if err := db.GetContext(ctx, &isFree, "SELECT COUNT(*) = 0 FROM (SELECT COUNT(chair_sent_at) = 6 AS completed FROM ride_statuses WHERE ride_id IN (SELECT id FROM rides WHERE chair_id = ?) GROUP BY ride_id) is_completed WHERE completed = FALSE", chair.ID); err != nil {
			writeError(w, http.StatusInternalServerError, err)
		}
		if isFree {
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
			pickupDist := abs(int(chair.LocationLat.Int32)-ride.PickupLatitude) + abs(int(chair.LocationLon.Int32)-ride.PickupLongitude)
			moveDist := abs(ride.PickupLatitude-ride.DestinationLatitude) + abs(ride.PickupLongitude-ride.DestinationLongitude)
			speed := chairModels[chair.Model]
			time := float64(pickupDist+moveDist*10) / float64(speed)
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
