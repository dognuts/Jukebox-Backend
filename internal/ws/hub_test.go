package ws

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/jukebox/backend/internal/models"
)

// ==================== fakes ====================

// fakeStore is an in-memory dataStore so hub tests never touch Postgres.
type fakeStore struct {
	mu     sync.Mutex
	room   *models.Room
	queue  []models.QueueEntry
	chats  []models.ChatMessage
	tracks map[string]*models.Track
}

func newFakeStore(room *models.Room) *fakeStore {
	return &fakeStore{room: room, tracks: map[string]*models.Track{}}
}

func (f *fakeStore) GetRoomByID(ctx context.Context, id string) (*models.Room, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.room == nil || f.room.ID != id {
		return nil, nil
	}
	room := *f.room
	return &room, nil
}
func (f *fakeStore) WasRoomEverLive(ctx context.Context, roomID string) (bool, error) {
	return true, nil
}
func (f *fakeStore) DeleteRoom(ctx context.Context, roomID string) error { return nil }
func (f *fakeStore) SetRoomLive(ctx context.Context, roomID string, live bool) error {
	return nil
}
func (f *fakeStore) EndRoom(ctx context.Context, roomID string) error { return nil }
func (f *fakeStore) UpdateRoomPolicy(ctx context.Context, roomID string, policy models.RequestPolicy) error {
	return nil
}

func (f *fakeStore) GetTrack(ctx context.Context, id string) (*models.Track, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tracks[id], nil
}
func (f *fakeStore) UpsertTrack(ctx context.Context, t *models.Track) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tracks[t.ID] = t
	return nil
}

func (f *fakeStore) GetQueue(ctx context.Context, roomID string) ([]models.QueueEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]models.QueueEntry, len(f.queue))
	copy(out, f.queue)
	return out, nil
}
func (f *fakeStore) GetPendingRequests(ctx context.Context, roomID string) ([]models.QueueEntry, error) {
	return nil, nil
}
func (f *fakeStore) AddToQueue(ctx context.Context, entry *models.QueueEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queue = append(f.queue, *entry)
	return nil
}
func (f *fakeStore) UpdateQueueEntryStatus(ctx context.Context, entryID string, status models.QueueEntryStatus) error {
	return nil
}
func (f *fakeStore) PopNextTrack(ctx context.Context, roomID string) (*models.QueueEntry, error) {
	return nil, nil
}
func (f *fakeStore) SetNowPlaying(ctx context.Context, roomID, trackID string) error { return nil }
func (f *fakeStore) ClearNowPlaying(ctx context.Context, roomID string) error        { return nil }

func (f *fakeStore) InsertChatMessage(ctx context.Context, msg *models.ChatMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chats = append(f.chats, *msg)
	return nil
}
func (f *fakeStore) GetRecentChat(ctx context.Context, roomID string, limit int) ([]models.ChatMessage, error) {
	return nil, nil
}

func (f *fakeStore) GetNeonTube(ctx context.Context, roomID string) (*models.NeonTube, error) {
	return nil, nil
}

func (f *fakeStore) StartListenEvent(ctx context.Context, evt *models.ListenEvent) error { return nil }
func (f *fakeStore) EndListenEvent(ctx context.Context, eventID string, tracksHeard int) error {
	return nil
}
func (f *fakeStore) EndListenEventsByUser(ctx context.Context, userID, roomID string) error {
	return nil
}

// fakePresence is an in-memory presenceStore so hub tests never touch Redis.
type fakePresence struct {
	mu          sync.Mutex
	listeners   map[string]bool
	addGate     chan struct{} // if non-nil, AddListener blocks until it is closed
	addStarted  int           // AddListener calls entered (counted before the gate)
	addCalls    int
	removeCalls int
}

func newFakePresence() *fakePresence {
	return &fakePresence{listeners: map[string]bool{}}
}

