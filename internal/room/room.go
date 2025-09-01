package room

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"rtc_testing/internal"
	"slices"
	"sort"
	"strings"
	"sync/atomic"

	"io"
	"log"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/pion/interceptor"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4/pkg/media/samplebuilder"
	openai "github.com/sashabaranov/go-openai"
	op2 "gopkg.in/hraban/opus.v2"

	"github.com/pion/webrtc/v4"

	//"github.com/pion/webrtc/v4/pkg/media/ivfwriter"
	//"github.com/pion/webrtc/v4/pkg/media/oggwriter"
	"golang.org/x/sync/errgroup"
)

const (
	sampleRate         = 48000
	channels           = 2
	frameSamples       = 960 // 20 ms при 48 кГц
	realtimeWhisperURL = "wss://api.openai.com/v1/realtime?intent=transcription"
)

type messageData struct {
	MessageType string `json:"message_type"`
	MessageData string `json:"message_data"`
}

// PeerKind presentation layer
type PeerKind string
type Lang string

const (
	PeerHuman PeerKind = "human"
	PeerBOT   PeerKind = "bot"
	PeerSys   PeerKind = "sys"
)

type EventType string

const (
	EvtPeerJoined  EventType = "evt.peer_joined"
	EvtPeerLeft    EventType = "evt.peer_left"
	EvtMessage     EventType = "evt.message_posted"
	EvtHeartbeat   EventType = "evt.heartbeat"
	EvtPeerTalking EventType = "evt.peer_talking"
	EvtCustom      EventType = "evt.custom"
)

type PeerRoomEventsPayload struct {
	Peer     PeerView `json:"peer,omitempty"`
	Snapshot Snapshot `json:"snapshot"`
	PeerID   string   `json:"peer_id,omitempty"`
}

type PeerView struct {
	Kind  PeerKind       `json:"kind"`
	ID    string         `json:"id,omitempty"`
	Name  string         `json:"name,omitempty"`
	Lang  Lang           `json:"lang,omitempty"`
	Attrs map[string]any `json:"attrs,omitempty"`
}

type Snapshot struct {
	Count int        `json:"count"`
	Peers []PeerView `json:"peers"`
	Langs []Lang     `json:"langs"`
}

type SnapshotDiff struct {
	Added   []PeerView `json:"added,omitempty"`
	Removed []string   `json:"removed,omitempty"`
	Updated []PeerView `json:"updated,omitempty"`
	Count   uint8      `json:"count"`
}

type MsgPart struct {
	Text string `json:"text,omitempty"`
	Blob []byte `json:"blob,omitempty"`
	Mime string `json:"mime,omitempty"`
}

type MessageView struct {
	From     PeerView         `json:"from_id"`
	PeerKind PeerKind         `json:"peer_kind"`
	Msg      map[Lang]MsgPart `json:"msg"`
	At       time.Time        `json:"at"`
	Meta     map[string]any   `json:"meta,omitempty"`
	MsgSeq   int              `json:"msgSeq"`
}

type EventEnvelope struct {
	Version int       `json:"version"`
	Type    EventType `json:"type"`
	RoomID  string    `json:"roomID"`
	Seq     int       `json:"seq"`
	At      time.Time `json:"at"`
	Data    any       `json:"data,omitempty"`
}

type Peer struct {
	ID          string `json:"id"`
	Lang        Lang   `json:"lang"`
	SignalConn  *websocket.Conn
	PeerConn    *webrtc.PeerConnection
	inStream    *InStream
	messages    chan []byte
	offers      chan []byte
	asrChan     chan []int16
	pcReady     atomic.Bool
	negotiateCh chan struct{}
	makingOffer atomic.Bool
	negMu       sync.Mutex
}

type Room struct {
	ID              string
	Peers           map[string]*Peer
	pendingSubs     map[string][]string
	OAKey           string
	mu              sync.RWMutex
	languages       map[Lang]bool
	routers         map[string]*Router
	shouldTranslate *atomic.Bool
	evtSeq, msgSeq  int
}

type Router struct {
	key     string
	ownerID string
	remote  *webrtc.TrackRemote
	subsMu  sync.RWMutex
	subs    map[string]*webrtc.TrackLocalStaticRTP
	cancel  context.CancelFunc
}

