package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync"

	"github.com/pion/opus"
	_ "github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	openai "github.com/sashabaranov/go-openai"
)

// Client represents a single browser/client connection
// Each client announces its preferred language via the signaling message.
type Client struct {
	ID   string
	Lang string

	PC          *webrtc.PeerConnection
	InTrack     *webrtc.TrackRemote
	cancelMedia context.CancelFunc

	// For every other client we keep an outbound audio track so that
	// we can fan-out translated or original audio.
	outTracks map[string]*webrtc.TrackLocalStaticRTP
}

// Server keeps track of all connected clients and the OpenAI helper.
type Server struct {
	mu      sync.RWMutex
	clients map[string]*Client
	oa      *openai.Client
}

func newServer(apiKey string) *Server {
	cfg := openai.DefaultConfig(apiKey)
	return &Server{
		clients: make(map[string]*Client),
		oa:      openai.NewClientWithConfig(cfg),
	}
}

// --- Signaling ---

// offerRequest is the payload we expect from the browser: SDP offer + metadata.
type offerRequest struct {
	SDP      string `json:"sdp"`
	Type     string `json:"type"`
	ClientID string `json:"client_id"`
	Lang     string `json:"lang"` // ISO-639-1, e.g. "en", "ru"
}

type answerResponse struct {
	SDP  string `json:"sdp"`
	Type string `json:"type"`
}

func (s *Server) handleOffer(w http.ResponseWriter, r *http.Request) {
	var req offerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Create peer connection
	mediaEngine := &webrtc.MediaEngine{}
	mediaEngine.RegisterDefaultCodecs()
	api := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine))

	pc, err := api.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302"}}},
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	client := &Client{
		ID:        req.ClientID,
		Lang:      req.Lang,
		PC:        pc,
		outTracks: map[string]*webrtc.TrackLocalStaticRTP{},
	}

	// When a remote ICE candidate is found, forward through HTTP polling WS, etc.
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		// Application-specific: send to client via signaling channel.
	})

	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		if track.Kind() != webrtc.RTPCodecTypeAudio {
			return
		}
		client.InTrack = track
		ctx, cancel := context.WithCancel(context.Background())
		client.cancelMedia = cancel
		go s.handleIncomingAudio(ctx, client)
	})

	// Set remote description
	if err = pc.SetRemoteDescription(webrtc.SessionDescription{SDP: req.SDP, Type: webrtc.NewSDPType(req.Type)}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Create answer
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err = pc.SetLocalDescription(answer); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Keep track of the client after successful negotiation
	s.addClient(client)

	// Respond with answer
	resp := answerResponse{SDP: answer.SDP, Type: answer.Type.String()}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) addClient(c *Client) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clients[c.ID] = c

	// Create outbound tracks towards existing clients (both directions)
	for _, other := range s.clients {
		if other.ID == c.ID {
			continue
		}

		// From new client to existing peer
		s.attachOutboundTrack(c, other)
		// From existing peer to new client
		s.attachOutboundTrack(other, c)
	}
}

// attachOutboundTrack ensures that 'src' has an outbound RTP track that arrives at 'dst'.
// If languages are the same we forward raw Opus RTP; otherwise we will deliver translated audio.
func (s *Server) attachOutboundTrack(src, dst *Client) {
	// Pion factory for local RTP track
	codec := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}
	localTrack, err := webrtc.NewTrackLocalStaticRTP(codec, fmt.Sprintf("%s_to_%s", src.ID, dst.ID), "audio")
	if err != nil {
		log.Printf("track create: %v", err)
		return
	}
	sender, err := dst.PC.AddTrack(localTrack)
	if err != nil {
		log.Printf("add track: %v", err)
		return
	}
	go func() {
		// Read RTCP to keep sender alive
		rtcpBuf := make([]byte, 1500)
		for {
			if _, _, rtcpErr := sender.Read(rtcpBuf); rtcpErr != nil {
				return
			}
		}
	}()
	src.outTracks[dst.ID] = localTrack
}

// --- Media handling ---