func (f *fakePresence) AddListener(ctx context.Context, roomID, sessionID string) (int64, error) {
	f.mu.Lock()
	f.addStarted++
	gate := f.addGate
	f.mu.Unlock()
	if gate != nil {
		<-gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addCalls++
	f.listeners[sessionID] = true
	return int64(len(f.listeners)), nil
}
func (f *fakePresence) RemoveListener(ctx context.Context, roomID, sessionID string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removeCalls++
	delete(f.listeners, sessionID)
	return int64(len(f.listeners)), nil
}
func (f *fakePresence) GetPlaybackState(ctx context.Context, roomID string) (*models.PlaybackState, error) {
	return nil, nil
}
func (f *fakePresence) SetPlaybackState(ctx context.Context, state *models.PlaybackState) error {
	return nil
}
func (f *fakePresence) ClearPlaybackState(ctx context.Context, roomID string) error { return nil }
func (f *fakePresence) ClearListeners(ctx context.Context, roomID string) error     { return nil }

// ==================== helpers ====================

const testRoomID = "room-1"

func testRoom(policy models.RequestPolicy) *models.Room {
	return &models.Room{
		ID:            testRoomID,
		Slug:          "test-room",
		Name:          "Test Room",
		RequestPolicy: policy,
		IsLive:        true,
	}
}

func testSession(id, name string) *models.Session {
	return &models.Session{ID: id, DisplayName: name, AvatarColor: "oklch(0.7 0.1 200)"}
}

// newTestHub starts a hub against the fakes and returns it with a cleanup.
func newTestHub(t *testing.T, pg dataStore) *Hub {
	t.Helper()
	return newTestHubWithPresence(t, pg, newFakePresence())
}

// newTestHubWithPresence is newTestHub with a caller-supplied presence
// fake, for tests that need to observe or gate presence calls.
func newTestHubWithPresence(t *testing.T, pg dataStore, pres presenceStore) *Hub {
	t.Helper()
	h := NewHub(testRoomID, "test-room", pg, pres)
	go h.Run()
	t.Cleanup(h.stop)
	return h
}

// addClient inserts a client into the hub's client set directly, bypassing
// the Register channel so tests aren't racing onRegister's join broadcasts
// (listener_join, listener_count) when counting frames.
func addClient(h *Hub, c *Client) {
	h.mu.Lock()
	h.Clients[c] = true
	h.mu.Unlock()
}

// recvFrame pops the next queued frame for the client or fails the test.
func recvFrame(t *testing.T, c *Client, timeout time.Duration) []byte {
	t.Helper()
	select {
	case out := <-c.Send:
		return out.data
	case <-time.After(timeout):
		t.Fatal("timed out waiting for frame")
		return nil
	}
}

type submitResultFrame struct {
	Type  string  `json:"type"`
	OK    bool    `json:"ok"`
	Error *string `json:"error"`
}

// waitForSubmitResult scans the client's outbound frames (skipping any
// other events, e.g. the legacy error event) until a submit_result arrives.
func waitForSubmitResult(t *testing.T, c *Client, timeout time.Duration) submitResultFrame {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case out := <-c.Send:
			var m submitResultFrame
			if json.Unmarshal(out.data, &m) == nil && m.Type == "submit_result" {
				return m
			}
		case <-deadline:
			t.Fatal("no submit_result received")
			return submitResultFrame{}
		}
	}
}

// ==================== tests ====================

// A client that never drains its send buffer must not stall broadcasts to
// the rest of the room; it gets evicted (slow-client disconnect policy).
func TestBroadcastDoesNotBlockOnSlowClient(t *testing.T) {
	h := newTestHub(t, newFakeStore(testRoom(models.RequestPolicyOpen)))

	healthy := NewClient(h, nil, testSession("s-healthy", "Alice"))
	slow := NewClient(h, nil, testSession("s-slow", "Bob"))
	addClient(h, healthy)
	addClient(h, slow)

	// Wedge the slow client: fill its outbound buffer to capacity so the
	// next fanout send to it must fail rather than block.
	for i := 0; i < sendBufferSize; i++ {
		if !slow.send(outbound{data: []byte(`{"event":"filler"}`)}) {
			t.Fatalf("filler frame %d unexpectedly dropped", i)
		}
	}

	h.BroadcastJSON(WSMessage{Event: EventChatMessage, Payload: map[string]string{"message": "hello"}})

	// The healthy client still receives the broadcast.
	frame := recvFrame(t, healthy, 2*time.Second)
	var msg WSMessage
	if err := json.Unmarshal(frame, &msg); err != nil {
		t.Fatalf("unmarshal broadcast: %v", err)
	}
	if msg.Event != EventChatMessage {
		t.Fatalf("healthy client got event %q, want %q", msg.Event, EventChatMessage)
	}

	// The slow client is evicted rather than left to wedge future fanouts.
	evictDeadline := time.Now().Add(2 * time.Second)
	for {
		select {
		case <-slow.done:
		default:
			if time.Now().After(evictDeadline) {
				t.Fatal("slow client was not evicted")
			}
			time.Sleep(5 * time.Millisecond)
			continue
		}
		break
	}
	if n := h.clientCount(); n != 1 {
		t.Fatalf("client count after eviction = %d, want 1", n)
	}
}