type StartConversationRequest struct {
	RoomID      string `json:"room_id"`
	PeerID      string `json:"peer_id"`
	Lang        Lang   `json:"lang"`
	OpenAiToken string `json:"open_ai_token,omitempty"`
}

type StartWebrtcConnectionRequest struct {
	RoomID string `json:"room_id"`
	PeerID string `json:"peer_id"`
}

func (room *Room) incLockedEvtSeq() int { room.evtSeq++; return room.evtSeq }
func (room *Room) incLockedMsgSeq() int { room.msgSeq++; return room.msgSeq }

func (room *Room) PublishEvent(evt EventEnvelope) {
	data, err := json.Marshal(evt)
	if err != nil {
		log.Printf("Error marshalling event: %s", err)
		return
	}

	room.mu.Lock()
	defer room.mu.Unlock()

	for _, peer := range room.Peers {
		select {
		case peer.messages <- data:
		default:
			log.Println("dropping message to peer", peer.ID)
		}
	}
}

type Server struct {
	HttpSrv     *http.Server
	rooms       map[string]*Room
	mu          sync.RWMutex
	mBufferSize int
	oBufferSize int
	logger      *slog.Logger
}

type InStream struct {
	sb        *samplebuilder.SampleBuilder
	dec       *op2.Decoder
	mixChan   chan []float32
	channels  int
	pending   []int16 // >>20ms
	scratch   []int16
	ring      []float32
	lastFrame []float32
	mu        sync.RWMutex
}

func (i *InStream) pop20ms() []int16 {
	if i.dec == nil {
		return nil
	}

	need := frameSamples * i.channels // 960 * ch

	for len(i.pending) < need {
		smp := i.sb.Pop()
		if smp == nil {
			// нет готовых RTP-фреймов — пока не можем отдать 20мс
			return nil
		}

		out := i.scratch[:cap(i.scratch)]
		n, err := i.dec.Decode(smp.Data, out)
		if err != nil || n == 0 {
			continue
		}
		out = out[:n*i.channels]

		i.pending = append(i.pending, out...)
	}

	out := make([]int16, need)
	copy(out, i.pending[:need])
	i.pending = i.pending[need:]
	return out
}

func NewPeer(peerId string, lang Lang, messagesBufSize int) *Peer {
	peerInstream := NewInStream()
	if peerInstream == nil {
		return nil
	}

	return &Peer{
		ID:          peerId,
		Lang:        lang,
		messages:    make(chan []byte, messagesBufSize),
		offers:      make(chan []byte, messagesBufSize),
		asrChan:     make(chan []int16, 12),
		inStream:    peerInstream,
		negotiateCh: make(chan struct{}, 1),
	}
}

func NewInStream() *InStream {
	opusDepay := &codecs.OpusPacket{}
	dec, err := op2.NewDecoder(sampleRate, channels)
	if err != nil {
		log.Println(err)
		return nil
	}
	return &InStream{
		sb:      samplebuilder.New(50, opusDepay, sampleRate),
		dec:     dec,
		mixChan: make(chan []float32, 12),
	}
}

func RunServer(ctx context.Context, port string, logger *slog.Logger) *Server {
	rs := Server{rooms: map[string]*Room{}, mBufferSize: 16, oBufferSize: 16, logger: logger}
	fs := http.FileServer(http.Dir("frontend"))

	mux := http.NewServeMux()
	mux.HandleFunc("/subscribe", rs.subscribeHandler(ctx))
	mux.HandleFunc("/create/room", rs.addRoom(ctx))
	mux.HandleFunc("/join/room", rs.joinRoom)
	mux.Handle("/", fs)

	srv := http.Server{
		Addr:    fmt.Sprintf("0.0.0.0:%s", port),
		Handler: mux,
	}
	rs.HttpSrv = &srv

	return &rs
}

func requestNegotiation(p *Peer) {
	select {
	case p.negotiateCh <- struct{}{}:
	default:
	}
}

