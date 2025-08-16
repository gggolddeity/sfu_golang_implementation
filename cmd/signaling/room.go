package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"rtc_testing/internal"
	"rtc_testing/internal/utils"
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
	"github.com/pion/interceptor/pkg/intervalpli"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4/pkg/media/samplebuilder"
	openai "github.com/sashabaranov/go-openai"
	op2 "gopkg.in/hraban/opus.v2"

	//"github.com/pion/rtp"
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
	samplesPerFrame    = sampleRate * 20 / 1000
)

const OAKey = "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"

type messageData struct {
	MessageType string `json:"message_type"`
	MessageData string `json:"message_data"`
}

type Peer struct {
	ID         string `json:"id"`
	Lang       string `json:"lang"`
	SignalConn *websocket.Conn
	PeerConn   *webrtc.PeerConnection
	inStream   *InStream
	outStream  *OutStream
	messages   chan []byte
	offers     chan []byte
	asrChan    chan []int16
	stopAudio  chan struct{}
	clock      *RoomRTPClock
}

type Room struct {
	mu              sync.RWMutex
	languages       map[string]bool
	Peers           map[string]*Peer
	ID              string
	mixer           *Mixer
	chatMessages    map[string][]byte
	shouldTranslate *atomic.Bool
}

func (m *Mixer) mix(src, dst []float32) {
	for i := 0; i < len(dst) && i < len(src); i++ {
		dst[i] += src[i]
	}
}

func (m *Mixer) addGain(buf []float32, g float32) {
	for i := range buf {
		buf[i] *= g
	}
}

func (m *Mixer) softClipInPlace(buf []float32) {
	for i := range buf {
		x := buf[i]
		// простая аппроксимация: x / (1 + |x|)
		if x >= 0 {
			buf[i] = x / (1 + x)
		} else {
			ax := -x
			buf[i] = x / (1 + ax)
		}
		// при желании можно лимитировать дополнительно до [-1,1]
		if buf[i] > 1 {
			buf[i] = 1
		}
		if buf[i] < -1 {
			buf[i] = -1
		}
	}
}

type RoomRTPClock struct {
	ts          uint32
	seq         uint16
	clockRate   uint32
	PayloadType uint8
	SSRC        uint32
}

func (c *RoomRTPClock) advance(frameDur time.Duration) {
	c.seq++
	incr := uint32(float64(c.clockRate) * frameDur.Seconds()) // 20ms → 960
	c.ts += incr
}

type RoomServer struct {
	rooms       map[string]*Room
	httpSrv     *http.Server
	mu          sync.RWMutex
	mBufferSize int
	oBufferSize int
	logger      *slog.Logger
}

type InStream struct {
	sb        *samplebuilder.SampleBuilder
	dec       *op2.Decoder
	mixChan   chan []float32
	lastFrame []float32
	channels  int
	pending   []int16 // >>20ms
	scratch   []int16
}

type OutStream struct {
	Track *webrtc.TrackLocalStaticRTP
	ready *atomic.Bool
}

func (i *InStream) pushRTP(rtp *rtp.Packet) {
	i.sb.Push(rtp)
}

func (i *InStream) pop20ms() []int16 {
	if i.dec == nil {
		return nil
	}

	need := frameSamples * i.channels // 960 * ch

	// добираем pending до 20мс
	for len(i.pending) < need {
		smp := i.sb.Pop()
		if smp == nil {
			// нет готовых RTP-фреймов — пока не можем отдать 20мс
			return nil
		}

		// декод в scratch (макс 60мс)
		out := i.scratch[:cap(i.scratch)]
		n, err := i.dec.Decode(smp.Data, out)
		if err != nil || n == 0 {
			continue // дроп битого/пустого
		}
		out = out[:n*i.channels] // n — на канал

		// возможно пришло 10/20/40/60мс; просто накапливаем
		i.pending = append(i.pending, out...)
		// цикл продолжится, пока не станет >= need
	}

	// отдаем ровно 20мс, остальное оставляем на следующий вызов
	out := make([]int16, need)
	copy(out, i.pending[:need])
	i.pending = i.pending[need:]
	return out
}

type Mixer struct {
	enc *op2.Encoder
}

type StartConversationRequest struct {
	RoomID      string `json:"room_id"`
	PeerID      string `json:"peer_id"`
	Lang        string `json:"lang"`
	OpenAiToken string `json:"open_ai_token,omitempty"`
}

