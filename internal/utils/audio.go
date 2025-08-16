package utils

const i16ToF32 = 1.0 / 32768.0

func Int16ToFloat32(src []int16, dst []float32) {
	// dst должен быть len(src)
	for i, s := range src {
		// -32768 → ~-1.0, 32767 → ~+0.99997
		dst[i] = float32(s) * i16ToF32
	}
}

func StereoToMono48(inStereo []float32) []float32 {
	n := len(inStereo) / 2
	out := make([]float32, n)
	for i := 0; i < n; i++ {
		l := inStereo[2*i]
		r := inStereo[2*i+1]
		out[i] = 0.5 * (l + r)
	}
	return out
}