func getTranscribeFunc(ctx context.Context, out chan<- string, lang Lang, token string) func([]int16, int) {
	closed := atomic.Bool{}
	closed.Store(false)
	headers := http.Header{
		"Authorization": []string{"Bearer " + token},
		"OpenAI-Beta":   []string{"realtime=v1"},
	}

	opts := &websocket.DialOptions{
		HTTPHeader: headers,
	}

	conn, r, err := websocket.Dial(ctx, realtimeWhisperURL, opts)
	if err != nil {
		log.Printf("dial error: %v", err)
		return nil
	}

	if r.StatusCode != http.StatusSwitchingProtocols {
		b, _ := io.ReadAll(r.Body)
		log.Printf("ws status=%d body=%s", r.StatusCode, string(b))
		return nil
	}

	conn.SetReadLimit(10 * 1024 * 1024)

	go func() {
		<-ctx.Done()
		fmt.Println("Context done for transcibe func")
		_ = conn.Close(websocket.StatusNormalClosure, "context done")
	}()

	go func() {
		for {
			mt, data, readErr := conn.Read(ctx)
			if readErr != nil {
				closed.Store(true)
				log.Println("realtime read:", readErr)
				return
			}
			if mt != websocket.MessageText {
				continue
			}

			var msg map[string]any
			if json.Unmarshal(data, &msg) != nil {
				continue
			}
			log.Println(msg)

			switch msg["type"] {
			case "conversation.item.input_audio_transcription.delta":
				log.Println("audio transcription recv")
				continue
			case "conversation.item.input_audio_transcription.completed":
				if tr, ok := msg["transcript"].(string); ok && tr != "" {
					select {
					case out <- tr:
					default:
					}
				}
			case "error":
				fmt.Println(msg)
			}
		}
	}()

	if r.Body != nil {
		fmt.Println(r.Body)
		_ = r.Body.Close()
	}

	wsWriteErr := wsjson.Write(ctx, conn, map[string]any{
		"type": "transcription_session.update",
		"session": map[string]any{
			"input_audio_format": "pcm16",
			"input_audio_transcription": map[string]any{
				"model":    "whisper-1", // или gpt-4o-transcribe / whisper-1
				"language": lang,
			},
			"turn_detection": map[string]any{
				"type":                "server_vad",
				"threshold":           0.6,
				"prefix_padding_ms":   300,
				"silence_duration_ms": 200,
			},
			"input_audio_noise_reduction": map[string]any{
				"type": "far_field",
			},
		},
	})

	if wsWriteErr != nil {
		return nil
	}

	return func(chunk []int16, channels int) {
		if closed.Load() {
			return
		}

		if err := wsjson.Write(ctx, conn, map[string]any{
			"type":  "input_audio_buffer.append",
			"audio": internal.Base64Encode(int16ToBytesLE(stereo48kToMono24k(chunk, channels))),
		}); err != nil {
			fmt.Println("write:", err)
			out <- "[error] " + err.Error()
		}
	}
}

func readRemoteTrackAudio(ctx context.Context, peer *Peer, transcriptFunc func([]int16, int)) (stop func()) {
	bufWindowSize := sampleRate * peer.inStream.channels / 5

	pcmBuf := make([]int16, 0, frameSamples*peer.inStream.channels*50)

	done := make(chan struct{})

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case payload, ok := <-peer.asrChan:
				if !ok {
					return
				}

				pcmBuf = append(pcmBuf, payload...)

				for len(pcmBuf) >= bufWindowSize {
					chunk := make([]int16, bufWindowSize)
					copy(chunk, pcmBuf[:bufWindowSize])
					pcmBuf = pcmBuf[bufWindowSize:]

					transcriptFunc(chunk, peer.inStream.channels)
				}
			}
		}
	}()

	return func() { close(done) }
}

func (room *Room) flushPendingSubs(p *Peer) {
	room.mu.Lock()
	keys := room.pendingSubs[p.ID]
	delete(room.pendingSubs, p.ID)
	room.mu.Unlock()

	for _, rtKey := range keys {
		room.mu.RLock()
		rt := room.routers[rtKey]
		if rt != nil {
			_ = room.subscribeRTP(p, rt)
		}
		room.mu.RUnlock()
	}
}