type StartWebrtcConnectionRequest struct {
	RoomID string `json:"room_id"`
	PeerID string `json:"peer_id"`
}

func NewMixer() (*Mixer, error) {
	enc, err := op2.NewEncoder(sampleRate, 1, op2.AppVoIP)
	if err != nil {
		return nil, err
	}
	return &Mixer{
		enc: enc,
	}, nil
}

func NewPeer(peerId, lang string, messagesBufSize int) *Peer {
	peerInstream := NewInStream()
	peerOutStream := NewOutStream()
	if peerInstream == nil {
		return nil
	}

	return &Peer{
		ID:        peerId,
		Lang:      lang,
		messages:  make(chan []byte, messagesBufSize),
		offers:    make(chan []byte, messagesBufSize),
		asrChan:   make(chan []int16, 12),
		stopAudio: make(chan struct{}),
		inStream:  peerInstream,
		outStream: peerOutStream,
		clock: &RoomRTPClock{
			ts:        rand.Uint32(),
			seq:       uint16(rand.Intn(65535)),
			clockRate: sampleRate,
		},
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

func NewOutStream() *OutStream {
	isReady := &atomic.Bool{}
	isReady.Store(false)
	return &OutStream{
		ready: isReady,
	}
}

func RunServer(port string, logger *slog.Logger) *RoomServer {
	rs := RoomServer{rooms: map[string]*Room{}, mBufferSize: 16, oBufferSize: 16, logger: logger}
	fs := http.FileServer(http.Dir("frontend"))

	mux := http.NewServeMux()
	mux.HandleFunc("/subscribe", rs.subscribeHandler)
	mux.HandleFunc("/create/room", rs.addRoom)
	mux.HandleFunc("/join/room", rs.joinRoom)
	mux.HandleFunc("/publish", rs.publishHandler)
	mux.Handle("/", fs)

	srv := http.Server{
		Addr:    fmt.Sprintf("0.0.0.0:%s", port),
		Handler: mux,
	}
	rs.httpSrv = &srv

	return &rs
}

func getTranscribeFunc(ctx context.Context, out chan<- string) func([]int16) {
	closed := atomic.Bool{}
	closed.Store(false)
	headers := http.Header{
		"Authorization": []string{"Bearer " + OAKey},
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
					// не шлём в закрытый канал
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
				//"model": "gpt-4o-transcribe", // или gpt-4o-transcribe / whisper-1
				"model":    "whisper-1", // или gpt-4o-transcribe / whisper-1
				"language": "ru",
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

	return func(chunk []int16) {
		if closed.Load() {
			return
		}

		if err := wsjson.Write(ctx, conn, map[string]any{
			"type":  "input_audio_buffer.append",
			"audio": internal.Base64Encode(int16ToBytesLE(stereo48kToMono24k(chunk, channels))),
		}); err != nil {
			fmt.Println("write:", err)
			out <- "[write error] " + err.Error()
		}
	}
}

func readRemoteTrackAudio(ctx context.Context, peer *Peer, transcriptFunc func([]int16)) (stop func()) {
	const bufWindowSize = sampleRate * channels / 5

	pcmBuf := make([]int16, 0, frameSamples*channels*50)

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-peer.stopAudio:
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

					transcriptFunc(chunk)
				}
			}
		}
	}()

	return func() { close(peer.stopAudio) }
}

