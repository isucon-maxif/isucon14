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
	if err := db.SelectContext(ctx, &rides, "SELECT * FROM rides WHERE chair_id IS NULL ORDER BY created_at FOR SHARE"); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	// 空きイスとその座標を取得
	freeChairs := []*Chair{}
	if err := db.SelectContext(ctx, &freeChairs, "SELECT * FROM chairs WHERE is_active = TRUE AND NOT EXISTS (SELECT rides.id FROM ride_statuses JOIN rides ON ride_statuses.ride_id = rides.id WHERE rides.chair_id = chairs.id GROUP BY rides.id HAVING COUNT(*) < 6) FOR SHARE"); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	println("[DEBUG]", "rides", len(rides), "freeChairs", len(freeChairs))

	// イスの座標を取得
	tmp := []*ChairLocation{}
	if err := db.SelectContext(ctx, &tmp, "SELECT A.chair_id, A.latitude, A.longitude FROM chair_locations A INNER JOIN (SELECT chair_id, MAX(created_at) AS cat FROM chair_locations GROUP BY chair_id) B ON A.chair_id = B.chair_id AND A.created_at = B.cat FOR SHARE"); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	chairLocations := map[string]*ChairLocation{}
	for _, loc := range tmp {
		chairLocations[loc.ChairID] = loc
	}

	// イスの性能を取得
	tmp2 := []*ChairModel{}
	if err := db.SelectContext(ctx, &tmp2, "SELECT * FROM chair_models FOR SHARE"); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	chairModels := map[string]int{}
	for _, model := range tmp2 {
		chairModels[model.Name] = model.Speed
	}

	// マッチング
	isChairUsed := map[int]bool{}
	cnt := 0
	for _, ride := range rides {
		bestChairIdx := -1
		bestChairTime := 1000000000.0
		for i, chair := range freeChairs {
			if isChairUsed[i] {
				continue
			}
			loc, ok := chairLocations[chair.ID]
			if !ok {
				continue
			}
			dist := abs(loc.Latitude-ride.PickupLatitude) + abs(loc.Longitude-ride.PickupLongitude)
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
		cnt++
		isChairUsed[bestChairIdx] = true
		if _, err := db.ExecContext(ctx, "UPDATE rides SET chair_id = ? WHERE id = ?", freeChairs[bestChairIdx].ID, ride.ID); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		chairByAuthTokenCacheMutex.Lock()
		delete(chairByAuthTokenCache, freeChairs[bestChairIdx].AccessToken)
		chairByAuthTokenCacheMutex.Unlock()
	}

	println("[DEBUG]", "matched", cnt, "rides", len(rides), "freeChairs", len(freeChairs))

	w.WriteHeader(http.StatusNoContent)
}