func (room *Room) subscribeRTP(peer *Peer, router *Router) error {
	if !peer.pcReady.Load() {
		room.mu.Lock()
		room.pendingSubs[peer.ID] = append(room.pendingSubs[peer.ID], router.key)
		room.mu.Unlock()

		return nil
	}

	router.subsMu.Lock()
	if _, exists := router.subs[peer.ID]; exists {
		router.subsMu.Unlock()
		return nil
	}

	trackID := fmt.Sprintf("%s-%s", router.ownerID, router.remote.ID())
	streamID := fmt.Sprintf("%s-%s", router.ownerID, router.remote.StreamID())

	loc, err := webrtc.NewTrackLocalStaticRTP(router.remote.Codec().RTPCodecCapability, trackID, streamID)
	if err != nil {
		router.subsMu.Unlock()
		return err
	}
	if _, err = peer.PeerConn.AddTrack(loc); err != nil {
		router.subsMu.Unlock()
		return err
	}

	router.subs[peer.ID] = loc
	router.subsMu.Unlock()

	requestNegotiation(peer)
	return nil
}

func (room *Room) onRouterCreated(rt *Router, ownerID string) {
	room.mu.Lock()
	room.routers[rt.key] = rt

	readyPeers := make([]*Peer, 0, len(room.Peers))
	for pid, p := range room.Peers {
		if pid == ownerID {
			continue
		}
		if p.PeerConn != nil && p.pcReady.Load() {
			readyPeers = append(readyPeers, p)
		} else {
			room.pendingSubs[p.ID] = append(room.pendingSubs[p.ID], rt.key)
		}
	}
	room.mu.Unlock()

	for _, p := range readyPeers {
		_ = room.subscribeRTP(p, rt)
	}
}

