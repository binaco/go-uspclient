package uspclient

import (
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/refractionPOINT/go-uspclient/protocol"
)

const (
	defaultCapacity      = 5000
	ackPercentOfCapacity = 0.5
)

type AckBufferOptions struct {
	BufferCapacity uint64 `json:"buffer_capacity" yaml:"buffer_capacity"`
	OnBackPressure func() `json:"-" yaml:"-"`
	OnAck          func() `json:"-" yaml:"-"`
}

type AckBuffer struct {
	sync.RWMutex

	isRunning   bool
	isAvailable *Event

	buff          []*protocol.DataMessage
	nextIndexFree uint64
	firstSeqNum   uint64
	nextSeqNum    uint64
	ackEvery      uint64

	nextIndexToDeliver uint64
	isReadyToDeliver   *Event

	onBackPressure func()
	onAck          func()
}

func NewAckBuffer(o AckBufferOptions) (*AckBuffer, error) {
	if o.BufferCapacity == 0 {
		o.BufferCapacity = defaultCapacity
	}
	b := &AckBuffer{
		buff:             make([]*protocol.DataMessage, o.BufferCapacity),
		ackEvery:         uint64(float64(o.BufferCapacity) * ackPercentOfCapacity),
		isRunning:        true,
		isAvailable:      NewEvent(),
		firstSeqNum:      1,
		nextSeqNum:       1,
		isReadyToDeliver: NewEvent(),
		onBackPressure:   o.OnBackPressure,
		onAck:            o.OnAck,
	}
	b.isAvailable.Set()

	return b, nil
}

func (b *AckBuffer) Close() {
	b.Lock()
	defer b.Unlock()
	b.isRunning = false
}

func (b *AckBuffer) Add(e *protocol.DataMessage, timeout time.Duration) bool {
	hasBeenAdded := false
	var deadline time.Time
	if timeout != 0 {
		deadline = time.Now().Add(timeout)
	}
	for !hasBeenAdded {
		if !deadline.IsZero() {
			now := time.Now()
			if now.After(deadline) {
				break
			}
			if b.onBackPressure != nil && !b.isAvailable.IsSet() {
				b.onBackPressure()
			}
			if !b.isAvailable.WaitFor(deadline.Sub(now)) {
				break
			}
		} else {
			b.isAvailable.WaitFor(500 * time.Millisecond)
		}

		b.Lock()
		if !b.isAvailable.IsSet() {
			isRunning := b.isRunning
			b.Unlock()
			if deadline.IsZero() && isRunning {
				continue
			}
			break
		}

		e.SeqNum = b.nextSeqNum
		b.nextSeqNum++
		if e.SeqNum%b.ackEvery == 0 {
			e.AckRequested = true
		}
		b.buff[b.nextIndexFree] = e
		b.nextIndexFree++
		if b.nextIndexFree >= uint64(len(b.buff)) {
			b.isAvailable.Clear()
		}
		hasBeenAdded = true
		b.isReadyToDeliver.Set()
		b.Unlock()
	}

	return hasBeenAdded
}

func (b *AckBuffer) Ack(seq uint64) error {
	if b.onAck != nil {
		b.onAck()
	}
	b.Lock()
	defer b.Unlock()
	if seq < b.firstSeqNum && uint64(len(b.buff))-(math.MaxUint64-b.firstSeqNum) <= seq {
		return fmt.Errorf("unexpected acked sequence number: %d", seq)
	}
	if seq >= b.nextSeqNum && b.nextSeqNum >= math.MaxUint64-seq {
		return fmt.Errorf("unexpected acked sequence number: %d", seq)
	}
	indexAcked := seq - b.firstSeqNum
	if indexAcked >= b.nextIndexToDeliver {
		nextSeq := uint64(0)
		if b.buff[b.nextIndexToDeliver] != nil {
			nextSeq = b.buff[b.nextIndexToDeliver].SeqNum
		}
		return fmt.Errorf("acked message (seq: %d) has not yet been delivered (next: %d)", seq, nextSeq)
	}
	b.firstSeqNum = seq + 1
	for i := indexAcked + 1; i < uint64(len(b.buff)); i++ {
		b.buff[i-indexAcked-1] = b.buff[i]
	}
	b.nextIndexFree = b.nextIndexFree - indexAcked - 1
	for i := b.nextIndexFree; i < uint64(len(b.buff)); i++ {
		b.buff[i] = nil
	}
	b.isAvailable.Set()
	b.nextIndexToDeliver -= (indexAcked + 1)
	if b.nextIndexToDeliver >= b.nextIndexFree {
		b.isReadyToDeliver.Clear()
	}
	return nil
}

func (b *AckBuffer) GetUnAcked() ([]*protocol.DataMessage, error) {
	b.RLock()
	defer b.RUnlock()
	out := make([]*protocol.DataMessage, b.nextIndexFree)
	copy(out, b.buff[:b.nextIndexFree])
	return out, nil
}

func (b *AckBuffer) GetNextToDeliver(timeout time.Duration) *protocol.DataMessage {
	if !b.isReadyToDeliver.WaitFor(timeout) {
		return nil
	}
	b.Lock()
	defer b.Unlock()
	if !b.isReadyToDeliver.IsSet() {
		return nil
	}
	n := b.buff[b.nextIndexToDeliver]
	b.nextIndexToDeliver++
	if b.nextIndexToDeliver >= b.nextIndexFree {
		b.isReadyToDeliver.Clear()
	}
	return n
}

func (b *AckBuffer) ResetDelivery() {
	b.Lock()
	defer b.Unlock()
	b.nextIndexToDeliver = 0
	if b.nextIndexToDeliver >= b.nextIndexFree {
		b.isReadyToDeliver.Clear()
	} else {
		b.isReadyToDeliver.Set()
	}
}
