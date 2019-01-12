package main

import (
	"fmt"
	"github.com/derlaft/synco/protocol"
	"google.golang.org/grpc"
	"log"
	"net"
	"os"
	"sync"
	"time"
)

type syncoServer struct{}

type client struct {
	id       string
	stream   protocol.Synco_ConnectServer
	ready    bool
	waste    bool
	done     chan interface{}
	lastPing time.Time
}

var (
	clients     = map[string]*client{}
	clientsLock = sync.Mutex{}
)

const (
	pingTimeoutTime = time.Second * 5
)

func addClient(c *client) error {
	_, ok := clients[c.id]
	if ok {
		delClient(c.id)
	}

	log.Printf("Client %v registered", c.id)
	clientsLock.Lock()
	clients[c.id] = c
	clientsLock.Unlock()
	return nil
}

func delClient(id string) {
	log.Printf("Client %v deregistered", id)
	clientsLock.Lock()
	clients[id].waste = true
	delete(clients, id)
	clientsLock.Unlock()
}

func (c *client) send(msg *protocol.Event) {
	err := c.stream.Send(msg)
	if err != nil {
		log.Printf("Could not send event to client %v: %v", c.id, err)
	}
}

func broadcast(origin string, event *protocol.Event) {
	clientsLock.Lock()
	for name, client := range clients {
		if origin == name {
			continue
		}

		go client.send(event)
	}
	clientsLock.Unlock()
}

func (c *client) setReady(ready bool) {
	if c.ready == ready {
		return
	}

	c.ready = ready

	if ready {
		log.Printf("client %v is now ready", c.id)
	} else {
		log.Printf("client %v is now not ready", c.id)
	}

	if !ready {
		broadcast(c.id, &protocol.Event{
			Reason: fmt.Sprintf("client %v is no more ready", c.id),
			Go: &protocol.GoEvent{
				Playback: false,
			},
		})

		// not forget to mark them non-ready locally
		clientsLock.Lock()
		for _, cl := range clients {
			cl.ready = false
		}
		clientsLock.Unlock()

	} else if canStart() {
		log.Println("Everyone is ready, starting")
		broadcast("", &protocol.Event{ // send to everyone, do not skip sender
			Reason: fmt.Sprintf("everyone is ready", c.id),
			Go: &protocol.GoEvent{
				Playback: true,
			},
		})
	}
}

// check if everyone is ready && number of people is kk
func canStart() bool {

	clientsLock.Lock()
	defer clientsLock.Unlock()
	for _, client := range clients {
		if !client.ready {
			return false
		}
	}

	return len(clients) > 1

}

func unreadyAll() {

	clientsLock.Lock()
	defer clientsLock.Unlock()
	for _, client := range clients {
		client.ready = false
	}
}

func main() {

	if len(os.Args) < 2 {
		log.Fatal(fmt.Errorf("Usage: %v listenaddr:port", os.Args[0]))
	}

	lis, err := net.Listen("tcp", os.Args[1])
	if err != nil {
		log.Fatal(err)
	}

	server := grpc.NewServer()
	protocol.RegisterSyncoServer(server, &syncoServer{})
	server.Serve(lis)

	return
}

func (c *client) healthPings() {
	var shitHappened = false
	for {
		select {
		case <-time.Tick(time.Millisecond * 500):
			if time.Since(c.lastPing) > pingTimeoutTime && !shitHappened {
				broadcast("", &protocol.Event{
					Reason: fmt.Sprintf("client %v did not respond ping in time", c.id),
					Go: &protocol.GoEvent{
						Playback: false,
					},
				})
				unreadyAll()
				shitHappened = true
			} else if time.Since(c.lastPing) < pingTimeoutTime && shitHappened {
				shitHappened = false
			}
		case <-c.done:
			return
		}
	}
}

func (s *syncoServer) Connect(stream protocol.Synco_ConnectServer) error {

	var (
		helloMsg *protocol.HelloEvent
		done     = make(chan interface{})
		this     *client
	)

	for {
		resp, err := stream.Recv()
		if err != nil {
			log.Println(err)
			break
		}

		if this != nil && this.waste {
			log.Println("Client seems to reconnect; removing the old one")
			break
		}

		switch {
		case helloMsg == nil && resp.Hello == nil:
			log.Printf("First client message is not hello (%v)", resp)
			break
		case resp.Hello != nil: // hello packet -- init shit
			helloMsg = resp.Hello
			if helloMsg.Id <= "" {
				log.Println("Empty client id!")
				break
			}
			this = &client{
				id:     helloMsg.Id,
				stream: stream,
				done:   done,
			}
			err := addClient(this)
			if err != nil {
				log.Print(err)
				break
			}
			defer delClient(helloMsg.Id)

			go this.healthPings()

		case resp.Ready != nil: // client is (not) ready, update info and probably broadcast shit
			this.setReady(resp.Ready.ClientReady)

		case resp.Time != nil: // broadcast seek
			broadcast(helloMsg.Id, resp)

		case resp.Ping != nil: // ping message
			this.lastPing = time.Now()
			this.send(resp)
		default:
			log.Printf("Unknown message type (%#v)", resp)
		}
	}

	if this != nil {
		this.done <- nil
	}
	return nil
}