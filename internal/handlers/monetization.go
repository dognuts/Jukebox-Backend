package handlers

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jukebox/backend/internal/middleware"
	"github.com/jukebox/backend/internal/models"
	"github.com/jukebox/backend/internal/store"
	"github.com/jukebox/backend/internal/ws"
)

type MonetizationHandler struct {
	pg   *store.PGStore
	hubs *ws.HubManager
}

func NewMonetizationHandler(pg *store.PGStore, hubs *ws.HubManager) *MonetizationHandler {
	return &MonetizationHandler{pg: pg, hubs: hubs}
}

// ============ Plus ============

// POST /api/billing/plus/subscribe (dev mode: instant activation)
func (h *MonetizationHandler) SubscribePlus(w http.ResponseWriter, r *http.Request) {
	user := middleware.GetUser(r.Context())
	if user == nil {
		http.Error(w, "login required", http.StatusUnauthorized)
		return
	}
	if user.IsPlus {
		writeJSON(w, http.StatusOK, map[string]string{"status": "already_plus"})
		return
	}

	// Dev mode: instant activation without Stripe
	periodEnd := time.Now().AddDate(0, 1, 0)
	if err := h.pg.ActivatePlus(r.Context(), user.ID, periodEnd, "dev_"+user.ID); err != nil {
		log.Printf("[billing] activate plus: %v", err)
		http.Error(w, "failed to activate Plus", http.StatusInternalServerError)
		return
	}

	updated, _ := h.pg.GetUserByID(r.Context(), user.ID)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "active",
		"user":   updated,
	})
}

// POST /api/billing/plus/cancel
func (h *MonetizationHandler) CancelPlus(w http.ResponseWriter, r *http.Request) {
	user := middleware.GetUser(r.Context())
	if user == nil {
		http.Error(w, "login required", http.StatusUnauthorized)
		return
	}
	if err := h.pg.DeactivatePlus(r.Context(), user.ID); err != nil {
		http.Error(w, "failed to cancel Plus", http.StatusInternalServerError)
		return
	}
	updated, _ := h.pg.GetUserByID(r.Context(), user.ID)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "cancelled",
		"user":   updated,
	})
}

// GET /api/billing/plus/status
func (h *MonetizationHandler) PlusStatus(w http.ResponseWriter, r *http.Request) {
	user := middleware.GetUser(r.Context())
	if user == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"isPlus": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"isPlus":    user.IsPlus,
		"expiresAt": user.PlusExpiresAt,
	})
}

// ============ DJ Subscriptions ============