func (s *Server) handleIncomingAudio(ctx context.Context, src *Client) {
	buf := make([]byte, 1400)

	for {
		n, _, readErr := src.InTrack.Read(buf)
		if readErr != nil {
			log.Printf("read RTP: %v", readErr)
			return
		}

		// Fan-out
		s.mu.RLock()
		for _, dst := range s.clients {
			out := src.outTracks[dst.ID]
			if out == nil {
				continue
			}
			if dst.Lang == src.Lang {
				// Same language – no processing.
				if writeErr := out.WriteRTP(buf[:n]); writeErr != nil {
					log.Println("write rtp:", writeErr)
				}
			} else {
				// Different language – translate async
				go s.translateAndSend(ctx, buf[:n], out, dst.Lang)
			}
		}
		s.mu.RUnlock()
	}
}

// translateAndSend decodes one Opus packet, accumulates PCM, performs STT -> translate -> TTS
// and writes the resulting RTP to the outgoing track.
func (s *Server) translateAndSend(ctx context.Context, pkt []byte, out *webrtc.TrackLocalStaticRTP, targetLang string) {
	// 1. Decode Opus -> PCM
	dec, _ := opus.NewDecoder(48000, 2)
	pcm := make([]int16, 1920*2) // 20ms @ 48kHz, stereo
	samples, err := dec.Decode(pkt, pcm)
	if err != nil {
		log.Println("decode opus:", err)
		return
	}
	pcm = pcm[:samples*2]

	// 2. Send to Whisper (speech->text):
	transcript, err := s.whisper(ctx, pcm)
	if err != nil {
		log.Println(err)
		return
	}

	// 3. Translate (if needed) via ChatCompletion.
	translatedText := transcript
	if targetLang != "" {
		translatedText, err = s.translateText(ctx, transcript, targetLang)
		if err != nil {
			log.Println(err)
			return
		}
	}

	// 4. TTS -> PCM -> Opus
	ttsPCM, err := s.tts(ctx, translatedText, targetLang)
	if err != nil {
		log.Println(err)
		return
	}

	enc, _ := opus.NewEncoder(48000, 2, opus.AppVoIP)
	opusBuf := make([]byte, 4000)
	opusLen, _ := enc.Encode(tTS_pcm_to_int16(ttsPCM), opusBuf)

	if err := out.WriteRTP(opusBuf[:opusLen]); err != nil {
		log.Println("write translated rtp:", err)
	}
}

// Placeholder stubs – fill in with real OpenAI calls.
func (s *Server) whisper(ctx context.Context, pcm []int16) (string, error) {
	// Stream via Whisper-Streaming or chunk + REST.
	return "TODO", nil
}

func (s *Server) translateText(ctx context.Context, text, targetLang string) (string, error) {
	resp, err := s.oa.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model:    "gpt-4o-realtime-preview",
		Messages: []openai.ChatCompletionMessage{{Role: "system", Content: fmt.Sprintf("Translate to %s", targetLang)}, {Role: "user", Content: text}},
		Stream:   false,
	})
	if err != nil {
		return "", err
	}
	return resp.Choices[0].Message.Content, nil
}

func (s *Server) tts(ctx context.Context, text, lang string) ([]byte, error) {
	audio, err := s.oa.CreateSpeech(ctx, openai.CreateSpeechRequest{
		Model:          "tts-1-streaming",
		Input:          text,
		Voice:          "alloy",
		ResponseFormat: openai.SpeechResponseFormatOpus,
		// For real-time you can use streaming chunked response.
	})
	if err != nil {
		return nil, err
	}
	return io.ReadAll(audio)
}

// Utility helper to convert []byte PCM from TTS to []int16 for Opus.
func tTS_pcm_to_int16(in []byte) []int16 {
	out := make([]int16, len(in)/2)
	for i := 0; i < len(out); i++ {
		out[i] = int16(in[2*i]) | int16(in[2*i+1])<<8
	}
	return out
}

func main() {
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		log.Fatalln("OPENAI_API_KEY env var is required")
	}

	srv := newServer(apiKey)
	http.HandleFunc("/offer", srv.handleOffer)
	log.Println("server :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
