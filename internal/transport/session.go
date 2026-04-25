package transport

import (
	"sync"
	"time"
)

// Direction indicates if a file is req (client to server) or res (server to client)
type Direction string

const (
	DirReq Direction = "req"
	DirRes Direction = "res"
)

// Session represents an active proxy connection mapped to files.
type Session struct {
	ID           string
	mu           sync.Mutex
	txBuf        []byte
	txSeq        uint64
	rxSeq        uint64
	rxQueue      map[uint64]*Envelope
	lastActivity time.Time
	closed       bool
	rxClosed     bool // Safely tracks if RxChan was successfully closed
	TargetAddr   string
	ClientID     string

	// Backpressure: blocked when txBuf is too large
	txCond *sync.Cond

	// App channel for receiving data downloaded from remote
	RxChan chan []byte
}

func NewSession(id string) *Session {
	s := &Session{
		ID:           id,
		rxQueue:      make(map[uint64]*Envelope),
		lastActivity: time.Now(),
		RxChan:       make(chan []byte, 1024),
	}
	s.txCond = sync.NewCond(&s.mu)
	return s
}

const DefaultMaxTxBuffer = 2 * 1024 * 1024

func (s *Session) SnapshotTx(maxBytes int) ([]byte, uint64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil, s.txSeq, true
	}

	if len(s.txBuf) == 0 {
		return nil, s.txSeq, false
	}

	n := len(s.txBuf)
	if maxBytes > 0 && n > maxBytes {
		n = maxBytes
	}

	payload := make([]byte, n)
	copy(payload, s.txBuf[:n])

	return payload, s.txSeq, false
}

func (s *Session) CommitTx(seq uint64, sentLen int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if seq != s.txSeq {
		return false
	}

	if sentLen > len(s.txBuf) {
		sentLen = len(s.txBuf)
	}

	copy(s.txBuf, s.txBuf[sentLen:])
	s.txBuf = s.txBuf[:len(s.txBuf)-sentLen]
	s.txSeq++
	s.txCond.Broadcast()

	return true
}

func (s *Session) ProcessRx(env *Envelope) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastActivity = time.Now()

	if s.rxClosed {
		return // Ignore packets if the channel is already safely closed
	}

	if env.Seq == s.rxSeq {
		if len(env.Payload) > 0 {
			s.RxChan <- env.Payload
		}
		s.rxSeq++
		if env.Close {
			s.rxClosed = true
			s.closed = true
			close(s.RxChan)
			return
		}

		// process any queued future packets
		for {
			if nextEnv, ok := s.rxQueue[s.rxSeq]; ok {
				if len(nextEnv.Payload) > 0 {
					s.RxChan <- nextEnv.Payload
				}
				delete(s.rxQueue, s.rxSeq)
				s.rxSeq++
				if nextEnv.Close {
					s.rxClosed = true
					s.closed = true
					close(s.RxChan)
					return
				}
			} else {
				break
			}
		}
	} else if env.Seq > s.rxSeq {
		s.rxQueue[env.Seq] = env
	}
}
