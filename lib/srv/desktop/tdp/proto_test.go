/*
Copyright 2021 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package tdp

import (
	"bufio"
	"encoding/json"
	"image"
	"image/color"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/gravitational/trace"
	"github.com/stretchr/testify/require"
)

func TestEncodeDecode(t *testing.T) {
	for _, m := range []Message{
		MouseMove{X: 1, Y: 2},
		MouseButton{Button: MiddleMouseButton, State: ButtonPressed},
		KeyboardButton{KeyCode: 1, State: ButtonPressed},
		func() Message {
			img := image.NewNRGBA(image.Rect(5, 5, 10, 10))
			for x := img.Rect.Min.X; x < img.Rect.Max.X; x++ {
				for y := img.Rect.Min.Y; y < img.Rect.Max.Y; y++ {
					img.Set(x, y, color.NRGBA{1, 2, 3, 4})
				}
			}
			return PNGFrame{Img: img}
		}(),
		ClientScreenSpec{Width: 123, Height: 456},
		ClientUsername{Username: "admin"},
		MouseWheel{Axis: HorizontalWheelAxis, Delta: -123},
		Error{Message: "An error occurred"},
	} {

		buf, err := m.Encode()
		require.NoError(t, err)

		out, err := Decode(buf)
		require.NoError(t, err)

		require.Empty(t, cmp.Diff(m, out, cmpopts.IgnoreUnexported(PNGFrame{})))
	}
}

func TestBadDecode(t *testing.T) {
	// 254 is an unknown message type.
	_, err := Decode([]byte{254})
	require.Error(t, err)
}

func TestRejectsLongUsername(t *testing.T) {
	clientUsername := []byte{byte(TypeClientUsername), 0x00, 0x00, 0x10, 0x00, 'a', 'b', 'c', 'd'}
	_, err := Decode(clientUsername)
	require.True(t, trace.IsBadParameter(err))
}

var encodedFrame []byte

func BenchmarkEncodePNG(b *testing.B) {
	b.StopTimer()
	frames := loadBitmaps(b)
	b.StartTimer()
	var err error
	for i := 0; i < b.N; i++ {
		fi := i % len(frames)
		encodedFrame, err = frames[fi].Encode()
		if err != nil {
			b.Fatal(err)
		}
	}
}

func loadBitmaps(b *testing.B) []PNGFrame {
	b.Helper()

	f, err := os.Open(filepath.Join("testdata", "png_frames.json"))
	require.NoError(b, err)
	defer f.Close()

	enc := PNGEncoder()

	var result []PNGFrame
	type record struct {
		Top, Left, Right, Bottom int
		Pix                      []byte
	}
	s := bufio.NewScanner(f)
	for s.Scan() {
		var r record
		require.NoError(b, json.Unmarshal(s.Bytes(), &r))

		img := image.NewNRGBA(image.Rectangle{
			Min: image.Pt(r.Left, r.Top),
			Max: image.Pt(r.Right, r.Bottom),
		})
		copy(img.Pix, r.Pix)
		result = append(result, NewPNG(img, enc))
	}
	require.NoError(b, s.Err())
	return result
}