// GET /api/billing/dj/{userId}/settings
func (h *MonetizationHandler) GetDJSubSettings(w http.ResponseWriter, r *http.Request) {
	djUserID := r.PathValue("userId")
	if djUserID == "" {
		djUserID = chi_param(r, "userId")
	}
	settings, err := h.pg.GetDJSubSettings(r.Context(), djUserID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if settings == nil {
		writeJSON(w, http.StatusOK, models.DJSubSettings{UserID: djUserID, PriceCents: 499, IsEnabled: false})
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

// POST /api/billing/dj/settings
func (h *MonetizationHandler) UpdateDJSubSettings(w http.ResponseWriter, r *http.Request) {
	user := middleware.GetUser(r.Context())
	if user == nil {
		http.Error(w, "login required", http.StatusUnauthorized)
		return
	}

	var req struct {
		PriceCents int  `json:"priceCents"`
		IsEnabled  bool `json:"isEnabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	// Guardrails: $0.99 - $49.99
	if req.PriceCents < 99 {
		req.PriceCents = 99
	}
	if req.PriceCents > 4999 {
		req.PriceCents = 4999
	}

	if err := h.pg.UpsertDJSubSettings(r.Context(), user.ID, req.PriceCents, req.IsEnabled); err != nil {
		http.Error(w, "failed to update settings", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"priceCents": req.PriceCents,
		"isEnabled":  req.IsEnabled,
	})
}

// POST /api/billing/dj/{userId}/subscribe (dev mode)
func (h *MonetizationHandler) SubscribeToDJ(w http.ResponseWriter, r *http.Request) {
	user := middleware.GetUser(r.Context())
	if user == nil {
		http.Error(w, "login required", http.StatusUnauthorized)
		return
	}
	djUserID := chi_param(r, "userId")
	if djUserID == user.ID {
		http.Error(w, "cannot subscribe to yourself", http.StatusBadRequest)
		return
	}

	// Check if already subscribed
	existing, _ := h.pg.GetDJSubscription(r.Context(), user.ID, djUserID)
	if existing != nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "already_subscribed"})
		return
	}

	// Get DJ price
	settings, _ := h.pg.GetDJSubSettings(r.Context(), djUserID)
	priceCents := 499
	if settings != nil {
		priceCents = settings.PriceCents
	}

	if err := h.pg.SubscribeToDJ(r.Context(), user.ID, djUserID, priceCents, "dev_"+user.ID); err != nil {
		http.Error(w, "failed to subscribe", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":     "active",
		"priceCents": priceCents,
	})
}

// GET /api/billing/dj/{userId}/subscription
func (h *MonetizationHandler) GetDJSubscription(w http.ResponseWriter, r *http.Request) {
	user := middleware.GetUser(r.Context())
	if user == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"subscribed": false})
		return
	}
	djUserID := chi_param(r, "userId")
	sub, _ := h.pg.GetDJSubscription(r.Context(), user.ID, djUserID)
	if sub == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"subscribed": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"subscribed": true,
		"sub":        sub,
	})
}

// ============ Neon ============

// GET /api/billing/neon/packs
func (h *MonetizationHandler) GetNeonPacks(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, models.NeonPacks)
}

// POST /api/billing/neon/buy (dev mode: instant credit)
func (h *MonetizationHandler) BuyNeon(w http.ResponseWriter, r *http.Request) {
	user := middleware.GetUser(r.Context())
	if user == nil {
		http.Error(w, "login required", http.StatusUnauthorized)
		return
	}

	var req struct {
		PackID string `json:"packId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	var pack *models.NeonPack
	for _, p := range models.NeonPacks {
		if p.ID == req.PackID {
			pack = &p
			break
		}
	}
	if pack == nil {
		http.Error(w, "unknown pack", http.StatusBadRequest)
		return
	}

	if err := h.pg.CreditNeon(r.Context(), user.ID, pack.NeonAmount, pack.ID, pack.PriceCents, "dev_"+user.ID); err != nil {
		http.Error(w, "failed to purchase neon", http.StatusInternalServerError)
		return
	}

	balance, _ := h.pg.GetNeonBalance(r.Context(), user.ID)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"neonAdded": pack.NeonAmount,
		"balance":   balance,
	})
}

// GET /api/billing/neon/balance
func (h *MonetizationHandler) GetNeonBalance(w http.ResponseWriter, r *http.Request) {
	user := middleware.GetUser(r.Context())
	if user == nil {
		writeJSON(w, http.StatusOK, map[string]int{"balance": 0})
		return
	}
	balance, _ := h.pg.GetNeonBalance(r.Context(), user.ID)
	writeJSON(w, http.StatusOK, map[string]int{"balance": balance})
}

// POST /api/billing/neon/send
func (h *MonetizationHandler) SendNeon(w http.ResponseWriter, r *http.Request) {
	user := middleware.GetUser(r.Context())
	if user == nil {
		http.Error(w, "login required", http.StatusUnauthorized)
		return
	}

	var req struct {
		RoomID string `json:"roomId"`
		Amount int    `json:"amount"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.Amount < 1 || req.Amount > 10000 {
		http.Error(w, "amount must be 1-10000", http.StatusBadRequest)
		return
	}

	// Look up room to find DJ
	room, _ := h.pg.GetRoomByID(r.Context(), req.RoomID)
	if room == nil {
		http.Error(w, "room not found", http.StatusNotFound)
		return
	}

	// Find DJ user ID (from session creator)
	var djUserID *string
	// Room's DJSessionID is the session, but we need the user.
	// For now, we track via room ID; DJ earnings come from neon_transactions.to_room_id

	if err := h.pg.SpendNeon(r.Context(), user.ID, req.RoomID, djUserID, req.Amount); err != nil {
		if err.Error() == "insufficient neon balance" {
			http.Error(w, "insufficient neon balance", http.StatusPaymentRequired)
			return
		}
		log.Printf("[neon] SpendNeon error for user=%s room=%s amount=%d: %v", user.ID, req.RoomID, req.Amount, err)
		http.Error(w, "failed to send neon: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Get updated tube state
	tube, _ := h.pg.GetNeonTube(r.Context(), req.RoomID)

	// Check if power-up triggered (tube overflowed)
	var poweredUp bool
	if tube != nil && tube.FillAmount >= tube.FillTarget {
		poweredUp = true
		// Level up
		overflow := tube.FillAmount - tube.FillTarget
		nextLevel := tube.Level + 1
		nextTarget := 100
		for _, tl := range models.TubeLevels {
			if tl.Level == nextLevel {
				nextTarget = tl.FillTarget
				break
			}
		}
		if nextLevel > len(models.TubeLevels) {
			nextLevel = 1 // loop back
		}
		h.pg.LevelUpTube(r.Context(), req.RoomID, nextLevel, nextTarget, overflow)
		tube.Level = nextLevel
		tube.FillAmount = overflow
		tube.FillTarget = nextTarget
	}

	balance, _ := h.pg.GetNeonBalance(r.Context(), user.ID)

	// Broadcast to room via WS
	hub := h.hubs.Get(req.RoomID)
	if hub != nil {
		// Use stage name, fall back to display name
		fromName := user.StageName
		if fromName == "" {
			fromName = user.DisplayName
		}
		if fromName == "" {
			fromName = "Someone"
		}

		hub.BroadcastJSON(ws.WSMessage{
			Event: "neon_gift",
			Payload: map[string]interface{}{
				"from":   fromName,
				"amount": req.Amount,
			},
		})
		hub.BroadcastJSON(ws.WSMessage{
			Event:   "tube_update",
			Payload: tube,
		})
		if poweredUp {
			hub.BroadcastJSON(ws.WSMessage{
				Event: "power_up",
				Payload: map[string]interface{}{
					"newLevel": tube.Level,
					"color":    getTubeColor(tube.Level),
				},
			})
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"balance":  balance,
		"tube":     tube,
		"powerUp":  poweredUp,
	})
}

// GET /api/rooms/{roomId}/tube
func (h *MonetizationHandler) GetTubeState(w http.ResponseWriter, r *http.Request) {
	roomID := chi_param(r, "roomId")
	tube, err := h.pg.GetNeonTube(r.Context(), roomID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, tube)
}

// ============ Creator Pool (admin) ============

// POST /api/admin/pool/compute
func (h *MonetizationHandler) ComputePool(w http.ResponseWriter, r *http.Request) {
	user := middleware.GetUser(r.Context())
	if user == nil || !user.IsAdmin {
		http.Error(w, "admin required", http.StatusForbidden)
		return
	}

	var req struct {
		Month   string `json:"month"`   // "2026-03"
		PoolPct int    `json:"poolPct"` // default 40
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.PoolPct <= 0 || req.PoolPct > 100 {
		req.PoolPct = 40
	}

	pool, allocations, err := h.pg.ComputeCreatorPool(r.Context(), req.Month, req.PoolPct)
	if err != nil {
		log.Printf("[pool] compute: %v", err)
		http.Error(w, "failed to compute pool", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"pool":        pool,
		"allocations": allocations,
	})
}

// GET /api/billing/pricing
func (h *MonetizationHandler) GetPricing(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"plus": map[string]interface{}{
			"priceCents":  799,
			"name":        "Jukebox Plus",
			"description": "Ad-free Jukebox UI, Plus badge, and exclusive chat perks",
		},
		"djSubs": map[string]interface{}{
			"defaultPriceCents": 499,
			"minPriceCents":     99,
			"maxPriceCents":     4999,
			"description":       "Support your favorite DJs with a monthly subscription",
		},
		"neonPacks": models.NeonPacks,
		"tubeLevels": models.TubeLevels,
	})
}

// helpers

func getTubeColor(level int) string {
	for _, tl := range models.TubeLevels {
		if tl.Level == level {
			return tl.Color
		}
	}
	return "cyan"
}

func chi_param(r *http.Request, name string) string {
	return chi.URLParam(r, name)
}