func startWebrtcConnection(ctx context.Context, offerEncoded string, peer *Peer, room *Room) {
	peerOfferChan := peer.offers
	mediaEngine := &webrtc.MediaEngine{}

	if err := mediaEngine.RegisterDefaultCodecs(); err != nil {
		log.Fatal(err)
	}
	interceptorRegistry := &interceptor.Registry{}
	intervalPliFactory, err := intervalpli.NewReceiverInterceptor()
	if err != nil {
		panic(err)
	}
	interceptorRegistry.Add(intervalPliFactory)

	if err = webrtc.RegisterDefaultInterceptors(mediaEngine, interceptorRegistry); err != nil {
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
	if err != nil {
		panic(err)
	}
	defer func() {
		if cErr := peerConnection.Close(); cErr != nil {
			fmt.Printf("cannot close peerConnection: %v\n", cErr)
		}
	}()

	outputTrack, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus}, "audio", "pion")
	if err != nil {
		panic(err)
	}

	transceiver, _ := peerConnection.AddTransceiverFromKind(
		webrtc.RTPCodecTypeAudio,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendrecv},
	)

	params := transceiver.Sender().GetParameters()
	pt := uint8(111)

	for _, c := range params.Codecs {
		if strings.EqualFold(c.MimeType, webrtc.MimeTypeOpus) {
			pt = uint8(c.PayloadType)
			break
		}
	}
	peer.clock.PayloadType = pt

	if len(params.Encodings) > 0 && params.Encodings[0].SSRC != 0 {
		peer.clock.SSRC = uint32(params.Encodings[0].SSRC)
	} else {
		peer.clock.SSRC = rand.Uint32()
	}

	err = transceiver.Sender().ReplaceTrack(outputTrack)
	if err != nil {
		fmt.Println(err)
		return
	}

	resultTranscriptionChan := make(chan string, 40)
	go func() {
		for r := range resultTranscriptionChan {
			fmt.Println(r)
		}
	}()

	transcriptionFn := getTranscribeFunc(ctx, resultTranscriptionChan)
	if transcriptionFn == nil {
		log.Println("transcriptionFn is nil")
		return
	}

	peerConnection.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) { //nolint: revive
		if track.Kind() != webrtc.RTPCodecTypeAudio {
			return
		}

		ch := int(track.Codec().Channels)
		if ch <= 0 {
			ch = 1
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
			rtpd, _, readErr := track.ReadRTP()
			if readErr != nil {
				if !errors.Is(readErr, io.EOF) && !errors.Is(readErr, context.Canceled) {
					fmt.Println("ReadRTP error:", readErr)
				}
				return
			}

			p := *rtpd
			peer.inStream.sb.Push(&p)

			if pcm48 := peer.inStream.pop20ms(); pcm48 != nil {
				select {
				case peer.asrChan <- pcm48:
				default:
				}

				float32Converted := make([]float32, len(pcm48))
				utils.Int16ToFloat32(pcm48, float32Converted)
				if peer.inStream.channels == 2 {
					float32Converted = utils.StereoToMono48(float32Converted)
				}

				select {
				case peer.inStream.mixChan <- float32Converted:
				default:
					log.Println("dropping frames")
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
			peer.outStream.ready.Store(true)
		}

		if connectionState == webrtc.ICEConnectionStateClosed {
			fmt.Println("Ice state closed")
		}
	})

	// Set the handler for Peer connection state
	// This will notify you when the peer has connected/disconnected
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

	// Wait for the offer to be pasted
	offer := webrtc.SessionDescription{}

	internal.DecodeOffer(offerEncoded, &offer)

	// Set the remote SessionDescription
	if err = peerConnection.SetRemoteDescription(offer); err != nil {
		panic(err)
	}

	answer, err := peerConnection.CreateAnswer(&webrtc.AnswerOptions{OfferAnswerOptions: webrtc.OfferAnswerOptions{VoiceActivityDetection: true}})
	if err != nil {
		panic(err)
	}

	// Create channel that is blocked until ICE Gathering is complete
	gatherComplete := webrtc.GatheringCompletePromise(peerConnection)

	// Sets the LocalDescription, and starts our UDP listeners
	if err = peerConnection.SetLocalDescription(answer); err != nil {
		panic(err)
	}

	// Block until ICE Gathering is complete, disabling trickle ICE
	// we do this because we only can exchange one signaling message
	// in a production application you should exchange ICE Candidates via OnICECandidate
	<-gatherComplete

	peer.outStream.Track = outputTrack
	peer.PeerConn = peerConnection
	// Output the answer in base64 so we can paste it in browser
	fmt.Println(internal.EncodeOffer(peerConnection.LocalDescription()))

	answerData := make(map[string]string)
	answerData["answer"] = internal.EncodeOffer(peerConnection.LocalDescription())
	data, err := json.Marshal(answerData)
	if err != nil {
		fmt.Println(err)
		return
	}
	peerOfferChan <- data
	// Block forever
	select {}
}

func (rs *RoomServer) getRoom(id string) *Room {
	rs.mu.RLock()
	defer rs.mu.RUnlock()

	if room, found := rs.rooms[id]; found {
		room.updateTranslationFlag()
		return room
	}

	return nil
}

