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

package srv

import (
	"sync"
	"sync/atomic"

	"github.com/gravitational/teleport/lib/utils"
	"github.com/gravitational/trace"
	log "github.com/sirupsen/logrus"
)

// BreakReader implements a reader wrapper that allows the connection
// to be temporarily paused at any moment.
type BreakReader struct {
	remaining []byte
	cond      *sync.Cond
	in        chan []byte
	on        bool
	R         *utils.TrackingReader
	closed    *int32
}

// NewBreakReader crates a new BreakReader from an underlying tracking reader.
func NewBreakReader(r *utils.TrackingReader) *BreakReader {
	data := make(chan []byte)
	closedVal := int32(0)
	closed := &closedVal

	go func() {
		for {
			buf := make([]byte, 1024)
			n, err := r.Read(buf)
			if err != nil {
				log.Error("BreakReader: failed to read from reader.")
				return
			}

			if atomic.LoadInt32(closed) == 1 {
				return
			}

			data <- buf[:n]
		}
	}()

	return &BreakReader{
		cond:   sync.NewCond(&sync.Mutex{}),
		in:     data,
		on:     true,
		R:      r,
		closed: closed,
	}
}

// On allows data to flow through the reader.
func (r *BreakReader) On() {
	r.cond.L.Lock()
	defer r.cond.L.Unlock()
	r.on = true
	r.cond.Broadcast()
}

// Off restricts data from flowing through the reader.
func (r *BreakReader) Off() {
	r.cond.L.Lock()
	defer r.cond.L.Unlock()
	r.on = false
	r.cond.Broadcast()
}

func (r *BreakReader) Read(p []byte) (int, error) {
	if len(r.remaining) > 0 {
		n := copy(p, r.remaining)
		r.remaining = r.remaining[n:]
		return n, nil
	}

	q := make(chan struct{})
	c := make(chan bool)
	go func() {
		r.cond.L.Lock()

	outer:
		for {
			select {
			case c <- r.on:
			case <-q:
				close(c)
				break outer
			}

			r.cond.Wait()
		}

		r.cond.L.Unlock()
	}()

	on := <-c
	for {
		if !on {
			on = <-c
			continue
		}

		select {
		case on = <-c:
			continue
		case r.remaining = <-r.in:
			close(q)
			n := copy(p, r.remaining)
			r.remaining = r.remaining[n:]
			return n, nil
		}
	}
}

// Close closes the reader and stops the dataflow but does not close the underlying reader.
func (r *BreakReader) Close() {
	atomic.StoreInt32(r.closed, 1)
}

// SwitchWriter implements a writer wrapper that allows the stream to be temporarily paused at any moment.
// It also allows unconditional writes to allow for message broadcasts.
type SwitchWriter struct {
	mu     sync.Mutex
	W      *utils.TrackingWriter
	buffer []byte
	on     bool
}

// NewSwitchWriter creates a new SwitchWriter from an underlying tracking writer.
func NewSwitchWriter(w *utils.TrackingWriter) *SwitchWriter {
	return &SwitchWriter{
		W:  w,
		on: true,
	}
}

// On allows data to flow through the writer.
func (w *SwitchWriter) On() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.on = true
	_, err := w.W.Write(w.buffer)
	return trace.Wrap(err)
}

// Off buffers incoming writes until the writer is turned on again.
func (w *SwitchWriter) Off() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.on = false
}

func (w *SwitchWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.on {
		return w.W.Write(p)
	}

	w.buffer = append(w.buffer, p...)
	return len(p), nil
}

// WriteUnconditional allows unconditional writes to the underlying writer.
func (w *SwitchWriter) WriteUnconditional(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.W.Write(p)
}
