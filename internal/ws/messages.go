package ws

import "encoding/json"

// Event types sent over WebSocket
const (
	// Server -> Client events
	EventPlaybackState = "playback_state"
	EventTrackChanged  = "track_changed"
	EventQueueUpdate   = "queue_update"
	EventChatMessage   = "chat_message"
	EventListenerCount = "listener_count"
	EventRoomSettings  = "room_settings"
	EventRequestUpdate = "request_update"
	EventAnnouncement  = "announcement"
	EventReaction      = "reaction"
	EventListenerList  = "listener_list"
	EventRoomEnded     = "room_ended"
	EventError         = "error"

	// Client -> Server events
	ActionSendChat    = "send_chat"
	ActionReaction    = "reaction"
	ActionSubmitTrack = "submit_track"
	ActionDJSkip      = "dj_skip"
	ActionDJPause     = "dj_pause"
	ActionDJResume    = "dj_resume"
	ActionDJApprove   = "dj_approve"
	ActionDJReject    = "dj_reject"
	ActionDJSetPolicy = "dj_set_policy"
	ActionDJAnnounce  = "dj_announce"
	ActionDJGoLive    = "dj_go_live"
	ActionDJEndRoom   = "dj_end_session"
	ActionDJMic       = "dj_mic"

	// DJ mic state broadcast
	EventDJMicState = "dj_mic_state"
)

// WSMessage is the envelope for all server -> client WebSocket communication.
type WSMessage struct {
	Event   string      `json:"event"`
	Payload interface{} `json:"payload,omitempty"`
}

// InboundMessage is the envelope for client -> server messages.
type InboundMessage struct {
	Action  string          `json:"action"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Typed payloads for inbound actions

type ChatPayload struct {
	Message string `json:"message"`
}

type SubmitTrackPayload struct {
	Title     string `json:"title"`
	Artist    string `json:"artist"`
	Duration  int    `json:"duration"`
	Source    string `json:"source"`
	SourceURL string `json:"sourceUrl"`
}

type ApproveRejectPayload struct {
	EntryID string `json:"entryId"`
}

type SetPolicyPayload struct {
	Policy string `json:"policy"`
}

type AnnouncePayload struct {
	Message string `json:"message"`
}

type ReactionPayload struct {
	Emoji string `json:"emoji"`
}
