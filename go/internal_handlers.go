package main

import (
	"database/sql"
	"errors"
	"net/http"
	"sort"
)

// このAPIをインスタンス内から一定間隔で叩かせることで、椅子とライドをマッチングさせる
func internalGetMatching(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	tx, err := db.Beginx()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()

	// 待っているリクエストを取得
	rides := []*Ride{}
	if err := tx.SelectContext(ctx, &rides, "SELECT * FROM rides WHERE chair_id IS NULL ORDER BY created_at"); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	// 空きイスとその座標を取得
	tmp_freeChairs := []*Chair{}
	if err := tx.SelectContext(ctx, &tmp_freeChairs, "SELECT * FROM chairs WHERE is_active = TRUE AND NOT EXISTS (SELECT id FROM rides WHERE rides.evaluation IS NOT NULL AND chair_id = chairs.id)"); err != nil {
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
		if err := tx.GetContext(ctx, &isFree, `SELECT COUNT(*) = 0 FROM (SELECT id FROM rides WHERE chair_id = ? AND evaluation IS NULL)`, chair.ID); err != nil {
			writeError(w, http.StatusInternalServerError, err)
		}
		if isFree {
			freeChairs = append(freeChairs, chair)
		}
	}

	// イスの性能を取得
	tmp2 := []*ChairModel{}
	if err := tx.SelectContext(ctx, &tmp2, "SELECT * FROM chair_models"); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	chairModels := map[string]int{}
	for _, model := range tmp2 {
		chairModels[model.Name] = model.Speed
	}

	// マッチング
	sort.Slice(rides, func(i, j int) bool {
		iDist := abs(rides[i].PickupLatitude-rides[i].DestinationLatitude) + abs(rides[i].PickupLongitude-rides[i].DestinationLongitude)
		jDist := abs(rides[j].PickupLatitude-rides[j].DestinationLatitude) + abs(rides[j].PickupLongitude-rides[j].DestinationLongitude)
		return iDist > jDist
	})
	isChairUsed := make([]bool, len(freeChairs))

	for _, ride := range rides {
		bestChairIdx := -1
		bestTime := 1e9

		for chairidx, chair := range freeChairs {
			if isChairUsed[chairidx] || !chair.LocationLat.Valid || !chair.LocationLon.Valid {
				continue
			}
			pickupDist := abs(int(chair.LocationLat.Int32)-ride.PickupLatitude) + abs(int(chair.LocationLon.Int32)-ride.PickupLongitude)
			moveDist := abs(ride.PickupLatitude-ride.DestinationLatitude) + abs(ride.PickupLongitude-ride.DestinationLongitude)
			speed := chairModels[chair.Model]
			time := float64(pickupDist+moveDist*10) / float64(speed)
			if time < bestTime {
				bestTime = time
				bestChairIdx = chairidx
			}
		}

		if bestChairIdx == -1 {
			continue
		}

		isChairUsed[bestChairIdx] = true
		if _, err := tx.ExecContext(ctx, "UPDATE rides SET chair_id = ? WHERE id = ?", freeChairs[bestChairIdx].ID, ride.ID); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		rideCacheByChairIDMutex.Lock()
		rideCacheByChairID[freeChairs[bestChairIdx].ID] = ride
		rideCacheByChairIDMutex.Unlock()
		chairByAuthTokenCacheMutex.Lock()
		delete(chairByAuthTokenCache, freeChairs[bestChairIdx].AccessToken)
		chairByAuthTokenCacheMutex.Unlock()
	}

	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