func startWebrtcConnection(ctx context.Context, offerEncoded string, peer *Peer, room *Room) {
	mediaEngine := &webrtc.MediaEngine{}

	if err := mediaEngine.RegisterDefaultCodecs(); err != nil {
		log.Fatal(err)
	}
	interceptorRegistry := &interceptor.Registry{}

	if err := webrtc.RegisterDefaultInterceptors(mediaEngine, interceptorRegistry); err != nil {
		panic(err)
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine), webrtc.WithInterceptorRegistry(interceptorRegistry))

	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
	}

	peerConnection, err := api.NewPeerConnection(config)
	peer.PeerConn = peerConnection
	if err != nil {
		panic(err)
	}
	defer func() {
		if cErr := peerConnection.Close(); cErr != nil {
			fmt.Printf("cannot close peerConnection: %v\n", cErr)
		}
	}()

	go func() {
		for range peer.negotiateCh {
			peer.negMu.Lock()
			if peer.PeerConn == nil {
				peer.negMu.Unlock()
				continue
			}
			if peer.makingOffer.Load() {
				peer.negMu.Unlock()
				continue
			}

			peer.makingOffer.Store(true)
			offer, err := peer.PeerConn.CreateOffer(nil)
			if err == nil {
				_ = peer.PeerConn.SetLocalDescription(offer)
				<-webrtc.GatheringCompletePromise(peer.PeerConn)
				data := map[string]string{"offer": internal.EncodeOffer(&offer)}
				payload, _ := json.Marshal(data)
				select {
				case peer.offers <- payload:
				default:
				}
			}
			peer.makingOffer.Store(false)
			peer.negMu.Unlock()
		}
	}()

	if err != nil {
		panic(err)
	}

	resultTranscriptionChan := make(chan string, 40)
	go func() {
		for r := range resultTranscriptionChan {
			if strings.Contains(r, "[error]") {
				continue
			}
			translations := make(map[Lang]MsgPart, 2)
			room.mu.RLock()
			for lang, _ := range room.languages {
				if peer.Lang == lang {
					translations[lang] = MsgPart{Text: r}
				} else {
					translation := translateTo(peer.Lang, lang, room.OAKey, r)
					translations[lang] = MsgPart{Text: translation}
				}
			}
			evtSeq := room.incLockedEvtSeq()
			msgSeq := room.incLockedMsgSeq()
			room.mu.RUnlock()
			now := time.Now()

			room.PublishEvent(EventEnvelope{RoomID: room.ID, Version: 1, Type: EvtMessage, Seq: evtSeq, At: now, Data: MessageView{From: room.toPeerView(peer), MsgSeq: msgSeq, PeerKind: PeerHuman, At: now, Msg: translations}})
		}
	}()

	transcriptionFn := getTranscribeFunc(ctx, resultTranscriptionChan, peer.Lang, room.OAKey)
	if transcriptionFn == nil {
		log.Println("transcriptionFn is nil")
		return
	}

	peerConnection.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) { //nolint: revive
		if track.Kind() != webrtc.RTPCodecTypeAudio {
			return
		}

		key := peer.ID + "|" + track.ID()
		trCtx, cancel := context.WithCancel(ctx)

		rt := &Router{
			key:     key,
			ownerID: peer.ID,
			remote:  track,
			subs:    make(map[string]*webrtc.TrackLocalStaticRTP),
			cancel:  cancel,
		}

		room.onRouterCreated(rt, peer.ID)

		fmt.Println(rt)

		ch := int(track.Codec().Channels)
		if ch <= 0 {
			ch = 1
		}

		if ch != 2 {
			dec, decErr := op2.NewDecoder(sampleRate, ch)
			if decErr != nil {
				log.Println("failed to create decoder", decErr)
				return
			}
			peer.inStream.dec = dec
		}

		peer.inStream.channels = ch
		peer.inStream.pending = peer.inStream.pending[:0]
		peer.inStream.scratch = make([]int16, 2880*ch)

		go func() {
			<-ctx.Done()
			receiver.Stop()
		}()

		log.Printf("Track has started, of type %d: %s \n", track.PayloadType(), track.Codec().MimeType)
		log.Printf("Channels count %d: \n", track.Codec().Channels)

		stopWorker := readRemoteTrackAudio(ctx, peer, transcriptionFn)
		defer stopWorker()

		for {
			select {
			case <-trCtx.Done():
				return
			default:
			}
			rtpd, _, readErr := rt.remote.ReadRTP()

			if readErr != nil {
				if !errors.Is(readErr, io.EOF) && !errors.Is(readErr, context.Canceled) {
					fmt.Println("ReadRTP error:", readErr)
				}
				rt.cancel()
				return
			}

			fmt.Println(rt.subs)

			rt.subsMu.RLock()
			for pid, loc := range rt.subs {
				if wErr := loc.WriteRTP(rtpd); wErr != nil {
					_ = wErr
					_ = pid
				}
			}
			rt.subsMu.RUnlock()

			peer.inStream.sb.Push(rtpd)

			if pcm48 := peer.inStream.pop20ms(); pcm48 != nil {
				select {
				case peer.asrChan <- pcm48:
				default:
				}
			}
		}
	})

	// Set the handler for ICE connection state
	// This will notify you when the peer has connected/disconnected
	peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		fmt.Printf("SignalConn State has changed %s \n", connectionState.String())

		if connectionState == webrtc.ICEConnectionStateConnected {
			fmt.Println("Ctrl+C the remote client to stop the demo")
		}

		if connectionState == webrtc.ICEConnectionStateClosed {
			fmt.Println("Ice state closed")
		}
	})

	peerConnection.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		switch state {
		case webrtc.PeerConnectionStateFailed:
			fmt.Println("SignalConn failed")
		case webrtc.PeerConnectionStateClosed:
			fmt.Println("SignalConn closed")
		default:
			fmt.Printf("Peer SignalConn State has changed: %s\n", state.String())
		}
	})

	offer := webrtc.SessionDescription{}

	internal.DecodeOffer(offerEncoded, &offer)

	if err = peerConnection.SetRemoteDescription(offer); err != nil {
		panic(err)
	}

	answer, err := peerConnection.CreateAnswer(&webrtc.AnswerOptions{OfferAnswerOptions: webrtc.OfferAnswerOptions{VoiceActivityDetection: true}})
	if err != nil {
		panic(err)
	}

	gatherComplete := webrtc.GatheringCompletePromise(peerConnection)

	if err = peerConnection.SetLocalDescription(answer); err != nil {
		panic(err)
	}

	<-gatherComplete

	answerData := make(map[string]string)
	answerData["answer"] = internal.EncodeOffer(peerConnection.LocalDescription())
	data, err := json.Marshal(answerData)
	if err != nil {
		fmt.Println(err)
		return
	}

	peer.offers <- data

	peer.pcReady.Store(true)

	room.flushPendingSubs(peer)

	select {
	case _ = <-ctx.Done():
		return
	}
}

