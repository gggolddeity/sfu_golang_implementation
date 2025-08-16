package main

import (
	"io"
	"log"

	"github.com/gordonklaus/portaudio"
	"github.com/pion/mediadevices"
	_ "github.com/pion/mediadevices/pkg/driver/microphone"
	"github.com/pion/mediadevices/pkg/prop"

	"github.com/hajimehoshi/oto/v2"
	"github.com/pion/mediadevices/pkg/codec/opus"
	opusDecoder "gopkg.in/hraban/opus.v2"
)

func int16SliceToBytes(s []int16) []byte {
	b := make([]byte, len(s)*2)
	for i, v := range s {
		b[2*i] = byte(v)
		b[2*i+1] = byte(v >> 8)
	}
	return b
}

func main() {
	// Инициализируем PortAudio (нужно для некоторых платформ)
	if err := portaudio.Initialize(); err != nil {
		log.Fatal(err)
	}
	defer portaudio.Terminate()

	// Готовим селектор с Opus-энкодером (если нужен, иначе можно убрать Codec)
	opusParams, _ := opus.NewParams()
	codecSelector := mediadevices.NewCodecSelector(
		mediadevices.WithAudioEncoders(&opusParams),
	)

	// Захватываем микрофон
	stream, err := mediadevices.GetUserMedia(mediadevices.MediaStreamConstraints{
		Audio: func(c *mediadevices.MediaTrackConstraints) {
			c.SampleRate = prop.Int(48000)
			c.ChannelCount = prop.Int(2)
		},
		Codec: codecSelector,
	})
	if err != nil {
		log.Fatal("GetUserMedia:", err)
	}
	defer func() {
		for _, t := range stream.GetAudioTracks() {
			t.Close()
		}
	}()

	track := stream.GetAudioTracks()[0]

	pr, pw := io.Pipe()

	otoCtx, ready, err := oto.NewContext(
		48000,
		2,
		oto.FormatSignedInt16LE,
	)
	if err != nil {
		log.Fatal("oto.NewContext:", err)
	}
	<-ready
	player := otoCtx.NewPlayer(pr)
	player.Play()
	defer player.Close()

	erc, err := track.NewEncodedReader("opus")
	if err != nil {
		log.Fatal("NewEncodedReader:", err)
	}
	defer erc.Close()

	dec, err := opusDecoder.NewDecoder(48000, 2)
	if err != nil {
		log.Fatal("opus.NewDecoder:", err)
	}

	go func() {
		defer pw.Close()
		for {
			enc, release, err := erc.Read()
			if err != nil {
				log.Println("Read error:", err)
				return
			}
			pcmBuf := make([]int16, enc.Samples*2)
			n, err := dec.Decode(enc.Data, pcmBuf)
			release()
			if err != nil {
				log.Println("Decode error:", err)
				continue
			}
			// конвертируем только n сэмплов на канал  байты
			raw := int16SliceToBytes(pcmBuf[:n*2])
			if _, err := pw.Write(raw); err != nil {
				log.Println("Pipe write error:", err)
				return
			}
		}
	}()

	// Блокируем main, пока играет звук
	select {}
}
