package internal

import (
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/pion/webrtc/v4"
)

func Base64Encode(data []byte) string {
	enc := base64.StdEncoding.EncodeToString(data)
	return enc
}

// todo: wytf I did that
// JSON EncodeOffer + base64 a SessionDescription.
func EncodeOffer(obj *webrtc.SessionDescription) string {
	b, err := json.Marshal(obj)
	if err != nil {
		panic(err)
	}

	return base64.StdEncoding.EncodeToString(b)
}

// DecodeOffer a base64 and unmarshal JSON into a SessionDescription.
func DecodeOffer(in string, obj *webrtc.SessionDescription) {
	b, err := base64.StdEncoding.DecodeString(in)
	fmt.Println(b)
	if err != nil {
		panic(err)
	}

	if err = json.Unmarshal(b, obj); err != nil {
		panic(err)
	}
}