func translateTo(from, to Lang, token, value string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client := openai.NewClient(token)

	resp, err := client.CreateChatCompletion(
		ctx,
		openai.ChatCompletionRequest{
			Model: openai.GPT4oMini,
			Messages: []openai.ChatCompletionMessage{
				{
					Role:    openai.ChatMessageRoleSystem,
					Content: fmt.Sprintf("You are a translator. Translate everything from %s into %s.", from, to),
				},
				{
					Role:    openai.ChatMessageRoleUser,
					Content: value,
				},
			},
		},
	)
	if err != nil {
		log.Fatalf("ChatCompletion error: %v\n", err)
	}

	return resp.Choices[0].Message.Content
}

func (rs *Server) getRoom(id string) *Room {
	rs.mu.RLock()
	defer rs.mu.RUnlock()

	if room, found := rs.rooms[id]; found {
		room.updateTranslationFlag()
		return room
	}

	return nil
}

func (rs *Server) createRoom(id, openaiKey string) *Room {
	room := NewRoom(id, openaiKey)
	rs.mu.Lock()
	rs.rooms[id] = room
	rs.mu.Unlock()
	return room
}

func NewRoom(id, openaiKey string) *Room {
	shouldTranslate := &atomic.Bool{}
	shouldTranslate.Store(false)
	return &Room{
		Peers:           make(map[string]*Peer),
		ID:              id,
		shouldTranslate: shouldTranslate,
		/// todo: test to
		languages:   map[Lang]bool{"en": true},
		evtSeq:      0,
		msgSeq:      0,
		OAKey:       openaiKey,
		pendingSubs: map[string][]string{},
		routers:     map[string]*Router{},
	}
}

func (room *Room) updateTranslationFlag() {
	room.mu.RLock()
	defer room.mu.RUnlock()

	langSet := make(map[Lang]struct{})
	for _, peer := range room.Peers {
		if peer.Lang != "" {
			langSet[peer.Lang] = struct{}{}
		}
	}

	room.shouldTranslate.Store(len(langSet) > 1)
}

func (rs *Server) removeRoom(id string) (err error) {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	if rs.rooms[id] == nil {
		return fmt.Errorf("room %s does not exist", id)
	}

	if len(rs.rooms[id].Peers) > 0 {
		rs.rooms[id].mu.Lock()
		for _, member := range rs.rooms[id].Peers {
			delete(rs.rooms[id].Peers, member.ID)
		}
		rs.rooms[id].mu.Unlock()
	}

	delete(rs.rooms, id)

	return nil
}

func (rs *Server) joinRoom(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		w.Write([]byte("only post method is allowed"))
		return
	}

	var reqData StartConversationRequest

	err := json.NewDecoder(r.Body).Decode(&reqData)
	if err != nil {
		log.Println("decode error: ", err)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(err.Error()))
		return
	}

	if reqData.PeerID == "" || reqData.RoomID == "" || reqData.Lang == "" {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("name, lang or roomID were not provided"))
		return
	}

	room := rs.getRoom(reqData.RoomID)
	if room == nil {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("room not found"))
		return
	}

	peer := NewPeer(reqData.PeerID, reqData.Lang, rs.mBufferSize)
	if peer == nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("peer was not added"))
		return
	}

	room.addPeer(peer)
	room.updateTranslationFlag()

	response := make(map[string]interface{})

	response["peer_id"] = peer.ID
	response["room_id"] = reqData.RoomID
	response["message"] = "success"

	w.WriteHeader(http.StatusOK)
	err = json.NewEncoder(w).Encode(response)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("fixing it"))
		return
	}
}

func (room *Room) runHeartbeatTicker(ctx context.Context) {
	ticker := time.NewTicker(time.Second * 2)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				room.mu.Lock()
				evtSeq := room.incLockedEvtSeq()
				room.mu.Unlock()

				room.PublishEvent(EventEnvelope{RoomID: room.ID, Version: 1, Type: EvtHeartbeat, At: time.Now(), Seq: evtSeq, Data: room.rosterSnapshotLocked()})
			case <-ctx.Done():
				return
			}
		}
	}()
}

