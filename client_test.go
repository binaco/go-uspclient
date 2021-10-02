package uspclient

import (
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
	EnableCompression: true,
}

func TestConnection(t *testing.T) {
	testOID := uuid.NewString()
	testIID := "456"
	testHostname := "testhost"
	testPlatform := "text"
	testArchitecture := "usp_adapter"
	testPort := 7777
	nConnections := uint32(0)
	wg := sync.WaitGroup{}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		isConnReceived := false
		isHeaderReceived := false
		isHeaderValid := false

		defer func() {
			if !isConnReceived {
				t.Error("connection not received")
			}
			if !isHeaderReceived {
				t.Error("header not received")
			}
			if !isHeaderValid {
				t.Error("header not valid")
			}
		}()

		if r.URL.Path != "/usp" {
			t.Errorf("unexpected URL path: %s", r.URL.Path)
			return
		}
		isConnReceived = true
		conn, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("Upgrade(): %v", err)
			return
		}

		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		msg := connectionHeader{}
		if err := conn.ReadJSON(&msg); err != nil {
			t.Errorf("ReadJSON(): %v", err)
			return
		}
		isHeaderReceived = true

		oid := msg.Oid
		iid := msg.InstallationKey
		hostname := msg.Hostname
		hint := msg.Platform
		arch := msg.Architecture

		if oid != testOID || iid != testIID || hostname != testHostname || hint != testPlatform || arch != testArchitecture {
			fmt.Printf("invalid headers: %#v\n", msg)
			return
		}
		isHeaderValid = true
		nConnections++
		atomic.AddUint32(&nConnections, 1)

		m := sync.Mutex{}
		if atomic.LoadUint32(&nConnections) == 1 {
			// Only do a reconnect on the first connection.
			wg.Add(1)
			go func() {
				defer wg.Done()
				time.Sleep(5 * time.Second)
				m.Lock()
				defer m.Unlock()
				conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
				if err := conn.WriteJSON(uspControlMessage{
					Verb: uspControlMessageRECONNECT,
				}); err != nil {
					fmt.Printf("WriteJSON(): %v\n", err)
					return
				}
			}()
		}

		for {
			conn.SetReadDeadline(time.Now().Add(20 * time.Second))
			msg := UspDataMessage{}
			if err := conn.ReadJSON(&msg); err != nil {
				fmt.Printf("ReadJSON(): %v\n", err)
				return
			}

			if msg.AckRequested {
				m.Lock()
				conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
				if err := conn.WriteJSON(&uspControlMessage{
					Verb:   uspControlMessageACK,
					SeqNum: msg.SeqNum,
				}); err != nil {
					fmt.Printf("WriteJSON(): %v\n", err)
					m.Unlock()
					return
				}
				m.Unlock()
			}
			if v, ok := msg.JsonPayload["some"]; !ok || v != "payload" {
				t.Errorf("missing payload: %#v", msg)
				return
			}
		}
	})
	srv := &http.Server{
		Handler: h,
		Addr:    fmt.Sprintf("127.0.0.1:%d", testPort),
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := srv.ListenAndServe(); err != nil {
			fmt.Printf("ListenAndServe(): %v\n", err)
		}
	}()
	time.Sleep(2 * time.Second)

	c, err := NewClient(ClientOptions{
		Identity: Identity{
			Oid:             testOID,
			InstallationKey: testIID,
		},
		DestURL:      fmt.Sprintf("ws://127.0.0.1:%d/usp", testPort),
		Hostname:     testHostname,
		Platform:     testPlatform,
		Architecture: testArchitecture,
		DebugLog: func(s string) {
			fmt.Println(s)
		},
		BufferOptions: AckBufferOptions{
			BufferCapacity: 10,
		},
	})
	if err != nil {
		t.Errorf("NewClient(): %v", err)
		return
	}

	time.Sleep(2 * time.Second)

	for i := 0; i < 30; i++ {
		if err := c.Ship(&UspDataMessage{
			JsonPayload: map[string]interface{}{
				"some": "payload",
			},
		}, 1*time.Second); err != nil {
			t.Errorf("Ship(): %v", err)
			break
		}
	}

	time.Sleep(10 * time.Second)

	c.Close()

	if atomic.LoadUint32(&nConnections) != 2 {
		t.Errorf("unexpected number of total connections: %d", atomic.LoadUint32(&nConnections))
	}

	srv.Close()
	wg.Wait()
}