// enqueueBroadcast must never block, even when nothing drains the Broadcast
// channel (drop-and-log policy) — this is the self-deadlock guard.
func TestEnqueueBroadcastNeverBlocks(t *testing.T) {
	h := NewHub(testRoomID, "test-room", newFakeStore(nil), newFakePresence())
	// Deliberately do NOT run h.Run(): the Broadcast channel has no consumer.

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < cap(h.Broadcast)+50; i++ {
			h.enqueueBroadcast([]byte(`{"event":"x"}`))
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("enqueueBroadcast blocked with a full Broadcast channel")
	}
}

// submit_track must produce a direct submit_result reply to the submitting
// client (WS contract), followed by the queue_update broadcast echo.
func TestSubmitTrackProducesSubmitResult(t *testing.T) {
	h := newTestHub(t, newFakeStore(testRoom(models.RequestPolicyOpen)))

	client := NewClient(h, nil, testSession("s-1", "Alice"))
	addClient(h, client)

	payload, err := json.Marshal(SubmitTrackPayload{
		Title:     "Test Track",
		Artist:    "Test Artist",
		Duration:  180,
		Source:    "youtube",
		SourceURL: "https://youtube.com/watch?v=abc",
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	select {
	case h.Inbound <- &ClientMessage{Client: client, Message: InboundMessage{Action: ActionSubmitTrack, Payload: payload}}:
	case <-time.After(time.Second):
		t.Fatal("inbound channel blocked")
	}

	res := waitForSubmitResult(t, client, 2*time.Second)
	if !res.OK {
		t.Fatalf("submit_result ok = false, error = %v", res.Error)
	}
	if res.Error != nil {
		t.Fatalf("submit_result error = %q, want null", *res.Error)
	}

	// The queue_update echo follows for an open-policy room.
	deadline := time.After(2 * time.Second)
	for {
		var frame []byte
		select {
		case out := <-client.Send:
			frame = out.data
		case <-deadline:
			t.Fatal("no queue_update broadcast after submit")
		}
		var msg struct {
			Event   string              `json:"event"`
			Payload []models.QueueEntry `json:"payload"`
		}
		if json.Unmarshal(frame, &msg) != nil || msg.Event != EventQueueUpdate {
			continue
		}
		if len(msg.Payload) != 1 || msg.Payload[0].Track.Title != "Test Track" {
			t.Fatalf("queue_update payload = %+v, want the submitted track", msg.Payload)
		}
		return
	}
}

// A failed submit_track must produce submit_result with ok=false and an
// error message — never silence (the client would show a false success).
func TestSubmitTrackFailureProducesSubmitResultError(t *testing.T) {
	// Store with no room: GetRoomByID returns nil → "room not found".
	h := newTestHub(t, newFakeStore(nil))

	client := NewClient(h, nil, testSession("s-1", "Alice"))
	addClient(h, client)

	payload, _ := json.Marshal(SubmitTrackPayload{Title: "T", Artist: "A", Source: "youtube"})
	h.Inbound <- &ClientMessage{Client: client, Message: InboundMessage{Action: ActionSubmitTrack, Payload: payload}}

	res := waitForSubmitResult(t, client, 2*time.Second)
	if res.OK {
		t.Fatal("submit_result ok = true for a failed submit")
	}
	if res.Error == nil || *res.Error == "" {
		t.Fatal("submit_result error missing for a failed submit")
	}
}

// initial_state must carry serverTime (unix epoch ms) so clients can
// compute their clock offset (WS contract).
func TestInitialStateCarriesServerTime(t *testing.T) {
	h := newTestHub(t, newFakeStore(testRoom(models.RequestPolicyOpen)))

	client := NewClient(h, nil, testSession("s-1", "Alice"))
	addClient(h, client)

	before := time.Now().UnixMilli()
	h.sendInitialState(client)
	after := time.Now().UnixMilli()

	frame := recvFrame(t, client, 2*time.Second)
	var msg struct {
		Event      string `json:"event"`
		ServerTime int64  `json:"serverTime"`
	}
	if err := json.Unmarshal(frame, &msg); err != nil {
		t.Fatalf("unmarshal initial_state: %v", err)
	}
	if msg.Event != EventInitialState {
		t.Fatalf("first frame event = %q, want %q", msg.Event, EventInitialState)
	}
	if msg.ServerTime < before || msg.ServerTime > after {
		t.Fatalf("serverTime %d outside [%d, %d]", msg.ServerTime, before, after)
	}
}

// Chat is broadcast before it is persisted; persistence happens async but
// must still reach the store in order.
func TestChatBroadcastFirstThenPersisted(t *testing.T) {
	store := newFakeStore(testRoom(models.RequestPolicyOpen))
	h := newTestHub(t, store)

	client := NewClient(h, nil, testSession("s-1", "Alice"))
	addClient(h, client)

	payload, _ := json.Marshal(ChatPayload{Message: "hello room"})
	h.Inbound <- &ClientMessage{Client: client, Message: InboundMessage{Action: ActionSendChat, Payload: payload}}

	// Broadcast arrives.
	deadline := time.After(2 * time.Second)
	for {
		var frame []byte
		select {
		case out := <-client.Send:
			frame = out.data
		case <-deadline:
			t.Fatal("chat broadcast not received")
		}
		var msg struct {
			Event   string             `json:"event"`
			Payload models.ChatMessage `json:"payload"`
		}
		if json.Unmarshal(frame, &msg) == nil && msg.Event == EventChatMessage {
			if msg.Payload.Message != "hello room" {
				t.Fatalf("chat message = %q, want %q", msg.Payload.Message, "hello room")
			}
			break
		}
	}

	// Persistence follows asynchronously.
	persistDeadline := time.Now().Add(2 * time.Second)
	for {
		store.mu.Lock()
		n := len(store.chats)
		store.mu.Unlock()
		if n == 1 {
			return
		}
		if time.Now().After(persistDeadline) {
			t.Fatalf("chat message not persisted (got %d rows)", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Register-vs-stop race on the DJ-left/room-ended path: a client can pass
// RegisterClient's stopping check and be in flight on the Register channel
// when a stop path commits (sets stopping). The Run loop must refuse it —
// never adding it to the Clients of a dying hub, where it would strand a
// ghost listener in Redis and hang on a hub that no longer broadcasts —
// and must tear it down so its pumps exit and the client reconnects fresh.
func TestRegisterRejectedAfterStopCommitted(t *testing.T) {
	h := newTestHub(t, newFakeStore(testRoom(models.RequestPolicyOpen)))

	client := NewClient(h, nil, testSession("s-late", "Alice"))

	// Reproduce the race window deterministically: claim the in-flight
	// registration slot exactly as RegisterClient does (its stopping check
	// passed), then let a stop path commit before the Run loop receives.
	h.mu.Lock()
	h.pendingRegs++
	h.stopping = true
	h.mu.Unlock()

	select {
	case h.Register <- client:
	case <-time.After(time.Second):
		t.Fatal("Run loop did not receive the in-flight registration")
	}

	// The loop must close the rejected client so its pumps exit.
	select {
	case <-client.done:
	case <-time.After(2 * time.Second):
		t.Fatal("rejected client was not closed")
	}
	// ...and release its ordering barrier: onRegister will never run for
	// it, so a stray onUnregister must not wait on registered forever.
	select {
	case <-client.registered:
	default:
		t.Error("registered barrier not released for rejected client")
	}

	if n := h.clientCount(); n != 0 {
		t.Fatalf("client count = %d, want 0 (client joined a stopping hub)", n)
	}
	h.mu.Lock()
	pending := h.pendingRegs
	h.mu.Unlock()
	if pending != 0 {
		t.Errorf("pendingRegs = %d, want 0", pending)
	}
}

// listener_count broadcasts must be serialized with their presence
// mutations: the per-client onRegister/onUnregister goroutines broadcast
// independently, so two concurrent joins could read counts 1 and 2 from the
// presence store but enqueue "2" before "1", leaving every client showing
// the stale count until the next churn. Regression test for the countMu
// (mutate, enqueue) pairing in updateListenerCount.
func TestListenerCountBroadcastsInOrder(t *testing.T) {
	pres := newFakePresence()
	gate := make(chan struct{})
	pres.addGate = gate

	h := newTestHubWithPresence(t, newFakeStore(testRoom(models.RequestPolicyOpen)), pres)

	// The observer joins directly (no registration broadcasts of its own)
	// and records the fanout order.
	observer := NewClient(h, nil, testSession("s-obs", "Watcher"))
	addClient(h, observer)

	c1 := NewClient(h, nil, testSession("s-1", "Alice"))
	c2 := NewClient(h, nil, testSession("s-2", "Bob"))
	if !h.RegisterClient(c1) || !h.RegisterClient(c2) {
		t.Fatal("RegisterClient returned false on a live hub")
	}

	// One join must be inside AddListener (held open by the gate)...
	deadline := time.Now().Add(2 * time.Second)
	for {
		pres.mu.Lock()
		started := pres.addStarted
		pres.mu.Unlock()
		if started >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no AddListener call started")
		}
		time.Sleep(2 * time.Millisecond)
	}
	// ...while the other join is excluded from the mutation+broadcast
	// critical section. Without that exclusion the second count can be
	// enqueued ahead of the first.
	time.Sleep(50 * time.Millisecond)
	pres.mu.Lock()
	started := pres.addStarted
	pres.mu.Unlock()
	if started != 1 {
		t.Fatalf("addStarted = %d while the first count broadcast is pending, want 1 (mutation+broadcast not serialized)", started)
	}

	close(gate)

	// The observer sees the counts in mutation order: 1 then 2.
	var counts []int
	countDeadline := time.After(2 * time.Second)
	for len(counts) < 2 {
		select {
		case out := <-observer.Send:
			var msg struct {
				Event   string         `json:"event"`
				Payload map[string]int `json:"payload"`
			}
			if json.Unmarshal(out.data, &msg) == nil && msg.Event == EventListenerCount {
				counts = append(counts, msg.Payload["count"])
			}
		case <-countDeadline:
			t.Fatalf("timed out waiting for listener_count frames, got %v", counts)
		}
	}
	if counts[0] != 1 || counts[1] != 2 {
		t.Fatalf("listener_count order = %v, want [1 2]", counts)
	}
}

// On a fast connect-then-disconnect, a client's leave-time I/O must not
// overtake its join-time I/O: RemoveListener before AddListener would
// strand a ghost session in the presence set forever (and EndListenEvent
// before StartListenEvent would leave a never-ended row). This is a
// logical ordering race between the onRegister/onUnregister goroutines —
// not a data race, so -race alone cannot catch it. Regression test for
// the per-client registered barrier.
func TestUnregisterWaitsForRegisterIO(t *testing.T) {
	pres := newFakePresence()
	gate := make(chan struct{})
	pres.addGate = gate

	h := newTestHubWithPresence(t, newFakeStore(testRoom(models.RequestPolicyOpen)), pres)

	client := NewClient(h, nil, testSession("s-fast", "Alice"))
	if !h.RegisterClient(client) {
		t.Fatal("RegisterClient returned false on a live hub")
	}
	// Disconnect immediately, while onRegister is still stuck in
	// AddListener behind the gate.
	h.unregister(client)

	// Give an unordered onUnregister every chance to run RemoveListener
	// first, then let AddListener proceed.
	time.Sleep(20 * time.Millisecond)
	close(gate)

	// Once both calls have landed, the session must be gone from the set.
	deadline := time.Now().Add(2 * time.Second)
	for {
		pres.mu.Lock()
		added, removed, n := pres.addCalls, pres.removeCalls, len(pres.listeners)
		pres.mu.Unlock()
		if added >= 1 && removed >= 1 {
			if n != 0 {
				t.Fatalf("ghost listener stranded in presence set (len=%d): RemoveListener overtook AddListener", n)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for presence calls (addCalls=%d removeCalls=%d)", added, removed)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitForMicState scans the client's outbound frames until a dj_mic_state
// with the wanted active value arrives, skipping every other frame
// (duplicate mic frames are legal at-least-once delivery).
func waitForMicState(t *testing.T, c *Client, wantActive bool, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case out := <-c.Send:
			var msg struct {
				Event   string `json:"event"`
				Payload struct {
					Active bool `json:"active"`
				} `json:"payload"`
			}
			if json.Unmarshal(out.data, &msg) == nil && msg.Event == EventDJMicState && msg.Payload.Active == wantActive {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for dj_mic_state active=%v", wantActive)
		}
	}
}

// A listener who joins while the DJ's mic is already live must receive the
// current mic state during initial state (not wait for the next toggle),
// and the state must clear (with a mic-off broadcast) when the DJ leaves.
//
// The DJ is inserted with addClient (no onRegister) so its initial-state
// goroutine cannot replay the mic event; the only mic-on frame the DJ can
// receive is the toggle's broadcast fanout, which makes it a deterministic
// sync point: once the DJ has the frame, the fanout snapshot is complete
// and a client added afterwards can only learn the state via replay.
func TestMicStateReplayedToLateJoinerAndClearedOnDJLeave(t *testing.T) {
	h := newTestHub(t, newFakeStore(testRoom(models.RequestPolicyOpen)))

	dj := NewClient(h, nil, testSession("s-dj", "DJ Nova"))
	dj.IsDJ = true
	addClient(h, dj)
	close(dj.registered) // addClient bypasses onRegister; release the barrier

	payload, err := json.Marshal(struct {
		Active     bool `json:"active"`
		PauseMusic bool `json:"pauseMusic"`
	}{Active: true, PauseMusic: true})
	if err != nil {
		t.Fatalf("marshal mic payload: %v", err)
	}
	select {
	case h.Inbound <- &ClientMessage{Client: dj, Message: InboundMessage{Action: ActionDJMic, Payload: payload}}:
	case <-time.After(time.Second):
		t.Fatal("inbound channel blocked")
	}
	waitForMicState(t, dj, true, 2*time.Second) // fanout complete

	// Late joiner: the replay is the only possible mic-on source.
	late := NewClient(h, nil, testSession("s-late", "Late"))
	addClient(h, late)
	h.sendInitialState(late)
	waitForMicState(t, late, true, 2*time.Second)

	// DJ leaves: remaining listeners hear mic-off.
	select {
	case h.Unregister <- dj:
	case <-time.After(time.Second):
		t.Fatal("unregister channel blocked")
	}
	waitForMicState(t, late, false, 2*time.Second)

	// A joiner after the off-broadcast must get no stale mic-on replay.
	// sendInitialState is synchronous, so its frames are queued when it
	// returns; mic-off fanout frames may also be present and are fine.
	late2 := NewClient(h, nil, testSession("s-late2", "Later"))
	addClient(h, late2)
	h.sendInitialState(late2)
	for {
		select {
		case out := <-late2.Send:
			var msg struct {
				Event   string `json:"event"`
				Payload struct {
					Active bool `json:"active"`
				} `json:"payload"`
			}
			if json.Unmarshal(out.data, &msg) == nil && msg.Event == EventDJMicState && msg.Payload.Active {
				t.Fatal("stale mic-on replayed to a joiner after the DJ left")
			}
		default:
			return
		}
	}
}