func (rs *Server) addRoom(appCtx context.Context) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			w.Write([]byte("only post method is allowed"))
			return
		}

		var reqData StartConversationRequest

		err := json.NewDecoder(r.Body).Decode(&reqData)
		if err != nil {
			log.Println("decode error: ", err)
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(err.Error()))
			return
		}

		if reqData.PeerID == "" || reqData.RoomID == "" || reqData.OpenAiToken == "" || reqData.Lang == "" {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte("name, lang or roomID were not provided"))
			return
		}

		ctx, cancel := context.WithTimeout(appCtx, time.Second*5)
		defer cancel()

		oaClient := openai.NewClient(reqData.OpenAiToken)
		//testing if key is valid
		_, err = oaClient.ListModels(ctx)
		if err != nil {
			fmt.Println(err)
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte("provided invalid token"))
			return
		}

		room := rs.getRoom(reqData.RoomID)
		if room == nil {
			room = rs.createRoom(reqData.RoomID, reqData.OpenAiToken)
		} else {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte("room already exists"))
			return
		}

		peer := NewPeer(reqData.PeerID, reqData.Lang, rs.mBufferSize)
		if peer == nil {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("peer was not added"))
			return
		}

		room.addPeer(peer)
		room.updateTranslationFlag()
		room.runHeartbeatTicker(appCtx)

		response := make(map[string]interface{})

		response["peer_id"] = peer.ID
		response["room_id"] = reqData.RoomID
		response["message"] = "success"

		w.WriteHeader(http.StatusOK)
		err = json.NewEncoder(w).Encode(response)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("fixing it"))
			return
		}
	}
}

func (rs *Server) subscribeHandler(appCtx context.Context) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			w.Write([]byte("only get method is allowed"))
			return
		}

		var reqData StartWebrtcConnectionRequest

		query := r.URL.Query()

		reqData.RoomID = query.Get("room_id")
		reqData.PeerID = query.Get("peer_id")

		if reqData.RoomID == "" || reqData.PeerID == "" {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte("room_id and peer_id are required"))
			return
		}

		room := rs.getRoom(reqData.RoomID)
		if room == nil {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte("room not found"))
			return
		}

		room.mu.RLock()
		// todo: add get methods for
		peer, found := room.Peers[reqData.PeerID]
		if !found {
			room.mu.RUnlock()
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte("peer not found"))
			return
		}
		room.mu.RUnlock()

		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			// todo: figure out if yes
			fmt.Println(err)
			return
		}

		peer.SignalConn = c

		roomCtx, cancel := context.WithCancel(appCtx)
		defer cancel()

		eg, ctx := errgroup.WithContext(roomCtx)

		eg.Go(func() error {
			for {
				var msg messageData
				if err := wsjson.Read(ctx, c, &msg); err != nil {
					cancel()
					return errors.New("failed to read from connection")
				}

				switch msg.MessageType {
				case "leave":
					cancel()
				case "server_answer":
					var ans webrtc.SessionDescription
					internal.DecodeOffer(msg.MessageData, &ans)
					if ans.Type != webrtc.SDPTypeAnswer {
						log.Println("unexpected SDP type:", ans.Type)
						break
					}
					if err := peer.PeerConn.SetRemoteDescription(ans); err != nil {
						log.Println("SetRemoteDescription(answer) err:", err)
					}
				case "offer":
					if peer.PeerConn == nil {
						// on first offer
						go startWebrtcConnection(ctx, msg.MessageData, peer, room)
						break
					}
				case "message":
					room.mu.Lock()
					evtSeq := room.incLockedEvtSeq()
					msgSeq := room.incLockedMsgSeq()

					translations := map[Lang]MsgPart{}

					for lang, _ := range room.languages {
						translations[lang] = MsgPart{Text: msg.MessageData}
					}

					room.mu.Unlock()

					now := time.Now()

					room.PublishEvent(EventEnvelope{RoomID: room.ID, Version: 1, Type: EvtMessage, Seq: evtSeq, At: now, Data: MessageView{From: room.toPeerView(peer), MsgSeq: msgSeq, PeerKind: PeerHuman, At: now, Msg: translations}})
				}
			}
		})

		eg.Go(func() error {
			for {
				select {
				case m, ok := <-peer.messages:
					if !ok {
						return nil
					}
					writeWithTimeout(ctx, peer, m)
					continue
				case o, ok := <-peer.offers:
					if !ok {
						return nil
					}
					writeWithTimeout(ctx, peer, o)
					continue
				case <-ctx.Done():
					log.Println("Context done in subscribe handler")
					room.kickPeer(peer.ID)
					return ctx.Err()
				}
			}
		})

		if err := eg.Wait(); err != nil {
			log.Printf("ws error: %v", err)
		}

		c.Close(websocket.StatusNormalClosure, "")
	}
}