func (rs *RoomServer) createRoom(id string) *Room {
	mixer, _ := NewMixer()
	shouldTranslate := &atomic.Bool{}
	shouldTranslate.Store(false)

	room := &Room{
		Peers:           make(map[string]*Peer),
		ID:              id,
		mixer:           mixer,
		shouldTranslate: shouldTranslate,
		languages:       make(map[string]bool),
	}
	rs.mu.Lock()
	rs.rooms[id] = room
	rs.mu.Unlock()
	return room
}

func (room *Room) updateTranslationFlag() {
	room.mu.RLock()
	defer room.mu.RUnlock()

	langSet := make(map[string]struct{})
	for _, peer := range room.Peers {
		if peer.Lang != "" {
			langSet[peer.Lang] = struct{}{}
		}
	}

	room.shouldTranslate.Store(len(langSet) > 1)
}

func (rs *RoomServer) removeRoom(id string) (err error) {
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

func (rs *RoomServer) publishHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		w.Write([]byte("you shall not post (passs)"))
		return
	}

	body := http.MaxBytesReader(w, r.Body, 8192)
	data, err := io.ReadAll(body)
	if err != nil {
		w.WriteHeader(http.StatusMethodNotAllowed)
		w.Write([]byte("you shall not post (passs)"))
		return
	}

	err = rs.publish(r.URL.Query().Get("room"), data)

	w.WriteHeader(http.StatusAccepted)
}

func (rs *RoomServer) joinRoom(w http.ResponseWriter, r *http.Request) {
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

	room.AddPeer(peer)

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

func (room *Room) runTicker() {
	ticker := time.NewTicker(time.Millisecond * 20)
	go func() {
		defer ticker.Stop()
		zeros := make([]float32, samplesPerFrame)

		for {
			select {
			case <-ticker.C:
				room.mu.RLock()
				for _, p := range room.Peers {
					mix := make([]float32, samplesPerFrame)

					active := 0
					peerReady := p.outStream.ready.Load()
					if !peerReady || p.outStream.Track == nil || p.inStream == nil {
						continue
					}

					for _, source := range room.Peers {
						f, ok := source.drainOut()
						if !ok {
							f = zeros
						} else {
							active++
						}
						//if source.ID == p.ID {
						//	continue
						//}

						room.mixer.mix(f, mix)
					}

					if active > 1 {
						gain := 1.0 / float32(active)
						room.mixer.addGain(mix, gain)
					}
					room.mixer.softClipInPlace(mix)

					payload := make([]byte, 1500)
					n, err := room.mixer.enc.EncodeFloat32(mix, payload)
					if err != nil {
						log.Println("encode error: ", err)
						continue
					}

					payload = payload[:n]

					if err = p.outStream.Track.WriteRTP(&rtp.Packet{
						Header: rtp.Header{
							Version:        2,
							PayloadType:    p.clock.PayloadType,
							SSRC:           p.clock.SSRC,
							SequenceNumber: p.clock.seq,
							Timestamp:      p.clock.ts,
						},
						Payload: payload,
					}); err == nil {
						p.clock.advance(time.Millisecond * 20)
					} else {
						log.Println(err)
						p.outStream.ready.Store(false)
					}
				}
				room.mu.RUnlock()
			}
		}
	}()
}

func (p *Peer) drainOut() ([]float32, bool) {
	var latest []float32
	var buf = make([]float32, 0)

	for {
		select {
		case f, ok := <-p.inStream.mixChan:
			if !ok {
				if p.inStream.lastFrame != nil {
					return p.inStream.lastFrame, false
				}
				return nil, false
			}
			latest = f
			buf = append(buf, f...)
		default:
			if latest != nil {
				p.inStream.lastFrame = latest
				return buf, true
			}
			if p.inStream.lastFrame != nil {
				return buf, false
			}
			return nil, false
		}
	}
}

//func (p *Peer) drainOut() ([]float32, bool) {
//	var latest []float32
//	for {
//		select {
//		case f, ok := <-p.inStream.mixChan:
//			if !ok {
//				if p.inStream.lastFrame != nil {
//					return p.inStream.lastFrame, false
//				}
//				return nil, false
//			}
//			latest = f
//		default:
//			if latest != nil {
//				p.inStream.lastFrame = latest
//				return latest, true
//			}
//			if p.inStream.lastFrame != nil {
//				return p.inStream.lastFrame, false
//			}
//			return nil, false
//		}
//	}
//}

func (rs *RoomServer) addRoom(w http.ResponseWriter, r *http.Request) {
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

	ctx, cancel := context.WithTimeout(r.Context(), time.Second*3)
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
		room = rs.createRoom(reqData.RoomID)
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

	room.AddPeer(peer)
	room.runTicker()

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

func (rs *RoomServer) subscribeHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		w.Write([]byte("only post method is allowed"))
		return
	}

	var reqData StartWebrtcConnectionRequest

	query := r.URL.Query()

	reqData.RoomID = query.Get("room_id")
	reqData.PeerID = query.Get("peer_id")

	log.Println(reqData)

	if reqData.RoomID == "" || reqData.PeerID == "" {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("room_id and peer_id are required"))
		return
	}

	rs.mu.RLock()
	room, found := rs.rooms[reqData.RoomID]
	log.Println(room, found)
	if !found {
		rs.mu.RUnlock()
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("room not found"))
		return
	}
	rs.mu.RUnlock()

	room.mu.RLock()
	// todo: add get methods for
	peer, found := room.Peers[reqData.PeerID]
	if !found {
		room.mu.RUnlock()
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("room not found"))
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

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	eg, ctx := errgroup.WithContext(ctx)

	eg.Go(func() error {
		for {
			var msg messageData
			if err := wsjson.Read(ctx, c, &msg); err != nil {
				fmt.Println(err)
				return errors.New("failed to read from connection")
			}

			switch msg.MessageType {
			case "offer":
				log.Println("recv offer ", msg.MessageData)
				go startWebrtcConnection(ctx, msg.MessageData, peer, room)
			case "message":
				room.mu.RLock()
				for _, m := range room.Peers {
					if m.ID != peer.ID {
						if msg.MessageType == "message" {
							m.messages <- []byte(msg.MessageData)
						}
					}
				}
				room.mu.RUnlock()
			}
		}
	})

	eg.Go(func() error {
		for {
			select {
			case m := <-peer.messages:
				writeWithTimeout(ctx, peer, m)
				continue
			case o := <-peer.offers:
				writeWithTimeout(ctx, peer, o)
				continue
			case <-ctx.Done():
				rs.removePeer(room.ID, peer.ID)
				return ctx.Err()
			}
		}
	})

	if err := eg.Wait(); err != nil {
		log.Printf("ws error: %v", err)
	}

	c.Close(websocket.StatusNormalClosure, "")
}