func int16ToBytesLE(xs []int16) []byte {
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.LittleEndian, xs)
	return buf.Bytes()
}

func stereo48kToMono24k(in []int16, channels int) []int16 {
	switch channels {
	case 1:
		outLen := len(in) / 2
		out := make([]int16, outLen)
		for i := 0; i < outLen; i++ {
			a := int(in[2*i])
			b := int(in[2*i+1])
			out[i] = int16((a + b) / 2)
		}
		return out

	case 2:
		// 48k stereo -> 24k mono:
		// шаг 1: downmix: (L+R)/2
		// шаг 2: децимация ×2 с усреднением соседних моно-сэмплов
		frames := len(in) / 2
		outLen := frames / 2
		out := make([]int16, outLen)
		for j := 0; j < outLen; j++ {
			// два последовательных стерео-фрейма: (L0,R0),(L1,R1)
			l0 := int(in[4*j])
			r0 := int(in[4*j+1])
			l1 := int(in[4*j+2])
			r1 := int(in[4*j+3])
			m0 := (l0 + r0) / 2
			m1 := (l1 + r1) / 2
			out[j] = int16((m0 + m1) / 2)
		}
		return out

	default:
		return nil
	}
}

func writeWithTimeout(ctx context.Context, member *Peer, m []byte) {
	timeoutCtx, cancel := context.WithTimeout(ctx, time.Second*10)
	defer cancel()
	member.SignalConn.Write(timeoutCtx, websocket.MessageText, m)
}

func (room *Room) kickPeer(peerID string) {
	room.mu.Lock()
	peer, memberFound := room.Peers[peerID]
	peerViewEntity := peer
	if !memberFound {
		room.mu.Unlock()
		return
	}

	if peer.PeerConn != nil {
		_ = peer.PeerConn.Close()
	}
	if peer.SignalConn != nil {
		_ = peer.SignalConn.Close(websocket.StatusNormalClosure, "bye")
	}

	close(peer.messages)
	close(peer.offers)
	delete(room.Peers, peerID)

	evtSeq := room.incLockedEvtSeq()

	room.mu.Unlock()

	room.PublishEvent(EventEnvelope{RoomID: room.ID, Version: 1, At: time.Now(), Seq: evtSeq, Type: EvtPeerLeft, Data: PeerRoomEventsPayload{Peer: room.toPeerView(peerViewEntity), Snapshot: room.rosterSnapshotLocked()}})
}

func (room *Room) rosterSnapshotLocked() Snapshot {
	out := make([]PeerView, 0, len(room.Peers))
	langs := map[Lang]bool{}
	room.mu.RLock()
	for _, p := range room.Peers {
		out = append(out, room.toPeerView(p))
		if _, found := langs[p.Lang]; !found {
			langs[p.Lang] = true
		}
	}
	room.mu.RUnlock()

	langsArr := slices.Collect(maps.Keys(langs))

	sort.Slice(out, func(i, j int) bool {
		if out[i].Name == out[j].Name {
			return out[i].ID < out[j].ID
		}
		return out[i].Name < out[j].Name
	})
	return Snapshot{Count: len(out), Peers: out, Langs: langsArr}
}

func (room *Room) addPeer(peer *Peer) {
	room.mu.Lock()
	room.Peers[peer.ID] = peer
	if _, foundLang := room.languages[peer.Lang]; !foundLang {
		room.languages[peer.Lang] = true
	}

	evtSeq := room.incLockedEvtSeq()

	room.mu.Unlock()

	room.PublishEvent(EventEnvelope{RoomID: room.ID, Version: 1, At: time.Now(), Seq: evtSeq, Type: EvtPeerJoined, Data: PeerRoomEventsPayload{Peer: room.toPeerView(peer), Snapshot: room.rosterSnapshotLocked()}})
}

func (room *Room) toPeerView(peer *Peer) PeerView {
	return PeerView{
		Kind:  PeerHuman,
		ID:    peer.ID,
		Name:  peer.ID,
		Lang:  peer.Lang,
		Attrs: map[string]any{},
	}
}