func (rs *RoomServer) publish(roomID string, data []byte) error {
	rs.mu.RLock()
	room, ok := rs.rooms[roomID]
	rs.mu.RUnlock()
	if !ok {
		return errors.New("room not found")
	}

	room.mu.RLock()
	peers := make([]*Peer, 0, len(room.Peers))
	for _, p := range room.Peers {
		peers = append(peers, p)
	}
	room.mu.RUnlock()

	for _, p := range peers { // уже без замков
		select {
		case p.messages <- data:
		default:
			rs.logger.Warn("dropping", "peer", p.ID)
		}
	}

	return nil
}

func int16ToBytesLE(xs []int16) []byte {
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.LittleEndian, xs)
	return buf.Bytes()
}

func stereo48kToMono24k(in []int16, channels int) []int16 {
	switch channels {
	case 1:
		// 48k mono -> 24k mono: усредняем пары соседних сэмплов и децимируем ×2
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
		// на всякий случай
		return nil
	}
}

func writeWithTimeout(ctx context.Context, member *Peer, m []byte) {
	timeoutCtx, cancel := context.WithTimeout(ctx, time.Second*3)
	defer cancel()
	member.SignalConn.Write(timeoutCtx, websocket.MessageText, m)
}

func (rs *RoomServer) removePeer(roomID, memberID string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	if room, exists := rs.rooms[roomID]; exists {
		room.mu.Lock()
		defer room.mu.Unlock()
		member, memberFound := room.Peers[memberID]
		if memberFound {
			close(member.messages)
			delete(room.Peers, memberID)
		}

		empty := len(room.Peers) == 0
		if empty {
			delete(rs.rooms, roomID)
		}
	}
}

func (room *Room) AddPeer(peer *Peer) {
	room.mu.Lock()
	room.Peers[peer.ID] = peer
	if _, foundLang := room.languages[peer.Lang]; !foundLang {
		room.languages[peer.Lang] = true
	}
	room.mu.Unlock()

	room.updateTranslationFlag()
}
